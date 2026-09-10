package oidc

// 标准 OIDC kind 下,一张**签名合法**但 sub 超出列宽的 id_token 必须拒登,
// 而且一行 identity 都不能落库。
//
// 这条用例存在的理由:上限守卫最初只挂在 plain-OAuth2 的信封解析上。那条路的
// 论证是"响应体不可信",看起来充分,于是标准 OIDC 那条路就没人加 —— 但列宽是
// **存储**的性质,与协议无关。签名证明这张 token 是那个 IdP 签的,不证明它的 sub
// 塞得进 VARCHAR(255)。
//
// provider 层的单元测试证明不了这件事:oidcProvider.Identity 会照常返回 claims
// (它没理由拒),缺陷在后面 —— claims 一路走到 IssueSession 建号,再到 INSERT
// 才炸。所以必须打真库,断言的是"表里没有行",不是"某个函数返了 error"。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

func TestOIDCCallback_OverlongSubject_WritesNoIdentityRow_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := NewMockProvider(t)
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-must-not-exist", LoginRespJSON: `{"token":"t"}`},
	}
	// 用生产路径的适配器打真库 —— 在测试里另造一条写库路径,测的就不是生产代码。
	realDB := NewDB(ctx)
	realStore := identityStoreAdapter{db: realDB}

	o := newTestOIDC(t, mp, users, nil)
	o.Log = log.NewTLog("OIDC-overlong-sub")
	o.ctx = ctx
	o.db = realDB
	o.store = realStore
	o.service = newService(o.cfg.Provider, realStore, users)
	r := newTestRouter(o)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/authorize?authcode=front-ac-long&return_to=/home", nil))
	require.Equal(t, http.StatusFound, w.Code)
	authURL, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	state := authURL.Query().Get("state")

	// 256 字节的 sub,比列宽多一个字节。IdP 正常签名 —— 缺陷不在签名。
	overlong := strings.Repeat("s", subjectMaxLen+1)
	mp.PrepCode("idp-code-long", overlong, authURL.Query().Get("nonce"))

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code-long", nil))

	assert.NotEqual(t, "/home", w2.Header().Get("Location"),
		"an unstorable subject must not complete the login")
	assert.Empty(t, users.loginCalls,
		"IssueSession ran for a subject that cannot be stored; under strict sql_mode the "+
			"INSERT then fails and leaves an orphaned user with no identity row")

	var total int
	require.NoError(t, ctx.DB().
		Select("COUNT(*)").From("`user_oidc_identity`").LoadOne(&total))
	assert.Zero(t, total, "no identity row may be written for an unstorable subject; under a "+
		"non-strict sql_mode the value is silently truncated instead, and two subjects "+
		"sharing the first %d bytes then collapse onto one account", subjectMaxLen)
}
