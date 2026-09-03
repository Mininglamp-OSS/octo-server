package oidc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// conformanceFixture 一个待验证的 provider 实现,连同能走通 happy path 所需的材料。
type conformanceFixture struct {
	name     string
	provider AuthProvider
	// happyCode 能成功换到 token 的授权码。
	happyCode string
	// wantSubject happy path 之后期望拿到的 subject。
	wantSubject string
	// unroutable 指向不可路由地址的同类 provider,用于验证传输层错误不泄漏凭据。
	unroutable AuthProvider
	// secretsThatMustNotLeak 该 fixture 配置里的敏感值。
	secretsThatMustNotLeak []string
}

func oidcConformanceFixture(t *testing.T) conformanceFixture {
	t.Helper()
	mp := NewMockProvider(t)
	const sub, nonce, code = "oidc-subject-1", "nonce-1", "code-1"
	mp.PrepCode(code, sub, nonce)
	mp.PrepUser(sub, map[string]interface{}{
		"email":          "oidc@example.com",
		"email_verified": true,
		"name":           "OIDC User",
		"phone_number":   "13000000001",
	})

	p, err := newOIDCProvider(oidcProviderConfig{
		Client:                newTestClient(t, mp),
		Scopes:                []string{"openid", "profile", "email"},
		PostLogoutRedirectURI: "https://app.example.com/login",
		// mock 的 Discovery 文档不声明 end_session_endpoint,用 override 补上,
		// 否则 logout 契约会被 skip 掉 —— 那等于该契约在 OIDC 侧没被验证。
		EndSessionURLOverride: "https://idp.example.com/end-session",
	})
	if err != nil {
		t.Fatalf("newOIDCProvider: %v", err)
	}

	// 不可路由变体:OIDC 不能直接用黑洞地址构造(Discovery 会在构造阶段就失败,
	// 拿不到实例)。改为先让 Discovery 成功,再关掉 mock server —— 之后的
	// Exchange 就走传输层失败分支。httptest.Server.Close 是幂等的,
	// 与 t.Cleanup 里的那次不冲突。
	deadMP := NewMockProvider(t)
	deadProvider, err := newOIDCProvider(oidcProviderConfig{
		Client: newTestClient(t, deadMP),
		Scopes: []string{"openid"},
	})
	if err != nil {
		t.Fatalf("newOIDCProvider(dead): %v", err)
	}
	deadMP.Server.Close()

	return conformanceFixture{
		name:                   "oidc",
		provider:               p,
		happyCode:              code,
		wantSubject:            sub,
		unroutable:             deadProvider,
		secretsThatMustNotLeak: []string{"test-secret"},
	}
}

func oauth2ConformanceFixture(t *testing.T) conformanceFixture {
	t.Helper()
	m := newMockOAuth2Provider(t)
	p, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	unroutable, err := newOAuth2Provider(oauth2ProviderConfig{
		Issuer: "test-idp", BaseURL: "http://127.0.0.1:1",
		ClientID: "cid", ClientSecret: "unroutable-secret-value",
		RedirectURI: "https://app.example.com/cb",
	})
	if err != nil {
		t.Fatalf("newOAuth2Provider(unroutable): %v", err)
	}
	return conformanceFixture{
		name:                   "oauth2",
		provider:               p,
		happyCode:              "any-code",
		wantSubject:            m.SubjectForUserInfo,
		unroutable:             unroutable,
		secretsThatMustNotLeak: []string{"csecret", "unroutable-secret-value"},
	}
}

func conformanceFixtures(t *testing.T) []conformanceFixture {
	t.Helper()
	return []conformanceFixture{
		oidcConformanceFixture(t),
		oauth2ConformanceFixture(t),
	}
}

// 这套测试是 AuthProvider 的**契约**,而不是某个实现的单元测试。
// 两个实现跑同一张表,所以将来接第三个 IdP 时不必重读 handler:
// 把它加进 conformanceFixtures 即可知道是否满足上层假设。
func TestAuthProviderConformance(t *testing.T) {
	for _, fx := range conformanceFixtures(t) {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			t.Run("kind_and_issuer_are_non_empty", func(t *testing.T) {
				if fx.provider.Kind() == "" {
					t.Error("Kind() is empty; it labels metrics and drives startup config validation")
				}
				// Issuer 会写进 user_oidc_identity.issuer 并参与唯一键,空值会让
				// 不同 IdP 的身份混进同一命名空间。
				if fx.provider.Issuer() == "" {
					t.Error("Issuer() is empty; it is part of the identity unique key")
				}
			})

			t.Run("authcode_url_requires_state", func(t *testing.T) {
				// state 是所有 provider 的强制 CSRF 绑定,与协议是否声明它可选无关。
				if _, err := fx.provider.AuthCodeURL(AuthCodeParams{State: ""}); err == nil {
					t.Error("AuthCodeURL accepted an empty state")
				}
			})

			t.Run("authcode_url_carries_state_and_no_secret", func(t *testing.T) {
				raw, err := fx.provider.AuthCodeURL(AuthCodeParams{
					State: "st-conformance", Nonce: "n", CodeChallenge: "cc",
				})
				if err != nil {
					t.Fatalf("AuthCodeURL: %v", err)
				}
				if !strings.Contains(raw, "st-conformance") {
					t.Errorf("authorize URL does not carry the state: %s", raw)
				}
				// 该 URL 进浏览器地址栏,绝不能带 client_secret。
				for _, s := range fx.secretsThatMustNotLeak {
					if strings.Contains(raw, s) {
						t.Errorf("authorize URL leaks a secret (%s): %s", s, raw)
					}
				}
			})

			t.Run("identity_rejects_nil_token", func(t *testing.T) {
				if _, err := fx.provider.Identity(context.Background(), nil); err == nil {
					t.Error("Identity accepted a nil token")
				}
			})

			t.Run("happy_path_yields_trustworthy_claims", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				tok, err := fx.provider.Exchange(ctx, fx.happyCode, "")
				if err != nil {
					t.Fatalf("Exchange: %v", err)
				}
				// capabilities 必须与实际返回自洽:声明没有 id_token 就不能带回来。
				if !fx.provider.Capabilities().IDToken && tok.RawIDToken != "" {
					t.Error("Capabilities.IDToken=false but Exchange returned a RawIDToken")
				}
				if fx.provider.Capabilities().IDToken && tok.RawIDToken == "" {
					t.Error("Capabilities.IDToken=true but Exchange returned no RawIDToken")
				}
				if tok.AccessToken == "" {
					t.Fatal("Exchange returned an empty access token")
				}

				claims, err := fx.provider.Identity(ctx, tok)
				if err != nil {
					t.Fatalf("Identity: %v", err)
				}
				// 契约的核心两条:两者皆非空。subject 为空会因
				// UNIQUE(issuer,subject) 把所有空 sub 用户塌成同一行。
				if claims.Subject == "" {
					t.Error("Identity returned an empty Subject")
				}
				if claims.Issuer == "" {
					t.Error("Identity returned an empty Issuer")
				}
				if claims.Subject != fx.wantSubject {
					t.Errorf("Subject = %q, want %q", claims.Subject, fx.wantSubject)
				}
			})

			t.Run("logout_url_matches_declared_capability", func(t *testing.T) {
				raw, ok := fx.provider.LogoutURL(context.Background(), LogoutHint{
					UID: "u1", RawIDToken: "raw-id-token-value",
				})
				if !fx.provider.Capabilities().UpstreamLogout {
					if ok {
						t.Error("Capabilities.UpstreamLogout=false but LogoutURL returned a URL")
					}
					return
				}
				if !ok {
					t.Skip("upstream logout is supported but not configured in this fixture")
				}
				if raw == "" {
					t.Error("LogoutURL returned ok=true with an empty URL")
				}
			})

			t.Run("transport_failure_does_not_leak_credentials", func(t *testing.T) {
				if fx.unroutable == nil {
					t.Skip("fixture provides no unroutable variant")
				}
				_, err := fx.unroutable.Exchange(context.Background(), "c", "")
				if err == nil {
					t.Fatal("want a transport error")
				}
				for _, s := range fx.secretsThatMustNotLeak {
					if strings.Contains(err.Error(), s) {
						t.Errorf("transport error leaks %q: %v", s, err)
					}
				}
				if strings.Contains(err.Error(), "client_secret") {
					t.Errorf("transport error leaks the client_secret parameter name and value: %v", err)
				}
			})
		})
	}
}

// OIDC 实现在重构后必须仍然做这四件事 —— 它们是最容易被"塌缩成一次
// Identity() 调用"顺手丢掉的,而其中三条是安全检查。
func TestOIDCProvider_PreservesSecurityChecks(t *testing.T) {
	t.Run("verifies_id_token_signature", func(t *testing.T) {
		mp := NewMockProvider(t)
		const sub, nonce, code = "s1", "n1", "c1"
		mp.PrepCode(code, sub, nonce)
		mp.PrepUser(sub, map[string]interface{}{"email": "a@example.com"})
		p, err := newOIDCProvider(oidcProviderConfig{Client: newTestClient(t, mp), Scopes: []string{"openid"}})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		// 篡改过的 id_token 必须被拒。
		if _, err := p.Identity(context.Background(), &TokenSet{
			AccessToken: "at", RawIDToken: "not.a.valid.jwt",
		}); err == nil {
			t.Fatal("Identity accepted an unverifiable id_token")
		}
	})

	t.Run("id_token_missing_is_rejected", func(t *testing.T) {
		mp := NewMockProvider(t)
		p, err := newOIDCProvider(oidcProviderConfig{Client: newTestClient(t, mp), Scopes: []string{"openid"}})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		if _, err := p.Identity(context.Background(), &TokenSet{AccessToken: "at"}); err == nil {
			t.Fatal("Identity accepted a token set with no id_token")
		}
	})

	t.Run("nonce_is_surfaced_for_the_handler_to_compare", func(t *testing.T) {
		mp := NewMockProvider(t)
		const sub, nonce, code = "s2", "n2", "c2"
		mp.PrepCode(code, sub, nonce)
		mp.PrepUser(sub, map[string]interface{}{"email": "a@example.com"})
		p, err := newOIDCProvider(oidcProviderConfig{Client: newTestClient(t, mp), Scopes: []string{"openid"}})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		tok, err := p.Exchange(context.Background(), code, "")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		claims, err := p.Identity(context.Background(), tok)
		if err != nil {
			t.Fatalf("Identity: %v", err)
		}
		// nonce 比对留在 handler(它持有 state 里的期望值),但 provider 必须
		// 把解出来的 nonce 交出去,否则那道校验就无声失效了。
		if claims.Nonce != nonce {
			t.Errorf("Nonce = %q, want %q; the handler can no longer compare it", claims.Nonce, nonce)
		}
	})

	t.Run("rejects_userinfo_sub_mismatch", func(t *testing.T) {
		mp := NewMockProvider(t)
		const sub, nonce, code = "s3", "n3", "c3"
		mp.PrepCode(code, sub, nonce)
		// 让 id_token 只有 sub(无 email/name/phone),以触发 /userinfo 补全。
		mp.PrepUser(sub, map[string]interface{}{})
		// mock 的 handleUserInfo 先写 out["sub"],再把 uiOnly 的 claims 合并进去,
		// 所以在 uiOnly 里放一个 sub 就能覆盖它 —— 这是构造"账号串台"的唯一途径,
		// 无需改动既有 mock。
		//
		// 顺带说明:在此之前没有测试覆盖过这道检查(现有 mock 造不出该场景),
		// 也就是说 callback 里这道安全检查一直是未被验证的。
		mp.PrepUserInfoOnly(sub, map[string]interface{}{
			"sub":   "someone-else",
			"email": "b@example.com",
		})
		p, err := newOIDCProvider(oidcProviderConfig{Client: newTestClient(t, mp), Scopes: []string{"openid"}})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		tok, err := p.Exchange(context.Background(), code, "")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if _, err := p.Identity(context.Background(), tok); err == nil {
			t.Fatal("Identity accepted a /userinfo sub that differs from the id_token sub")
		}
	})

	t.Run("userinfo_failure_does_not_block_login", func(t *testing.T) {
		mp := NewMockProvider(t)
		const sub, nonce, code = "s4", "n4", "c4"
		mp.PrepCode(code, sub, nonce)
		// id_token 缺 email/name → 会触发 /userinfo 补全;让它 500。
		mp.ForceUserInfoStatus(500)
		p, err := newOIDCProvider(oidcProviderConfig{Client: newTestClient(t, mp), Scopes: []string{"openid"}})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		tok, err := p.Exchange(context.Background(), code, "")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		claims, err := p.Identity(context.Background(), tok)
		if err != nil {
			t.Fatalf("a /userinfo failure must not block login (it only costs autolink): %v", err)
		}
		if claims.Subject != sub {
			t.Errorf("Subject = %q, want %q", claims.Subject, sub)
		}
	})

	t.Run("logout_url_carries_id_token_hint", func(t *testing.T) {
		mp := NewMockProvider(t)
		p, err := newOIDCProvider(oidcProviderConfig{
			Client:                newTestClient(t, mp),
			Scopes:                []string{"openid"},
			PostLogoutRedirectURI: "https://app.example.com/login",
			EndSessionURLOverride: "https://idp.example.com/end-session",
		})
		if err != nil {
			t.Fatalf("newOIDCProvider: %v", err)
		}
		raw, ok := p.LogoutURL(context.Background(), LogoutHint{UID: "u1", RawIDToken: "the-raw-id-token"})
		if !ok {
			t.Fatal("want a logout URL")
		}
		if !strings.Contains(raw, "id_token_hint=the-raw-id-token") {
			t.Errorf("RP-Initiated Logout URL lost id_token_hint: %s", raw)
		}
		if !strings.Contains(raw, "post_logout_redirect_uri=") {
			t.Errorf("RP-Initiated Logout URL lost post_logout_redirect_uri: %s", raw)
		}
	})
}
