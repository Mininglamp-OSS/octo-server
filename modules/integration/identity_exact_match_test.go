package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// (issuer, subject) 在这条认证路径上也必须逐字节比较。
//
// user_oidc_identity 的 COLLATE 是 utf8mb4_general_ci,所以 `WHERE subject='ABC'`
// 会命中一行 subject='abc' 的记录。登录路径已经在 identityStoreAdapter.Get 里做了
// 逐字节复核,但本模块直连了原始查询 —— 而这里是**认证中间件**,后果更重:它把
// 上游 subject 解析成本地 uid、写进 context、然后放行列 space 与签发 uk_ API key。
//
// 也就是说两个只差大小写的上游主体,其中一个**用自己完全合法的凭据**就能认成
// 另一个:列出对方的 space,拿到对方账号下的 API key。
//
// 这条路特别现实,是因为本 PR 自己的选择:checkSubjectShape 刻意放行含字母的
// subject(UUID/base64 形态),因为真实 subject 形态至今未经实测确认 ——
// identity_exact_match.go 的文件头就是为这个未知写的。守卫只装在两个消费者之一,
// 等于把那个文件想关的口子又开了一半。
func TestIntegration_CaseFoldedSubjectDoesNotAuthenticateAsAnotherUser(t *testing.T) {
	route, ctx, mp := setupOAuth2KindAPITest(t)

	// 库里存在的是小写 subject 的用户。
	const storedSubject = "abc123def456ghi789"
	victimUID := seedIntegrationUser(t, ctx, "https://idp-test.example.com", storedSubject)
	victimSpace := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, victimUID, victimSpace, "Victim", 2, "2026-01-01 10:00:00")

	// 上游认证通过的是**大写**变体 —— 是另一个主体,库里没有它的绑定行。
	mp.SetSubject("ABC123DEF456GHI789")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", mp.AccessToken(), nil))

	if w.Code == http.StatusOK {
		var resp struct {
			UID string `json:"uid"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.NotEqual(t, victimUID, resp.UID,
			"a subject differing only in case resolved onto another user's account; "+
				"the ci collation makes the raw query match, so this path needs the same "+
				"byte-exact recheck the login path has")
	}
	// 正确行为:视为未绑定(而不是认成别人)。
	assert.NotEqual(t, http.StatusOK, w.Code,
		"an unbound subject must not authenticate; expected the not-linked response")
}

// 反面:完全一致的 subject 照常认证成功 —— 复核不能把正常登录一起挡掉。
func TestIntegration_ExactSubjectStillAuthenticates(t *testing.T) {
	route, ctx, mp := setupOAuth2KindAPITest(t)

	const subject = "abc123def456ghi789"
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", subject)
	space := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, space, "Exact", 2, "2026-01-01 10:00:00")
	mp.SetSubject(subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		UID string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, uid, resp.UID)
}

// 折叠碰撞在这条认证中间件上必须表现为"未绑定",而不是 500。
//
// QueryIdentityExact 现在把碰撞报成一个可区分的错误(之前是 (nil,nil),那会让
// 登录路径先建号再撞唯一键)。但这里原来的 err != nil 分支一律记 Error + 回 500 ——
// 碰撞不是内部故障,报 500 会误呼运维,而且对客户端说了错的话。
//
// 另外要求日志能区分:yujiawei 早前指出折叠碰撞与"确实没绑定"在日志里长得一样,
// 运维查不出来。所以这一类走独立的 Warn。
func TestIntegration_CaseFoldedIdentityIsNotLinkedNotInternalError(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)

	// 库里存小写 subject;让上游返回大小写不同的同一个值。
	const stored = "abc123def"
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", stored)
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")
	mp.SetSubject("ABC123DEF")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", mp.AccessToken(), nil))

	assert.NotEqual(t, http.StatusOK, w.Code,
		"a folded subject must never resolve onto the stored account")
	assert.NotEqual(t, http.StatusInternalServerError, w.Code,
		"a case-folded collision is not an internal failure: answering 500 pages ops and "+
			"tells the client the wrong thing. It is 'this principal is not linked and "+
			"cannot safely be'")
}
