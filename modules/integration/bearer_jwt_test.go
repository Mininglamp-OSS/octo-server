package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// 桌面客户端把这张 JWT 存下来长期复用(exp 约 15 天),每次调这两个端点都出示
// 同一张 —— 它**不会**每次重签。
//
// 所以 /exchange-jwt 那条"iat 起 10 分钟"的上限不能套在这里:那个上限的论证是
// "用途只是登录那一刻兑换一次会话",对**每请求都验同一张 assertion 的常驻认证器**
// 不成立。套上去的后果是桌面端登录 10 分钟后这两个端点永久 401,而且错误与
// "凭据无效"不可区分。
//
// 认证器用凭据自己的 exp —— 那是上游对生命周期的声明。至于"15 天的 bearer 窗口
// 偏长",那是已经记在 Pending 的同一个问题(需要上游给 aud/jti),不该用一个会把
// 功能弄坏的上限来假装解决。
func TestBearerJWT_LongLivedTokenStillAuthenticatesOnIntegrationPath(t *testing.T) {
	route, ctx, _ := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200003")
	space := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, space, "Desktop", 2, "2026-01-01 10:00:00")

	// 10 天前签发,exp 还有 5 天 —— 桌面端正常复用的形态。
	tok := signDesktopJWT(t, testBearerJWTSecret, 2200003, "desk.user",
		time.Now().Add(-10*24*time.Hour))

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", tok, nil))
	require.Equal(t, http.StatusOK, w.Code,
		"a token the desktop client legitimately reuses within its exp must authenticate; "+
			"applying the one-shot redemption ceiling here breaks the client 10 minutes "+
			"after login. body=%s", w.Body.String())
	var resp struct {
		UID string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, uid, resp.UID)
}

// 但真正过了 exp 的 token 必须被拒 —— 认证器换成看 exp,不是不看。
func TestBearerJWT_ExpiredTokenIsRejectedOnIntegrationPath(t *testing.T) {
	route, ctx, _ := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200007")
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	// signDesktopJWT 的 exp = iat + 15 天,所以 iat 取 -20 天即已过期。
	tok := signDesktopJWT(t, testBearerJWTSecret, 2200007, "desk.user",
		time.Now().Add(-20*24*time.Hour))

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", tok, nil))
	assert.NotEqual(t, http.StatusOK, w.Code, "an expired token must not authenticate")
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

// 一张**本地验签认定是我们自己的、但按其自身条件被拒**的 JWT,绝不能被转发到
// 第三方 IdP。
//
// 回落逻辑原本是无条件的:只要本地 Verify 返回任何 error 就继续问上游。于是一张
// HMAC 完全合法、只是过了新鲜度窗口的业务 JWT 会被塞进上游 /userinfo 的
// **query string**(那个 IdP 的凭据形态就是放 URL 上),从而落进对方的访问日志和
// 任何中间设备的日志里。
//
// 泄漏的东西有两层:载荷里的 userId / domainAccount 是 PII;而带着我方密钥下
// 合法签名的 token 交给第三方,等于送给对方一份可离线校验密钥的材料 —— 本 PR
// 自己给密钥设 32 字节下限时的论证就是"短密钥可以从一张合法 token 离线爆破出来,
// 之后可以伪造任何人的登录"。
//
// 区分方式现成:格式/alg/签名错 = "不是我们的",可以回落;其余(新鲜度、claims)
// = "是我们的但被拒",必须就地 401。
func TestBearerJWT_OurOwnRejectedTokenIsNotForwardedUpstream(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200005")
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	// 注意这里**不能**用"iat 太旧":认证器改成只看 exp 之后,10 天前签发但未过期的
	// token 在这条路径上是合法的(见 TestBearerJWT_LongLivedToken...)。这条用例要的是
	// "HMAC 已匹配、因此确定是我们的,但按自身条件被拒"的形态。
	for name, tok := range map[string]string{
		// 签名合法,但已过 exp(signDesktopJWT 的 exp = iat + 15 天)
		"expired": signDesktopJWT(t, testBearerJWTSecret, 2200005, "desk.user",
			time.Now().Add(-20*24*time.Hour)),
		// 签名合法,但 userId 为 0(claims 不合格)
		"zero userId": signDesktopJWT(t, testBearerJWTSecret, 0, "desk.user",
			time.Now().Add(-time.Minute)),
		// 签名合法,但 iat 在远期未来(把可用窗口整体后移)
		"iat far in the future": signDesktopJWT(t, testBearerJWTSecret, 2200005, "desk.user",
			time.Now().Add(24*time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			mp.ResetRequestLog()
			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
				"/v1/integrations/oidc/spaces", tok, nil))

			assert.NotEqual(t, http.StatusOK, w.Code, "a rejected token must not authenticate")

			q := mp.LastUserInfoQuery()
			assert.Empty(t, q,
				"the token was forwarded to the upstream IdP (userinfo query=%q); a token "+
					"our own verifier recognised and rejected carries a valid HMAC under our "+
					"secret, and this IdP takes credentials in the URL, so forwarding it "+
					"leaks both the payload and signature material into a third party's logs", q)
			assert.NotContains(t, q, tok,
				"the business JWT itself appeared in the upstream request URL")
		})
	}
}

// 反面:**不是**我们的 token(签名对不上/根本不是 JWT)必须照常回落到上游 ——
// 否则这个分流就把上游凭据路径掐断了。
func TestBearerJWT_ForeignTokenStillFallsThroughToUpstream(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")

	// 上游的不透明 access_token —— 本地验签会以"格式不对"拒绝,必须回落。
	mp.ResetRequestLog()
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, mp.LastUserInfoQuery(),
		"an opaque upstream token must still reach /userinfo; otherwise the split has "+
			"broken the upstream credential path")

	// 用别的密钥签的 JWT:签名对不上 = 不是我们的 → 也回落(然后被上游拒)。
	foreign := signDesktopJWT(t, "a-different-secret-also-32-bytes!!", 2200006, "x",
		time.Now().Add(-time.Minute))
	mp.ResetRequestLog()
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", foreign, nil))
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, mp.LastUserInfoQuery(),
		"a JWT signed with an unknown key is not ours, so it should fall through")
}

// -----------------------------------------------------------------------------
// 以下两组用例覆盖上一轮"自家 token 不得转发上游"那条修复漏掉的两道门。
// -----------------------------------------------------------------------------

// signDesktopJWTRaw 用给定密钥签一段**原样给定**的 payload JSON。
//
// signDesktopJWT 走 json.Marshal,只能造出字段类型正确的 payload —— 而下面这条
// 用例要的恰好是"签名合法、字段类型写错",用那个 helper 表达不出来。上一轮的
// 用例表因此整类漏掉了这种形态。
func signDesktopJWTRaw(t *testing.T, secret, payloadJSON string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// 一张**签名合法但 payload 字段类型写错**的业务 JWT,同样不得被转发到上游。
//
// 这是上一轮那条修复的第一道漏网门:归属判定原先按错误哨兵的身份做,而
// ErrJWTMalformed 在 hmac.Equal 之后也会出现(payload 不是 JSON / exp 不是整数 /
// 反序列化到 claims 失败)。于是这些"明确是我们签的"token 被判成"别人的",
// 落进 /userinfo 的 query string。
//
// 触发条件不刁钻:JS 后端写 `iat: Date.now()/1000` 不取整就是浮点;把整数 id
// 序列化成字符串是 JSON API 里最常见的做法之一。而且客户端把这张 token 存下来
// 在整个有效期(约 15 天)内反复出示 —— 泄漏是持续的,不是一次性的。
func TestBearerJWT_ValidSignatureWithWrongPayloadTypesIsNotForwarded(t *testing.T) {
	route, ctx, mp := setupBothCredentialsTest(t)
	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com#bearer-jwt", "2200011")
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Desktop", 2, "2026-01-01 10:00:00")

	iat := time.Now().Add(-time.Minute).Unix()
	exp := time.Now().Add(15 * 24 * time.Hour).Unix()

	for name, payload := range map[string]string{
		"userId as string": fmt.Sprintf(
			`{"userId":"2200011","domainAccount":"desk.user","iat":%d,"exp":%d}`, iat, exp),
		"iat as float": fmt.Sprintf(
			`{"userId":2200011,"domainAccount":"desk.user","iat":%d.75,"exp":%d}`, iat, exp),
		"exp as string": fmt.Sprintf(
			`{"userId":2200011,"domainAccount":"desk.user","iat":%d,"exp":"%d"}`, iat, exp),
		"payload is not json": `plain-text-payload`,
	} {
		t.Run(name, func(t *testing.T) {
			tok := signDesktopJWTRaw(t, testBearerJWTSecret, payload)
			mp.ResetRequestLog()

			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
				"/v1/integrations/oidc/spaces", tok, nil))

			assert.NotEqual(t, http.StatusOK, w.Code, "a malformed payload must not authenticate")

			q := mp.LastUserInfoQuery()
			assert.Empty(t, q,
				"the token reached the upstream IdP (userinfo query=%q). Its HMAC is valid under "+
					"our own secret, so it is unambiguously ours; this IdP takes credentials in the "+
					"URL, so forwarding leaks the payload plus signature material into a third "+
					"party's logs", q)
			assert.NotContains(t, q, tok, "the business JWT appeared in the upstream request URL")
		})
	}
}

// 验签器**构造失败**时,这条凭据路径必须 fail-closed。
//
// 这是第二道漏网门,而且比第一道更彻底。NewBearerJWTVerifier 刻意区分两种返回:
// (nil, nil) = "这条路径没配,属合法部署形态";(nil, err) = "运维配错了"。
// modules/oidc 那侧遵守了(api_exchange_jwt.go 回 500),integration 这侧原先
// 只 log 然后带着 nil 验签器继续 —— 就写在一句"不能静默降级"的注释下面。
//
// 后果不是"401 看不懂":整个归属判定都在 `if it.bearerJWT != nil` 里面,验签器
// 一 nil,**每一张**桌面端 JWT 都无条件走 claims==nil 分支进 /userinfo 的 query。
// 触发条件是纯运维手滑:31 字节的密钥。而"密钥太短"正是泄漏签名材料最要命的
// 那个场景 —— 32 字节下限的存在理由就是防它被离线爆破。
func TestBearerJWT_VerifierConstructionFailureFailsClosed(t *testing.T) {
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
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_RPS", "1000")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_BURST", "10000")

	// 31 字节 —— 差一个字节不达 32 字节下限,NewBearerJWTVerifier 返回 error。
	const shortSecret = "only-thirty-one-bytes-long-key!"
	require.Len(t, shortSecret, 31)
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", shortSecret)

	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	seedIntegrationClient(t, ctx, defaultClientID, 1)
	route := s.GetRoute()

	// 客户端拿的是用那把(过短的)密钥签出来的 token —— 运维配错时的真实形态。
	tok := signDesktopJWT(t, shortSecret, 2200012, "desk.user", time.Now().Add(-time.Minute))
	mp.ResetRequestLog()

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", tok, nil))

	assert.NotEqual(t, http.StatusOK, w.Code, "a misconfigured deployment must not authenticate")

	q := mp.LastUserInfoQuery()
	assert.Empty(t, q,
		"the business JWT was forwarded to the upstream IdP (userinfo query=%q) because the "+
			"verifier failed to construct and the whole classification sits behind a nil check. "+
			"A construction error is an operator mistake, not a signal that this credential path "+
			"is off — and this particular mistake (a short secret) is exactly the case where "+
			"leaking valid signature material matters most", q)
	assert.NotContains(t, q, tok, "the business JWT appeared in the upstream request URL")
}

// 反面守住合法形态:**没有**配密钥时,这条路径就是关的,上游凭据必须照常工作。
//
// 这条和上一条一起把 NewBearerJWTVerifier 那个二分钉住 —— 否则"构造失败要拒绝"
// 很容易被实现成"只要没有验签器就拒绝",那会把一个合法部署形态搞坏。
func TestBearerJWT_AbsentSecretKeepsUpstreamPathWorking(t *testing.T) {
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
	t.Setenv("OCTO_OIDC_BEARER_JWT_SECRET", "")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_RPS", "1000")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_BURST", "10000")

	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	seedIntegrationClient(t, ctx, defaultClientID, 1)
	route := s.GetRoute()

	uid := seedIntegrationUser(t, ctx, "https://idp-test.example.com", mp.Subject())
	seedSpaceMembership(t, ctx, uid, "sp_"+util.GenerUUID()[:8], "Web", 2, "2026-01-01 10:00:00")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet,
		"/v1/integrations/oidc/spaces", mp.AccessToken(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, mp.LastUserInfoQuery(),
		"an absent secret is a legal deployment shape ('upstream OIDC on, business JWT off'); "+
			"it must not be conflated with a misconfiguration")
}
