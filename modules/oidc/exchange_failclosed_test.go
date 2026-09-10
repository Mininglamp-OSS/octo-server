package oidc

// exchange_failclosed_test.go — 验签器**构造失败**时 /exchange 必须 fail-closed。
//
// "没配密钥" 和 "配了但无效" 不是一回事:
//
//   - 没配:C3 凭据不可能存在,回落上游是对的,这是合法部署形态;
//   - 配错(比如 31 字节):客户端业务后端拿的是**同一个值**在签。HMAC 不在乎密钥
//     长度 —— 32 字节是我方准入策略,不是 HMAC 约束。于是那张 token 带着在我方
//     配置密钥下**合法的签名**,连 userId 载荷一起进第三方 /userinfo 的 URL query。
//
// modules/integration 在同一状态下是对的(bearerJWTErr → 拒绝**每一个**凭据)。
// modules/oidc 的 New() 只打日志就把错误丢了,而 /exchange 的守卫写成
// `if o.bearerJWT != nil`,于是整段被静默跳过 —— 同一个误配,两个消费者相反的
// 失败方向。矩阵的"构造失败列"里 P3/P4 都有行,P2 那格是空的。

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// /exchange 在构造失败态下必须拒绝**每一个**凭据,且绝不外呼。
func TestExchange_FailsClosedWhenVerifierConstructionFailed(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	require.NoError(t, err)
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.ownCred = newDetector(&fakeTokenReader{})

	// 构造失败态:验签器为 nil,但错误被保留下来。
	o.bearerJWT = nil
	o.bearerJWTErr = errors.New("bearer-jwt: shared secret is too short: 31 bytes, min 32")

	// 用那个无效密钥签一张 token —— 客户端手上就是这个值。
	invalidSecret := []byte("this-secret-is-only-31-bytes!!!")
	require.Len(t, invalidSecret, 31)
	tok := signBearerTesting(t, invalidSecret, 2200009, "desk.user", time.Now().Add(15*24*time.Hour))

	for name, cred := range map[string]string{
		"business JWT signed with the invalid secret": tok,
		// 上游凭据也要一并拒 —— 归属问题在这个状态下**没有答案**,
		// 放过任何一类都等于把守卫的失败方向设成泄漏。
		"upstream opaque credential": m.KnownAccessToken,
	} {
		t.Run(name, func(t *testing.T) {
			m.mu.Lock()
			m.LastUserInfoRequest = nil
			m.mu.Unlock()

			w := postExchange(o, `{"access_token":"`+cred+`"}`)

			if w.Code == http.StatusOK {
				t.Error("with an unconstructible verifier the provenance question has no " +
					"answer; nothing may be accepted")
			}
			m.mu.Lock()
			last := m.LastUserInfoRequest
			m.mu.Unlock()
			if last != nil {
				t.Errorf("credential reached the upstream IdP (userinfo query=%q) while the "+
					"bearer verifier had failed to construct. modules/integration refuses every "+
					"credential in this exact state (bearerJWTErr); /exchange must inherit that "+
					"standard, because the client signs with the same invalid value and the "+
					"resulting HMAC is valid under it", last.Query.Encode())
			}
		})
	}
}

// 反面:**没配**密钥仍是合法部署形态,上游凭据照常可用。
func TestExchange_AbsentSecretKeepsUpstreamPathWorking(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	require.NoError(t, err)
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.ownCred = newDetector(&fakeTokenReader{})
	o.bearerJWT, o.bearerJWTErr = nil, nil // 没配

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	postExchange(o, `{"access_token":"`+m.KnownAccessToken+`"}`)

	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last == nil {
		t.Error("an absent bearer secret must stay a legal deployment shape; the upstream " +
			"credential path has to keep working")
	}
}

// New() 必须把构造错误**留在结构体上**,而不是打完日志就丢。
//
// 只测 handler 不够:handler 读的字段如果生产从来没被填上,那道守卫就是死的 ——
// 这个模块已经栽过一次(id_token 接线被删而套件全绿)。
func TestNew_RetainsBearerVerifierConstructionError_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	setNewWiringEnv(t, mp)
	// 31 字节:低于 32 字节下限,构造必失败。
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", "this-secret-is-only-31-bytes!!!")

	o := New(ctx)
	require.NotNil(t, o)
	if o.bearerJWT != nil {
		t.Fatal("an invalid secret must not produce a usable verifier")
	}
	if o.bearerJWTErr == nil {
		t.Fatal("the construction error was discarded. A nil verifier then means both " +
			"'not configured' (legal) and 'misconfigured' (must fail closed), and the " +
			"handlers cannot tell them apart — which is how /exchange came to skip its " +
			"provenance guard silently")
	}
}
