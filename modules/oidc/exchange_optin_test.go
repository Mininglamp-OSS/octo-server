package oidc

// exchange_optin_test.go — 两个 exchange 端点必须显式开启,不能靠 DM_OIDC_ENABLED 顺带挂上。
//
// main 上 oidc 模块只有三条路由(authorize / callback / logout)。本改动之后,
// **每一个**已经设了 DM_OIDC_ENABLED=true 的部署同时白得两个未认证的会话签发端点,
// 而且没有任何办法只关它们 —— 只能整个关掉 OIDC。
//
// 为什么这算回归而不只是新功能:kind=oidc 下 /exchange 的语义是"出示 id_token,
// 换一个完整会话",且没有 nonce 绑定、没有重放记录。id_token 是**前端信道产物** ——
// 它本来就会出现在浏览器历史、前端 JS、客户端存储和 Referer 里,那是它的用途,
// 泄漏后的影响原本是"一个会过期作废的断言"。挂上这个端点之后,同一个泄漏物在
// exp 之前等价于该用户的账号,而会话本身还能再活约 30 天。
//
// 本 PR 的 brief 声称"对存量 OIDC 部署无行为变更" —— 多挂两个会话签发端点与这句
// 话冲突。默认关掉之后这句话重新成立,而需要它的部署显式打开一个开关。

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// minimalBootableOIDCEnv 一份能通过 LoadConfig 的最小标准 OIDC 配置。
func minimalBootableOIDCEnv(t *testing.T) {
	t.Helper()
	clearOIDCEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// 未显式开启时,配置里的 exchange 开关必须是关的。
func TestLoadConfig_ExchangeEndpointsAreOptIn(t *testing.T) {
	minimalBootableOIDCEnv(t)
	// 刻意不设 OCTO_OIDC_EXCHANGE_ENABLED。
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	if cfg.ExchangeEnabled {
		t.Error("the exchange endpoints defaulted to on. Every deployment that already has " +
			"DM_OIDC_ENABLED=true would gain two unauthenticated session-minting endpoints " +
			"it never asked for, with no way to decline short of turning OIDC off entirely")
	}
}

// 显式打开时必须真的生效 —— 否则需要它的部署没法用。
func TestLoadConfig_ExchangeEndpointsHonourTheFlag(t *testing.T) {
	minimalBootableOIDCEnv(t)
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "true")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	if !cfg.ExchangeEnabled {
		t.Error("the flag was set but did not take effect")
	}
}

// 真正要钉的是**路由挂不挂** —— 配置字段的零值会让上面那条默认用例空转通过。
//
// 走真实 Route(),按 gin 的已注册路由表断言,而不是猜。
// 关掉时:两个端点不挂载,而且**连限流器的 Redis client 都不构造** ——
// 那是为一组不存在的端点开一个连接池。
func TestRoute_ExchangeOptOutMountsNothingAndBuildsNoLimiter_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	minimalBootableOIDCEnv(t)
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "")
	cfg, err := LoadConfig()
	require.NoError(t, err)

	o := &OIDC{Log: log.NewTLog("OIDC-optout"), cfg: cfg, ctx: ctx}
	defer func() { _ = o.Close() }()
	r := wkhttp.New()
	o.Route(r)

	for _, path := range []string{
		"/v1/auth/oidc/oidc/exchange",
		"/v1/auth/oidc/oidc/exchange-jwt",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s answered HTTP %d, want 404 — main exposes only authorize/"+
				"callback/logout, so mounting these by default hands every existing "+
				"deployment two unauthenticated session-minting endpoints it cannot decline",
				path, w.Code)
		}
	}

	if n := len(o.exchangeLimiterClients); n != 0 {
		t.Errorf("%d exchange rate-limiter Redis client(s) were constructed for endpoints "+
			"that are not mounted", n)
	}

	// 既有路由必须不受影响。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/auth/oidc/oidc/authorize", nil))
	if w.Code == http.StatusNotFound {
		t.Error("authorize went missing; this flag must not affect pre-existing routes")
	}
}

// 打开时必须真的挂上 —— 否则需要这两个端点的部署没法用。
func TestRoute_ExchangeEndpointsMountedWhenOptedIn_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	minimalBootableOIDCEnv(t)
	t.Setenv("OCTO_OIDC_EXCHANGE_ENABLED", "true")
	cfg, err := LoadConfig()
	require.NoError(t, err)

	o := &OIDC{Log: log.NewTLog("OIDC-optin-on"), cfg: cfg, ctx: ctx}
	defer func() { _ = o.Close() }()
	r := wkhttp.New()
	o.Route(r)

	for _, path := range []string{
		"/v1/auth/oidc/oidc/exchange",
		"/v1/auth/oidc/oidc/exchange-jwt",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("POST %s is not mounted even though the flag is on", path)
		}
	}
}
