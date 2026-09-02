package oidc

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)

// 空 subject 必须拒登,而且**一行 identity 都不能落库**。
//
// 为什么要打真库来断言:user_oidc_identity.subject 是 NOT NULL DEFAULT ” 且带
// UNIQUE(issuer, subject)。空串是一个合法的列值,所以数据库不会替我们兜底 ——
// 一旦有一条空 subject 的行落进去,第二个空 subject 的用户会撞上唯一键,而
// ResolveOrLink 在此之前就会先把他"认成"第一条行的那个 uid。后果不是脏数据,
// 是账号接管:两个陌生人登进同一个账号。
//
// provider 层已经有拒绝的断言(oauth2_provider_http_test.go),但那只能证明
// "解析函数返回了 error",证明不了"handler 在错误路径上没有先写库再返错"。
// 这个用例守的是后者,所以必须看真实的表。
func TestOAuth2Callback_EmptySubject_WritesNoIdentityRow_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	mp := newMockOAuth2Provider(t)
	// 上游返回一个信封完整、success=true,但 sub 为空串的 userinfo。
	// 这是最危险的形态:HTTP 200 + 业务成功,只有 subject 是空的。
	mp.UserInfoBody = `{
	  "success": true,
	  "code": "200",
	  "requestId": "req-empty-sub",
	  "data": {"sub": "", "nickname": "no-subject", "email": "ghost@example.com"}
	}`

	pcfg := mp.providerConfig()
	prov, err := newOAuth2Provider(pcfg)
	require.NoError(t, err)

	realDB := NewDB(ctx)
	// 用生产路径同一个适配器,别在测试里另造一条写库路径 —— 否则测的就不是
	// 生产会走的那段代码。
	realStore := identityStoreAdapter{db: realDB}
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-should-not-exist", LoginRespJSON: `{"token":"t"}`},
	}
	cfg := &Config{
		Enabled: true,
		Provider: ProviderConfig{
			ID: "test", Name: "Test IdP", Kind: KindOAuth2,
			Issuer: pcfg.Issuer, BaseURL: pcfg.BaseURL,
			ClientID: pcfg.ClientID, ClientSecret: pcfg.ClientSecret,
			RedirectURI: pcfg.RedirectURI, AppID: pcfg.AppID, Scopes: pcfg.Scopes,
			RequireEmailVerified: true, AutoLinkByEmail: true, AllowNewUser: true,
			ReturnToHosts: []string{"app.example.com"},
		},
	}
	o := &OIDC{
		Log:        log.NewTLog("OIDC-empty-sub"),
		ctx:        ctx,
		cfg:        cfg,
		provider:   prov,
		service:    newService(cfg.Provider, realStore, users),
		store:      realStore,
		db:         realDB,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
	}
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-empty&return_to=/home")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET",
		"/v1/auth/oidc/test/callback?state="+state+"&code=code-empty-sub", nil))

	// 登录不能成立。
	assert.NotEqual(t, http.StatusOK, w.Code, "an empty subject must not produce a success page")
	assert.Empty(t, users.loginCalls, "IssueSession must not run when the subject is empty")

	// 核心断言:表里没有任何 identity 行 —— 包括 subject='' 的那种。
	var total int
	require.NoError(t, ctx.DB().
		Select("COUNT(*)").From("`user_oidc_identity`").
		LoadOne(&total))
	assert.Zero(t, total, "no identity row may be written for an empty subject")

	// 再针对性查一次空 subject:上面的计数为 0 时这条是冗余的,但它把失败信息
	// 直接指到"空 subject 行"这个具体形态上,而不是让人从总数 1 去猜是哪一行。
	got, err := realDB.QueryIdentityByIssuerSubject(pcfg.Issuer, "")
	require.NoError(t, err)
	assert.Nil(t, got, "an (issuer, '') row must not exist — it would collapse every "+
		"subject-less login onto one account")
}
