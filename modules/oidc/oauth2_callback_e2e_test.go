package oidc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gin-gonic/gin"
)

// plain-OAuth2 kind 的浏览器登录全链路(authorize → IdP → callback → 会话)。
//
// 为什么单独写:provider 层已经对着真 HTTP mock 测透了(oauth2_provider_http_test.go),
// 但 handler 这一段 —— state 签发与单次消费、Exchange/Identity 的调用顺序、
// ResolveOrLink → IssueSession → ThirdAuthcode 的落地、302 回 return_to ——
// 之前只被 KindOIDC 驱动过。两个 kind 在这一段的差异不是零:OAuth2 没有
// id_token、没有 nonce、没有 PKCE,authorize 少三个参数,callback 少一次验签。
// 只测 OIDC 等于假设"handler 与 kind 无关",而那正是这次重构要验证的东西。
// -----------------------------------------------------------------------------

// newTestOIDCOAuth2 组一个 KindOAuth2 的 OIDC 实例,provider 指向 mock IdP。
//
// 与 newTestOIDC 的关键差异:client 留 nil(plain OAuth2 没有 Discovery,也没有
// JWKS),provider 走 newOAuth2Provider。callback 路径不碰 o.client —— 它只被
// SyncWorker 使用 —— 所以 nil 是正确的形态,而不是测试偷懒。
func newTestOIDCOAuth2(t *testing.T, mp *mockOAuth2Provider, users *fakeUserLookup, store *fakeIdentityStore) *OIDC {
	t.Helper()
	pcfg := mp.providerConfig()
	prov, err := newOAuth2Provider(pcfg)
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	cfg := &Config{
		Enabled: true,
		Provider: ProviderConfig{
			ID:           "test",
			Name:         "Test IdP",
			Kind:         KindOAuth2,
			Issuer:       pcfg.Issuer,
			BaseURL:      pcfg.BaseURL,
			ClientID:     pcfg.ClientID,
			ClientSecret: pcfg.ClientSecret,
			RedirectURI:  pcfg.RedirectURI,
			AppID:        pcfg.AppID,
			Scopes:       pcfg.Scopes,
			// 这个 kind 下 autolink 只能靠 email,而协议不提供 verified 语义,
			// 所以 RequireEmailVerified 留 true 时 email 绑定天然不成立 ——
			// 与生产一致,别在测试里放宽。
			RequireEmailVerified: true,
			AutoLinkByEmail:      true,
			AllowNewUser:         true,
			ReturnToHosts:        []string{"app.example.com"},
		},
	}
	return &OIDC{
		Log:        log.NewTLog("OIDC-oauth2-test"),
		cfg:        cfg,
		provider:   prov,
		service:    newService(cfg.Provider, store, users),
		store:      store,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
	}
}

func newOAuth2TestRouter(o *OIDC) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/auth/oidc/test")
	g.GET("/authorize", func(c *gin.Context) { o.authorize(wrapWk(c)) })
	g.GET("/callback", func(c *gin.Context) { o.callback(wrapWk(c)) })
	return r
}

// authorizeAndGetState 走一次 authorize,返回 IdP 跳转 URL 上的 state。
func authorizeAndGetState(t *testing.T, r *gin.Engine, query string) (state string, loc *url.URL) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/auth/oidc/test/authorize?"+query, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.9")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, body=%s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state = loc.Query().Get("state")
	if state == "" {
		t.Fatal("authorize did not put a state on the IdP URL")
	}
	return state, loc
}

// 存量用户:identity 行已存在 → 直接命中,不建号。
func TestOAuth2Callback_E2E_ExistingUser(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	mp.SubjectForUserInfo = "823071756087671783"

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "u-known",
			LoginRespJSON: `{"token":"tok-oauth2","uid":"u-known"}`,
		},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		UID: "u-known", Issuer: mp.providerConfig().Issuer, Subject: mp.SubjectForUserInfo,
	})

	o := newTestOIDCOAuth2(t, mp, users, store)
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newOAuth2TestRouter(o)

	state, loc := authorizeAndGetState(t, r, "authcode=front-ac-1&return_to=/inbox")

	// 这个 kind 的 authorize 不该带 OIDC 专有参数 —— 上游会直接拒。
	for _, absent := range []string{"nonce", "code_challenge", "code_challenge_method"} {
		if v := loc.Query().Get(absent); v != "" {
			t.Errorf("authorize URL carries %s=%q; the plain-OAuth2 upstream has no "+
				"place for it and rejects unknown parameters", absent, v)
		}
	}
	if got := loc.Query().Get("scope"); got != "read" {
		t.Errorf("scope = %q, want read (this IdP only accepts read)", got)
	}

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/test/callback?state="+state+"&code=idp-code-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/inbox" {
		t.Errorf("redirect = %q, want /inbox", got)
	}
	if got := fakeAC.get("front-ac-1"); !strings.Contains(got, `"token":"tok-oauth2"`) {
		t.Errorf("ThirdAuthcode payload = %q, want the LoginRespJSON", got)
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("IssueSession calls = %d, want 1", len(users.loginCalls))
	}
	call := users.loginCalls[0]
	if call.UID != "u-known" {
		t.Errorf("IssueSession UID = %q, want u-known", call.UID)
	}
	if call.CreateUser {
		t.Error("CreateUser = true for an already-linked identity")
	}
	// 客户端真实 IP 取 XFF 的最后一跳。
	if call.PublicIP != "198.51.100.9" {
		t.Errorf("PublicIP = %q, want 198.51.100.9", call.PublicIP)
	}

	// 我方实际怎么发的:token 端点必须被调到,且带上 code。
	mp.mu.Lock()
	tokReq := mp.LastTokenRequest
	uiReq := mp.LastUserInfoRequest
	mp.mu.Unlock()
	if tokReq == nil {
		t.Fatal("token endpoint was never called")
	}
	if uiReq == nil {
		t.Fatal("userinfo endpoint was never called — identity must come from it")
	}
}

// 新用户:identity 行不存在,且 email 未 verified → 不 autolink,走建号。
//
// 顺带钉住 TrustedSSOCreate:只有真正新建时才置 true,它会绕过 register.off。
func TestOAuth2Callback_E2E_NewUserCreatesIdentityRow(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	mp.SubjectForUserInfo = "823071756087671999"

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "u-fresh",
			LoginRespJSON: `{"token":"tok-fresh","uid":"u-fresh"}`,
		},
	}
	store := newFakeIdentityStore()
	o := newTestOIDCOAuth2(t, mp, users, store)
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-2&return_to=/home")
	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/test/callback?state="+state+"&code=idp-code-2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("IssueSession calls = %d, want 1", len(users.loginCalls))
	}
	if !users.loginCalls[0].CreateUser {
		t.Error("CreateUser = false; an unlinked subject must create a user here")
	}
	if !users.loginCalls[0].TrustedSSOCreate {
		t.Error("TrustedSSOCreate = false; the create path must be allowed to bypass register.off")
	}

	written := store.written
	if len(written) != 1 {
		t.Fatalf("identity rows written = %d, want 1", len(written))
	}
	if written[0].Subject != mp.SubjectForUserInfo {
		t.Errorf("identity subject = %q, want %q", written[0].Subject, mp.SubjectForUserInfo)
	}
	// issuer 必须是我方配置值,不是响应体里的任何字段。
	if written[0].Issuer != mp.providerConfig().Issuer {
		t.Errorf("identity issuer = %q, want the configured %q",
			written[0].Issuer, mp.providerConfig().Issuer)
	}
}

// state 只能消费一次 —— 防重放。第二次带同一个 state 必须失败且不再建会话。
func TestOAuth2Callback_StateIsSingleUse(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-replay", LoginRespJSON: `{"token":"t"}`},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		UID: "u-replay", Issuer: mp.providerConfig().Issuer, Subject: mp.SubjectForUserInfo,
	})
	o := newTestOIDCOAuth2(t, mp, users, store)
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-3&return_to=/home")
	path := "/v1/auth/oidc/test/callback?state=" + state + "&code=idp-code-3"

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, httptest.NewRequest("GET", path, nil))
	if w1.Code != http.StatusFound {
		t.Fatalf("first callback status = %d, body=%s", w1.Code, w1.Body.String())
	}
	firstSessions := len(users.loginCalls)

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest("GET", path, nil))
	if w2.Code == http.StatusFound {
		t.Errorf("replayed state produced another redirect (status %d); state must be single-use", w2.Code)
	}
	if len(users.loginCalls) != firstSessions {
		t.Errorf("IssueSession calls went %d → %d on replay; a consumed state must not "+
			"be able to mint a second session", firstSessions, len(users.loginCalls))
	}
}

// 上游 token 端点失败 → 不建会话、不写 identity 行,且 302 带 oidc_error。
func TestOAuth2Callback_UpstreamTokenFailureDoesNotCreateAnything(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	mp.TokenStatus = http.StatusBadRequest // {"error":"invalid_grant"}

	users := &fakeUserLookup{}
	store := newFakeIdentityStore()
	o := newTestOIDCOAuth2(t, mp, users, store)
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-4&return_to=/home")
	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/test/callback?state="+state+"&code=bad-code", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if len(users.loginCalls) != 0 {
		t.Errorf("IssueSession was called %d times despite the token exchange failing",
			len(users.loginCalls))
	}
	if len(store.written) != 0 {
		t.Errorf("identity rows written = %d, want 0 on a failed exchange", len(store.written))
	}
	// 失败必须让前端感知(而不是傻等 authcode TTL 超时)。
	if loc := w.Header().Get("Location"); loc != "" && !strings.Contains(loc, "oidc_error") {
		t.Errorf("redirect = %q, want an oidc_error marker so the front end stops polling", loc)
	}
}

// newRecorderForCallback 走一次 callback 并返回 recorder。
func newRecorderForCallback(t *testing.T, r *gin.Engine, state, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET",
		"/v1/auth/oidc/test/callback?state="+state+"&code="+code, nil))
	return w
}
