package oidc

// absent_secret_forwarding_test.go — 密钥**缺失**时也不得把业务 JWT 转发上游。
//
// 上一轮只关了"密钥无效",而否掉它的正是同一条论证:客户端业务后端持有并使用
// **它自己的**密钥,跟我方配没配无关。运维给客户开了 kind=oauth2、挂了端点、
// 漏配 bearer 密钥 —— 桌面客户端每个请求都在泄漏,持续的,不是瞬时边缘。
//
// 为什么可以在这里判定形态而不算"按形态猜":这个上游的 access_token 是
// **不透明 UUID**(见 brief),JWT 形态的值在这条路上不可能是合法上游凭据,
// 转发它只能泄漏、不可能成功。而这是**厂商事实不是协议事实**,所以判据挂在
// ProviderCapabilities 上,不无条件生效 —— 标准 OIDC 下客户端出示的凭据本身
// 就是 JWT(id_token),无条件拒绝会把那条路整条掐断。

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExchange_AbsentSecretStillRefusesJWTShapedCredential(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	require.NoError(t, err)
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.ownCred = newDetector(&fakeTokenReader{})
	// 密钥**缺失**:两者皆 nil。这是被文档称为合法的部署形态。
	o.bearerJWT, o.bearerJWTErr = nil, nil

	// 客户端用它自己的密钥签的 —— 我方没有这个密钥,验不了,但它确实是我方格式。
	tok := signBearerTesting(t, []byte("client-side-secret-32-bytes-long!"),
		2200011, "desk.user", time.Now().Add(15*24*time.Hour))

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	w := postExchange(o, `{"access_token":"`+tok+`"}`)

	if w.Code == http.StatusOK {
		t.Error("an unverifiable credential must not authenticate")
	}
	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last != nil {
		t.Errorf("a JWT-shaped credential was forwarded to the upstream IdP (userinfo "+
			"query=%q) while our bearer path was unconfigured. This provider's access_token "+
			"is an opaque UUID, so a JWT can never be a valid upstream credential here — "+
			"forwarding it cannot succeed, it can only put the payload PII and a signature "+
			"valid under the client's secret into the vendor's access log",
			last.Query.Encode())
	}
}

// 反面一:不透明上游凭据必须照常转发 —— 这是这条路的主用途。
func TestExchange_AbsentSecretStillForwardsOpaqueCredential(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	require.NoError(t, err)
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.ownCred = newDetector(&fakeTokenReader{})
	o.bearerJWT, o.bearerJWTErr = nil, nil

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	postExchange(o, `{"access_token":"`+m.KnownAccessToken+`"}`)

	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last == nil {
		t.Error("an opaque upstream token must still reach /userinfo with no secret " +
			"configured; that deployment shape is legal and must keep working")
	}
}

// 反面二(关键):标准 OIDC 下客户端出示的凭据**本身就是 JWT**,
// 所以这道拒绝必须由能力位关掉,否则 /exchange 在 kind=oidc 下整条断掉。
func TestOIDCKind_DoesNotDeclareOpaqueClientCredential(t *testing.T) {
	mp := NewMockProvider(t)
	p, err := newOIDCProvider(oidcProviderConfig{
		Client: newTestClient(t, mp), Scopes: []string{"openid"},
	})
	require.NoError(t, err)
	if p.Capabilities().OpaqueClientCredential {
		t.Fatal("the standard kind must NOT declare an opaque client credential: the " +
			"credential a client presents there IS a JWT (the id_token), so refusing " +
			"JWT-shaped input would break every /exchange call on that kind")
	}

	m2 := newMockOAuth2Provider(t)
	op, err := newOAuth2Provider(m2.providerConfig())
	require.NoError(t, err)
	if !op.Capabilities().OpaqueClientCredential {
		t.Error("the plain-OAuth2 kind must declare it: that vendor's access_token is an " +
			"opaque UUID, which is what makes a JWT-shaped value provably not a credential " +
			"for it")
	}
}
