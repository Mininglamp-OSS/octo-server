package incomingwebhook_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3（平台适配器）push 端到端用例。与现有 push 测试同风格走 testutil.NewTestServer
// （需 MySQL/Redis/WuKongIM，CI 执行）。成功路径只断言「通过鉴权/校验」（非 4xx）——
// 测试桩下游 SendMessage 可能 200 或 502，口径与 richtext_push_test.go 一致。

// pushAdapterRaw 向适配器后缀路由发原始 body（可带平台事件头）。
func pushAdapterRaw(handler http.Handler, whID, token, suffix string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	r := anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s/%s", whID, token, suffix), body)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return do(handler, r)
}

// create/regenerate 响应携带各推送形态的 URL（#297 顺延的 onboarding 项）。
func TestCreate_ReturnsAdapterURLs(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "adapter-wh",
	}))
	require.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	created := parseJSON(t, w)

	urls, ok := created["urls"].(map[string]interface{})
	require.True(t, ok, "create response must carry urls; body=%s", w.Body.String())
	native, _ := urls["native"].(string)
	assert.Equal(t, created["url"], native, "urls.native must equal the legacy url field")
	assert.Equal(t, native+"/github", urls["github"])
	assert.Equal(t, native+"/wecom", urls["wecom"])
	assert.Equal(t, native+"/multica", urls["multica"])
}

func TestCreate_ReturnsLocalizedAdapterExamples(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks?lang=en-US", groupNo), map[string]interface{}{
		"name": "adapter-examples-wh",
	}))
	require.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	created := parseJSON(t, w)

	urls, ok := created["urls"].(map[string]interface{})
	require.True(t, ok, "create response must carry urls; body=%s", w.Body.String())
	rawExamples, ok := created["adapter_examples"].([]interface{})
	require.True(t, ok, "create response must carry adapter_examples; body=%s", w.Body.String())

	keys := make([]string, 0, len(rawExamples))
	for _, raw := range rawExamples {
		ex, ok := raw.(map[string]interface{})
		require.True(t, ok, "adapter example must be object: %#v", raw)
		key, _ := ex["key"].(string)
		keys = append(keys, key)
		assert.Equal(t, urls[key], ex["url"], "example URL must match urls[%s]", key)
		assert.Equal(t, "application/json", ex["content_type"])
		assert.NotEmpty(t, ex["title"])
		assert.NotEmpty(t, ex["description"])
		steps, ok := ex["steps"].([]interface{})
		require.True(t, ok, "steps must be array for %s", key)
		assert.NotEmpty(t, steps)
	}
	assert.Equal(t, []string{"github", "gitlab", "feishu", "multica", "wecom"}, keys)
	firstDescription, _ := rawExamples[0].(map[string]interface{})["description"].(string)
	assert.Contains(t, firstDescription, "repository")
}

// GitHub ping：200 + skipped，不投递消息，且异步记一条 status=3(skipped) 的投递。
func TestPush_GitHubPing_SkippedAndAudited(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github",
		[]byte(`{"zen":"Keep it logically awesome.","hook_id":1}`),
		map[string]string{"X-GitHub-Event": "ping"})
	require.Equalf(t, http.StatusOK, w.Code, "ping must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`, "ping response must mark the delivery as skipped")

	require.Eventually(t, func() bool {
		dw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
		if dw.Code != http.StatusOK {
			return false
		}
		list, _ := parseJSON(t, dw)["list"].([]interface{})
		for _, item := range list {
			row, _ := item.(map[string]interface{})
			if row["adapter"] == "github" && int(row["status"].(float64)) == 3 && row["reason"] == "ping" {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "ping must be recorded as a skipped delivery")
}

// GitHub push 事件通过鉴权/翻译（非 4xx）。
func TestPush_GitHubPushEvent_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	body := []byte(`{
		"ref": "refs/heads/main",
		"commits": [{"id": "aaaabbbbcccc", "message": "feat: hello", "url": "https://github.com/o/r/commit/aaaabbbb"}],
		"repository": {"full_name": "o/r", "html_url": "https://github.com/o/r"},
		"sender": {"login": "alice"}
	}`)
	w := pushAdapterRaw(handler, whID, token, "github", body, map[string]string{"X-GitHub-Event": "push"})
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid push event must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), "message_id")
	}
}

// 渲染子集之外的事件：200 + skipped（GitHub 侧不标红，群内不刷屏）。
func TestPush_GitHubUnsupportedEvent_Skipped(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github",
		[]byte(`{"action":"started"}`), map[string]string{"X-GitHub-Event": "watch"})
	require.Equalf(t, http.StatusOK, w.Code, "unsupported event must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`)
}

// 缺事件头 → 400 invalid（误配置要立刻可见，而非静默跳过）。
func TestPush_GitHubMissingEventHeader_Invalid(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github", []byte(`{}`), nil)
	assert.Equalf(t, http.StatusBadRequest, w.Code, "missing X-GitHub-Event must 400; body=%s", w.Body.String())
}

// 适配器路由沿用同一鉴权：错 token 统一 401（反枚举口径不变）。
func TestPush_AdapterRoute_AuthEnforced(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	for _, suffix := range []string{"github", "wecom", "multica"} {
		w := pushAdapterRaw(handler, whID, "wrong-token", suffix,
			[]byte(`{"msgtype":"text","text":{"content":"hi"}}`),
			map[string]string{"X-GitHub-Event": "push"})
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "%s: bad token must 401; body=%s", suffix, w.Body.String())
	}
}

// 真实 GitHub 事件 JSON 普遍超过 native 的 8KiB body cap 且发送方无法修短——github
// 路由使用独立宽上限，>8KiB 的合法 push 事件必须被接受并渲染（PR #330 review 阻断项）；
// 同一 payload 打 native 路由仍须 413，宽上限只属于平台事件形态。
func TestPush_GitHubLargePayload_Not413(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	// 20 个提交 × ~1KiB 提交信息 ≈ >20KiB body，模拟真实大 push 事件。
	longMsg := strings.Repeat("x", 1024)
	commits := make([]map[string]interface{}, 0, 20)
	for i := 0; i < 20; i++ {
		commits = append(commits, map[string]interface{}{
			"id":      fmt.Sprintf("sha%037d", i),
			"message": fmt.Sprintf("c%d: %s", i, longMsg),
			"url":     fmt.Sprintf("https://github.com/o/r/commit/%d", i),
		})
	}
	body, err := json.Marshal(map[string]interface{}{
		"ref":        "refs/heads/main",
		"commits":    commits,
		"repository": map[string]interface{}{"full_name": "o/r", "html_url": "https://github.com/o/r"},
		"sender":     map[string]interface{}{"login": "alice"},
	})
	require.NoError(t, err)
	require.Greater(t, len(body), 8*1024, "fixture must exceed the native body cap")

	w := pushAdapterRaw(handler, whID, token, "github", body, map[string]string{"X-GitHub-Event": "push"})
	assert.NotEqualf(t, http.StatusRequestEntityTooLarge, w.Code, "github events beyond 8KiB must not 413; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid event must render; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())

	// 对照组：native 路由对同一 payload 仍按 8KiB cap 413。
	wn := do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equalf(t, http.StatusRequestEntityTooLarge, wn.Code, "native route keeps the caller-authored cap; body=%s", wn.Body.String())
}

// 企微 text 格式通过鉴权/翻译；成功响应附带 errcode=0（平台 SDK 兼容）。
func TestPush_WeComText_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "wecom",
		[]byte(`{"msgtype":"text","text":{"content":"hello from wecom"}}`), nil)
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid wecom text must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), `"errcode":0`)
	}
}

// 企微素材类消息（base64 图片）→ 400 invalid（显式失败优于静默丢弃）。
func TestPush_WeComImage_Rejected(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "wecom",
		[]byte(`{"msgtype":"image","image":{"base64":"...","md5":"..."}}`), nil)
	assert.Equalf(t, http.StatusBadRequest, w.Code, "wecom image must 400; body=%s", w.Body.String())
}

// Multica issue.status_changed envelope 通过鉴权/翻译；成功响应含 message_id
// 且不带平台兼容字段（multica 适配器无 successExtra）。
func TestPush_MulticaIssueStatusChanged_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "multica",
		[]byte(`{"event":"issue.status_changed","actor":{"type":"member","id":"u-1"},`+
			`"issue":{"identifier":"MUL-123","title":"Fix login redirect","status":"in_progress"},`+
			`"previous_status":"todo"}`), nil)
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid multica envelope must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), "message_id")
		// multica 不附带 errcode/errmsg（与 native 同，区别于 wecom）。
		assert.NotContains(t, w.Body.String(), `"errcode"`)
	}
}

// 渲染子集之外的 multica 事件（issue.created 等）→ 200 + skipped=event，
// 与 github 适配器对称。
func TestPush_MulticaUnsupportedEvent_Skipped(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "multica",
		[]byte(`{"event":"issue.created","issue":{"identifier":"MUL-1","title":"new","status":"todo"}}`),
		nil)
	require.Equalf(t, http.StatusOK, w.Code, "unsupported event must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped":"event"`)
}

// 缺 event 字段 → 400 invalid（reason=no_event 由 pushPayloadInvalid 路由，
// 在 TestPushPayloadInvalidSurfacesReason 钉死；此处 e2e 不能断言 body 里的
// reason 字段——testutil.NewTestServer 不挂 i18n ErrorRenderer，e2e 响应只
// 渲染成 legacy {msg,status} 形态，details 不出现，详见 api_i18n_test.go
// TestPushPayloadInvalidSurfacesReason 旁的注释）。
func TestPush_MulticaMissingEvent_NoEventReason(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "multica",
		[]byte(`{"issue":{"identifier":"MUL-1","title":"x","status":"todo"}}`), nil)
	require.Equalf(t, http.StatusBadRequest, w.Code, "missing event must 400; body=%s", w.Body.String())
}
