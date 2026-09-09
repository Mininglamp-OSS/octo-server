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

// MockOAuth2Provider 是 mockOAuth2Provider 的导出门面,供**其他模块**的测试使用。
//
// 为什么需要:modules/integration 的两个端点要在 plain-OAuth2 kind 下被端到端
// 驱动,而这个 kind 的身份来源是上游 /userinfo —— 那必须有一个会返回厂商私有
// 信封的服务在跑。让 integration 自己起一个 httptest server 就等于把信封形状
// 抄第二份,而信封解析正是本模块的信任边界:抄错一个字段,那边的测试就在验证
// 一个我们并不接受的形态。
//
// 只暴露测试真正需要的四件事,内部那些故障注入开关不外泄 —— 它们是本包用来
// 测信任边界的,跨模块使用会把别人的测试绑在本包的实现细节上。
type MockOAuth2Provider struct {
	inner *mockOAuth2Provider
}

// NewMockOAuth2Provider 起一个返回厂商信封的 /userinfo 与 /token 的 mock IdP。
// 生命周期绑定 t,测试结束自动关闭。
func NewMockOAuth2Provider(t *testing.T) *MockOAuth2Provider {
	t.Helper()
	return &MockOAuth2Provider{inner: newMockOAuth2Provider(t)}
}

// BaseURL 站点根,填给 OCTO_OIDC_PROVIDER_BASE_URL。
func (m *MockOAuth2Provider) BaseURL() string { return m.inner.Server.URL }

// AccessToken mock 唯一认可的不透明 access_token。
//
// 刻意是不透明串而非 JWT:这个协议下 access_token 没有可本地验证的结构,
// 用 JWT 形态当 fixture 会让"按形态猜"的实现意外通过。
func (m *MockOAuth2Provider) AccessToken() string { return m.inner.KnownAccessToken }

// Subject mock 在 /userinfo 里返回的 sub。
func (m *MockOAuth2Provider) Subject() string { return m.inner.SubjectForUserInfo }

// SetSubject 改变 /userinfo 返回的 sub。
//
// 取值需满足 checkSubjectShape(纯数字时至少 10 位),否则会在信任边界被拒 ——
// 那是有意的守卫,不是 mock 的限制。
func (m *MockOAuth2Provider) SetSubject(sub string) { m.inner.SubjectForUserInfo = sub }

// LastUserInfoQuery 返回最近一次 /userinfo 请求的 query 原文;从未被请求时返回空串。
//
// 用途是断言**没有**发生外呼:一张本地验签已经认定"是我们自己的、但被拒绝"的
// token 绝不能被转发到第三方 IdP —— 那条路径把凭据放在 query string 里
// (该 IdP 的形态),于是它会落进对方的访问日志。
func (m *MockOAuth2Provider) LastUserInfoQuery() string {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	if m.inner.LastUserInfoRequest == nil {
		return ""
	}
	return m.inner.LastUserInfoRequest.Query.Encode()
}

// ResetRequestLog 清掉记录,便于在一次用例里分段断言。
func (m *MockOAuth2Provider) ResetRequestLog() {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	m.inner.LastUserInfoRequest = nil
	m.inner.LastTokenRequest = nil
}
