package oidc

// oidc_userinfo_no_access_token_test.go — 没有 access_token 时不得请求 /userinfo。
//
// 这是本改动引入的**存量回归**。改动前 modules/integration 的 oidcAuth 走
// it.oidcClient.VerifyIDToken —— 纯本地,零外呼。现在它走
// oidcProvider.IdentityFromClientCredential,而那条路构造的 TokenSet **只有**
// RawIDToken,AccessToken 是零值:
//
//	return p.Identity(ctx, &TokenSet{RawIDToken: raw})
//
// Identity 随后按 needUserInfo 决定要不要补全,而 needUserInfo 的条件是
// email/phone/name 任一缺失 —— 几乎没有真实 id_token 三者齐备(那段注释自己就这么写)。
// 于是每个请求都会带着空凭据打一次 /userinfo:go-oidc 的 StaticTokenSource 不做
// 有效性检查,SetAuthHeader 产出 "Authorization: Bearer "(空凭据),GET 照发。
//
// 后果:一条原本可离线完成的认证路径,现在每个请求都阻塞在一次**架构上必然 401**
// 的 IdP 往返上,慢上游时最长等到 HTTPTimeout;而失败被静默吞掉,唯一症状是每请求
// 一行 warn。
//
// 套件为什么全绿:mock 用 HasPrefix(auth, "Bearer ") 判定,空凭据也匹配,然后 401 ——
// 正好落进"吞掉并继续"那个分支。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 客户端出示 id_token 这条路上没有 access_token,必须直接跳过补全。
func TestOIDCProvider_IdentityFromClientCredential_MakesNoUserInfoCall(t *testing.T) {
	mp := NewMockProvider(t)
	const sub, nonce, code = "100000000000000001", "n-1", "c-1"
	mp.PrepCode(code, sub, nonce)
	// 只放 sub —— 真实 id_token 的常见形态,于是 needUserInfo 为真。
	mp.PrepUser(sub, map[string]interface{}{})

	p, err := newOIDCProvider(oidcProviderConfig{
		Client: newTestClient(t, mp), Scopes: []string{"openid"},
	})
	require.NoError(t, err)

	raw, serr := mp.signIDToken(sub, nonce)
	require.NoError(t, serr)
	mp.ResetUserInfoCalls()

	claims, ierr := p.IdentityFromClientCredential(context.Background(), raw)
	require.NoError(t, ierr)
	require.Equal(t, sub, claims.Subject)

	if n := mp.UserInfoCalls(); n != 0 {
		t.Errorf("/userinfo was requested %d time(s) with no access_token. This path only "+
			"holds an id_token, so the call is architecturally guaranteed to fail — it costs "+
			"an IdP round trip per request on a path that was offline-capable before this "+
			"change, and the failure is swallowed so nothing surfaces but a warn line", n)
	}
}

// 反面:callback 那条路有真 access_token,补全必须照常发生 ——
// 否则这个修复就把"IdP 只在 /userinfo 暴露 email/phone"那类部署的自动绑号弄没了。
func TestOIDCProvider_Identity_StillCompletesFromUserInfoWithAnAccessToken(t *testing.T) {
	mp := NewMockProvider(t)
	const sub, nonce, code = "100000000000000002", "n-2", "c-2"
	mp.PrepCode(code, sub, nonce)
	mp.PrepUser(sub, map[string]interface{}{})
	mp.PrepUserInfoOnly(sub, map[string]interface{}{
		"email":          "only-in-userinfo@example.com",
		"email_verified": true,
	})

	p, err := newOIDCProvider(oidcProviderConfig{
		Client: newTestClient(t, mp), Scopes: []string{"openid", "email"},
	})
	require.NoError(t, err)

	tok, err := p.Exchange(context.Background(), code, "")
	require.NoError(t, err)
	require.NotEmpty(t, tok.AccessToken, "the fixture must supply an access token")

	mp.ResetUserInfoCalls()
	claims, ierr := p.Identity(context.Background(), tok)
	require.NoError(t, ierr)

	if mp.UserInfoCalls() == 0 {
		t.Fatal("/userinfo was not requested even though a real access token was available; " +
			"deployments whose IdP exposes email only at /userinfo would lose autolink")
	}
	require.Equal(t, "only-in-userinfo@example.com", claims.Email)
}
