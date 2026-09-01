package oidc

import (
	"context"
	"strings"
	"testing"
)

func newProviderAgainstMock(t *testing.T, m *mockOAuth2Provider) *oauth2Provider {
	t.Helper()
	p, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	return p
}

// 对方的官方参考实现是 POST + 全部参数在 query string + 空 form body。
// 那是唯一被验证过的调用形态,所以我方必须照发 —— form body 虽然在 Spring
// 侧理论可行,但未经验证,上游网关可能只读 query。本测试把形态钉死。
func TestOAuth2Provider_ExchangeSendsParamsInQuery(t *testing.T) {
	m := newMockOAuth2Provider(t)
	p := newProviderAgainstMock(t, m)

	tok, err := p.Exchange(context.Background(), "the-code", "")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != m.KnownAccessToken {
		t.Errorf("AccessToken = %q, want %q", tok.AccessToken, m.KnownAccessToken)
	}
	// 该协议不下发 id_token;上层据此跳过验签与 RP-Initiated Logout。
	if tok.RawIDToken != "" {
		t.Errorf("RawIDToken = %q, want empty (this protocol has no ID token)", tok.RawIDToken)
	}

	rec := m.LastTokenRequest
	if rec == nil {
		t.Fatal("token endpoint was not called")
	}
	if rec.Method != "POST" {
		t.Errorf("method = %s, want POST", rec.Method)
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"client_id":     "cid",
		"client_secret": "csecret",
		"redirect_uri":  "https://app.example.com/v1/auth/oidc/test/callback",
	} {
		if got := rec.Query.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
	// 参考实现即使 body 为空也带 form content type,而对方的错误码表里列了
	// 415 UnsupportedMediaType —— 说明它确实会因 Content-Type 拒请求。
	if !strings.HasPrefix(rec.ContentType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded "+
			"(the documented error table includes 415)", rec.ContentType)
	}
	// PKCE 不存在于该协议,不能把 code_verifier 发出去。
	if rec.Query.Has("code_verifier") {
		t.Error("sent code_verifier; this protocol has no PKCE")
	}
}

func TestOAuth2Provider_ExchangeRejectsBadInputAndResponses(t *testing.T) {
	t.Run("empty_code", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		p := newProviderAgainstMock(t, m)
		if _, err := p.Exchange(context.Background(), "", ""); err == nil {
			t.Fatal("want error for empty code")
		}
	})
	t.Run("http_error_status", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.TokenStatus = 400
		p := newProviderAgainstMock(t, m)
		if _, err := p.Exchange(context.Background(), "c", ""); err == nil {
			t.Fatal("want error for HTTP 400")
		}
	})
	t.Run("response_without_access_token", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.TokenBody = `{"token_type":"bearer","expires_in":7199}`
		p := newProviderAgainstMock(t, m)
		if _, err := p.Exchange(context.Background(), "c", ""); err == nil {
			t.Fatal("want error when the response carries no access_token")
		}
	})
	t.Run("malformed_json", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.TokenBody = `{"access_token":`
		p := newProviderAgainstMock(t, m)
		if _, err := p.Exchange(context.Background(), "c", ""); err == nil {
			t.Fatal("want error for malformed JSON")
		}
	})
}

// 传输层失败时 Go 的 *url.Error 会带上完整 URL,而 client_secret 就在 query 里。
// provider 边界必须把它剥掉,不能指望每个调用方都记得别打 err。
func TestOAuth2Provider_ExchangeErrorDoesNotLeakSecret(t *testing.T) {
	cfg := oauth2ProviderConfig{
		Issuer: "test-idp",
		// 指向一个不可路由地址,强制走传输层失败分支。
		BaseURL:      "http://127.0.0.1:1",
		ClientID:     "cid",
		ClientSecret: "top-secret-value",
		RedirectURI:  "https://app.example.com/cb",
	}
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	_, err = p.Exchange(context.Background(), "the-code", "")
	if err == nil {
		t.Fatal("want a transport error")
	}
	for _, leak := range []string{"top-secret-value", "client_secret", "the-code"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q:\n  %s", leak, err)
		}
	}
}

// userinfo 的 token 在 query 而不是 Authorization 头 —— 这是该 IdP 的形态,
// go-oidc 的 UserInfo 只会走 Bearer 头,所以这条路径必须自己实现。
func TestOAuth2Provider_IdentityPutsTokenInQuery(t *testing.T) {
	m := newMockOAuth2Provider(t)
	p := newProviderAgainstMock(t, m)

	claims, err := p.Identity(context.Background(), &TokenSet{AccessToken: m.KnownAccessToken})
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if claims.Subject != m.SubjectForUserInfo {
		t.Errorf("Subject = %q, want %q", claims.Subject, m.SubjectForUserInfo)
	}
	if claims.Issuer != "test-idp" {
		t.Errorf("Issuer = %q, want the configured namespace", claims.Issuer)
	}
	if claims.Name != "Mock User" || claims.Email != "mock@example.com" {
		t.Errorf("claims not mapped: %+v", claims)
	}
	// 本协议无 verified 语义,必须 fail-closed,否则 autolink 会拿未验证邮箱认领账号。
	if claims.EmailVerified || claims.PhoneVerified {
		t.Error("verified flags must stay false for this protocol")
	}

	rec := m.LastUserInfoRequest
	if rec == nil {
		t.Fatal("userinfo endpoint was not called")
	}
	if got := rec.Query.Get("access_token"); got != m.KnownAccessToken {
		t.Errorf("access_token not in query: %q", got)
	}
	if rec.AuthHeader != "" {
		t.Errorf("sent an Authorization header (%q); this IdP reads the token from the query", rec.AuthHeader)
	}
}

func TestOAuth2Provider_IdentityRejectsBadInputAndResponses(t *testing.T) {
	t.Run("nil_token", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		p := newProviderAgainstMock(t, m)
		if _, err := p.Identity(context.Background(), nil); err == nil {
			t.Fatal("want error for nil token")
		}
	})
	t.Run("empty_access_token", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		p := newProviderAgainstMock(t, m)
		if _, err := p.Identity(context.Background(), &TokenSet{}); err == nil {
			t.Fatal("want error for empty access token")
		}
	})
	t.Run("envelope_failure_with_http_200", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		// 关键场景:HTTP 200 承载业务失败。只看状态码就会把它当登录成功。
		m.UserInfoBody = `{"success":false,"code":"403","message":"locked"}`
		p := newProviderAgainstMock(t, m)
		if _, err := p.Identity(context.Background(), &TokenSet{AccessToken: "x"}); err == nil {
			t.Fatal("want error: HTTP 200 with success=false must not be treated as a login")
		}
	})
	t.Run("empty_subject", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.UserInfoBody = `{"success":true,"code":"200","data":{"sub":""}}`
		p := newProviderAgainstMock(t, m)
		if _, err := p.Identity(context.Background(), &TokenSet{AccessToken: "x"}); err == nil {
			t.Fatal("want error for empty subject; it would collapse users onto one identity row")
		}
	})
	t.Run("http_error_status", func(t *testing.T) {
		m := newMockOAuth2Provider(t)
		m.UserInfoStatus = 401
		p := newProviderAgainstMock(t, m)
		if _, err := p.Identity(context.Background(), &TokenSet{AccessToken: "x"}); err == nil {
			t.Fatal("want error for HTTP 401")
		}
	})
}

func TestOAuth2Provider_IdentityErrorDoesNotLeakToken(t *testing.T) {
	cfg := oauth2ProviderConfig{
		Issuer: "test-idp", BaseURL: "http://127.0.0.1:1",
		ClientID: "cid", ClientSecret: "csecret", RedirectURI: "https://app.example.com/cb",
	}
	p, err := newOAuth2Provider(cfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	_, err = p.Identity(context.Background(), &TokenSet{AccessToken: "secret-access-token-value"})
	if err == nil {
		t.Fatal("want a transport error")
	}
	for _, leak := range []string{"secret-access-token-value", "access_token"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q:\n  %s", leak, err)
		}
	}
}

// LogoutURL 在 AppID 或回跳地址缺失时必须降级为 ("", false),
// 而不是产出畸形 URL —— 上层据此退回"仅清本地"。
func TestOAuth2Provider_LogoutURL(t *testing.T) {
	m := newMockOAuth2Provider(t)

	t.Run("configured", func(t *testing.T) {
		p := newProviderAgainstMock(t, m)
		raw, ok := p.LogoutURL(context.Background(), LogoutHint{UID: "u1"})
		if !ok {
			t.Fatal("want ok=true when app id and redirect are configured")
		}
		if !strings.Contains(raw, "/"+upstreamLogoutPathPrefix+"/app1") {
			t.Errorf("app id is not in the path segment: %s", raw)
		}
		if !strings.Contains(raw, "redirect_url=") {
			t.Errorf("missing redirect_url parameter: %s", raw)
		}
	})

	t.Run("missing_app_id_degrades", func(t *testing.T) {
		cfg := m.providerConfig()
		cfg.AppID = ""
		p, err := newOAuth2Provider(cfg)
		if err != nil {
			t.Fatalf("newOAuth2Provider: %v", err)
		}
		if _, ok := p.LogoutURL(context.Background(), LogoutHint{}); ok {
			t.Error("want ok=false when the logout app id is not configured")
		}
	})

	t.Run("missing_redirect_degrades", func(t *testing.T) {
		cfg := m.providerConfig()
		cfg.PostLogoutRedirectURI = ""
		p, err := newOAuth2Provider(cfg)
		if err != nil {
			t.Fatalf("newOAuth2Provider: %v", err)
		}
		if _, ok := p.LogoutURL(context.Background(), LogoutHint{}); ok {
			t.Error("want ok=false when the post-logout redirect is not configured")
		}
	})
}

// 上游在协议错误时返回 Spring Security 的标准形态
// {"error":"...","error_description":"..."},而不是文档描述的成功信封。
//
// 我方必须把 error 这个**枚举字段**带进错误信息 —— 它是区分
// "code 无效" / "凭据被拒" / "缺凭据" 的唯一依据,联调和线上排查都靠它。
// 但 error_description 不能带:实测见过其中包含手机号一类用户数据,
// 而本 error 会一路 wrap 到 zap.Error 落进日志。
func TestOAuth2Provider_UpstreamErrorCodeIsSurfacedWithoutDescription(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantInErr string
		wantNotIn []string
	}{
		{
			name:      "invalid_grant",
			status:    400,
			body:      `{"error":"invalid_grant","error_description":"Invalid authorization code: SECRET-CODE-123"}`,
			wantInErr: "invalid_grant",
			wantNotIn: []string{"SECRET-CODE-123", "Invalid authorization code"},
		},
		{
			name:      "invalid_client",
			status:    401,
			body:      `{"error":"invalid_client","error_description":"Bad client credentials for user 13000000000"}`,
			wantInErr: "invalid_client",
			wantNotIn: []string{"13000000000", "Bad client credentials"},
		},
		{
			name:      "unauthorized",
			status:    401,
			body:      `{"error":"unauthorized","error_description":"An Authentication object was not found in the SecurityContext"}`,
			wantInErr: "unauthorized",
			wantNotIn: []string{"SecurityContext"},
		},
		{
			// 上游/中间设备返回 HTML 错误页时不能崩,退回只报状态码。
			name:      "non_json_body_falls_back_to_status",
			status:    502,
			body:      `<html><body>Bad Gateway from edge proxy</body></html>`,
			wantInErr: "502",
			wantNotIn: []string{"Bad Gateway from edge proxy"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockOAuth2Provider(t)
			m.TokenStatus = tc.status
			m.TokenBody = tc.body
			p, err := newOAuth2Provider(m.providerConfig())
			if err != nil {
				t.Fatalf("newOAuth2Provider: %v", err)
			}
			_, err = p.Exchange(context.Background(), "c", "")
			if err == nil {
				t.Fatal("want error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantInErr) {
				t.Errorf("error does not surface %q, leaving nothing to diagnose with:\n  %s", tc.wantInErr, msg)
			}
			for _, leak := range tc.wantNotIn {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks upstream description %q (may contain user data):\n  %s", leak, msg)
				}
			}
		})
	}
}
