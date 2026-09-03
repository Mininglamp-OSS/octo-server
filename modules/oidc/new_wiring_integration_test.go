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
