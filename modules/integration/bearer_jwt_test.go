package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/oidc"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 桌面客户端手上只有业务后端自签的 HS256 JWT —— 它没有上游的 access_token,
// 也没有 id_token。而它需要用这两个端点列 space、换 uk_ key。
//
// 所以同一个 Authorization: Bearer 头上必须同时接受两类凭据。这不需要客户端
// 改契约,因为"这是不是业务 JWT"可以由**验签**回答:一张 token 要么带着我方
// 密钥下的合法 HMAC,要么没有。那是确定性检验,不是按形态猜。
//
// 两个方向都不会误判:
//   - 上游不透明 access_token 不可能带出我方密钥下的合法签名(需要知道密钥);
//   - 上游 id_token 是 RS256,而验签把 alg 钉死 HS256 并显式拒绝 RS256。
//
// 顺序上先试本地验签、再回落上游:本地这步不外呼、无副作用,反过来会让桌面端
// 每个请求都白打一次 /userinfo。
// -----------------------------------------------------------------------------

const testBearerJWTSecret = "integration-test-secret-32-bytes!!"

// signDesktopJWT 造一张业务后端形态的 JWT。
func signDesktopJWT(t *testing.T, secret string, userID int64, domainAccount string, iat time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(map[string]any{
		"userId":        userID,
		"domainAccount": domainAccount,
		"iat":           iat.Unix(),
		// 上游给的 exp 约为签发后 15 天;我方另有基于 iat 的更短上限。
		"exp": iat.Add(15 * 24 * time.Hour).Unix(),
	})
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// setupBothCredentialsTest 起一个**同时**启用上游 OAuth2 与业务 JWT 的环境。
func setupBothCredentialsTest(t *testing.T) (http.Handler, *config.Context, *oidc.MockOAuth2Provider) {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "fedcba9876543210fedcba9876543210")

	mp := oidc.NewMockOAuth2Provider(t)
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://idp-test.example.com")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "cid")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "csecret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://octo.example/callback")
	t.Setenv("DM_OIDC_RT_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("OCTO_OIDC_PROVIDER_KIND", "oauth2")
	t.Setenv("OCTO_OIDC_PROVIDER_BASE_URL", mp.BaseURL())
	t.Setenv("OCTO_OIDC_ALLOW_INSECURE_UPSTREAM", "1")
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", testBearerJWTSecret)
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_RPS", "1000")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_BURST", "10000")

	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	seedIntegrationClient(t, ctx, defaultClientID, 1)
	return s.GetRoute(), ctx, mp
}

// 桌面端凭据(业务 JWT)在两个端点上都能用。
func TestBearerJWT_SpacesAndExchangeWorkForDesktopCredential(t *testing.T) {
	route, ctx, _ := setupBothCredentialsTest(t)

	// 桌面端的身份落在独立命名空间下 —— 与上游 subject 隔离。
	const userID = int64(2200001)
	bearerIssuer := "https://idp-test.example.com#bearer-jwt"
	uid := seedIntegrationUser(t, ctx, bearerIssuer, "2200001")
	spaceA := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceA, "Desktop", 2, "2026-01-01 10:00:00")
	seedOwnBot(t, ctx, uid, spaceA, "bot_"+util.GenerUUID()[:8], "")

	tok := signDesktopJWT(t, testBearerJWTSecret, userID, "desk.user", time.Now().Add(-time.Minute))

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", tok, nil))
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
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", tok, map[string]interface{}{
		"space_id": spaceA,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exResp struct {
		UID    string `json:"uid"`
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exResp))
	assert.Equal(t, uid, exResp.UID)
	assert.True(t, strings.HasPrefix(exResp.APIKey, "uk_"), exResp.APIKey)
}

// **兼容性核心断言**:业务 JWT 启用之后,上游凭据仍然照常工作。
//
// 两条路径共存才是需求;把其中一条换掉不算适配。
func TestBearerJWT_UpstreamCredentialStillWorksAlongside(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)

	const upstreamIssuer = "https://idp-test.example.com"
	upstreamUID := seedIntegrationUser(t, ctx, upstreamIssuer, mp.Subject())
	spaceU := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, upstreamUID, spaceU, "Web", 2, "2026-01-01 10:00:00")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		UID string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, upstreamUID, resp.UID,
		"enabling the desktop credential must not take the upstream credential away")
}

// 两类凭据解析到各自的命名空间,不会互相冒充。
//
// 如果两条路径共用 issuer,同一个 (issuer, subject) 会把两个不同的人合成一个
// 账号 —— 而这张表的唯一键不可逆。
func TestBearerJWT_TwoCredentialTypesResolveToDifferentIdentities(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)

	// 故意让两边的 subject 字面相同,只有 issuer 不同。
	const sharedSubject = "823071756087671783"
	mp.SetSubject(sharedSubject)
	upstreamUID := seedIntegrationUser(t, ctx, "https://idp-test.example.com", sharedSubject)
	desktopUID := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", sharedSubject)
	require.NotEqual(t, upstreamUID, desktopUID)

	seedSpaceMembership(t, ctx, upstreamUID, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")
	seedSpaceMembership(t, ctx, desktopUID, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	// subject 是数字串,业务 JWT 的 userId 用同一个值。
	tok := signDesktopJWT(t, testBearerJWTSecret, 823071756087671783, "desk.user", time.Now().Add(-time.Minute))

	for name, tc := range map[string]struct {
		token   string
		wantUID string
	}{
		"upstream access token": {mp.AccessToken(), upstreamUID},
		"desktop business jwt":  {tok, desktopUID},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", tc.token, nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var r struct {
				UID string `json:"uid"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
			assert.Equal(t, tc.wantUID, r.UID,
				"the two credential types must resolve into separate issuer namespaces")
		})
	}
}

// 用错密钥签的 JWT 必须被拒 —— 它不能因为"回落到上游路径"而拿到别的结果。
func TestBearerJWT_WrongSecretIsRejected(t *testing.T) {
	route, ctx, _ := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200002")
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	tok := signDesktopJWT(t, "a-different-secret-also-32-bytes!!", 2200002, "desk.user", time.Now().Add(-time.Minute))
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", tok, nil))
	assert.NotEqual(t, http.StatusOK, w.Code,
		"a JWT signed with an unknown key must not authenticate")
	assert.NotEqual(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

// 过期(iat 太旧)的业务 JWT 被拒 —— 我方的新鲜度上限在这条路径上同样生效。
func TestBearerJWT_StaleTokenIsRejectedOnIntegrationPath(t *testing.T) {
	route, ctx, _ := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200003")
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	// 10 天前签发但 exp 还远 —— 抓包后长期复用的形态。
	tok := signDesktopJWT(t, testBearerJWTSecret, 2200003, "desk.user", time.Now().Add(-10*24*time.Hour))
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", tok, nil))
	assert.NotEqual(t, http.StatusOK, w.Code,
		"the max-lifetime ceiling must apply here too, not just on /exchange-jwt")
}

// 未配置业务 JWT 密钥时,行为与改动前完全一致(只认上游凭据)。
func TestBearerJWT_NotConfiguredKeepsUpstreamOnlyBehaviour(t *testing.T) {
	route, ctx, mp := setupOAuth2KindAPITest(t) // 这个 setup 不配 bearer secret
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")

	// 上游凭据照常。
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 业务 JWT 无从验证 → 拒绝。
	tok := signDesktopJWT(t, testBearerJWTSecret, 2200004, "desk.user", time.Now().Add(-time.Minute))
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", tok, nil))
	assert.NotEqual(t, http.StatusOK, w.Code,
		"without a configured secret there is nothing to verify a business JWT against")
}
