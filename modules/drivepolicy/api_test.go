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
	"github.com/prometheus/client_golang/prometheus"
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

// newRouterWithI18nMiddleware mounts the production EarlyMiddleware so a test can
// drive per-request language negotiation (Accept-Language) through the real
// renderer. Without it the renderer falls back to i18n.DefaultLanguage, which is
// unavoidable for the plain newRouter harness.
func newRouterWithI18nMiddleware(m *Module) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.UseGin(i18n.EarlyMiddleware(i18n.MiddlewareOptions{DefaultLanguage: i18n.SourceLanguage}))
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

// 503 的 wire 文案口径（PR #930 fix-round-1 P2）：该码是 5xx，按仓库
// 「5xx ⇒ Internal=true」约定（见 pkg/errcode/drive_policy.go 与
// ErrOIDCBindServiceUnavailable），renderer 一律输出 err.shared.internal 占位文案，
// 而不是本码的 DefaultMessage / zh-CN 翻译（那些是 marker，wire 上不可达）。
//
// 这里在真实 EarlyMiddleware 下驱动 HTTP 路径，钉住这条被 test 复验质疑过的行为：
// 状态 503、error.code 仍是可编程分支用的本码、error.message 是随 Accept-Language
// 变化的共享占位文案，且**永不**是本码专属文案。若将来有人为了让专属文案上线而
// 去掉 Internal=true，本用例与 pkg/errcode 的 TestDrivePolicyCodesInternalFlag
// 会一起失败，把这次「有意的约定变更」顶到 review 面前。
func TestGetUploadPolicy_UnavailableSnapshotRendersSharedInternalCopy(t *testing.T) {
	unavailable := func() (common.DriveUploadPolicy, bool) {
		return common.DriveUploadPolicy{
			MaxSizeKB:             1048576,
			MultipartThresholdKB:  131072,
			PartSizeKB:            65536,
			MaxConcurrentParts:    3,
			MaxConcurrentSessions: 3,
			PolicyVersion:         "84c1b192b780",
		}, false
	}
	r := newRouterWithI18nMiddleware(newModule(unavailable))

	cases := []struct {
		acceptLanguage string
		wantMessage    string
	}{
		{"en-US", "Internal server error."},
		{"zh-CN", "服务器内部错误。"},
	}
	for _, tc := range cases {
		t.Run(tc.acceptLanguage, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/internal/drive/upload-policy", nil)
			req.Header.Set(internalTokenHeader, testToken)
			req.Header.Set("Accept-Language", tc.acceptLanguage)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusServiceUnavailable, w.Code, "body=%s", w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			errObj, ok := body["error"].(map[string]any)
			require.True(t, ok, "body=%s", w.Body.String())
			assert.Equal(t, "err.server.drive_policy.unavailable", errObj["code"],
				"可编程分支用的 error.code 必须保持本码")
			assert.Equal(t, tc.wantMessage, errObj["message"],
				"5xx 必须下发共享占位文案（随 Accept-Language 变化）")
			assert.NotContains(t, w.Body.String(), "Drive 上传策略尚未就绪",
				"本码专属 zh-CN 文案是 marker，不得出现在 5xx wire 上")
			assert.NotContains(t, w.Body.String(), "Drive upload policy is not available yet.",
				"本码专属 DefaultMessage 不得出现在 5xx wire 上")
		})
	}
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

// ---------------------------------------------------------------------------
// PR #930 review fix（reviewer yujiawei）
// ---------------------------------------------------------------------------

// 401 鉴权：现对「定长 digest 比较」做定值回归。相等 → 放行；不等 / 长度不同 /
// 只是前缀 → 一律拒绝。长度不同以前会在 ConstantTimeCompare 的首个长度判断处
// 短路；hash 之后两条输入都折成 32 字节，比较不再以长度先行分叉。
func TestInternalTokenMatches_FixedLengthDigestSemantics(t *testing.T) {
	secret := testToken
	assert.True(t, internalTokenMatches(secret, secret), "相等 token 必须匹配")
	assert.False(t, internalTokenMatches("wrong-token-wrong-token-wrong-tok", secret))
	assert.False(t, internalTokenMatches(secret[:len(secret)-1], secret), "前缀必须拒绝")
	assert.False(t, internalTokenMatches(secret+"-extra-bytes", secret), "更长输入必须拒绝")
	assert.False(t, internalTokenMatches("", secret), "空输入必须拒绝")
	// 换一个字符也不行（防 digest 比较被退化成前缀比较）。
	flipped := []byte(secret)
	flipped[0] ^= 0x01
	assert.False(t, internalTokenMatches(string(flipped), secret))
}

// servedTotalValue 读默认 Registry 上 octo_drive_upload_policy_served_total 的某个
// result 分支当前值。计数器是全局的，所以测试断言增量而非绝对值。
func servedTotalValue(t *testing.T, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != "octo_drive_upload_policy_served_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// 生产 429 路径的覆盖（reviewer yujiawei P2）：route-order 测试用的是 pass-through
// stub，从不触发真实拒绝，所以 rate_limited 指标与 429 响应此前无断言。这里把
// countRateLimited 和真实拒绝形态的 limiter（abort 429、不调 Next）拼起来，钉住：
// 响应 429、handler 不执行、rate_limited 计数 +1。
func TestCountRateLimited_RejectedRequestSets429AndCountsMetric(t *testing.T) {
	m := newModule(samplePolicy)
	handlerRan := false

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	// 复刻 StrictIPRateLimitMiddleware 的拒绝形态：abort 429，且**不**调 c.Next()。
	reject := func(c *wkhttp.Context) { c.AbortWithStatus(http.StatusTooManyRequests) }
	internal := r.Group("/v1/internal")
	internal.GET(
		"/drive/upload-policy",
		countRateLimited(reject),
		m.internalAuthMiddleware(),
		func(c *wkhttp.Context) {
			handlerRan = true
			m.getUploadPolicy(c)
		},
	)

	before := servedTotalValue(t, resultRateLimited)
	req := httptest.NewRequest(http.MethodGet, "/v1/internal/drive/upload-policy", nil)
	req.Header.Set(internalTokenHeader, testToken) // 即便带着正确 token，也被限流拦下
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusTooManyRequests, w.Code, "body=%s", w.Body.String())
	assert.False(t, handlerRan, "被限流的请求不得进入 handler")
	assert.Equal(t, before+1, servedTotalValue(t, resultRateLimited),
		"429 拒绝必须在 served_total{result=rate_limited} 上 +1")
}

// 与上一条成对：放行的请求不得污染 rate_limited 计数（否则告警语义会反过来）。
func TestCountRateLimited_AdmittedRequestDoesNotCount(t *testing.T) {
	m := newModule(samplePolicy)

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	admit := func(c *wkhttp.Context) { c.Next() }
	internal := r.Group("/v1/internal")
	internal.GET("/drive/upload-policy", countRateLimited(admit), m.internalAuthMiddleware(), m.getUploadPolicy)

	before := servedTotalValue(t, resultRateLimited)
	w := doGet(t, r, testToken)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, before, servedTotalValue(t, resultRateLimited),
		"放行请求不得递增 rate_limited")
}

// 401 路径（鉴权 abort）不是限流拒绝，不得计入 rate_limited。
func TestCountRateLimited_UnauthorizedAbortIsNotRateLimited(t *testing.T) {
	m := newModule(samplePolicy)

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	admit := func(c *wkhttp.Context) { c.Next() }
	internal := r.Group("/v1/internal")
	internal.GET("/drive/upload-policy", countRateLimited(admit), m.internalAuthMiddleware(), m.getUploadPolicy)

	before := servedTotalValue(t, resultRateLimited)
	w := doGet(t, r, "wrong-token-wrong-token-wrong-tok")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, before, servedTotalValue(t, resultRateLimited),
		"401 鉴权失败不得被记成 rate_limited")
}
