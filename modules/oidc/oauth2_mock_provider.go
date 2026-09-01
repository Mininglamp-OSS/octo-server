package oidc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// mockOAuth2Provider 一个 plain-OAuth2 IdP 的测试替身。
//
// 与 mock_provider.go(标准 OIDC 用)相比刻意简单得多:不需要 Discovery 文档、
// 不需要 JWKS、不需要 RSA 签名,因为这个协议根本没有 id_token。
//
// 它同时充当**协议契约的记录仪**:token 端点断言参数来自 query
// (对方的官方参考实现即如此),userinfo 断言 access_token 在 query 而非
// Authorization 头。若我方实现哪天改成 form body 或 Bearer 头,这里会立刻发现,
// 而不是等到联调。
type mockOAuth2Provider struct {
	Server *httptest.Server

	mu sync.Mutex
	// LastTokenRequest / LastUserInfoRequest 记录最近一次请求,供测试断言
	// 我方"怎么发"的,而不只是"发成功了"。
	LastTokenRequest    *recordedRequest
	LastUserInfoRequest *recordedRequest

	// 可注入的行为开关
	TokenStatus        int    // 非 0 时 token 端点返回该状态码
	TokenBody          string // 非空时原样返回该 token 响应体
	UserInfoStatus     int
	UserInfoBody       string
	KnownAccessToken   string // userinfo 只认这个 token
	SubjectForUserInfo string
}

type recordedRequest struct {
	Method      string
	Query       url.Values
	Form        url.Values
	ContentType string
	AuthHeader  string
	RawBody     string
}

func newMockOAuth2Provider(t *testing.T) *mockOAuth2Provider {
	t.Helper()
	m := &mockOAuth2Provider{
		KnownAccessToken:   "mock-access-token",
		SubjectForUserInfo: "100000000000000001",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+upstreamTokenPath, m.handleToken)
	mux.HandleFunc("/"+upstreamUserInfoPath, m.handleUserInfo)
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

func (m *mockOAuth2Provider) record(r *http.Request) *recordedRequest {
	body := new(strings.Builder)
	// ParseForm 会消费 body,先留一份原文供断言"body 是否为空"。
	if r.Body != nil {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body.Write(buf[:n])
	}
	rec := &recordedRequest{
		Method:      r.Method,
		Query:       r.URL.Query(),
		ContentType: r.Header.Get("Content-Type"),
		AuthHeader:  r.Header.Get("Authorization"),
		RawBody:     body.String(),
	}
	if rec.RawBody != "" {
		if parsed, err := url.ParseQuery(rec.RawBody); err == nil {
			rec.Form = parsed
		}
	}
	return rec
}

func (m *mockOAuth2Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	rec := m.record(r)
	m.mu.Lock()
	m.LastTokenRequest = rec
	m.mu.Unlock()

	if m.TokenStatus != 0 {
		// TokenBody 优先:非 2xx 时也要能注入任意错误体,否则无法覆盖
		// invalid_client / unauthorized / 非 JSON 错误页等真实形态。
		body := m.TokenBody
		if body == "" {
			body = `{"error":"invalid_grant"}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.TokenStatus)
		_, _ = w.Write([]byte(body))
		return
	}
	if m.TokenBody != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.TokenBody))
		return
	}
	// 对方文档给出的成功响应形态:access_token 是不透明 UUID,**无 id_token**。
	resp := map[string]interface{}{
		"access_token":  m.KnownAccessToken,
		"token_type":    "bearer",
		"refresh_token": "mock-refresh-token",
		"expires_in":    7199,
		"scope":         "read",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *mockOAuth2Provider) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	rec := m.record(r)
	m.mu.Lock()
	m.LastUserInfoRequest = rec
	m.mu.Unlock()

	if m.UserInfoStatus != 0 {
		w.WriteHeader(m.UserInfoStatus)
		_, _ = w.Write([]byte(`{"success":false,"code":"401"}`))
		return
	}
	if m.UserInfoBody != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.UserInfoBody))
		return
	}
	// 该 IdP 用 query 传 token,不用 Authorization 头。
	if got := rec.Query.Get("access_token"); got != m.KnownAccessToken {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"code":"401","message":"bad token"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{
	  "success": true, "code": "200", "message": null, "requestId": "REQ-MOCK-1",
	  "data": {
	    "sub": %q, "user_id": "123", "ou_id": "20000000000000000002",
	    "nickname": "Mock User", "phone_number": "13000000000",
	    "ou_name": "Example Org", "email": "mock@example.com", "username": "0000001"
	  }
	}`, m.SubjectForUserInfo)))
}

// providerConfig 返回指向本 mock 的 provider 配置。
func (m *mockOAuth2Provider) providerConfig() oauth2ProviderConfig {
	return oauth2ProviderConfig{
		Issuer:                "test-idp",
		BaseURL:               m.Server.URL,
		ClientID:              "cid",
		ClientSecret:          "csecret",
		RedirectURI:           "https://app.example.com/v1/auth/oidc/test/callback",
		Scopes:                []string{"read"},
		AppID:                 "app1",
		PostLogoutRedirectURI: "https://app.example.com/login",
	}
}
