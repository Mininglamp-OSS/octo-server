package integration

// own_credential_test.go — 本服务**自己签发**的凭据绝不能被转发给上游 IdP。
//
// `kind=oauth2` 的 /userinfo 把凭据放在 URL query 上。而凭据归属判定原先只回答
// "这是不是我们用 HS256 签的 JWT" —— 会话 token 和 uk_ key 同样是我们签发的,
// 只是不走 HMAC 那条路,于是整个掉出了分类,被当成"别人的"转发出去。
//
// 两种撞法都不需要攻击者:
//   - 本 PR 自己加的全局 BearerTokenCompat 就是在推广"所有接口都用
//     Authorization: Bearer",客户端照做去调 /spaces 就把会话 token 送出去;
//   - userAPIKeyAuth 读的是**同一个头**,挂在**同一个 route group** 的
//     /binding、/groups 上,差一个路径。
//
// 会话 token 落进第三方访问日志是直接可重放的。
//
// 这条在改动前不存在:main 上 oidcAuth 走的是本地 VerifyIDToken(纯 JWKS,不外呼),
// 认不出来的凭据一个字节都不出去。kind=oidc 至今仍然如此。只有新增的
// kind=oauth2 这条路会外呼。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 会话 token 那一类在这里造不出来:签发要 writer lease + rollout state,不是这个
// 测试该复刻的东西。它由 modules/oidc 的 detector 单测覆盖 —— 那里还能测出
// "Redis 报错时守卫往哪边失败",端到端根本造不出那个条件。

func TestOwnCredential_NeverForwardedUpstream(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)
	// 上游那条路能走通(有 identity 行 + space),这样"没外呼"就不会是因为
	// 请求在更早的地方就挂了。
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")

	for name, cred := range map[string]string{
		"user api key": botfather.UserAPIKeyPrefix + "l1Ve" + util.GenerUUID()[:24],
		"bot token":    botfather.BotTokenPrefix + util.GenerUUID()[:24],
	} {
		t.Run(name, func(t *testing.T) {
			mp.ResetRequestLog()
			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
				"/v1/integrations/oidc/spaces", cred, nil))

			assert.NotEqual(t, http.StatusOK, w.Code,
				"a credential of ours is not an identity assertion for this endpoint")

			q := mp.LastUserInfoQuery()
			assert.Empty(t, q,
				"a credential this service issued reached the upstream IdP (userinfo "+
					"query=%q). That endpoint takes credentials in the URL, so it lands in a "+
					"third party's access log — and a session token there is directly "+
					"replayable against us", q)
			assert.NotContains(t, q, cred,
				"our own credential appeared verbatim in the upstream request URL")
		})
	}
}

// 反面:**不是**我们的不透明凭据必须照常回落上游,否则这道守卫就把上游凭据
// 路径掐断了 —— 那才是这两个端点的主用途。
func TestOwnCredential_UpstreamOpaqueTokenStillFallsThrough(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")

	mp.ResetRequestLog()
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, mp.LastUserInfoQuery(),
		"an opaque upstream token must still reach /userinfo; refusing it would break the "+
			"very credential path these endpoints exist for")
}
