package featuregate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestUpdateRejectsClientVisibleKeyWhoseScopesAreAllUnusable 是评审抓到的 P1 阻塞项，
// 也是 brief 验收里我漏实现的那半条。
//
// 触发顺序正是注册表的**正常生命周期**（注册表是编译期清单、gate 是 DB 行）：
//  1. gate 还不是 client-visible，ops 加了 group 白名单 —— 当时完全正确；
//  2. 后续发版把它加进 clientFlagList；
//  3. ops 改成 whitelist + bucket_by=user。
//
// 第 3 步若放行，白名单里就全是永远命不中的条目，而运维看到的是：名单有行、mode
// 对、写入 200、日志无声，flag 对所有人 false。这是本模块唯一一处写侧读侧皆无信号
// 的错配，必须在写侧挡住。
func TestUpdateRejectsClientVisibleKeyWhoseScopesAreAllUnusable(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))

	const key = "fgtest_lifecycle"
	db := newDB(ctx)

	// 阶段一：key 还不是 client-visible，group 白名单是合法配置。
	internalOnly := mountManager(t, ctx, nil)
	require.Equal(t, http.StatusOK, doJSON(t, internalOnly, http.MethodPut,
		"/v1/manager/featuregate/gates/"+key,
		map[string]any{"mode": "whitelist", "bucket_by": "group"}).Code)
	require.Equal(t, http.StatusOK, doJSON(t, internalOnly, http.MethodPost,
		"/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "group", "scope_id": "g1"}).Code)

	// 阶段二：一次发版把它变成 client-visible。
	visible := mountManager(t, ctx, []ClientFlag{{FeatureKey: key, ClientKey: "lifecycle"}})

	// 阶段三：这次 update 必须被拒 —— 白名单存量全是用不上的维度。
	w := doJSON(t, visible, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
		map[string]any{"mode": "whitelist", "bucket_by": "user"})
	require.Equal(t, http.StatusBadRequest, w.Code,
		"存量白名单全是 group 条目，改成 client-visible 后必须拒绝；body: %s", w.Body.String())
	code, reason := errCodeOf(t, w)
	require.Equal(t, "err.server.featuregate.request_invalid", code)
	require.Equal(t, "client_visible_scopes", reason)

	// 补一条 user 条目后，同样的 update 应当放行。
	require.NoError(t, db.addScope(key, fg.ScopeTypeUser, "u1", "tester"))
	require.Equal(t, http.StatusOK, doJSON(t, visible, http.MethodPut,
		"/v1/manager/featuregate/gates/"+key,
		map[string]any{"mode": "whitelist", "bucket_by": "user"}).Code,
		"存在可用维度条目后不应再拒")
}

// TestUpdateAllowsClientVisibleKeyWithEmptyScopes 钉住边界：空白名单是**合法状态**
// （mode=whitelist 且暂时谁都不放），不能被上面那条检查误伤。
func TestUpdateAllowsClientVisibleKeyWithEmptyScopes(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))

	const key = "fgtest_empty_scopes"
	r := mountManager(t, ctx, []ClientFlag{{FeatureKey: key, ClientKey: "empty_scopes"}})
	w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
		map[string]any{"mode": "whitelist", "bucket_by": "user"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestManagerRecordsUpdatedBy 钉住审计列：能对全体用户关功能的开关必须留下操作人。
// 仓库既有惯例见 octo_space_welcome_config / bot_mention_pref 的 updated_by。
func TestManagerRecordsUpdatedBy(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	const key = "fgtest_audit"
	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodPut,
		"/v1/manager/featuregate/gates/"+key, map[string]any{"mode": "on"}).Code)
	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodPost,
		"/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "user", "scope_id": "u1"}).Code)

	rule, err := newDB(ctx).queryRule(key)
	require.NoError(t, err)
	require.NotNil(t, rule)
	require.Equal(t, testutil.UID, rule.UpdatedBy, "规则必须记录最后修改人")

	scopes, err := newDB(ctx).queryScopes(key)
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	require.Equal(t, testutil.UID, scopes[0].UpdatedBy, "白名单条目必须记录添加人")

	// 管理端列表要回显，否则运维得去查库。
	w := doJSON(t, r, http.MethodGet, "/v1/manager/featuregate/gates", nil)
	require.Contains(t, w.Body.String(), `"updated_by"`)
	require.Contains(t, w.Body.String(), testutil.UID)
}

// TestManagerRejectsScopeIDWithPathSeparator 关掉「能建不能删」的不对称：scope_id
// 含 '/' 时可插入、但 delScope 按单个路径段取值，永远删不掉（gin 匹配的是已解码
// 路径，%2F 也绕不过）。
func TestManagerRejectsScopeIDWithPathSeparator(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	for _, bad := range []string{"a/b", "a b", "a\tb"} {
		w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/fgtest_sep/scopes",
			map[string]any{"scope_type": "user", "scope_id": bad})
		require.Equal(t, http.StatusBadRequest, w.Code, "scope_id=%q 应被拒；body: %s", bad, w.Body.String())
		_, reason := errCodeOf(t, w)
		require.Equal(t, "scope_id", reason)
	}
}

// TestManagerDescriptionCountedInRunes 钉住 description 按字符而非字节计数 —— 列是
// VARCHAR(255) 即 255 个字符，按字节校验会把中文描述砍到 ~85 字。
func TestManagerDescriptionCountedInRunes(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/fgtest_desc",
		map[string]any{"mode": "on", "description": strings.Repeat("灰", 200)})
	require.Equal(t, http.StatusOK, w.Code, "200 个汉字（600 字节）在 255 字符列内，应当接受；body: %s", w.Body.String())

	w = doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/fgtest_desc",
		map[string]any{"mode": "on", "description": strings.Repeat("灰", 256)})
	require.Equal(t, http.StatusBadRequest, w.Code, "超过 255 字符必须拒")
	_, reason := errCodeOf(t, w)
	require.Equal(t, "description", reason)
}

// TestManagerCapsWhitelistSize 给单个 key 的白名单条数设上限。
//
// 评审建议给 queryScopes 加 LIMIT，但那会在**评估路径**上静默丢条目 —— 把「慢但
// 正确」换成「快但答案错」。改在写侧封顶：同时封住查询量和 Redis 缓存值大小，且
// 超限时运维会收到明确拒绝而不是悄悄少了几个人。
func TestManagerCapsWhitelistSize(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	const key = "fgtest_cap"
	db := newDB(ctx)
	for i := 0; i < maxScopesPerKey; i++ {
		require.NoError(t, db.addScope(key, fg.ScopeTypeUser, fmt.Sprintf("u%d", i), "seed"))
	}
	w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "user", "scope_id": "one_too_many"})
	require.Equal(t, http.StatusBadRequest, w.Code, "超过上限必须拒；body: %s", w.Body.String())
	_, reason := errCodeOf(t, w)
	require.Equal(t, "scope_quota", reason)
}

// TestOffAndOnStayWritableForClientVisibleKeyWithUnusableScopes 是 round-2 阻塞项：
// 上一轮为了堵「白名单全是用不上的维度」而加的校验跑在**每一次** update 上，包括
// 降级。后果是一条 client-visible 且带遗留 group 白名单的 gate **关不掉**——
// `{"mode":"off"}` 先被 bucket_by 默认成 group 拦掉，显式写 bucket_by=user 又被
// 存量 scope 检查拦掉，只剩逐条删白名单（最多 1000 次）或 env 杀开关。
//
// 而 off/on 根本不读白名单也不读 bucket_by（见 modeNeedsScopes 与 Evaluate 的
// ModeOff/ModeOn 分支），校验的是一个目标状态压根不使用的前置条件。
//
// 原则：**关掉一个东西永远不该比打开它更难。**
func TestOffAndOnStayWritableForClientVisibleKeyWithUnusableScopes(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))

	const key = "fgtest_off_rollback"
	db := newDB(ctx)

	// 造出这条检查存在的理由所描述的状态：内部 gate + 全 group 白名单，之后被
	// 某次发版变成 client-visible。
	require.NoError(t, db.upsertRule(key, string(fg.ModeWhitelist), 0, fg.ScopeTypeGroup, "legacy", "seed"))
	require.NoError(t, db.addScope(key, fg.ScopeTypeGroup, "g1", "seed"))
	r := mountManager(t, ctx, []ClientFlag{{FeatureKey: key, ClientKey: "off_rollback"}})

	t.Run("最朴素的回滚 body 必须可用", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
			map[string]any{"mode": "off"})
		require.Equal(t, http.StatusOK, w.Code,
			"关停不该被一个 off 模式根本不读的前置条件拦住；body: %s", w.Body.String())
	})

	t.Run("显式 bucket_by 的 off 同样可用", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
			map[string]any{"mode": "off", "bucket_by": "user"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("on 同理：不读 scope 也不读 bucket_by", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
			map[string]any{"mode": "on"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("切回真正会读白名单的模式仍必须被拦", func(t *testing.T) {
		w := doJSON(t, r, http.MethodPut, "/v1/manager/featuregate/gates/"+key,
			map[string]any{"mode": "whitelist", "bucket_by": "user"})
		require.Equal(t, http.StatusBadRequest, w.Code,
			"whitelist 会读白名单，存量全不可用时必须仍然拒绝；body: %s", w.Body.String())
		_, reason := errCodeOf(t, w)
		require.Equal(t, "client_visible_scopes", reason)
	})

	// 关停后规则确实落库为 off。
	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodPut,
		"/v1/manager/featuregate/gates/"+key, map[string]any{"mode": "off"}).Code)
	rule, err := db.queryRule(key)
	require.NoError(t, err)
	require.Equal(t, string(fg.ModeOff), rule.Mode)
}

// TestAddScopeStaysIdempotentAtQuota 钉住配额检查不得破坏 addScope 的幂等承诺。
//
// 先数后插的写法在**恰好满额**时会把「重加一条已存在的条目」也拒掉——而那正是
// 运维最可能重试的时刻（网络抖动后重放）。已存在的条目不占新增名额。
func TestAddScopeStaysIdempotentAtQuota(t *testing.T) {
	ctx := newIntegrationCtx(t)
	loginAs(t, ctx, string(libwkhttp.SuperAdmin))
	r := mountManager(t, ctx, nil)

	const key = "fgtest_quota_idem"
	db := newDB(ctx)
	for i := 0; i < maxScopesPerKey; i++ {
		require.NoError(t, db.addScope(key, fg.ScopeTypeUser, fmt.Sprintf("u%d", i), "seed"))
	}

	// 满额时重加**已存在**的条目：幂等，应当成功。
	w := doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "user", "scope_id": "u0"})
	require.Equal(t, http.StatusOK, w.Code,
		"已存在的条目不占新增名额，重试必须仍然幂等；body: %s", w.Body.String())

	// 满额时加**新**条目：仍应被配额拒绝。
	w = doJSON(t, r, http.MethodPost, "/v1/manager/featuregate/gates/"+key+"/scopes",
		map[string]any{"scope_type": "user", "scope_id": "brand_new"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	_, reason := errCodeOf(t, w)
	require.Equal(t, "scope_quota", reason)
}
