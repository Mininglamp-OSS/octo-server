package featuregate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

// stubEvaluator 让「存储故障 → 省略」这条路径可被确定性地断言，而不必真去弄坏
// DB/Redis（那会污染同包其它用例，或引入时序依赖）。
type stubEvaluator struct {
	// allow[key] 是确定性结论；unavailable[key] 为 true 时模拟存储故障。
	allow       map[string]bool
	unavailable map[string]bool
}

func (s stubEvaluator) AllowDisplay(_ context.Context, key string, _ fg.Dims) (bool, bool) {
	if s.unavailable[key] {
		return false, false
	}
	return s.allow[key], true
}

// newIntegrationCtx 是本包所有 DB/Redis 集成测试的**唯一**入口。
//
// 除了建库跑迁移，它还清掉两类**跨运行存活**的 Redis 状态。CleanAllTables 只清
// MySQL，Redis 一概不动，于是「drop 测试库重跑」这个本地标准动作会留下：
//
//   - ft:rule:* / ft:scope:*  ——  featuregate 自己的规则读缓存（TTL 60s）。
//     上一轮跑完留下的缓存会让新一轮把「规则不存在」读成上一轮写过的规则，
//     症状是随机失败、且两次运行间隔超过 60s 就自动消失（最难查的那种）。
//   - ratelimit:uid:*         ——  共享令牌桶，先跑的用例会消耗后跑用例的配额。
//
// 收敛到一个 fixture 是为了消灭「这个用例有没有记得重置」这一整类问题：新增用例
// 只要用它，就不可能漏。
func newIntegrationCtx(t *testing.T) *config.Context {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	flushRedisPrefixes(t, ctx, "ft:rule:*", "ft:scope:*", "ratelimit:uid:*")
	return ctx
}

// flushRedisPrefixes 按模式删除 key。测试 Redis 是专用容器，与仓库既有做法一致
// （见 modules/category 的 resetUIDRateLimit）。
func flushRedisPrefixes(t *testing.T, ctx *config.Context, patterns ...string) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	for _, p := range patterns {
		keys, err := rds.Keys(p).Result()
		if err == nil && len(keys) > 0 {
			_ = rds.Del(keys...).Err()
		}
	}
}

// mountFlags 在一条干净的路由上挂载指定注册表 + 指定判定器的 flags 端点。
//
// 不复用 testutil.NewTestServer 自动挂载的那份路由，是因为它绑的是生产注册表
// （当前为空）。同时显式装上 i18n ErrorRenderer —— NewTestServer 不装，缺了它
// 错误响应里拿不到 error.code。
func mountFlags(t *testing.T, ctx *config.Context, flags []ClientFlag, eval displayEvaluator) *libwkhttp.WKHttp {
	t.Helper()
	r := libwkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	registry := mustNewClientRegistry(flags)
	api := newFlagsAPI(ctx, eval, registry)
	g := r.Group("/v1/featuregate", ctx.AuthMiddleware(r))
	g.GET("/flags", api.get)
	return r
}

func getFlags(t *testing.T, r *libwkhttp.WKHttp) (*httptest.ResponseRecorder, map[string]bool) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/featuregate/flags", nil)
	req.Header.Set("token", testutil.Token)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w, nil
	}
	var body struct {
		Flags map[string]bool `json:"flags"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body: %s", w.Body.String())
	return w, body.Flags
}

// TestFlagsEndpointWireContract 覆盖响应的三条 wire 约定：用 client_key 作字段名、
// 值为 false 的 key 必须出现、存储故障的 key 必须缺席。
func TestFlagsEndpointWireContract(t *testing.T) {
	ctx := newIntegrationCtx(t)

	flags := []ClientFlag{
		{FeatureKey: "alpha_rollout", ClientKey: "alpha"},
		{FeatureKey: "beta_rollout", ClientKey: "beta"},
		{FeatureKey: "gamma_rollout", ClientKey: "gamma"},
	}
	eval := stubEvaluator{
		allow:       map[string]bool{"alpha_rollout": true, "beta_rollout": false},
		unavailable: map[string]bool{"gamma_rollout": true},
	}

	w, got := getFlags(t, mountFlags(t, ctx, flags, eval))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 用 client_key 作字段名，feature_key 不得出现在 wire 上——两者解耦正是为了
	// 让运维改 feature_key 不破坏客户端。
	require.Contains(t, got, "alpha")
	require.NotContains(t, got, "alpha_rollout", "响应必须用 client_key，不能透出 feature_key")

	require.True(t, got["alpha"])

	// 值为 false 的 key **必须**出现（不得被 omitempty 吞掉），否则它会与
	// 「存储故障省略」混为一谈，客户端保留旧值，灰度关不掉。
	v, ok := got["beta"]
	require.True(t, ok, "确定性的 false 必须出现在响应里，实际 body: %s", w.Body.String())
	require.False(t, v)

	// 存储故障的 key **必须**缺席，让客户端保留上次值。
	_, ok = got["gamma"]
	require.False(t, ok, "存储故障的 key 应从响应中省略，而不是下发 false；body: %s", w.Body.String())

	// 结果因人而异但 URL 对所有用户相同：必须禁掉共享缓存。
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	// Vary 必须点名本仓实际使用的鉴权头（octo-lib AuthMiddleware 读 `token`）。
	// 写成 Authorization 会指向一个所有请求都为空的头 —— 没有区分度，反而像是在
	// 宣告这些响应可以互换。
	require.Equal(t, "token", w.Header().Get("Vary"))
}

// TestFlagsEndpointIgnoresClientSuppliedKeys 钉住「请求不接受任何 key 参数」。
// 端点恒返回注册表全集，客户端因此无法用构造的 key 去探测内部 gate 是否存在。
func TestFlagsEndpointIgnoresClientSuppliedKeys(t *testing.T) {
	ctx := newIntegrationCtx(t)

	r := mountFlags(t, ctx,
		[]ClientFlag{{FeatureKey: "alpha_rollout", ClientKey: "alpha"}},
		stubEvaluator{allow: map[string]bool{"alpha_rollout": true}})

	w := httptest.NewRecorder()
	// 试图指定一个未注册的内部 key，并试图只要某一个 key。
	req, _ := http.NewRequest(http.MethodGet,
		"/v1/featuregate/flags?keys=internal_secret_gate&key=alpha", nil)
	req.Header.Set("token", testutil.Token)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Flags map[string]bool `json:"flags"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Flags, 1, "响应必须恒为注册表全集，不受 query 参数影响")
	require.Contains(t, body.Flags, "alpha")
	require.NotContains(t, w.Body.String(), "internal_secret_gate",
		"未注册 key 不得以任何形式回显，否则就是一个枚举探针")
}

// TestFlagsEndpointRequiresAuth 确认端点在 AuthMiddleware 之后，未登录拿不到。
func TestFlagsEndpointRequiresAuth(t *testing.T) {
	ctx := newIntegrationCtx(t)

	r := mountFlags(t, ctx,
		[]ClientFlag{{FeatureKey: "alpha_rollout", ClientKey: "alpha"}},
		stubEvaluator{allow: map[string]bool{"alpha_rollout": true}})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/featuregate/flags", nil) // 不带 token
	r.ServeHTTP(w, req)

	require.NotEqual(t, http.StatusOK, w.Code, "未登录必须拿不到灰度位；body: %s", w.Body.String())
}

// TestFlagsEndpointEmptyRegistry 覆盖生产当前状态（注册表为空）：请求成功，
// flags 是一个空对象而不是 null。null 会让弱类型客户端在解引用时炸掉。
func TestFlagsEndpointEmptyRegistry(t *testing.T) {
	ctx := newIntegrationCtx(t)

	r := mountFlags(t, ctx, []ClientFlag{}, stubEvaluator{})
	w, got := getFlags(t, r)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, got, "空注册表必须序列化为 {}，不能是 null；body: %s", w.Body.String())
	require.Empty(t, got)
}

// TestFlagsEndpointZeroRulesDeployment 是全新部署场景的端到端版本：走**真实**
// Service，feature_gate 表里一条规则都没有时，每个已注册 key 都下发 false
// （确定性的关，非省略），请求本身 200。
//
// 用一条测试钉住，避免日后有人把「新部署什么都看不到」当成 bug 改成 fail-open。
func TestFlagsEndpointZeroRulesDeployment(t *testing.T) {
	ctx := newIntegrationCtx(t)

	flags := []ClientFlag{
		{FeatureKey: "fgtest_zero_a", ClientKey: "zero_a"},
		{FeatureKey: "fgtest_zero_b", ClientKey: "zero_b"},
	}
	r := mountFlags(t, ctx, flags, NewService(ctx))
	w, got := getFlags(t, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, got, 2, "无规则时仍应下发全部已注册 key（值为 false），而不是省略")
	require.False(t, got["zero_a"])
	require.False(t, got["zero_b"])
}

// TestFlagsEndpointUserWhitelistEndToEnd 走真实 Service：用户在 user 白名单内得
// true，不在的得 false。
func TestFlagsEndpointUserWhitelistEndToEnd(t *testing.T) {
	ctx := newIntegrationCtx(t)

	const featureKey = "fgtest_e2e_whitelist"
	svc := NewService(ctx)
	db := newDB(ctx)
	seedRule(t, svc, db, featureKey, string(fg.ModeWhitelist), 0, fg.ScopeTypeUser)
	seedScope(t, svc, db, featureKey, fg.ScopeTypeUser, testutil.UID)

	r := mountFlags(t, ctx, []ClientFlag{{FeatureKey: featureKey, ClientKey: "e2e"}}, svc)
	w, got := getFlags(t, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, got["e2e"], "登录用户在 user 白名单内应当为 true")

	// 把白名单换成别人，同一个登录用户应当变 false。
	_, err := db.deleteScope(featureKey, fg.ScopeTypeUser, testutil.UID)
	require.NoError(t, err)
	seedScope(t, svc, db, featureKey, fg.ScopeTypeUser, "someone_else")

	_, got = getFlags(t, r)
	require.False(t, got["e2e"], "不在白名单的用户应当为 false")
}
