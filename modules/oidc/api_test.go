package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// wrapWk 把 gin.Context 提升为 wkhttp.Context(后者就是嵌的 *gin.Context)。
// 测试避开 wkhttp 的认证中间件,直接调 handler 函数。
func wrapWk(c *gin.Context) *wkhttp.Context {
	return &wkhttp.Context{Context: c}
}

// fakeAudit 内存版审计写入,用于断言成败路径都落了审计。
type fakeAudit struct {
	mu      sync.Mutex
	entries []*AuditModel
}

func newFakeAudit() *fakeAudit { return &fakeAudit{} }

func (f *fakeAudit) InsertAudit(m *AuditModel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *m
	f.entries = append(f.entries, &cp)
	return nil
}

func (f *fakeAudit) events() []AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AuditEvent, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.Event)
	}
	return out
}

// fakeAuthcode 内存版 ThirdAuthcode 写入,用于 handler 测试。
// failNext > 0 时下一次 Set 调用会返错(模拟 Redis 抖动),用完自减。
type fakeAuthcode struct {
	mu       sync.Mutex
	saved    map[string]string
	failNext int
}

func newFakeAuthcode() *fakeAuthcode {
	return &fakeAuthcode{saved: make(map[string]string)}
}
func (f *fakeAuthcode) SetAuthcode(_ context.Context, authcode, payload string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("fake redis down")
	}
	f.saved[authcode] = payload
	return nil
}
func (f *fakeAuthcode) get(authcode string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved[authcode]
}

// newTestOIDC 用 mock provider + memory state store + fake authcode + fake user lookup
// 拼一个可独立测的 *OIDC,免触 testutil.NewTestServer 的迁移地雷。
func newTestOIDC(t *testing.T, mp *MockProvider, users *fakeUserLookup, store *fakeIdentityStore) *OIDC {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewClient(ctx, ClientConfig{
		Issuer:       mp.Issuer,
		ClientID:     mp.ClientID,
		ClientSecret: "test-secret",
		RedirectURI:  "https://app.example.com/callback",
		Scopes:       []string{"openid", "profile", "email"},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg := &Config{
		Enabled: true,
		Provider: ProviderConfig{
			ID:                   "aegis",
			Name:                 "Aegis",
			Issuer:               mp.Issuer,
			ClientID:             mp.ClientID,
			RedirectURI:          "https://app.example.com/callback",
			AutoLinkByEmail:      true,
			AllowNewUser:         true,
			RequireEmailVerified: true,
			ReturnToHosts:        []string{"app.example.com"},
			// YUJ-382:带上 identity_verification scope,确保
			// hasIdentityVerificationScope 返回 true,测试覆盖 userinfo-only 实名合并路径。
			Scopes: []string{"openid", "profile", "email", "identity_verification"},
		},
	}
	return &OIDC{
		Log:        log.NewTLog("OIDC-test"),
		cfg:        cfg,
		client:     client,
		service:    newService(cfg.Provider, store, users),
		store:      store,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
	}
}

func newTestRouter(o *OIDC) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/auth/oidc/aegis")
	g.GET("/authorize", func(c *gin.Context) { o.authorize(wrapWk(c)) })
	g.GET("/callback", func(c *gin.Context) { o.callback(wrapWk(c)) })
	g.POST("/logout", func(c *gin.Context) { o.logout(wrapWk(c)) })
	return r
}

// 测试中走 gin.Context 直接调 handler 函数;wkhttp.Context 内部就是嵌的 gin.Context,
// 我们走的方法都在 gin.Context 上,直接强转即可。
//
// 这种"测试用 wrapper"避免在测试里复刻 wkhttp 的全部初始化(认证中间件等)。

// Cycle 13: authorize 应 302 到 IdP authorize URL,且 query 含 client_id / state /
// nonce / code_challenge,等价于成功的 RFC 7636 PKCE 起步。
func TestAPI_Authorize_RedirectsToIdP(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-1&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("missing Location header")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if !strings.HasPrefix(loc, mp.Issuer) {
		t.Errorf("location should redirect to IdP, got %q", loc)
	}
	q := u.Query()
	for _, k := range []string{"client_id", "state", "nonce", "code_challenge", "code_challenge_method"} {
		if q.Get(k) == "" {
			t.Errorf("missing query param %q", k)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("PKCE method = %q, want S256", q.Get("code_challenge_method"))
	}
}

// authorize 缺 authcode 应 400。
func TestAPI_Authorize_MissingAuthcode(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// authcode 包含非法字符或超长应 400 拒,防 Redis key 注入 / 跨用户覆盖。
func TestAPI_Authorize_RejectsBadAuthcode(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	cases := []string{
		"",
		"../../etc/passwd",
		"with space",
		"with:colon",
		"with/slash",
		strings.Repeat("a", 129), // 超长
	}
	for _, ac := range cases {
		t.Run(ac, func(t *testing.T) {
			req := httptest.NewRequest("GET",
				"/v1/auth/oidc/aegis/authorize?authcode="+url.QueryEscape(ac), nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("authcode=%q: status = %d, want 400", ac, w.Code)
			}
		})
	}
}

// authorize 收到非法 return_to(host 不在白名单)应 400。
func TestAPI_Authorize_RejectsBadReturnTo(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/authorize?authcode=x&return_to=https://evil.com/grab", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// Cycle 14: authorize → callback 全链路成功,ThirdAuthcode Redis 应被写入。
func TestAPI_Callback_E2E_ExistingUser(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-X", map[string]interface{}{
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice",
	})
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "u-existing",
			LoginRespJSON: `{"token":"t-1","uid":"u-existing"}`,
		},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{UID: "u-existing", Issuer: mp.Issuer, Subject: "sub-X"})

	o := newTestOIDC(t, mp, users, store)
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	// Step 1: authorize → 拿到 state
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=front-ac&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("authorize status = %d", w.Code)
	}
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")

	// 模拟 IdP 端发 code,要让 mock 接受 code 并签 ID Token 时回填 nonce
	nonce := authURL.Query().Get("nonce")
	mp.PrepCode("idp-code", "sub-X", nonce)

	// Step 2: callback
	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if got := w2.Header().Get("Location"); got != "/home" {
		t.Errorf("redirect = %q, want /home", got)
	}
	got := fakeAC.get("front-ac")
	if !strings.Contains(got, `"token":"t-1"`) {
		t.Errorf("ThirdAuthcode payload = %q, want LoginRespJSON", got)
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("expected 1 IssueSession call, got %d", len(users.loginCalls))
	}
	if c := users.loginCalls[0]; c.UID != "u-existing" || c.CreateUser {
		t.Errorf("IssueSession call wrong: %+v", c)
	}
}

// 成功路径若 SetAuthcode 写 LoginRespJSON 失败,应:
//  1. 立刻补写 "0" 让前端轮询尽早感知
//  2. redirect URL 拼 ?oidc_error=1
//
// 不能让前端傻等 1 分钟 TTL 超时。
func TestAPI_Callback_SetAuthcodeFailureSurfacesToFrontend(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-z", map[string]interface{}{
		"email":          "z@example.com",
		"email_verified": true,
	})
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-z", LoginRespJSON: `{"token":"t"}`},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{UID: "u-z", Issuer: mp.Issuer, Subject: "sub-z"})
	o := newTestOIDC(t, mp, users, store)
	fakeAC := newFakeAuthcode()
	fakeAC.failNext = 1 // 第一次写 LoginRespJSON 时失败
	o.authcode = fakeAC
	audit := newFakeAudit()
	o.audit = audit
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-fail&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-zfail", "sub-z", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-zfail", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// 1. redirect 带 ?oidc_error=1
	loc := w2.Header().Get("Location")
	if !strings.Contains(loc, "oidc_error=1") {
		t.Errorf("location should carry oidc_error=1, got %q", loc)
	}
	// 2. Redis 兜底写了 "0"(因为 fakeAC.failNext=1 只失败 1 次,补写 "0" 这次成功)
	if got := fakeAC.get("ac-fail"); got != "0" {
		t.Errorf("ThirdAuthcode payload = %q, want \"0\" (fallback)", got)
	}
	// 3. 审计日志必须记 EventCallbackFail,否则线上 Redis 抖动无法事后追溯
	foundFail := false
	for _, e := range audit.events() {
		if e == EventCallbackFail {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Errorf("expected EventCallbackFail in audit, got %v", audit.events())
	}
}

// recoverFromIdentityRace 失败路径(查不到赢家): callback 不应把 ghost session 写到 Redis。
func TestAPI_Callback_RaceRecoveryFailureDoesNotLeakGhost(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-race", map[string]interface{}{
		"email":          "race@example.com",
		"email_verified": true,
	})
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{
			UID:           "u-ghost",
			IsNewUser:     true,
			LoginRespJSON: `{"token":"ghost-token"}`,
		},
	}
	store := newFakeIdentityStore()
	store.failInsertWithDuplicate = true // Insert 直接返 MySQL 1062
	store.failGetAfterDuplicate = true   // 查赢家也失败,模拟 recover 走不通
	o := newTestOIDC(t, mp, users, store)
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-race&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-race", "sub-race", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-race", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// Redis 必须是 "0" 而不是 ghost-token,否则前端会用一个无 OIDC 绑定的孤立账号
	got := fakeAC.get("ac-race")
	if got == `{"token":"ghost-token"}` {
		t.Fatal("ghost session leaked to ThirdAuthcode! security regression")
	}
	if got != "0" {
		t.Errorf("ThirdAuthcode payload = %q, want \"0\"", got)
	}
	if !strings.Contains(w2.Header().Get("Location"), "oidc_error=1") {
		t.Errorf("redirect should carry oidc_error=1, got %q", w2.Header().Get("Location"))
	}
}

// callback 拿到无效 state(已消费 / 过期 / 未存在)应 400 不走签发。
func TestAPI_Callback_BadState(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state=never-saved&code=x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// IP 失败计数已达阈值时,/callback 应直接 429 不消费 state、不调 IdP。
//
// 测试要点:
//   - 失败计数不再 +1(阈值已锁,再 +1 等于自我续期成永久锁)。
//   - state 还在 store 里没被消费,可被合法用户复用 → 间接验证未走 Consume。
func TestAPI_Callback_RateLimited(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.cbGuard = NewCallbackGuard(newCallbackGuardRedis(t), 3, 5*time.Minute)

	// 直接把计数顶到阈值。GetClientPublicIP 在无 X-Forwarded-For 时
	// 会返回 RemoteAddr 的 IP,httptest 默认 192.0.2.1。
	const testIP = "192.0.2.1"
	o.cbGuard.Reset(testIP)
	t.Cleanup(func() { o.cbGuard.Reset(testIP) })
	for i := 0; i < 3; i++ {
		if err := o.cbGuard.RecordFailure(testIP); err != nil {
			t.Fatalf("priming counter: %v", err)
		}
	}

	r := newTestRouter(o)
	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state=any&code=x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", w.Code, w.Body.String())
	}
}

// 成功 callback 应落 EventCallbackOK 审计;IdP 错误应落 EventCallbackFail。
func TestAPI_Callback_AuditTrail(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-audit", map[string]interface{}{
		"email":          "audit@example.com",
		"email_verified": true,
	})
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-audit", LoginRespJSON: `{"token":"t"}`},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{UID: "u-audit", Issuer: mp.Issuer, Subject: "sub-audit"})

	o := newTestOIDC(t, mp, users, store)
	audit := newFakeAudit()
	o.audit = audit
	r := newTestRouter(o)

	// 成功路径
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-ok&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-ok", "sub-audit", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-ok", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	events := audit.events()
	foundOK := false
	for _, e := range events {
		if e == EventCallbackOK {
			foundOK = true
		}
	}
	if !foundOK {
		t.Errorf("expected EventCallbackOK in audit, got %v", events)
	}
}

func TestAPI_Callback_AuditOnIdPError(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	audit := newFakeAudit()
	o.audit = audit
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-fail&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	state := mustQueryParam(t, w.Header().Get("Location"), "state")

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&error=access_denied", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	events := audit.events()
	foundFail := false
	for _, e := range events {
		if e == EventCallbackFail {
			foundFail = true
		}
	}
	if !foundFail {
		t.Errorf("expected EventCallbackFail in audit, got %v", events)
	}
}

// callback IdP 错误回包(error 参数)应写 "0" 到 authcode + 跳回 return_to。
func TestAPI_Callback_IdPError_WritesZero(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	// 先建 state
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=front-fail&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	state := mustQueryParam(t, w.Header().Get("Location"), "state")

	// callback 带 error
	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&error=access_denied", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w2.Code)
	}
	if got := fakeAC.get("front-fail"); got != "0" {
		t.Errorf("ThirdAuthcode payload = %q, want \"0\"", got)
	}
}

// fakeRevoker 内存版 rtRevoker,记录被吊销的 uid 列表。
type fakeRevoker struct {
	mu    sync.Mutex
	calls []string
	count int64
	err   error
}

func (r *fakeRevoker) RevokeRefreshByUID(uid string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, uid)
	if r.err != nil {
		return 0, r.err
	}
	return r.count, nil
}

// authorize 成功 → audit EventAuthorize 落库,带 IP/UA。
// 用于风控数据面追溯 state 数 / 异常 IP 起步等指标。
func TestAPI_Authorize_AuditsEventAuthorize(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	audit := newFakeAudit()
	o.audit = audit
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-audit&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	events := audit.events()
	foundAuth := false
	for _, e := range events {
		if e == EventAuthorize {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Errorf("expected EventAuthorize in audit, got %v", events)
	}
}

// 已登录 logout:踢全部设备 + 吊销 RT + 审计 EventLogout,返回 200。
func TestAPI_Logout_KicksAndRevokesAndAudits(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	killer := &fakeKiller{}
	revoker := &fakeRevoker{count: 2}
	audit := newFakeAudit()
	o.killer = killer
	o.revoker = revoker
	o.audit = audit

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 模拟 AuthMiddleware 已校验过,把 uid 写入 gin.Context
	r.POST("/v1/auth/oidc/aegis/logout", func(c *gin.Context) {
		c.Set("uid", "u-logout")
		o.logout(wrapWk(c))
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := killer.snapshot(); len(got) != 1 || got[0] != "u-logout" {
		t.Errorf("kicks = %v, want [u-logout]", got)
	}
	revoker.mu.Lock()
	calls := append([]string(nil), revoker.calls...)
	revoker.mu.Unlock()
	if len(calls) != 1 || calls[0] != "u-logout" {
		t.Errorf("revoke calls = %v, want [u-logout]", calls)
	}
	events := audit.events()
	foundLogout := false
	for _, e := range events {
		if e == EventLogout {
			foundLogout = true
		}
	}
	if !foundLogout {
		t.Errorf("expected EventLogout in audit, got %v", events)
	}
}

// logout 只作废当前请求携带的 HTTP token,不影响同 uid 的其他 token。
func TestAPI_Logout_InvalidatesCurrentHTTPTokenOnly(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	c := common.NewMemoryCache()
	const (
		uid          = "u-logout-current"
		currentToken = "tok-current"
		otherToken   = "tok-other"
	)
	if err := c.Set("token:"+currentToken, "u-logout-current@test"); err != nil {
		t.Fatalf("seed current token: %v", err)
	}
	if err := c.Set("token:"+otherToken, "u-logout-current@test"); err != nil {
		t.Fatalf("seed other token: %v", err)
	}
	if err := c.Set("uidtoken:0"+uid, currentToken); err != nil {
		t.Fatalf("seed current uidtoken: %v", err)
	}
	if err := c.Set("uidtoken:1"+uid, otherToken); err != nil {
		t.Fatalf("seed other uidtoken: %v", err)
	}
	o.tokenKill = cacheCurrentTokenInvalidator{
		cache:          c,
		tokenPrefix:    "token:",
		uidTokenPrefix: "uidtoken:",
	}
	o.killer = &fakeKiller{}
	o.revoker = &fakeRevoker{}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/auth/oidc/aegis/logout", func(gc *gin.Context) {
		gc.Set("uid", uid)
		o.logout(wrapWk(gc))
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	req.Header.Set("token", currentToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got, _ := c.Get("token:" + currentToken); got != "" {
		t.Fatalf("current token still cached: %q", got)
	}
	if got, _ := c.Get("uidtoken:0" + uid); got != "" {
		t.Fatalf("current uidtoken index still cached: %q", got)
	}
	if got, _ := c.Get("token:" + otherToken); got == "" {
		t.Fatalf("other token should remain cached")
	}
	if got, _ := c.Get("uidtoken:1" + uid); got != otherToken {
		t.Fatalf("other uidtoken index = %q, want %q", got, otherToken)
	}
}

type raceyCompareDeleter struct {
	cache    *common.MemoryCache
	newToken string
}

func (d raceyCompareDeleter) DeleteIfValue(key, want string) (bool, error) {
	if err := d.cache.Set(key, d.newToken); err != nil {
		return false, err
	}
	got, err := d.cache.Get(key)
	if err != nil {
		return false, err
	}
	if got == want {
		return true, d.cache.Delete(key)
	}
	return false, nil
}

func TestCurrentTokenInvalidator_DoesNotDeleteConcurrentUIDTokenUpdate(t *testing.T) {
	c := common.NewMemoryCache()
	const (
		uid          = "u-race-index"
		currentToken = "tok-current"
		newToken     = "tok-new-login"
	)
	if err := c.Set("token:"+currentToken, "u-race-index@test"); err != nil {
		t.Fatalf("seed current token: %v", err)
	}
	if err := c.Set("uidtoken:1"+uid, currentToken); err != nil {
		t.Fatalf("seed uidtoken: %v", err)
	}
	invalidator := cacheCurrentTokenInvalidator{
		cache:          c,
		tokenPrefix:    "token:",
		uidTokenPrefix: "uidtoken:",
		indexDel:       raceyCompareDeleter{cache: c, newToken: newToken},
	}

	if err := invalidator.InvalidateCurrentToken(context.Background(), uid, currentToken); err != nil {
		t.Fatalf("InvalidateCurrentToken: %v", err)
	}

	if got, _ := c.Get("token:" + currentToken); got != "" {
		t.Fatalf("current token still cached: %q", got)
	}
	if got, _ := c.Get("uidtoken:1" + uid); got != newToken {
		t.Fatalf("concurrent uidtoken update was deleted: got %q, want %q", got, newToken)
	}
}

// 未登录 logout:无 uid → 401,不调踢线/吊销/审计。
func TestAPI_Logout_NoAuth_Rejected(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	killer := &fakeKiller{}
	revoker := &fakeRevoker{}
	o.killer = killer
	o.revoker = revoker

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/auth/oidc/aegis/logout", func(c *gin.Context) {
		// 不 Set uid → 模拟未登录请求绕过 AuthMiddleware 直达 handler
		o.logout(wrapWk(c))
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(killer.kicks) != 0 || len(revoker.calls) != 0 {
		t.Errorf("unauthenticated logout must not call killer/revoker; kicks=%v calls=%v",
			killer.kicks, revoker.calls)
	}
}

// 踢线 / 吊销失败时仍返 200 + 写审计。
// 客户端关心的是"我点了登出,本地状态已清",失败由 SyncWorker 兜底。
func TestAPI_Logout_KickerFailureStillReturnsOK(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.killer = &fakeKiller{err: errors.New("imd down")}
	o.revoker = &fakeRevoker{err: errors.New("db down")}
	audit := newFakeAudit()
	o.audit = audit

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/auth/oidc/aegis/logout", func(c *gin.Context) {
		c.Set("uid", "u-resilient")
		o.logout(wrapWk(c))
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort)", w.Code)
	}
	if len(audit.events()) == 0 {
		t.Errorf("audit must record logout even on downstream failures")
	}
}

// callback 应在 ID Token 缺 email 时调 /userinfo 补全,使 ResolveOrLink
// 能命中已有账号(场景:Aegis 仅在 /userinfo 暴露 email)。
func TestAPI_Callback_BackfillsEmailFromUserInfo(t *testing.T) {
	mp := NewMockProvider(t)
	// ID Token 只有 sub,不暴露 email
	mp.PrepUser("sub-aegis", map[string]interface{}{})
	// /userinfo 暴露 email + email_verified
	mp.PrepUserInfoOnly("sub-aegis", map[string]interface{}{
		"email":          "alice@example.com",
		"email_verified": true,
	})

	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {"u-existing"}},
		loginResp:    &IssueSessionResp{UID: "u-existing", LoginRespJSON: `{"token":"t-1"}`},
	}
	store := newFakeIdentityStore()
	o := newTestOIDC(t, mp, users, store)
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-bf&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-aegis", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("expected 1 IssueSession call, got %d", len(users.loginCalls))
	}
	// 关键断言:UserInfo 拉取的 email 让 ResolveOrLink 命中已有 uid
	if call := users.loginCalls[0]; call.UID != "u-existing" || call.CreateUser {
		t.Errorf("IssueSession call = %+v, want UID=u-existing CreateUser=false (绑定到已有账户)", call)
	}
}

// /userinfo 请求失败时不阻断登录,只是失去自动绑定能力 → 走 AllowNewUser=true 创建新用户。
// 等价于 IdP 没返这些 claim,降级到 ID Token 的有限信息继续走流程。
func TestAPI_Callback_UserInfoFailure_NonBlocking(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-fail", map[string]interface{}{}) // ID Token 仅有 sub
	mp.ForceUserInfoStatus(http.StatusInternalServerError)

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-new", LoginRespJSON: `{"token":"t"}`},
	}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-uifail&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-fail", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if got := w2.Header().Get("Location"); strings.Contains(got, "oidc_error=1") {
		t.Errorf("redirect = %q, expected success (no oidc_error=1)", got)
	}
	if len(users.loginCalls) != 1 || !users.loginCalls[0].CreateUser {
		t.Errorf("expected 1 IssueSession call with CreateUser=true (degraded), got %+v", users.loginCalls)
	}
}

// TestAPI_Callback_TakesOverWithBindEnabled PR4 callback 接管端到端:
//   - AllowNewUser=false 让 ResolveOrLink autolink 全失败时返 ErrUnknownUser
//   - Bind.Enabled=true + issuer 在 allowlist
//   - 应:302 跳 BindRedirectBase, query 含 token + authcode + return_to
//   - 不应:写 LoginRespJSON / "0" 到 ThirdAuthcode
func TestAPI_Callback_TakesOverWithBindEnabled(t *testing.T) {
	mp := NewMockProvider(t)
	// sub-newcomer 没有任何 dmwork 用户 + email/phone 也不命中 → autolink 全失败
	mp.PrepUser("sub-newcomer", map[string]interface{}{
		"email":          "nobody@example.com",
		"email_verified": true,
		"name":           "Newcomer",
	})

	users := &fakeUserLookup{} // 默认空 byEmail/byPhone
	store := newFakeIdentityStore()
	o := newTestOIDC(t, mp, users, store)
	// 必须 AllowNewUser=false 才会 returnEr UnknownUser(否则 AllowNewUser=true 路径建新用户)。
	// newTestOIDC 在构造 service 时按值捕获 cfg.Provider,所以这里改完要重建 service。
	o.cfg.Provider.AllowNewUser = false
	o.service = newService(o.cfg.Provider, store, users)
	// 装 bind + 配置
	o.cfg.Bind = BindConfig{
		Enabled:         true,
		IssuerAllowlist: []string{mp.Issuer},
		TokenTTL:        time.Minute,
		VerifyMax:       5, OTPSendMax: 3, ConfirmMax: 3, UIDFailPerDay: 10,
		Methods:      []BindMethod{BindMethodPassword, BindMethodSMSOTP},
		RedirectBase: "https://im.example.com/oidc/bind",
	}
	o.bind = newBindService(o.cfg.Bind, newMemoryBindStore(), &fakeBindAuth{}, &fakeBindLocator{
		byUsername: map[string]string{}, byPhone: map[string][]string{},
	})
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/authorize?authcode=front-bind&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-newcomer", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w2.Code, w2.Body.String())
	}
	loc := w2.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://im.example.com/oidc/bind?") {
		t.Fatalf("expected redirect to bind page, got %q", loc)
	}
	u, _ := url.Parse(loc)
	q := u.Query()
	if q.Get("token") == "" {
		t.Errorf("redirect missing token param: %q", loc)
	}
	if q.Get("authcode") != "front-bind" {
		t.Errorf("redirect authcode=%q want front-bind", q.Get("authcode"))
	}
	// 前端从 query 取 provider 拼回 /v1/auth/oidc/<provider>/bind/* API URL。
	if q.Get("provider") != "aegis" {
		t.Errorf("redirect provider=%q want aegis", q.Get("provider"))
	}
	// Referrer-Policy: no-referrer 防止 callback URL (含 authcode) 经 Referer 泄漏。
	if got := w2.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy=%q want no-referrer", got)
	}
	// LoginRespJSON / "0" 都不该出现 —— bind 还没完成
	if got := fakeAC.get("front-bind"); got != "" {
		t.Errorf("ThirdAuthcode must NOT be set during bind takeover, got %q", got)
	}
	if len(users.loginCalls) != 0 {
		t.Errorf("IssueSession must NOT be called during bind takeover, got %d calls", len(users.loginCalls))
	}
}

// TestAPI_Callback_BindFlagOffPreservesLegacy NFR-6 可回滚:Bind.Enabled=false
// 时 callback 行为与 PR3 之前完全一致 —— autolink 失败仍走 failWithAuthcode("0"),
// 不会触发任何 bind 逻辑。
func TestAPI_Callback_BindFlagOffPreservesLegacy(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-newcomer", map[string]interface{}{
		"email": "nobody@example.com", "email_verified": true,
	})
	users := &fakeUserLookup{}
	store := newFakeIdentityStore()
	o := newTestOIDC(t, mp, users, store)
	o.cfg.Provider.AllowNewUser = false
	o.service = newService(o.cfg.Provider, store, users)
	o.cfg.Bind = BindConfig{Enabled: false} // 显式关
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/authorize?authcode=ac-legacy&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-newcomer", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// 旧行为:回 return_to + oidc_error=1
	if loc := w2.Header().Get("Location"); !strings.Contains(loc, "oidc_error=1") {
		t.Fatalf("flag off must preserve legacy fail path, redirect=%q", loc)
	}
	if got := fakeAC.get("ac-legacy"); got != "0" {
		t.Fatalf("flag off must write \"0\" to ThirdAuthcode, got %q", got)
	}
}

// TestAPI_Callback_BindIssuerNotAllowed Bind.Enabled=true 但 issuer 不在 allowlist
// 时,callback 仍走旧失败路径(灰度精确控制)。
func TestAPI_Callback_BindIssuerNotAllowed(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-newcomer", map[string]interface{}{
		"email": "nobody@example.com", "email_verified": true,
	})
	users := &fakeUserLookup{}
	store := newFakeIdentityStore()
	o := newTestOIDC(t, mp, users, store)
	o.cfg.Provider.AllowNewUser = false
	o.service = newService(o.cfg.Provider, store, users)
	// issuer = mp.Issuer,但 allowlist 故意只放 "https://other"
	o.cfg.Bind = BindConfig{
		Enabled:         true,
		IssuerAllowlist: []string{"https://other"},
		TokenTTL:        time.Minute,
		RedirectBase:    "https://im.example.com/oidc/bind",
		Methods:         []BindMethod{BindMethodPassword},
	}
	o.bind = newBindService(o.cfg.Bind, newMemoryBindStore(), &fakeBindAuth{}, &fakeBindLocator{})
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC
	r := newTestRouter(o)

	req := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/authorize?authcode=ac-deny&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-newcomer", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if loc := w2.Header().Get("Location"); !strings.Contains(loc, "oidc_error=1") {
		t.Fatalf("issuer not in allowlist must use legacy fail, redirect=%q", loc)
	}
	if got := fakeAC.get("ac-deny"); got != "0" {
		t.Fatalf("ThirdAuthcode must be \"0\", got %q", got)
	}
}

// /userinfo 返回 sub 与 ID Token sub 不一致,视为账号串台,直接拒绝登录。
// 这是关键安全分支:防止 confused deputy 攻击。
func TestAPI_Callback_UserInfoSubMismatch_Rejects(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-A", map[string]interface{}{}) // ID Token sub=sub-A
	// userinfo 返回的 sub 故意篡改为 sub-EVIL
	mp.PrepUserInfoOnly("sub-A", map[string]interface{}{
		"sub":   "sub-EVIL",
		"email": "victim@example.com",
	})

	users := &fakeUserLookup{}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	audit := newFakeAudit()
	o.audit = audit
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-mismatch&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-A", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// 应 302 回 return_to 但带 oidc_error=1,且不调 IssueSession
	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if loc := w2.Header().Get("Location"); !strings.Contains(loc, "oidc_error=1") {
		t.Errorf("redirect = %q, want oidc_error=1 (rejected)", loc)
	}
	if len(users.loginCalls) != 0 {
		t.Errorf("expected 0 IssueSession calls (rejected before login), got %d", len(users.loginCalls))
	}
	// 应有 callback_fail 审计记录
	foundFail := false
	for _, e := range audit.events() {
		if e == EventCallbackFail {
			foundFail = true
		}
	}
	if !foundFail {
		t.Errorf("expected EventCallbackFail in audit, got %v", audit.events())
	}
}

// failWithAuthcode 对 long subject 的审计 uid 应截断到 maxAuditUID 长度,
// 防止超过 oidc_audit_log.uid VARCHAR(64) 导致 INSERT 失败。
func TestFailWithAuthcode_LongSubject_TruncatesAuditUID(t *testing.T) {
	o := &OIDC{
		Log:      log.NewTLog("OIDC-test"),
		audit:    newFakeAudit(),
		authcode: newFakeAuthcode(),
	}
	longSub := strings.Repeat("A", 100)
	claims := &IDTokenClaims{Subject: longSub}
	sd := &StateData{ClientAuthcode: "ac-long-sub"}

	o.failWithAuthcode(context.Background(), sd, claims, errors.New("test error"))

	audit := o.audit.(*fakeAudit)
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	uid := audit.entries[0].UID
	if len(uid) > maxAuditUID {
		t.Errorf("audit uid length = %d, want <= %d; uid = %q", len(uid), maxAuditUID, uid)
	}
	if !strings.HasPrefix(uid, "sub:") {
		t.Errorf("audit uid should start with 'sub:', got %q", uid)
	}
}

// callback 应在 ID Token 缺 name 时调 /userinfo 补全 name,避免新建用户时
// 落到 createUserWithRespAndTx 的随机汉名兜底分支(issue #1307)。
// 场景:OCTO 等 IdP 把 name 仅放在 /userinfo,ID Token 只含 sub/email/phone。
func TestAPI_Callback_BackfillsNameFromUserInfo(t *testing.T) {
	mp := NewMockProvider(t)
	// ID Token 含 email 但不含 name(模拟 OCTO 行为)
	mp.PrepUser("sub-octo", map[string]interface{}{
		"email":          "bob@example.com",
		"email_verified": true,
	})
	// /userinfo 暴露 name
	mp.PrepUserInfoOnly("sub-octo", map[string]interface{}{
		"name": "Bob Real Name",
	})

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-new", IsNewUser: true, LoginRespJSON: `{"token":"t"}`},
	}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-name&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-octo", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("expected 1 IssueSession call, got %d", len(users.loginCalls))
	}
	if got := users.loginCalls[0].Name; got != "Bob Real Name" {
		t.Errorf("IssueSession.Name = %q, want %q (userinfo.name 应回填到 claims.Name)", got, "Bob Real Name")
	}
}

// ID Token 已含 name 时,即便 /userinfo 也返 name,以 ID Token 为准
// (ID Token 是已签名权威源;userinfo 仅在 ID Token 缺字段时补全)。
func TestAPI_Callback_IDTokenNameWinsOverUserInfo(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-both", map[string]interface{}{
		"email": "carol@example.com",
		"name":  "Carol From IDToken",
	})
	mp.PrepUserInfoOnly("sub-both", map[string]interface{}{
		"name": "Carol From UserInfo",
	})
	// 注意:ID Token 已经有 email + name,触发 userinfo 拉取需要 phone 缺失
	// 这里 PhoneNumber 默认空,会触发 userinfo,但 claims.Name 已非空,不应被覆盖。

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-c", IsNewUser: true, LoginRespJSON: `{"token":"t"}`},
	}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-both&return_to=/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-both", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if len(users.loginCalls) != 1 {
		t.Fatalf("expected 1 IssueSession call, got %d", len(users.loginCalls))
	}
	if got := users.loginCalls[0].Name; got != "Carol From IDToken" {
		t.Errorf("IssueSession.Name = %q, want ID Token's %q (ID Token 优先)", got, "Carol From IDToken")
	}
}

func mustQueryParam(t *testing.T, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	v := u.Query().Get(name)
	if v == "" {
		t.Fatalf("missing query %q in %q", name, rawURL)
	}
	return v
}
