package oidc

// api_exchange_jwt_test.go — POST /v1/auth/oidc/<id>/exchange-jwt
//
// 原生客户端持有的是 客户端后端自签的 HS256 JWT(不是上游 OIDC access_token),
// 因此不能走 /exchange(那个端点调 IdP /userinfo 换身份)。本端点做本地 HS256
// 验签 + 过期校验,通过后走与 /exchange 同样的 ResolveOrLink → IssueSession
// 流程签发我方 session token。
//
// 与 /exchange 的关键差异:
//   - 验签在本地完成,不做任何外呼(/exchange 会调 IdP);
//   - issuer 使用独立命名空间(上游 issuer + "#bearer-jwt"),与上游 issuer 隔离,
//     避免/上游 OIDC flow 两条链路出现"同一人被建成两号"时静默互相覆盖;
//   - subject 是 客户端 userId 数字转字符串,不是上游 IdP sub 18 位长整型;
//   - 不携带 email/phone/verified,AutoLink 天然 fail-closed。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gin-gonic/gin"
)

// fakeBearerProvider 是 /exchange-jwt 测试用的空 AuthProvider 实现:本端点本地验签,
// 不会调用 provider 的任何方法,但 handler 入口要求 provider!=nil(模拟"provider
// 构造成功"的生产状态)。只需满足接口,无需真实逻辑。
type fakeBearerProvider struct{}

func (fakeBearerProvider) Kind() ProviderKind                 { return KindOAuth2 }
func (fakeBearerProvider) Capabilities() ProviderCapabilities { return ProviderCapabilities{} }
func (fakeBearerProvider) Issuer() string                     { return "https://idp-test.example.com" }
func (fakeBearerProvider) AuthCodeURL(AuthCodeParams) (string, error) {
	return "", errors.New("not used")
}
func (fakeBearerProvider) Exchange(context.Context, string, string) (*TokenSet, error) {
	return nil, errors.New("not used")
}
func (fakeBearerProvider) LogoutURL(context.Context, LogoutHint) (string, bool) { return "", false }
func (fakeBearerProvider) IdentityFromClientCredential(context.Context, string) (*IdentityClaims, error) {
	return nil, errors.New("not used")
}
func (fakeBearerProvider) Identity(context.Context, *TokenSet) (*IdentityClaims, error) {
	return nil, errors.New("not used")
}

// newTestOIDCForBearerJWT 构造开启 bearer JWT 支持的 OIDC 实例。
// 不需要 AuthProvider(本端点本地验签,不外呼),但要 service/store/audit。
func newTestOIDCForBearerJWT(t *testing.T, secret []byte, issuer string, users *fakeUserLookup, store *fakeIdentityStore) *OIDC {
	t.Helper()
	cfg := &Config{
		Enabled: true,
		Provider: ProviderConfig{
			ID:           "sso",
			Name:         "SSO",
			Issuer:       "upstream-idp-test", // 上游 issuer(本端点不使用,但保持配置合法)
			ClientID:     "test-cid",
			RedirectURI:  "https://app.example.com/callback",
			AllowNewUser: true,
			Scopes:       []string{"read"},
		},
	}
	// bearerJWT 是 OIDC 结构体上的字段,New() 经 NewBearerJWTVerifier 填入。
	// 测试直接构造它,避免依赖 env。
	o := &OIDC{
		Log: log.NewTLog("OIDC-test"),
		cfg: cfg,
		// provider 填一个非 nil sentinel:本端点本地验签,不会真正调用它,但
		// handler 入口会做 provider!=nil 守卫避免构造失败路径的 nil panic。
		provider:   fakeBearerProvider{},
		service:    newService(cfg.Provider, store, users),
		store:      store,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
		bearerJWT:  newBearerJWTVerifierForTest(secret, issuer),
	}
	return o
}

func postBearerExchange(o *OIDC, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/exchange-jwt", func(c *gin.Context) { o.exchangeJWT(wrapWk(c)) })
	req := httptest.NewRequest(http.MethodPost, "/exchange-jwt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// signBearerTesting 用传入的 secret 签一个 bearer JWT,便于测试构造合法输入。
// 不依赖 bearer_jwt_test.go 的 signBearerToken(那是包内测试,绑定 bearerJWTClaims 形状,
// 这里可能需要注入坏字段),直接用与 jwt_hs256_test.go 同样的 HS256 签名原语。
func signBearerTesting(t *testing.T, secret []byte, userId int64, domainAccount string, exp time.Time) string {
	t.Helper()
	// iat 是**签发时刻**,不是从 exp 倒推的。上游的 exp 约为签发后 15 天,
	// 早先这里用 exp-15d 反推 iat,等于造出一张"15 天前签发"的 token ——
	// 那是被 bearerJWTMaxAge 正确拒绝的重放形态,不是新登录的形态。
	claims := map[string]any{
		"userId":        userId,
		"domainAccount": domainAccount,
		"iat":           time.Now().Add(-1 * time.Minute).Unix(),
		"exp":           exp.Unix(),
	}
	// 借用 jwt_hs256_test.go 里的 signJWT(同包测试可见):header 固定 HS256+JWT。
	return signJWT(t, string(secret), claims)
}

// ---- RED:handler 不存在时 404 ----------------------------------------------

func TestExchangeJWT_HandlerExists(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("test-secret"), "https://idp-test.example.com#bearer-jwt", users, store)

	w := postBearerExchange(o, `{"access_token":"x"}`)
	if w.Code == http.StatusNotFound {
		t.Fatal("POST /exchange-jwt returned 404 — handler not mounted yet")
	}
}

// ---- 请求校验 ----------------------------------------------------------------

func TestExchangeJWT_MissingBody(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	if w := postBearerExchange(o, ""); w.Code != http.StatusBadRequest {
		t.Errorf("empty body: want 400, got %d", w.Code)
	}
}

func TestExchangeJWT_BlankToken(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	for _, body := range []string{`{}`, `{"access_token":""}`, `{"access_token":"   "}`} {
		if w := postBearerExchange(o, body); w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: want 400, got %d", body, w.Code)
		}
	}
}

// ---- secret 未配置时 500(配置错误,不给探测面) ---------------------------------

func TestExchangeJWT_SecretNotConfigured(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, nil, "", users, store) // 无 secret
	// 发任意 token,handler 必须返 500 而不是 panic 或 401
	w := postBearerExchange(o, `{"access_token":"abc"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("secret not configured: want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- 反枚举:所有验签/过期/claims 非法错误统一 401 ----------------------------

func TestExchangeJWT_BadSignatureIs401(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("correct-secret"), "https://idp-test.example.com#bearer-jwt", users, store)
	// 用错密钥签
	bad := signBearerTesting(t, []byte("wrong-secret"), 1, "u", time.Now().Add(time.Hour))
	w := postBearerExchange(o, `{"access_token":"`+bad+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad sig: want 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExchangeJWT_ExpiredIs401(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	expired := signBearerTesting(t, []byte("s"), 1, "u", time.Now().Add(-time.Minute))
	w := postBearerExchange(o, `{"access_token":"`+expired+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired: want 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExchangeJWT_ZeroUserIDIs401(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	z := signBearerTesting(t, []byte("s"), 0, "anon", time.Now().Add(time.Hour))
	w := postBearerExchange(o, `{"access_token":"`+z+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("userId=0: want 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExchangeJWT_GarbageTokenIs401(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForBearerJWT(t, []byte("s"), "https://idp-test.example.com#bearer-jwt", users, store)
	w := postBearerExchange(o, `{"access_token":"not-a-jwt.at.all"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("garbage: want 401, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- Happy path --------------------------------------------------------------

func TestExchangeJWT_HappyPathReturnsSession(t *testing.T) {
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "uid-alice",
			IsNewUser:     true,
			LoginRespJSON: `{"token":"sess-alice","uid":"uid-alice"}`,
		},
	}
	store := newFakeIdentityStore()
	secret := []byte("s")
	issuer := "https://idp-test.example.com#bearer-jwt"
	o := newTestOIDCForBearerJWT(t, secret, issuer, users, store)

	// 合成用户 id 与显示名,不使用真实 PII。
	const synthUserID int64 = 54321
	const synthName = "test.user"
	tok := signBearerTesting(t, secret, synthUserID, synthName, time.Now().Add(time.Hour))
	w := postBearerExchange(o, `{"access_token":"`+tok+`","flag":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v; body=%s", err, w.Body.String())
	}
	if resp["status"] != "ok" || resp["uid"] != "uid-alice" {
		t.Errorf("unexpected resp: %+v", resp)
	}
	// login_resp 必须包含我们签发的 session token
	lr, ok := resp["login_resp"].(string)
	if !ok || !strings.Contains(lr, "sess-alice") {
		t.Errorf("login_resp missing token: %v", resp["login_resp"])
	}

	// 校验 identity 行确实用 bearer JWT issuer + userId 作为 subject 写入
	if len(store.written) != 1 {
		t.Fatalf("expected 1 identity insert, got %d", len(store.written))
	}
	ins := store.written[0]
	if ins.Issuer != issuer {
		t.Errorf("identity.issuer=%q, want %q", ins.Issuer, issuer)
	}
	wantSubject := "54321"
	if ins.Subject != wantSubject {
		t.Errorf("identity.subject=%q, want %q (userId as string)", ins.Subject, wantSubject)
	}
	// email/phone 必须为空,verified 位必须是 0(fail-closed,防 autolink 误绑)
	if ins.Email != "" || ins.Phone != "" || ins.EmailVerified != 0 || ins.PhoneVerified != 0 {
		t.Errorf("identity row should have empty contact/verified: %+v", ins)
	}
}

// ---- issuer 隔离:不同环境不能混淆 -------------------------------------------

func TestExchangeJWT_IssuerUsedFromConfig(t *testing.T) {
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u", IsNewUser: true, LoginRespJSON: "{}"},
	}
	store := newFakeIdentityStore()
	secret := []byte("s")
	o := newTestOIDCForBearerJWT(t, secret, "https://idp.example.com#bearer-jwt", users, store)
	tok := signBearerTesting(t, secret, 42, "bob", time.Now().Add(time.Hour))
	w := postBearerExchange(o, `{"access_token":"`+tok+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if store.written[0].Issuer != "https://idp.example.com#bearer-jwt" {
		t.Errorf("issuer=%q, want https://idp.example.com#bearer-jwt", store.written[0].Issuer)
	}
}

// Sanity:unused import guard removed below;package compiles cleanly.
