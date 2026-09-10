package oidc

import (
	"net/url"
	"strings"
	"testing"
)

// 编译期断言:plain-OAuth2 实现必须满足 AuthProvider。
// 接口一旦被两个实现共同约束,新增第三个 IdP 就不需要再改调用方。
var _ AuthProvider = (*oauth2Provider)(nil)

func newTestOAuth2Provider(t *testing.T) *oauth2Provider {
	t.Helper()
	p, err := newOAuth2Provider(oauth2ProviderConfig{
		Issuer:       "test-idp",
		BaseURL:      "https://idp.example.com",
		ClientID:     "cid",
		ClientSecret: "csecret",
		RedirectURI:  "https://app.example.com/v1/auth/oidc/test/callback",
		Scopes:       []string{"read"},
	})
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	return p
}

// Capabilities 是业务分支的唯一依据(而不是 Kind),所以它必须诚实地声明
// 本协议**没有**的能力 —— 上层据此跳过 nonce 比对、id_token 验签、
// refresh 轮转,而不是靠 if kind == ... 散落判断。
func TestOAuth2Provider_CapabilitiesDeclareMissingFeatures(t *testing.T) {
	caps := newTestOAuth2Provider(t).Capabilities()

	if caps.PKCE {
		t.Error("PKCE = true; the upstream authorize endpoint has no code_challenge parameter")
	}
	if caps.Nonce {
		t.Error("Nonce = true; there is no signed payload to carry a nonce back")
	}
	if caps.IDToken {
		t.Error("IDToken = true; the token response carries no id_token")
	}
	if caps.RefreshToken {
		t.Error("RefreshToken = true; a refresh_token is returned but the spec documents no refresh endpoint")
	}
	if caps.CrossCheckSub {
		t.Error("CrossCheckSub = true; there is only one identity source, so no cross-check is possible")
	}
	if !caps.UpstreamLogout {
		t.Error("UpstreamLogout = false; the IdP does provide a front-channel single-logout endpoint")
	}
}

func TestOAuth2Provider_KindAndIssuer(t *testing.T) {
	p := newTestOAuth2Provider(t)
	if p.Kind() != KindOAuth2 {
		t.Errorf("Kind = %q, want %q", p.Kind(), KindOAuth2)
	}
	// issuer 是我方配置的稳定命名空间,不是从 IdP 读来的。
	if p.Issuer() != "test-idp" {
		t.Errorf("Issuer = %q, want %q", p.Issuer(), "test-idp")
	}
}

func TestOAuth2Provider_AuthCodeURL(t *testing.T) {
	p := newTestOAuth2Provider(t)
	raw, err := p.AuthCodeURL(AuthCodeParams{State: "st-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("unparseable URL %q: %v", raw, err)
	}
	if u.Path != "/oauth/authorize" {
		t.Errorf("path = %q, want /oauth/authorize", u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code",
		"scope":         "read",
		"client_id":     "cid",
		"redirect_uri":  "https://app.example.com/v1/auth/oidc/test/callback",
		"state":         "st-123",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// client_secret 只在 token 端点出现,绝不能进浏览器可见的 authorize URL。
	if q.Has("client_secret") {
		t.Error("authorize URL leaks client_secret into the browser")
	}
}

// 即使调用方传了 nonce / code_challenge(标准 OIDC 路径会传),也不能发给
// 这个 IdP:协议里没有这两个参数,对方对未注册参数的处理未经验证,
// 而它们在这里也提供不了任何保护。
func TestOAuth2Provider_AuthCodeURLOmitsUnsupportedParams(t *testing.T) {
	p := newTestOAuth2Provider(t)
	raw, err := p.AuthCodeURL(AuthCodeParams{
		State:         "st-123",
		Nonce:         "should-not-be-sent",
		CodeChallenge: "should-not-be-sent-either",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, forbidden := range []string{"nonce", "code_challenge", "code_challenge_method"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("authorize URL carries unsupported parameter %q: %s", forbidden, raw)
		}
	}
}

// state 是本协议下唯一的 CSRF 绑定(IdP 侧文档把它标为可选,其参考实现
// 甚至完全不读它),所以我方必须强制存在 —— 空 state 属于编程错误,
// 不能静默生成一个无保护的 authorize URL。
func TestOAuth2Provider_AuthCodeURLRequiresState(t *testing.T) {
	p := newTestOAuth2Provider(t)
	if _, err := p.AuthCodeURL(AuthCodeParams{State: ""}); err == nil {
		t.Fatal("want error for empty state; state is the only CSRF binding in this protocol")
	}
}

func TestNewOAuth2Provider_RejectsIncompleteConfig(t *testing.T) {
	base := oauth2ProviderConfig{
		Issuer: "test-idp", BaseURL: "https://idp.example.com",
		ClientID: "cid", ClientSecret: "csecret",
		RedirectURI: "https://app.example.com/cb", Scopes: []string{"read"},
	}
	cases := []struct {
		name   string
		mutate func(*oauth2ProviderConfig)
	}{
		{"empty_issuer", func(c *oauth2ProviderConfig) { c.Issuer = "" }},
		{"empty_base_url", func(c *oauth2ProviderConfig) { c.BaseURL = "" }},
		{"empty_client_id", func(c *oauth2ProviderConfig) { c.ClientID = "" }},
		{"empty_client_secret", func(c *oauth2ProviderConfig) { c.ClientSecret = "" }},
		{"empty_redirect_uri", func(c *oauth2ProviderConfig) { c.RedirectURI = "" }},
		{"relative_base_url", func(c *oauth2ProviderConfig) { c.BaseURL = "/oauth" }},
		{"non_http_base_url", func(c *oauth2ProviderConfig) { c.BaseURL = "javascript:alert(1)" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := newOAuth2Provider(cfg); err == nil {
				t.Fatal("want error for incomplete config; a half-configured provider must not start")
			}
		})
	}
}
