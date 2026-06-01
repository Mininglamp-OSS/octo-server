package oidc

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, mp *MockProvider) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, ClientConfig{
		Issuer:       mp.Issuer,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURI:  "https://app.example.com/callback",
		Scopes:       []string{"openid", "profile", "email"},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// Cycle 1 RED: 通过 Discovery 构造 Client,issuer 应与 mock 匹配。
func TestClient_Discover(t *testing.T) {
	mp := NewMockProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := NewClient(ctx, ClientConfig{
		Issuer:       mp.Issuer,
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURI:  "https://app.example.com/callback",
		Scopes:       []string{"openid", "profile", "email"},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil client")
	}
	if got := c.Issuer(); got != mp.Issuer {
		t.Fatalf("Issuer() = %q, want %q", got, mp.Issuer)
	}
}

// EndSessionEndpoint 应从 Discovery 文档的 end_session_endpoint 解出,
// 供 logout 拼 RP-Initiated Logout URL。
func TestClient_EndSessionEndpoint_FromDiscovery(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)
	if got, want := c.EndSessionEndpoint(), mp.Issuer+"/end_session"; got != want {
		t.Errorf("EndSessionEndpoint() = %q, want %q", got, want)
	}
}

// Cycle 6 RED: 用 refresh_token 换新 token。
func TestClient_Refresh(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)
	mp.PrepUser("sub-006", map[string]interface{}{"email": "dave@example.com"})
	mp.PrepCode("code-6", "sub-006", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := c.Exchange(ctx, "code-6", "v")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.RefreshToken == "" {
		t.Fatal("expected refresh_token from initial exchange")
	}
	oldRT := tok.RefreshToken

	newTok, err := c.Refresh(ctx, oldRT)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newTok.AccessToken == "" {
		t.Error("missing new access_token")
	}
	if newTok.RefreshToken == "" {
		t.Error("missing new refresh_token (rotation expected)")
	}
	if newTok.RefreshToken == oldRT {
		t.Error("refresh_token did not rotate")
	}

	// 旧 RT 应已失效
	if _, err := c.Refresh(ctx, oldRT); err == nil {
		t.Error("expected old refresh_token to be invalid after rotation")
	}
}

// Cycle 5 RED: 用 access_token 拉 /userinfo。
func TestClient_UserInfo(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)
	mp.PrepUser("sub-005", map[string]interface{}{
		"email":          "carol@example.com",
		"email_verified": true,
		"phone_number":   "+8613800000000",
		"name":           "Carol",
	})
	mp.PrepCode("code-5", "sub-005", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := c.Exchange(ctx, "code-5", "v")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	info, err := c.UserInfo(ctx, tok)
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if info.Subject != "sub-005" {
		t.Errorf("Subject = %q", info.Subject)
	}
	if info.Email != "carol@example.com" {
		t.Errorf("Email = %q", info.Email)
	}
	if info.PhoneNumber != "+8613800000000" {
		t.Errorf("PhoneNumber = %q", info.PhoneNumber)
	}
}

// Cycle 4 RED: 通过 JWKS 验签 ID Token,解出 sub / email / nonce。
func TestClient_VerifyIDToken(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)

	mp.PrepUser("sub-002", map[string]interface{}{
		"email":          "bob@example.com",
		"email_verified": true,
	})
	mp.PrepCode("code-2", "sub-002", "nonce-XYZ")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := c.Exchange(ctx, "code-2", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		t.Fatal("missing id_token")
	}

	claims, err := c.VerifyIDToken(ctx, rawID)
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Subject != "sub-002" {
		t.Errorf("Subject = %q, want sub-002", claims.Subject)
	}
	if claims.Email != "bob@example.com" {
		t.Errorf("Email = %q", claims.Email)
	}
	if !claims.EmailVerified {
		t.Error("EmailVerified should be true")
	}
	if claims.Nonce != "nonce-XYZ" {
		t.Errorf("Nonce = %q, want nonce-XYZ", claims.Nonce)
	}
}

// ClockSkew 应该让刚过期(在 skew 窗口内)的 token 仍能验签通过。
func TestClient_VerifyIDToken_ClockSkewLeeway(t *testing.T) {
	mp := NewMockProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 用 30s skew 构造 client
	c, err := NewClient(ctx, ClientConfig{
		Issuer:       mp.Issuer,
		ClientID:     mp.ClientID,
		ClientSecret: "test-secret",
		RedirectURI:  "https://app.example.com/callback",
		Scopes:       []string{"openid"},
		ClockSkew:    30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// 直接用 mock 签一个 exp 在 5s 前的过期 token
	mp.PrepUser("sub-skew", map[string]interface{}{"email": "skew@example.com"})
	mp.expFor = -5 * time.Second
	defer func() { mp.expFor = 0 }()
	mp.PrepCode("code-skew", "sub-skew", "")
	tok, err := c.Exchange(ctx, "code-skew", "v")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	rawID, _ := tok.Extra("id_token").(string)

	// 30s skew 应该容忍 5s 过期
	if _, err := c.VerifyIDToken(ctx, rawID); err != nil {
		t.Fatalf("VerifyIDToken should accept token within skew window: %v", err)
	}
}

// 篡改的 ID Token(签名错位)应该验签失败。
func TestClient_VerifyIDToken_BadSignature(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)
	mp.PrepUser("sub-003", map[string]interface{}{"email": "x@y.com"})
	mp.PrepCode("code-3", "sub-003", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := c.Exchange(ctx, "code-3", "v")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	// 篡改 payload 段(中间段),签名一定不再匹配
	parts := strings.Split(rawID, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected jwt shape: %d parts", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "AAAA." + parts[2]
	if _, err := c.VerifyIDToken(ctx, tampered); err == nil {
		t.Fatal("expected verify error on tampered token")
	}
}

// Cycle 3 RED: 用 mock 预置 code → user 映射,Exchange 应换出 OAuth2 token,
// 含 access_token / id_token / refresh_token。
func TestClient_Exchange(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)

	mp.PrepUser("sub-001", map[string]interface{}{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})
	mp.PrepCode("auth-code-1", "sub-001", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tok, err := c.Exchange(ctx, "auth-code-1", "ignored-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken == "" {
		t.Error("missing access_token")
	}
	if tok.RefreshToken == "" {
		t.Error("missing refresh_token")
	}
	if id, _ := tok.Extra("id_token").(string); id == "" {
		t.Error("missing id_token")
	}
}

// Cycle 2 RED: AuthCodeURL 应包含 client_id / redirect_uri / scope / state /
// nonce / response_type=code / code_challenge(S256) / code_challenge_method。
func TestClient_AuthCodeURL_PKCE(t *testing.T) {
	mp := NewMockProvider(t)
	c := newTestClient(t, mp)

	state := "stateXYZ"
	nonce := "nonceABC"
	verifier, challenge, err := NewPKCEPair()
	if err != nil {
		t.Fatalf("NewPKCEPair: %v", err)
	}

	got, err := c.AuthCodeURL(state, nonce, challenge)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()

	checks := map[string]string{
		"client_id":             "test-client",
		"redirect_uri":          "https://app.example.com/callback",
		"response_type":         "code",
		"state":                 state,
		"nonce":                 nonce,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %q = %q, want %q", k, got, want)
		}
	}
	scope := q.Get("scope")
	for _, s := range []string{"openid", "profile", "email"} {
		if !strings.Contains(scope, s) {
			t.Errorf("scope %q missing %q", scope, s)
		}
	}
	// 防止 verifier 误泄露到 URL
	if strings.Contains(got, verifier) {
		t.Error("verifier leaked into authorize URL")
	}
}
