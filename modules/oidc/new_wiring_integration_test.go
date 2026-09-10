package oidc

import (
	"encoding/base64"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

// 走真实 New() 的接线测试。
//
// 为什么必须有:RP-Initiated Logout 的 id_token 缓存曾经被一次重构整块删掉,
// 而**没有任何测试发现** —— 因为每个 logout/bind 测试都手工 `o.idTokens = ...`
// 注入了一个 double。于是套件全绿,生产上 o.idTokens 恒为 nil:callback 不缓存
// id_token,logout 拿不到 id_token_hint,end_session_url 永不下发,用户登出
// DMWork 之后在 IdP 侧仍是登录态。
//
// 这正是本 PR 自己记下的那条 learning(double 必须复现生产行为)的另一面:
// double 再忠实,也证明不了"生产真的把它装上了"。所以要有一条从 New() 出发的
// 用例断言接线存在。
// -----------------------------------------------------------------------------

// setNewWiringEnv 铺一份可启动的标准 OIDC 配置,指向 mock IdP。
func setNewWiringEnv(t *testing.T, mp *MockProvider) {
	t.Helper()
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", mp.Issuer)
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", mp.ClientID)
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/callback")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	// mock 的 end_session 是 http(httptest),放宽位允许它。
	t.Setenv("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", "1")
	// 与 kind 相关的键清空,避免受其他用例残留影响。
	for _, k := range []string{
		"OCTO_OIDC_PROVIDER_KIND", "OCTO_OIDC_PROVIDER_BASE_URL",
		"OCTO_OIDC_PROVIDER_APP_ID", "OCTO_OIDC_PROVIDER_END_SESSION_URL",
		"OCTO_OIDC_BEARER_JWT_SECRET",
	} {
		t.Setenv(k, "")
	}
}

// 配了回跳地址 + Discovery 给出可用 end_session 端点 → id_token 缓存必须被装上。
func TestNew_WiresIDTokenStoreWhenRPLogoutConfigured_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	require.NotNil(t, o.cfg, "config must load")
	require.NotNil(t, o.provider, "provider must be constructed")
	assert.NotNil(t, o.idTokens,
		"the id_token store must be wired by New(); without it logout never emits an "+
			"id_token_hint and the upstream IdP session is never ended")
}

// 没配回跳地址 → 不装(RP-Initiated Logout 未启用是合法形态)。
func TestNew_NoIDTokenStoreWithoutPostLogoutRedirect_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	require.NotNil(t, o.provider)
	assert.Nil(t, o.idTokens,
		"without a post-logout redirect there is nothing to hand off, so the cache "+
			"must stay unwired rather than holding id_tokens for no consumer")
}

// plain-OAuth2 kind 不装:该协议没有 id_token,登出走 SLO 的 appId 路径。
func TestNew_NoIDTokenStoreForOAuth2Kind_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
	t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", mp.Issuer)
	t.Setenv("OCTO_OIDC_ALLOW_INSECURE_UPSTREAM", "1")
	t.Setenv("OCTO_OIDC_PROVIDER_APP_ID", "app1")
	t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	require.NotNil(t, o.provider)
	assert.Nil(t, o.idTokens,
		"the plain-OAuth2 protocol has no id_token; caching one would be dead weight")
}

// 回归守卫:构造函数不能变成死代码。
//
// 上一次的缺陷形态就是"构造函数还在,但没人调用了"。这条用例直接断言生产装配
// 会产出一个非 nil 的 store,所以删掉调用点会让它红 —— 而单纯的编译或 vet 不会。
func TestNew_IDTokenStoreConstructorIsReachableFromProduction_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	t.Setenv("OCTO_OIDC_POST_LOGOUT_REDIRECT_URI", "https://app.example.com/login")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	store, ok := o.idTokens.(*redisIDTokenStore)
	assert.True(t, ok,
		"production must wire the real redis-backed store, not a nil interface or a "+
			"test double; got %T", o.idTokens)
	assert.NotNil(t, store)
}

// 兑换台账的接线,理由与上面的 id_token 缓存完全一样。
//
// /exchange-jwt 的所有 handler 级用例都手工 `o.redeemLedger = ...` 注入 double
// (redemption_ledger_test.go),所以 New() 哪天不再构造台账,它们照样全绿 ——
// 而生产上的后果是这条路径**永久**降级成"只按 iat 判一个上限":空闲窗口 T 完全
// 不生效,且不会自愈。degraded/unconfigured 两组 metric label 能在事后区分出这种
// 状态,这条用例是在它发生**之前**挡住。
func TestNew_WiresRedemptionLedgerWhenExchangeEnabled_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	// 端点是显式选择的(见 Config.ExchangeEnabled),且需要一把够长的验签密钥。
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "true")
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", "wiring-test-secret-not-real-0123456789")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	require.NotNil(t, o.bearerJWT, "the bearer verifier must be wired for this scenario")
	led, ok := o.redeemLedger.(*redisRedemptionLedger)
	require.True(t, ok,
		"production must wire the real redis-backed ledger whenever /exchange-jwt is "+
			"mounted; got %T — without it the endpoint silently runs on the iat ceiling "+
			"alone and T is never enforced", o.redeemLedger)
	assert.NotNil(t, led.client, "the ledger must hold a live Redis client")
	// 策略必须是收敛后的取值 —— 启动日志打印的也是这两个数。
	assert.Equal(t, o.redeemPolicy.normalized(), o.redeemPolicy,
		"New() must store the normalized policy, so the startup log reports what actually applies")
}

// 端点没开 → 不构造台账(不给一组不存在的路由开 Redis 连接池)。
func TestNew_NoRedemptionLedgerWhenExchangeDisabled_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "false")
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", "wiring-test-secret-not-real-0123456789")

	o := New(ctx)
	t.Cleanup(func() { _ = o.Close() })

	assert.Nil(t, o.redeemLedger,
		"opening a connection pool for routes that are not mounted is waste; the handler "+
			"is served by o.disabled and never reaches admission")
	// 策略仍然加载:降级判定要用到它,而它不依赖端点是否挂载。
	assert.Positive(t, o.redeemPolicy.firstRedeemMaxAge, "the policy must load regardless")
}
