package oidc

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// liveUpstreamConfig 从环境变量读真实上游凭据;缺失即跳过。
//
// 跑法(凭据不入库,从本地文件注入):
//
//	set -a; . <local-credentials-file>; set +a   # gitignored, not checked in
//	OCTO_OIDC_LIVE_TEST=1 go test ./modules/oidc/ -run Live -v
//
// 为什么值得有:我们没有真实的 authorization code 或 access_token(那要走完
// 浏览器登录),但**错误路径打真实上游**已经能验证一批只有联调才暴露的问题:
//   - BaseURL + 路径常量是否命中真实端点(拼错会是 404,而不是 invalid_grant)
//   - Content-Type 是否被接受(对方错误码表里有 415)
//   - client_id/secret 是否被认(不认会是 invalid_client,而不是 invalid_grant)
//   - 真实传输层错误下凭据是否泄漏
//   - 我们的 http.Client 实际走不走环境代理
func liveUpstreamConfig(t *testing.T) oauth2ProviderConfig {
	t.Helper()
	if os.Getenv("OCTO_OIDC_LIVE_TEST") == "" {
		t.Skip("set OCTO_OIDC_LIVE_TEST=1 (and the upstream credentials) to run live upstream tests")
	}
	// SSO_GET_CODE_URL 实际是 token 端点,SSO_AUTH_URL 实际是 userinfo 端点
	// (提供方的键名与用途不一致)。这里只取站点根,路径由我方常量拼。
	tokenURL := os.Getenv("SSO_GET_CODE_URL")
	clientID := os.Getenv("SSO_CLIENT_ID")
	clientSecret := os.Getenv("SSO_CLIENT_SECRET")
	redirect := os.Getenv("SSO_REDIRECT")
	if tokenURL == "" || clientID == "" || clientSecret == "" || redirect == "" {
		t.Skip("upstream credentials not present in env; skipping live test")
	}
	base := strings.TrimSuffix(tokenURL, "/"+upstreamTokenPath)
	if base == tokenURL {
		t.Fatalf("cannot derive base URL: token endpoint %q does not end with %q",
			tokenURL, "/"+upstreamTokenPath)
	}
	return oauth2ProviderConfig{
		Issuer:       "live-probe-issuer",
		BaseURL:      base,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirect,
	}
}

// 用一个必然无效的 code 打真实 token 端点。
//
// 期望:invalid_grant。这个结果比"成功"更有信息量 —— 它同时证明了端点拼对、
// Content-Type 被接受、凭据被认。任何其它结果都指向一个具体的配置错误。
func TestLiveUpstream_ExchangeRejectsFakeCode_Integration(t *testing.T) {
	cfg := liveUpstreamConfig(t)
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	t.Logf("base URL = %s", cfg.BaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err = p.Exchange(ctx, "FAKE-CODE-FROM-GO-INTEGRATION-TEST", "")
	if err == nil {
		t.Fatal("a fake authorization code was accepted; this should be impossible")
	}
	msg := err.Error()
	t.Logf("upstream error (as our layer surfaces it):\n  %s", msg)

	// 诊断分流:每种结果对应一个具体的配置问题。
	switch {
	case strings.Contains(msg, "invalid_grant"):
		t.Log("=> OK: endpoint reachable, Content-Type accepted, client credentials accepted")
	case strings.Contains(msg, "invalid_client"):
		t.Error("=> client_id / client_secret rejected by upstream")
	case strings.Contains(msg, "404"):
		t.Errorf("=> 404: base URL + %q did not hit the real token endpoint", upstreamTokenPath)
	case strings.Contains(msg, "415"):
		t.Error("=> 415: upstream rejected our Content-Type")
	case strings.Contains(msg, "unauthorized"):
		t.Error("=> upstream saw no credentials at all; check how we transmit client_id/secret")
	default:
		t.Errorf("=> unexpected upstream failure mode, needs a human look")
	}

	// 真实传输/协议错误下凭据不得泄漏。
	if strings.Contains(msg, cfg.ClientSecret) {
		t.Error("!! client_secret leaked into the error surfaced to callers")
	}
}

// 用一个必然无效的 access_token 打真实 userinfo 端点。
//
// 真实上游在这种情况下返回的是 Spring Security 的
// {"error":"invalid_token",...},而**不是**文档描述的 {success,code,...} 信封。
// 我方必须拒登(信封校验 fail-closed),这个测试钉住这一点。
func TestLiveUpstream_IdentityRejectsFakeToken_Integration(t *testing.T) {
	cfg := liveUpstreamConfig(t)
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	claims, err := p.Identity(ctx, &TokenSet{AccessToken: "FAKE-ACCESS-TOKEN-FROM-GO-TEST"})
	if err == nil {
		t.Fatalf("a fake access token produced claims: %+v", claims)
	}
	t.Logf("upstream error (as our layer surfaces it):\n  %s", err.Error())

	if strings.Contains(err.Error(), cfg.ClientSecret) {
		t.Error("!! client_secret leaked")
	}
	if strings.Contains(err.Error(), "FAKE-ACCESS-TOKEN-FROM-GO-TEST") {
		t.Error("!! access token echoed back into the error (it would reach the logs)")
	}
}

// 我方 http.Client 的 Transport 为 nil,因此走 http.DefaultTransport,
// 后者会读 HTTP(S)_PROXY 环境变量。生产 pod 里若注入了出口代理,IdP 调用就会
// 经由它 —— 通常是期望行为,但必须是**知情**的选择,所以这里把实际去向打出来。
func TestLiveUpstream_ProxyBehaviour_Integration(t *testing.T) {
	_ = liveUpstreamConfig(t) // 仅用于 gate
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY", "no_proxy", "NO_PROXY"} {
		if v := os.Getenv(k); v != "" {
			t.Logf("%s is set (value hidden)", k)
		}
	}
	t.Log("note: our provider uses &http.Client{Timeout} with a nil Transport, " +
		"so it inherits http.DefaultTransport and therefore honours the proxy env vars above")
}

// 指向黑洞地址,验证真实的 *url.Error 路径不泄漏 query 上的凭据。
// 这条不需要上游可达,但放在一起便于一次性跑完。
func TestLiveUpstream_TransportErrorIsSanitised_Integration(t *testing.T) {
	cfg := liveUpstreamConfig(t)
	cfg.BaseURL = "http://127.0.0.1:1"
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	_, err = p.Exchange(context.Background(), "c", "")
	if err == nil {
		t.Fatal("want a transport error")
	}
	t.Logf("sanitised transport error:\n  %s", err.Error())
	if strings.Contains(err.Error(), cfg.ClientSecret) {
		t.Error("!! client_secret leaked from a real *url.Error")
	}
	var ue interface{ Timeout() bool }
	if errors.As(err, &ue) {
		t.Logf("(underlying error still classifiable: Timeout()=%v)", ue.Timeout())
	}
}

// 用真实配置生成 authorize URL,供人工在浏览器里跑一次登录、抄回 code。
//
// 同时也是对 AuthCodeURL 的真实配置验证:redirect_uri 必须与 IdP 侧注册值
// **完全一致**(对方会校验,错误码表里有 Redirect URI mismatch),任何编码差异
// 都会在这一步暴露。
//
// state 在手工流程里不会被我方 callback 校验(code 不回调到我们),这里只是
// 满足协议要求并便于在 302 Location 里辨认是哪一次尝试。
func TestLiveUpstream_PrintAuthorizeURL_Integration(t *testing.T) {
	cfg := liveUpstreamConfig(t)
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	raw, err := p.AuthCodeURL(AuthCodeParams{State: "manual-probe-001"})
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	t.Log("")
	t.Log("在浏览器打开下面这个 URL,完成登录后从 302 的 Location 里抄出 code:")
	t.Log("")
	t.Logf("%s", raw)
	t.Log("")
	t.Logf("回调会落到(我们拿不到,需从开发者工具的 Location 抄): %s", cfg.RedirectURI)
	t.Log("提示: code 一次性、有效期通常数分钟,抄到后尽快用。")
}
