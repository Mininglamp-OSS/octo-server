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

// stubEvaluator 让「存储故障 → 进 unavailable」这条路径可被确定性地断言，而不必真去弄坏
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
//   - featuregate:*           ——  本模块的规则读缓存（TTL 60s，键含 schema 版本）。
//     上一轮跑完留下的缓存会让新一轮把「规则不存在」读成上一轮写过的规则，
//     症状是随机失败、且两次运行间隔超过 60s 就自动消失（最难查的那种）。
//   - ratelimit:uid:*         ——  共享令牌桶，先跑的用例会消耗后跑用例的配额。
//
// 收敛到一个 fixture 是为了消灭「这个用例有没有记得重置」这一整类问题：新增用例
// 只要用它，就不可能漏。
func newIntegrationCtx(t *testing.T) *config.Context {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	flushRedisPrefixes(t, ctx, "featuregate:*", "ratelimit:uid:*")
	// 跑完把表清掉，与仓库约定的 `defer testutil.CleanAllTables(ctx)` 同效，
	// 但集中在 fixture 里，新增用例不会漏。
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
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

type flagsBody struct {
	Flags       map[string]bool `json:"flags"`
	Unavailable []string        `json:"unavailable"`
}

func getFlags(t *testing.T, r *libwkhttp.WKHttp) (*httptest.ResponseRecorder, flagsBody) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/featuregate/flags", nil)
	req.Header.Set("token", testutil.Token)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w, flagsBody{}
	}
	var body flagsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body: %s", w.Body.String())
	return w, body
}

// TestFlagsEndpointWireContract 覆盖响应的三条 wire 约定：用 client_key 作字段名、
// 值为 false 的 key 必须出现在 flags 里、判定不可得的 key 必须显式出现在 unavailable。
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

	w, body := getFlags(t, mountFlags(t, ctx, flags, eval))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 用 client_key 作字段名，feature_key 不得出现在 wire 上——两者解耦正是为了
	// 让运维改 feature_key 不破坏客户端。
	require.Contains(t, body.Flags, "alpha")
	require.NotContains(t, w.Body.String(), "alpha_rollout", "响应必须用 client_key，不能透出 feature_key")

	require.True(t, body.Flags["alpha"])

	// 值为 false 的 key **必须**出现在 flags 里（不得被 omitempty 吞掉）。
	v, ok := body.Flags["beta"]
	require.True(t, ok, "确定性的 false 必须出现在 flags 里，实际 body: %s", w.Body.String())
	require.False(t, v)

	// 判定不可得的 key：不进 flags，而是**显式**列进 unavailable。
	//
	// 早先用"从 flags 里缺席"来表达这层含义，语义等价但任何 schema 语言都描述不了
	// 「缺席携带含义」，codegen 客户端会把缺席读成 false —— 正是这套设计要防的失败。
	_, ok = body.Flags["gamma"]
	require.False(t, ok, "判定不可得的 key 不应出现在 flags 里；body: %s", w.Body.String())
	require.Equal(t, []string{"gamma"}, body.Unavailable,
		"判定不可得的 key 必须显式列出；body: %s", w.Body.String())

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
	w, body := getFlags(t, r)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, body.Flags, "空注册表必须序列化为 {}，不能是 null；body: %s", w.Body.String())
	require.Empty(t, body.Flags)
	require.NotNil(t, body.Unavailable, "unavailable 空时必须是 []，不能是 null；body: %s", w.Body.String())
	require.Empty(t, body.Unavailable)
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
	w, body := getFlags(t, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, body.Flags, 2, "无规则时仍应下发全部已注册 key（值为 false）")
	require.False(t, body.Flags["zero_a"])
	require.False(t, body.Flags["zero_b"])
	require.Empty(t, body.Unavailable, "规则不存在是确定性的关，不是判定不可得")
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
	w, body := getFlags(t, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, body.Flags["e2e"], "登录用户在 user 白名单内应当为 true")

	// 把白名单换成别人，同一个登录用户应当变 false。
	_, err := db.deleteScope(featureKey, fg.ScopeTypeUser, testutil.UID)
	require.NoError(t, err)
	seedScope(t, svc, db, featureKey, fg.ScopeTypeUser, "someone_else")

	_, body = getFlags(t, r)
	require.False(t, body.Flags["e2e"], "不在白名单的用户应当为 false")
}

// TestFlagsEndpointEmptyUIDReportsUnavailable 钉住空 uid 走 unavailable 而非全 false。
//
// AuthMiddleware 之后 uid 必然非空，所以这是不可达路径——但它一旦发生，返回「每个
// key 都确定性地 false」恰恰是本模块最想避免的那个形状：客户端会用它覆盖本地缓存，
// 功能对所有人消失。没有 uid 时答案本就**不可知**，而不可知正是 unavailable 的语义。
// 让不变量由代码保证，而不是靠注释声称"不会发生"。
func TestFlagsEndpointEmptyUIDReportsUnavailable(t *testing.T) {
	ctx := newIntegrationCtx(t)

	flags := []ClientFlag{
		{FeatureKey: "alpha_rollout", ClientKey: "alpha"},
		{FeatureKey: "beta_rollout", ClientKey: "beta"},
	}
	// 绕过 AuthMiddleware 直接挂 handler，模拟鉴权链被改坏的情形。
	r := libwkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	api := newFlagsAPI(ctx, stubEvaluator{allow: map[string]bool{"alpha_rollout": true}},
		mustNewClientRegistry(flags))
	r.GET("/v1/featuregate/flags", api.get)

	w, body := getFlags(t, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, body.Flags, "没有 uid 时不得给出任何确定性判定；body: %s", w.Body.String())
	require.ElementsMatch(t, []string{"alpha", "beta"}, body.Unavailable,
		"答案不可知时每个 key 都应进 unavailable，客户端才会保留旧值")
}
