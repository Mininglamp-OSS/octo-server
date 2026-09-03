package integration

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/oidc"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integration 的两个端点必须跟着 provider kind 走,而不是无条件假设标准 OIDC。
//
// 起因:New() 无条件调 oidc.NewClient()(Discovery),oidcAuth() 无条件调
// VerifyIDToken。plain-OAuth2 的上游明确没有 Discovery、没有 JWKS、也不发
// id_token,所以切到那个 kind 之后:
//
//	New()      → Discovery 失败 → provider 为 nil
//	oidcAuth() → provider==nil → 500
//
// 也就是 /v1/integrations/oidc/spaces 与 /exchange 在新 kind 下整体不可用,
// 而它们是这次适配的两个关键对外面。
// -----------------------------------------------------------------------------

// setOAuth2KindEnv 铺一份 plain-OAuth2 kind 的最小可用配置。
//
// 刻意不铺任何 Discovery 可达的 issuer:这个 kind 下就不该有人去做 Discovery,
// 一旦实现偷偷回落到 OIDC 路径,这里会以构造失败的形式暴露出来。
func setOAuth2KindEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
	t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", "https://idp.example.com")
}

// kind=oauth2 时必须构造出可用的 provider —— 不做 Discovery,也不留 nil。
func TestNew_OAuth2KindBuildsProviderWithoutDiscovery(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	setOAuth2KindEnv(t)

	it := New(ctx)
	require.NotNil(t, it.provider,
		"plain-OAuth2 kind must yield a usable provider; a nil one makes both "+
			"integration endpoints answer 500")
	assert.Equal(t, "oauth2", string(it.provider.Kind()))
	// 这个 kind 没有 id_token,能力声明必须诚实 —— 上层靠它决定跳过哪些步骤。
	assert.False(t, it.provider.Capabilities().IDToken,
		"the plain-OAuth2 upstream issues no id_token")
}

// kind=oidc(含存量不配 KIND 的部署)行为不变:仍然走 Discovery。
//
// 这里用一个不可解析的 issuer 让 Discovery 失败,断言失败方式与改动前一致
// (provider 留 nil),以证明标准路线没有被顺带改掉。
func TestNew_OIDCKindStillRequiresDiscovery(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://app.example.com/cb")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "://bad")

	it := New(ctx)
	assert.Nil(t, it.provider,
		"the standard kind must keep failing closed when Discovery is impossible")
}

// 配置本身起不来时(RT key 非法),provider 必须留 nil —— 不能因为新增分派而
// 把一份不可启动的配置放行。
func TestNew_UnbootableConfigLeavesNoProvider(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	setOAuth2KindEnv(t)
	t.Setenv("DM_OIDC_RT_ENC_KEY", "not-base64")

	it := New(ctx)
	assert.Nil(t, it.provider)
}

// 关闭时同样不构造 provider。
func TestNew_DisabledLeavesNoProvider(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	setOAuth2KindEnv(t)
	t.Setenv("DM_OIDC_ENABLED", "false")

	it := New(ctx)
	assert.Nil(t, it.provider)
}

// setupOAuth2KindAPITest 起一个 plain-OAuth2 kind 的 integration 测试环境。
//
// 与 setupIntegrationAPITest 的差别只有身份来源:那边是会签 id_token 的 OIDC
// mock,这边是会返回厂商私有信封的 /userinfo mock。其余(client 白名单、
// 限流放宽、错误渲染器)一致。
func setupOAuth2KindAPITest(t *testing.T) (http.Handler, *config.Context, *oidc.MockOAuth2Provider) {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "fedcba9876543210fedcba9876543210")

	mp := oidc.NewMockOAuth2Provider(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	// 这个 kind 下 issuer 是我方配置的身份命名空间,不是 Discovery 起点。
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp-test.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://octo.example/callback")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
	// mock 是 http 的,所以要显式开逃生阀 —— 生产强制 https。
	t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", mp.BaseURL())
	t.Setenv("OCTO_OIDC_ALLOW_INSECURE_UPSTREAM", "1")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_RPS", "1000")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_BURST", "10000")

	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	seedIntegrationClient(t, ctx, defaultClientID, 1)
	return s.GetRoute(), ctx, mp
}

// 两个端点在 plain-OAuth2 kind 下端到端可用:客户端出示不透明 access_token,
// 我方拿它问 /userinfo 确立身份,再按 (issuer, subject) 找到已绑定的本地用户。
//
// 这是本次适配的验收面。改动前它们在这个 kind 下整体返回 500(provider 为 nil)。
func TestOAuth2Kind_SpacesAndExchangeWorkEndToEnd(t *testing.T) {
	route, ctx, mp := setupOAuth2KindAPITest(t)

	// issuer 必须用我方配置值,不是 mock 的地址 —— 这个 kind 下 issuer 由配置注入。
	const issuer = "https://idp-test.example.com"
	uid := seedIntegrationUser(t, ctx, issuer, mp.Subject())
	spaceA := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceA, "Research", 2, "2026-01-01 10:00:00")
	seedOwnBot(t, ctx, uid, spaceA, "bot_"+util.GenerUUID()[:8], "")

	// 凭据是不透明串,不是 JWT。
	token := mp.AccessToken()
	require.NotContains(t, token, ".",
		"the fixture must be an opaque token; a JWT-shaped one would let a "+
			"shape-sniffing implementation pass")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", token, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var spacesResp struct {
		UID    string `json:"uid"`
		Spaces []struct {
			SpaceID string `json:"space_id"`
		} `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spacesResp))
	assert.Equal(t, uid, spacesResp.UID)
	require.Len(t, spacesResp.Spaces, 1)
	assert.Equal(t, spaceA, spacesResp.Spaces[0].SpaceID)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", token, map[string]interface{}{
		"space_id": spaceA,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		UID     string `json:"uid"`
		SpaceID string `json:"space_id"`
		APIKey  string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))
	assert.Equal(t, uid, exchangeResp.UID)
	assert.Equal(t, spaceA, exchangeResp.SpaceID)
	assert.True(t, strings.HasPrefix(exchangeResp.APIKey, "uk_"), exchangeResp.APIKey)
}

// 上游不认的 access_token 一律 401,且不能因为形态不同而走进别的分支。
func TestOAuth2Kind_UnknownAccessTokenIsRejected(t *testing.T) {
	route, ctx, mp := setupOAuth2KindAPITest(t)
	const issuer = "https://idp-test.example.com"
	uid := seedIntegrationUser(t, ctx, issuer, mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Research", 2, "2026-01-01 10:00:00")

	for name, token := range map[string]string{
		"unknown opaque token": "not-the-known-token",
		// 关键用例:一个 JWT 形态的字符串。按形态猜的实现会把它送进本地验签,
		// 而这个 kind 根本没有可用于本地验签的密钥。
		"jwt-shaped token": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln",
		"empty-ish token":  "   ",
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", token, nil))
			assert.NotEqual(t, http.StatusOK, w.Code,
				"an access token the upstream does not recognise must not authenticate")
			assert.NotEqual(t, http.StatusInternalServerError, w.Code,
				"a rejected credential is a client error, not ours: body=%s", w.Body.String())
		})
	}
}

// 未绑定的 subject:身份确立成功,但本地没有对应用户 → 明确的"未绑定"错误,
// 不是 500,也不是静默建号。
func TestOAuth2Kind_UnlinkedSubjectIsNotFound(t *testing.T) {
	route, _, mp := setupOAuth2KindAPITest(t)
	// 不 seed 任何 identity 行。
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.NotEqual(t, http.StatusInternalServerError, w.Code, w.Body.String())
}
