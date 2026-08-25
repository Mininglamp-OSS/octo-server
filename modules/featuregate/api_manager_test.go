package featuregate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

// mountManager 在干净路由上挂管理端 + 用户端，并装上 i18n ErrorRenderer
// （NewTestServer 不装，缺了它错误响应里拿不到 error.code）。
func mountManager(t *testing.T, ctx *config.Context, flags []ClientFlag) *libwkhttp.WKHttp {
	t.Helper()
	r := libwkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	newManagerWithRegistry(ctx, mustNewClientRegistry(flags)).Route(r)
	return r
}

func loginAs(t *testing.T, ctx *config.Context, role string) {
	t.Helper()
	v := testutil.UID + "@test"
	if role != "" {
		v += "@" + role
	}
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, v))
}

func doJSON(t *testing.T, r *libwkhttp.WKHttp, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Buffer
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewBuffer(raw)
	} else {
		body = bytes.NewBuffer(nil)
	}
	req, _ := http.NewRequest(method, path, body)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// errCodeOf 取出 i18n 错误信封里的 error.code / error.details.reason。
func errCodeOf(t *testing.T, w *httptest.ResponseRecorder) (code, reason string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body: %s", w.Body.String())
	if v, ok := env.Error.Details["reason"].(string); ok {
		reason = v
	}
	return env.Error.Code, reason
}

// TestManagerRejectsNonSuperAdminViaEnvelope 钉住拒绝路径走 i18n 信封。
//
// 本框架初版四个 handler 写的是 c.ResponseError(c.CheckLoginRoleIsSuperAdmin())
// —— 绕开信封的 legacy 裸响应。同时确认用的是共享的 403 通用码而不是新造的
// featuregate 专用码：鉴权失败统一收敛到一个码是反枚举约定。
func TestManagerRejectsNonSuperAdminViaEnvelope(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, "") // 普通用户
	r := mountManager(t, ctx, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/manager/featuregate/gates"},
		{http.MethodPut, "/v1/manager/featuregate/gates/k"},
		{http.MethodPost, "/v1/manager/featuregate/gates/k/scopes"},
		{http.MethodDelete, "/v1/manager/featuregate/gates/k/scopes/s"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := doJSON(t, r, tc.method, tc.path, map[string]any{})
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			code, _ := errCodeOf(t, w)
			require.Equal(t, "err.shared.auth.forbidden", code,
				"鉴权失败必须收敛到共享 403 码，不得新造 featuregate 专用码（反枚举）")
		})
	}
}

// TestManagerUpdateValidation 覆盖字段级校验，每条都断言 details.reason，
// 好让运维从响应里就知道是哪个字段不合法。
func TestManagerUpdateValidation(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	cases := []struct {
		name       string
		payload    map[string]any
		wantReason string
	}{
		{"未知 mode", map[string]any{"mode": "rollout"}, "mode"},
		{"percent 超上界", map[string]any{"mode": "percent", "percent": 101}, "percent"},
		{"percent 负数", map[string]any{"mode": "percent", "percent": -1}, "percent"},
		{"未知 bucket_by", map[string]any{"mode": "percent", "percent": 10, "bucket_by": "tenant"}, "bucket_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/fgtest_validate", tc.payload)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			code, reason := errCodeOf(t, w)
			require.Equal(t, "err.server.featuregate.request_invalid", code)
			require.Equal(t, tc.wantReason, reason)
		})
	}
}

// TestManagerRejectsMalformedFeatureKey 钉住 feature_key 的字符集校验。
//
// 只校验长度不校验字符集时，后果不是脏数据而是**丢掉急停能力**：feature_key
// "my gate" 推导出的 OCTO_FEATUREGATE_MY GATE_KILL 含空格、常规手段设不进去，
// 这条 gate 就
// 永久失去 env 级紧急停止 —— 而那是「DB/Redis 全挂时仍能一键停」的最后一条路径。
// 连字符则会破坏 key → env 名的单射（docs-beta 与 docs_beta 共用一个开关）。
func TestManagerRejectsMalformedFeatureKey(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	// URL 路径段里能直接表达的非法形态（空格走 %20，连字符/大写直接给）。
	for _, badKey := range []string{"my%20gate", "docs-beta", "Docs", "1abc", "docs.beta"} {
		t.Run(badKey, func(t *testing.T) {
			w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+badKey,
				map[string]any{"mode": "on"})
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			code, reason := errCodeOf(t, w)
			require.Equal(t, "err.server.featuregate.request_invalid", code)
			require.Equal(t, "key", reason)
		})
	}

	// addScope / delScope 同样受约束，否则能给一条非法 key 挂白名单。
	w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/docs-beta/scopes",
		map[string]any{"scope_type": "user", "scope_id": "u1"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	_, reason := errCodeOf(t, w)
	require.Equal(t, "key", reason)

	w = doJSON(t, r, http.MethodDelete,
		"/v1/manager/featuregate/gates/docs-beta/scopes/u1?scope_type=user", nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	_, reason = errCodeOf(t, w)
	require.Equal(t, "key", reason)

	// 合法 key 仍应通过，确认校验没有误伤。
	w = doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/docs_beta",
		map[string]any{"mode": "on"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestRoutesMountUIDRateLimiterAfterAuth 验证两组路由都挂了 SharedUIDRateLimiter，
// 且挂在 AuthMiddleware **之后**。
//
// 断言 X-RateLimit-Scope=uid 同时覆盖了这两件事：octo-lib 的
// UIDRateLimitMiddleware 在 uid 取不到时会直接跳过（不写任何头），所以这个头存在
// 就意味着中间件既被挂上、又读到了 uid。挂错顺序的失败模式是**静默 fail-open**，
// 线上不会报错，只是限流不生效——没有测试就无从发现。
func TestRoutesMountUIDRateLimiterAfterAuth(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil) // 走真实 Route()，不是测试自建的路由组

	for _, path := range []string{
		"/v1/featuregate/flags",         // 用户侧只读端点
		"/v1/manager/featuregate/gates", // 管理面
	} {
		t.Run(path, func(t *testing.T) {
			w := doJSON(t, r, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"),
				"缺 X-RateLimit-Scope=uid 说明限流没挂，或挂在了 AuthMiddleware 之前（读不到 uid 会静默跳过）")
			require.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"))
		})
	}
}

// TestManagerRejectsUnusableDimensionForClientVisibleKey 是写侧的错配拦截。
//
// 客户端展示端点只有 UID：一条配成 bucket_by=group 的客户端可见规则，评估时
// GroupNo 为空，管理台却仍显示「50%」——一个无报错的静默错配。读侧另有 fail-closed
// 兜底，但直接改库能绕过写侧，所以两侧都要。
func TestManagerRejectsUnusableDimensionForClientVisibleKey(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))

	const visibleKey = "fgtest_visible"
	r := mountManager(t, ctx, []ClientFlag{{FeatureKey: visibleKey, ClientKey: "visible"}})

	t.Run("update 拒绝 group 分桶", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+visibleKey,
			map[string]any{"mode": "percent", "percent": 50, "bucket_by": "group"})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		code, reason := errCodeOf(t, w)
		require.Equal(t, "err.server.featuregate.request_invalid", code)
		require.Equal(t, "client_visible_dimension", reason)
	})

	t.Run("update 拒绝空 bucket_by（归一后是 group）", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+visibleKey,
			map[string]any{"mode": "percent", "percent": 50})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		_, reason := errCodeOf(t, w)
		require.Equal(t, "client_visible_dimension", reason,
			"缺省 bucket_by 归一到 group，对客户端可见的 key 同样不可用")
	})

	t.Run("update 接受 user 分桶", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+visibleKey,
			map[string]any{"mode": "percent", "percent": 50, "bucket_by": "user"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("addScope 拒绝 group 条目", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+visibleKey+"/scopes",
			map[string]any{"scope_type": "group", "scope_id": "g1"})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		_, reason := errCodeOf(t, w)
		require.Equal(t, "client_visible_dimension", reason,
			"允许写入一条永不可能命中的 group 条目，等于让运维以为灰度开了而实际没开")
	})

	t.Run("addScope 接受 user 条目", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+visibleKey+"/scopes",
			map[string]any{"scope_type": "user", "scope_id": "u1"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("非客户端可见的 key 不受此约束", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/fgtest_internal_only",
			map[string]any{"mode": "percent", "percent": 50, "bucket_by": "group"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
}

// TestManagerScopeLifecycle 覆盖白名单增删与 404，并确认 user 维度被接受。
func TestManagerScopeLifecycle(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	const key = "fgtest_scope_lifecycle"
	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodPut,
		"/v1/manager/featuregate/gates/"+key,
		map[string]any{"mode": "whitelist", "bucket_by": "user"}).Code)

	// 未知 scope_type 拒绝。
	w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "tenant", "scope_id": "x"})
	require.Equal(t, http.StatusBadRequest, w.Code)
	_, reason := errCodeOf(t, w)
	require.Equal(t, "scope_type", reason)

	// user 维度接受，且幂等（重复加仍 200）。
	for i := 0; i < 2; i++ {
		w = doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+key+"/scopes",
			map[string]any{"scope_type": "user", "scope_id": "u1"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	// 删不存在的条目 → 404。
	w = doJSON(t, r, http.MethodDelete,
		"/v1/manager/featuregate/gates/"+key+"/scopes/nobody?scope_type=user", nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	code, _ := errCodeOf(t, w)
	require.Equal(t, "err.server.featuregate.not_found", code)

	// 删存在的条目 → 200。
	w = doJSON(t, r, http.MethodDelete,
		"/v1/manager/featuregate/gates/"+key+"/scopes/u1?scope_type=user", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestManagerListSurfacesOperatorAffordances 验证列表回显运维需要的三项：
// 归一后的 bucket_by、是否客户端可见、以及 env 杀开关名。
func TestManagerListSurfacesOperatorAffordances(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))

	const visibleKey = "fgtest_list_visible"
	r := mountManager(t, ctx, []ClientFlag{{FeatureKey: visibleKey, ClientKey: "list_visible"}})

	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodPut,
		"/v1/manager/featuregate/gates/"+visibleKey,
		map[string]any{"mode": "whitelist", "bucket_by": "user"}).Code)

	w := doJSON(t, r, http.MethodGet, "/v1/manager/featuregate/gates", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Gates []struct {
			FeatureKey    string `json:"feature_key"`
			Mode          string `json:"mode"`
			BucketBy      string `json:"bucket_by"`
			ClientVisible bool   `json:"client_visible"`
			KillSwitchEnv string `json:"kill_switch_env"`
		} `json:"gates"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.NotEmpty(t, body.Gates)

	var found bool
	for _, g := range body.Gates {
		if g.FeatureKey != visibleKey {
			continue
		}
		found = true
		require.Equal(t, string(fg.ModeWhitelist), g.Mode)
		require.Equal(t, fg.ScopeTypeUser, g.BucketBy)
		require.True(t, g.ClientVisible, "运维需要知道这条规则会影响终端展示")
		require.Equal(t, "OCTO_FEATUREGATE_FGTEST_LIST_VISIBLE_KILL", g.KillSwitchEnv,
			"回显 env 杀开关名，省得运维手工推导大小写/连字符")
	}
	require.True(t, found, "列表里应能找到刚写入的规则")
}

// TestManagerListNormalizesEmptyBucketBy 确认历史/直改库留下的空 bucket_by 在回显
// 时归一成实际生效的维度，而不是给运维一个空白让他去猜默认值是什么。
func TestManagerListNormalizesEmptyBucketBy(t *testing.T) {
	got := toGateResp(&gateModel{FeatureKey: "k", Mode: "percent", Percent: 10, BucketBy: ""}, nil, false)
	require.Equal(t, fg.ScopeTypeGroup, got.BucketBy, "空 bucket_by 应回显为实际生效的默认维度")
	require.Equal(t, "OCTO_FEATUREGATE_K_KILL", got.KillSwitchEnv)
	require.NotNil(t, got.Scopes, "scopes 必须序列化为 []，不能是 null")
}
