package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenShape 锁定 wire 契约:每个 key 必须是 [xxx] 形式的消息正文 token。老消息/老客户端
// 依赖它逐字节不变,清单里出现非 [xxx] 的 key 即是破坏性改动。
var tokenShape = regexp.MustCompile(`^\[[^\]]+\]$`)

// TestEmojiManifest_Embedded 校验内嵌真源能被解析,且内容符合契约:非空、版本 >=1、
// 每个 key 是 [xxx] token、内置项 URL 留空(客户端复用本地图)、ETag 非空。纯内存,无需 DB。
func TestEmojiManifest_Embedded(t *testing.T) {
	loadEmojiManifest()

	require.NotEmpty(t, emojiManifestValue.List, "内置表情清单不应为空")
	assert.GreaterOrEqual(t, emojiManifestValue.Version, 1, "version 必须 >=1")
	assert.NotEmpty(t, emojiManifestETag, "ETag 必须预计算")

	// 当前内置自定义表情集合(顺序敏感)。
	wantKeys := []string{"[使命必达]", "[崇尚行动]", "[有品位]", "[尚方宝剑]"}
	gotKeys := make([]string, 0, len(emojiManifestValue.List))
	for _, e := range emojiManifestValue.List {
		gotKeys = append(gotKeys, e.Key)
		assert.Regexp(t, tokenShape, e.Key, "key 必须是 [xxx] token: %q", e.Key)
		assert.NotEmpty(t, e.Name, "name 不应为空: %q", e.Key)
		assert.Empty(t, e.URL, "内置表情 URL 应留空(客户端复用本地图): %q", e.Key)
	}
	assert.Equal(t, wantKeys, gotKeys, "内置表情集合/顺序与真源一致")
}

// TestParseEmojiManifest_Validation 锁定启动期语义校验:合法清单通过,各类非法清单(坏 JSON、
// version<1、空 list、非 [xxx] token、重复 key、空 name)都被拒 —— 即便将来 manifest 被改坏
// 且 endpoint 测试被弱化,New() 里的 loadEmojiManifest 也会 fail-fast。
func TestParseEmojiManifest_Validation(t *testing.T) {
	if _, err := parseEmojiManifest([]byte(`{"version":1,"list":[{"key":"[a]","name":"A","url":""}]}`)); err != nil {
		t.Fatalf("valid manifest should parse, got: %v", err)
	}
	bad := map[string]string{
		"bad json":      `{`,
		"version < 1":   `{"version":0,"list":[{"key":"[a]","name":"A","url":""}]}`,
		"empty list":    `{"version":1,"list":[]}`,
		"empty key":     `{"version":1,"list":[{"key":"","name":"A","url":""}]}`,
		"non-token key": `{"version":1,"list":[{"key":"a","name":"A","url":""}]}`,
		"empty name":    `{"version":1,"list":[{"key":"[a]","name":"","url":""}]}`,
		"duplicate key": `{"version":1,"list":[{"key":"[a]","name":"A","url":""},{"key":"[a]","name":"B","url":""}]}`,
	}
	for name, body := range bad {
		if _, err := parseEmojiManifest([]byte(body)); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

// newEmojiTestRouter 构造一个仅挂载 emoji 清单公开路由的最小 wkhttp 路由。emojiManifest
// 不读取任何 *Common 字段,故零值 &Common{} 即可,无需 DB/Redis —— 让本地与 CI 都能跑。
func newEmojiTestRouter() *wkhttp.WKHttp {
	gin.SetMode(gin.TestMode)
	r := wkhttp.New()
	cn := &Common{}
	r.GET("/v1/common/emojis", cn.emojiManifest)
	return r
}

// TestEmojiManifest_Endpoint 校验 200 响应体含全部 token、且带 ETag + Cache-Control。
func TestEmojiManifest_Endpoint(t *testing.T) {
	r := newEmojiTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/common/emojis", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// 反序列化整个响应体,断言**精确契约**而非子串:顶层就是 {version, list}(c.Response 即
	// 原样 JSON,无 {status,data,msg} 信封)。若将来被信封包裹,Unmarshal 出来的 List 会为空,
	// gotKeys 与 wantKeys 不等,本用例即报错 —— 作为 wire 形状的回归护栏。
	var got emojiManifestResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), "响应应为顶层 {version,list}: %s", w.Body.String())
	assert.GreaterOrEqual(t, got.Version, 1, "version 必须 >=1")
	wantKeys := []string{"[使命必达]", "[崇尚行动]", "[有品位]", "[尚方宝剑]"}
	gotKeys := make([]string, 0, len(got.List))
	for _, e := range got.List {
		gotKeys = append(gotKeys, e.Key)
		assert.Regexp(t, tokenShape, e.Key, "key 必须是 [xxx] token: %q", e.Key)
		assert.NotEmpty(t, e.Name, "name 不应为空: %q", e.Key)
		assert.Empty(t, e.URL, "内置表情 URL 应留空: %q", e.Key)
	}
	assert.Equal(t, wantKeys, gotKeys, "响应的 key 集合/顺序与内置真源一致")

	assert.NotEmpty(t, w.Header().Get("ETag"), "应返回 ETag")
	assert.Equal(t, "public, max-age=300, must-revalidate", w.Header().Get("Cache-Control"))
}

// TestEmojiManifest_NotModified 校验 If-None-Match 命中当前 ETag 时返回 304 且无 body。
func TestEmojiManifest_NotModified(t *testing.T) {
	r := newEmojiTestRouter()

	// 先取一次拿到当前 ETag。
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/common/emojis", nil)
	r.ServeHTTP(w1, req1)
	etag := w1.Header().Get("ETag")
	require.NotEmpty(t, etag)

	// 带上该 ETag 再取,应 304 且无 body。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/common/emojis", nil)
	req2.Header.Set("If-None-Match", etag)
	r.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusNotModified, w2.Code)
	assert.Empty(t, strings.TrimSpace(w2.Body.String()), "304 不应带 body")
}

// TestEmojiManifest_PublicNoAuth 校验**真实注册的路由**(经 testutil.NewTestServer 注册
// 全部模块路由)无需 token 即可访问 —— 锁定"公开、不鉴权"这条 acceptance。需要 MySQL/Redis/
// WuKongIM(见 testing 规则),仅在 CI 运行;本地无 DB 时会在 setup 处 panic 跳过。
func TestEmojiManifest_PublicNoAuth(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/emojis", nil)
	// 故意不带 token。
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "公开端点不带 token 也应 200: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "使命必达")
}
