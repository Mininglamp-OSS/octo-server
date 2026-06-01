package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
)

// MockProvider 自签 RSA 的 httptest OIDC server。
//
// 端点按测试需要增量挂载。字段全部线程安全,测试可在多 goroutine 中 PrepXxx。
//
// 设计原则:mock 只覆盖被测代码路径会触达的最小协议子集,不追求 RFC 完备。
type MockProvider struct {
	Server   *httptest.Server
	Issuer   string
	ClientID string

	// expFor 测试可调:签 ID Token 时 exp = now + expFor。零值走默认 10 分钟。
	// 给 ClockSkew leeway 测试构造刚过期 token 用。
	expFor time.Duration

	mu      sync.Mutex
	privKey *rsa.PrivateKey
	keyID   string
	users   map[string]map[string]interface{} // sub -> claims (放 ID Token + userinfo)
	uiOnly  map[string]map[string]interface{} // sub -> claims (仅 userinfo,模拟 IdP 不在 ID Token 暴露 email)
	codes   map[string]mockGrant              // code -> grant
	rts     map[string]string                 // refresh_token -> sub

	// userinfoForceStatus 测试可调:>=400 时 /userinfo 直接返该状态码,模拟 IdP 抖动。
	userinfoForceStatus int
}

type mockGrant struct {
	Sub   string
	Nonce string
}

// NewMockProvider 启动 httptest server,t.Cleanup 自动 Close。
//
// 默认 ClientID="test-client" / ClientSecret="test-secret",与 newTestClient 对齐。
func NewMockProvider(t *testing.T) *MockProvider {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("mock_provider: gen rsa: %v", err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := &MockProvider{
		Server:   srv,
		Issuer:   srv.URL,
		ClientID: "test-client",
		privKey:  priv,
		keyID:    "mock-key-1",
		users:    make(map[string]map[string]interface{}),
		uiOnly:   make(map[string]map[string]interface{}),
		codes:    make(map[string]mockGrant),
		rts:      make(map[string]string),
	}
	mux.HandleFunc("/.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("/keys", m.handleJWKS)
	mux.HandleFunc("/oauth/token", m.handleToken)
	mux.HandleFunc("/userinfo", m.handleUserInfo)
	return m
}

func (m *MockProvider) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	forceStatus := m.userinfoForceStatus
	m.mu.Unlock()
	if forceStatus >= 400 {
		http.Error(w, "forced failure", forceStatus)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return
	}
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	// access_token 形态: at|<sub>|<nano>,| 不在 sub 字符集里,直接反向解出 sub
	parts := strings.Split(tokenStr, "|")
	if len(parts) != 3 || parts[0] != "at" {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	sub := parts[1]

	m.mu.Lock()
	claims, ok := m.users[sub]
	uiExtra := m.uiOnly[sub]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "unknown sub", http.StatusUnauthorized)
		return
	}
	out := map[string]interface{}{"sub": sub}
	for k, v := range claims {
		out[k] = v
	}
	for k, v := range uiExtra {
		out[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// PrepUser 预置 sub → claims 映射。重复 sub 后写覆盖前写。
func (m *MockProvider) PrepUser(sub string, claims map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]interface{}, len(claims))
	for k, v := range claims {
		cp[k] = v
	}
	m.users[sub] = cp
}

// ForceUserInfoStatus 让 /userinfo 直接返指定 HTTP 状态(>=400),用于测试 IdP 抖动。
// 设为 0 恢复正常行为。
func (m *MockProvider) ForceUserInfoStatus(code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userinfoForceStatus = code
}

// PrepUserInfoOnly 预置仅在 /userinfo 暴露的 claims(不入 ID Token)。
// 模拟 Aegis 等 IdP 把 email/phone 放 /userinfo 而非 ID Token 的行为。
func (m *MockProvider) PrepUserInfoOnly(sub string, claims map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]interface{}, len(claims))
	for k, v := range claims {
		cp[k] = v
	}
	m.uiOnly[sub] = cp
}

// PrepCode 预置 authorization_code → sub + nonce(可空)。
//
// nonce 模拟 IdP 在 /authorize 阶段记录的 nonce,后续签 ID Token 时回填。
func (m *MockProvider) PrepCode(code, sub, nonce string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code] = mockGrant{Sub: sub, Nonce: nonce}
}

func (m *MockProvider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]interface{}{
		"issuer":                                m.Issuer,
		"authorization_endpoint":                m.Issuer + "/authorize",
		"token_endpoint":                        m.Issuer + "/oauth/token",
		"userinfo_endpoint":                     m.Issuer + "/userinfo",
		"end_session_endpoint":                  m.Issuer + "/end_session",
		"jwks_uri":                              m.Issuer + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (m *MockProvider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	jwk := jose.JSONWebKey{
		Key:       &m.privKey.PublicKey,
		KeyID:     m.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
}

func (m *MockProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	grantType := r.PostForm.Get("grant_type")

	m.mu.Lock()
	defer m.mu.Unlock()

	var sub, nonce string
	switch grantType {
	case "authorization_code":
		code := r.PostForm.Get("code")
		grant, ok := m.codes[code]
		if !ok {
			writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "unknown code")
			return
		}
		delete(m.codes, code) // 一次性
		sub = grant.Sub
		nonce = grant.Nonce
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		s, ok := m.rts[rt]
		if !ok {
			writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "unknown refresh_token")
			return
		}
		delete(m.rts, rt)
		sub = s
	default:
		writeOAuthErr(w, http.StatusBadRequest, "unsupported_grant_type", grantType)
		return
	}

	idToken, err := m.signIDToken(sub, nonce)
	if err != nil {
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	newRT := fmt.Sprintf("rt-%s-%d", sub, time.Now().UnixNano())
	m.rts[newRT] = sub

	resp := map[string]interface{}{
		"access_token":  fmt.Sprintf("at|%s|%d", sub, time.Now().UnixNano()),
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": newRT,
		"id_token":      idToken,
		"scope":         "openid profile email",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeOAuthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

func (m *MockProvider) signIDToken(sub, nonce string) (string, error) {
	expDelta := 10 * time.Minute
	if m.expFor != 0 {
		expDelta = m.expFor
	}
	claims := map[string]interface{}{
		"iss": m.Issuer,
		"sub": sub,
		"aud": m.ClientID,
		"exp": time.Now().Add(expDelta).Unix(),
		"iat": time.Now().Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	for k, v := range m.users[sub] {
		claims[k] = v
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.privKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", fmt.Errorf("mock: new signer: %w", err)
	}
	tok, err := jwt.Signed(signer).Claims(claims).CompactSerialize()
	if err != nil {
		return "", fmt.Errorf("mock: sign id_token: %w", err)
	}
	if strings.Count(tok, ".") != 2 {
		return "", fmt.Errorf("mock: invalid jwt shape")
	}
	return tok, nil
}
