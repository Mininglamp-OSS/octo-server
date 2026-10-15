package drivepolicy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// samplePolicy is the fixed policy the hermetic tests serve. Values chosen so
// every field is distinguishable from a zero value and from the others. The
// second result is the availability flag: a snapshot that loaded successfully
// and produced this policy, so the handler answers 200.
func samplePolicy() (common.DriveUploadPolicy, bool) {
	return common.DriveUploadPolicy{
		MaxSizeKB:             2097152,
		MultipartThresholdKB:  131072,
		PartSizeKB:            65536,
		MaxConcurrentParts:    3,
		MaxConcurrentSessions: 3,
		PolicyVersion:         "abcdef123456",
	}, true
}

func newRouter(m *Module) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	// Hermetic router: auth + handler only. The production chain adds the
	// per-IP strict limiter at the head; that ordering is asserted at runtime
	// in route_runtime_order_test.go without a live Redis.
	r.GET("/v1/internal/drive/upload-policy", m.internalAuthMiddleware(), m.getUploadPolicy)
	return r
}

func doGet(t *testing.T, r *wkhttp.WKHttp, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/internal/drive/upload-policy", nil)
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newModule(policy func() (common.DriveUploadPolicy, bool)) *Module {
	return &Module{
		internalToken: testToken,
		policy:        policy,
		Log:           log.NewTLog("DrivePolicyTest"),
	}
}

func TestGetUploadPolicy_MissingToken(t *testing.T) {
	r := newRouter(newModule(samplePolicy))
	w := doGet(t, r, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGetUploadPolicy_WrongToken(t *testing.T) {
	r := newRouter(newModule(samplePolicy))
	w := doGet(t, r, "wrong-token-wrong-token-wrong-tok")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGetUploadPolicy_UnconfiguredTokenFailsClosed(t *testing.T) {
	m := newModule(samplePolicy)
	m.internalToken = "" // token failed to resolve at boot
	r := newRouter(m)
	w := doGet(t, r, testToken) // even the "right" value is refused
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGetUploadPolicy_OK_SixFields(t *testing.T) {
	r := newRouter(newModule(samplePolicy))
	w := doGet(t, r, testToken)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	want := map[string]float64{
		"max_size_kb":             2097152,
		"multipart_threshold_kb":  131072,
		"part_size_kb":            65536,
		"max_concurrent_parts":    3,
		"max_concurrent_sessions": 3,
	}
	for k, v := range want {
		got, ok := body[k]
		require.True(t, ok, "响应必须含字段 %q：%s", k, w.Body.String())
		assert.Equal(t, v, got, "字段 %q", k)
	}
	assert.Equal(t, "abcdef123456", body["policy_version"])
	// 恰为 6 个字段 —— 本期不纳入 max_part_count / max_part_presign_batch（06 冻结决策 6）。
	assert.Len(t, body, 6, "内部接口响应体恰为 6 字段：%s", w.Body.String())
	assert.NotContains(t, body, "max_part_count")
	assert.NotContains(t, body, "max_part_presign_batch")
}

// 响应体不得含 URL / token / secret 字样（设计 §5.2 禁止项的同款回归）。
func TestGetUploadPolicy_BodyHasNoSecrets(t *testing.T) {
	r := newRouter(newModule(samplePolicy))
	w := doGet(t, r, testToken)
	require.Equal(t, http.StatusOK, w.Code)
	lower := strings.ToLower(w.Body.String())
	for _, forbidden := range []string{"token", "secret", "url", "http://", "https://", "presign", "cookie"} {
		assert.NotContains(t, lower, forbidden, "响应体不得出现 %q：%s", forbidden, w.Body.String())
	}
	assert.NotContains(t, w.Body.String(), testToken)
}

// handler 每请求只读一次策略（单次快照读的下游约束）：把 policy 替换为计数器，
// 一次请求只能加一。
func TestGetUploadPolicy_ReadsPolicyOncePerRequest(t *testing.T) {
	calls := 0
	m := newModule(func() (common.DriveUploadPolicy, bool) {
		calls++
		return samplePolicy()
	})
	r := newRouter(m)
	w := doGet(t, r, testToken)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, calls, "handler 每请求只能读一次生效策略")

	// 401 路径不读策略。
	calls = 0
	_ = doGet(t, r, "")
	assert.Zero(t, calls, "鉴权失败不得触碰策略读取")
}

// 未授权响应与 200 响应的字段形状不同：401 走共享错误信封，不应泄漏 6 字段。
func TestGetUploadPolicy_UnauthorizedOmitsPolicy(t *testing.T) {
	r := newRouter(newModule(samplePolicy))
	w := doGet(t, r, "nope")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.NotContains(t, w.Body.String(), "policy_version")
	assert.NotContains(t, w.Body.String(), "max_size_kb")
}

// 快照从未成功加载时返回 503，而不是把默认值当成合法策略下发（fail-closed，
// 设计 C2）。响应不得携带 6 字段 —— 一个看似合法的 200 + fingerprint 会让 drive
// 侧据此抬升上限。
func TestGetUploadPolicy_UnavailableSnapshotFailsClosed(t *testing.T) {
	unavailable := func() (common.DriveUploadPolicy, bool) {
		// 值本身是代码默认 + 正常 fingerprint：若 handler 只看值不看 flag，
		// 就会把它当合法策略 200 下发 —— 这正是本用例要拦住的行为。
		return common.DriveUploadPolicy{
			MaxSizeKB:             1048576,
			MultipartThresholdKB:  131072,
			PartSizeKB:            65536,
			MaxConcurrentParts:    3,
			MaxConcurrentSessions: 3,
			PolicyVersion:         "84c1b192b780",
		}, false
	}
	r := newRouter(newModule(unavailable))
	w := doGet(t, r, testToken)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
	body := w.Body.String()
	assert.NotContains(t, body, "policy_version", "503 不得携带策略指纹")
	assert.NotContains(t, body, "max_size_kb", "503 不得携带 6 字段策略体")
	assert.NotContains(t, body, "84c1b192b780", "503 不得泄漏被拒绝的默认值指纹")
}

// 与上一条成对：快照已成功加载但为空 map（管理员什么都没配）是**合法**状态，
// 必须 200 下发默认策略。区分「从未加载」与「加载成功但为空」是本次修复的核心。
func TestGetUploadPolicy_LoadedButEmptySnapshotIsOK(t *testing.T) {
	empty := func() (common.DriveUploadPolicy, bool) {
		return common.DriveUploadPolicy{
			MaxSizeKB:             1048576,
			MultipartThresholdKB:  131072,
			PartSizeKB:            65536,
			MaxConcurrentParts:    3,
			MaxConcurrentSessions: 3,
			PolicyVersion:         "84c1b192b780",
		}, true
	}
	r := newRouter(newModule(empty))
	w := doGet(t, r, testToken)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "policy_version")
	assert.Contains(t, w.Body.String(), "max_size_kb")
}
