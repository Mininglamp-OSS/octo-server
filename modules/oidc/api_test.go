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
		Aegis: ProviderConfig{
			Issuer:               mp.Issuer,
			ClientID:             mp.ClientID,
			RedirectURI:          "https://app.example.com/callback",
			AutoLinkByEmail:      true,
			AllowNewUser:         true,
			RequireEmailVerified: true,
			ReturnToHosts:        []string{"app.example.com"},
		},
	}
	return &OIDC{
		Log:        log.NewTLog("OIDC-test"),
		cfg:        cfg,
		client:     client,
		service:    newService(cfg.Aegis, store, users),
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
//   1. 立刻补写 "0" 让前端轮询尽早感知
//   2. redirect URL 拼 ?oidc_error=1
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

// logout 当前 P1.2 仅返回 200。
func TestAPI_Logout_OK(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	r := newTestRouter(o)

	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
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
