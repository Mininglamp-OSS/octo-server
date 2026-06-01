package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gin-gonic/gin"
)

// fakeIDTokenStore 内存版 id_token_hint 存取,断言 logout 拼 URL 与一次性消费。
type fakeIDTokenStore struct {
	mu      sync.Mutex
	tokens  map[string]string
	saveErr error
	takeErr error
}

func newFakeIDTokenStore() *fakeIDTokenStore {
	return &fakeIDTokenStore{tokens: make(map[string]string)}
}

func (f *fakeIDTokenStore) Save(_ context.Context, uid, idToken string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.tokens[uid] = idToken
	return nil
}

func (f *fakeIDTokenStore) Take(_ context.Context, uid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.takeErr != nil {
		return "", f.takeErr
	}
	v := f.tokens[uid]
	delete(f.tokens, uid)
	return v, nil
}

func (f *fakeIDTokenStore) get(uid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[uid]
}

// runLogout 模拟 AuthMiddleware 已校验,把 uid 注入 gin.Context 后调 logout handler。
func runLogout(o *OIDC, uid string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/auth/oidc/aegis/logout", func(c *gin.Context) {
		if uid != "" {
			c.Set("uid", uid)
		}
		o.logout(wrapWk(c))
	})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeLogoutBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode logout body %q: %v", w.Body.String(), err)
	}
	return m
}

// 配置齐全(回跳地址 + end_session 端点 + 存有 id_token)时,logout 应在 200 响应里
// 返回 end_session_url,带 id_token_hint + post_logout_redirect_uri,且 id_token 被消费。
func TestAPI_Logout_ReturnsEndSessionURL(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.killer = &fakeKiller{}
	o.revoker = &fakeRevoker{}
	ids := newFakeIDTokenStore()
	ids.tokens["u-1"] = "raw-id-token-xyz"
	o.idTokens = ids
	o.cfg.Provider.PostLogoutRedirectURI = "https://app.example.com/login"
	// MockProvider 的 end_session 端点是 http(httptest),dev 放宽位允许 http。
	t.Setenv("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", "1")

	w := runLogout(o, "u-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	raw, _ := decodeLogoutBody(t, w)["end_session_url"].(string)
	if raw == "" {
		t.Fatalf("missing end_session_url; body=%s", w.Body.String())
	}
	if !strings.HasPrefix(raw, mp.Issuer+"/end_session") {
		t.Errorf("end_session_url = %q, want prefix %q", raw, mp.Issuer+"/end_session")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse end_session_url: %v", err)
	}
	if got := u.Query().Get("id_token_hint"); got != "raw-id-token-xyz" {
		t.Errorf("id_token_hint = %q, want raw-id-token-xyz", got)
	}
	if got := u.Query().Get("post_logout_redirect_uri"); got != "https://app.example.com/login" {
		t.Errorf("post_logout_redirect_uri = %q", got)
	}
	if ids.get("u-1") != "" {
		t.Errorf("id_token should be consumed (one-time) after logout")
	}
	// 含 id_token_hint 的响应必须禁缓存。
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// 没有存 id_token(非 OIDC 登录 / 已过期)时,logout 仍 200 + 踢线 + 吊销,
// 但不返回 end_session_url —— 前端据此降级为仅清本地。
func TestAPI_Logout_NoIDToken_OmitsURL(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	killer := &fakeKiller{}
	revoker := &fakeRevoker{}
	o.killer = killer
	o.revoker = revoker
	o.idTokens = newFakeIDTokenStore() // 空
	o.cfg.Provider.PostLogoutRedirectURI = "https://app.example.com/login"
	t.Setenv("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", "1") // 让流程走到 Take(端点是 http mock)

	w := runLogout(o, "u-2")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := decodeLogoutBody(t, w)["end_session_url"]; ok {
		t.Errorf("end_session_url should be omitted when no id_token stored")
	}
	if got := killer.snapshot(); len(got) != 1 || got[0] != "u-2" {
		t.Errorf("kick should still happen, got %v", got)
	}
}

// 未配置 post_logout_redirect_uri 时,即便有 id_token 也不返回 end_session_url。
func TestAPI_Logout_NoRedirectConfig_OmitsURL(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.killer = &fakeKiller{}
	o.revoker = &fakeRevoker{}
	ids := newFakeIDTokenStore()
	ids.tokens["u-3"] = "tok"
	o.idTokens = ids
	// PostLogoutRedirectURI 默认空

	w := runLogout(o, "u-3")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := decodeLogoutBody(t, w)["end_session_url"]; ok {
		t.Errorf("end_session_url should be omitted when redirect URI not configured")
	}
}

// idTokens 未启用(nil,缺省未配 PostLogoutRedirectURI)时,logout 仍 200 且不返回
// end_session_url —— 守住 #1 修复的降级路径。
func TestAPI_Logout_NilStore_OmitsURL(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.killer = &fakeKiller{}
	o.revoker = &fakeRevoker{}
	o.idTokens = nil // 功能禁用
	o.cfg.Provider.PostLogoutRedirectURI = "https://app.example.com/login"

	w := runLogout(o, "u-x")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := decodeLogoutBody(t, w)["end_session_url"]; ok {
		t.Errorf("end_session_url must be omitted when id_token store disabled")
	}
}

// 取 id_token 出错(Redis 抖动)时,logout 仍 200 且省略 end_session_url —— 守住
// "best-effort 降级"契约的 Take 错误分支。
func TestAPI_Logout_TakeError_OmitsURL(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	o.killer = &fakeKiller{}
	o.revoker = &fakeRevoker{}
	ids := newFakeIDTokenStore()
	ids.tokens["u-e"] = "tok"
	ids.takeErr = errors.New("redis down")
	o.idTokens = ids
	o.cfg.Provider.PostLogoutRedirectURI = "https://app.example.com/login"
	t.Setenv("OCTO_OIDC_LOGOUT_ALLOW_INSECURE", "1") // 让流程走到 Take 才能触发 takeErr

	w := runLogout(o, "u-e")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort)", w.Code)
	}
	if _, ok := decodeLogoutBody(t, w)["end_session_url"]; ok {
		t.Errorf("end_session_url must be omitted when Take errors")
	}
}

// 缓存 id_token 出错(Save 失败)时,callback 仍完成登录(302)—— 守住 Save 错误分支
// 不阻断登录的契约。
func TestAPI_Callback_IDTokenSaveError_LoginStillSucceeds(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-se", map[string]interface{}{
		"email": "se@example.com", "email_verified": true, "name": "SE",
	})
	users := &fakeUserLookup{loginResp: &IssueSessionResp{UID: "u-se", LoginRespJSON: `{"token":"t-se"}`}}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{UID: "u-se", Issuer: mp.Issuer, Subject: "sub-se"})
	o := newTestOIDC(t, mp, users, store)
	ids := newFakeIDTokenStore()
	ids.saveErr = errors.New("redis down")
	o.idTokens = ids
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-se&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("code-se", "sub-se", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/callback?state="+state+"&code=code-se", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("login must still succeed despite id_token save error; status=%d body=%s", w2.Code, w2.Body.String())
	}
	if ids.get("u-se") != "" {
		t.Errorf("save failed, so nothing should be stored for uid")
	}
}

// 自助绑定接管(callback bind_pending)时,应按 bind token(jti)暂存 id_token,
// 供 confirm/create 后迁移到 uid。校验暂存键 = bindIDTokenKey(redirect 的 token 参数)。
func TestAPI_Callback_BindPending_StoresIDTokenUnderBindKey(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-newcomer", map[string]interface{}{
		"email": "nobody@example.com", "email_verified": true, "name": "Newcomer",
	})
	users := &fakeUserLookup{}
	store := newFakeIdentityStore()
	o := newTestOIDC(t, mp, users, store)
	o.cfg.Provider.AllowNewUser = false
	o.service = newService(o.cfg.Provider, store, users)
	o.cfg.Bind = BindConfig{
		Enabled: true, IssuerAllowlist: []string{mp.Issuer}, TokenTTL: time.Minute,
		VerifyMax: 5, OTPSendMax: 3, ConfirmMax: 3, UIDFailPerDay: 10,
		Methods:      []BindMethod{BindMethodPassword, BindMethodSMSOTP},
		RedirectBase: "https://im.example.com/oidc/bind",
	}
	o.bind = newBindService(o.cfg.Bind, newMemoryBindStore(), &fakeBindAuth{}, &fakeBindLocator{
		byUsername: map[string]string{}, byPhone: map[string][]string{},
	})
	ids := newFakeIDTokenStore()
	o.idTokens = ids
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=front-bind&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("idp-code", "sub-newcomer", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/callback?state="+state+"&code=idp-code", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", w2.Code, w2.Body.String())
	}
	loc, _ := url.Parse(w2.Header().Get("Location"))
	jti := loc.Query().Get("token")
	if jti == "" {
		t.Fatalf("bind redirect missing token; loc=%s", w2.Header().Get("Location"))
	}
	if got := ids.get(bindIDTokenKey(jti)); got == "" {
		t.Errorf("id_token must be stashed under bind key %q during bind_pending", bindIDTokenKey(jti))
	}
	// 还没确定 uid,不应有 uid 键
	if ids.get("u-newcomer") != "" {
		t.Errorf("must not store under uid before confirm/create")
	}
}

// callback 成功后应把验签过的 id_token 存进 idTokenStore,供后续 RP-Initiated Logout
// 取用作 id_token_hint。存储失败不阻断登录(best-effort),此处只断言成功路径存了。
func TestAPI_Callback_StoresIDTokenForLogout(t *testing.T) {
	mp := NewMockProvider(t)
	mp.PrepUser("sub-LT", map[string]interface{}{
		"email":          "lt@example.com",
		"email_verified": true,
		"name":           "LT",
	})
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-lt", LoginRespJSON: `{"token":"t-lt"}`},
	}
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{UID: "u-lt", Issuer: mp.Issuer, Subject: "sub-LT"})

	o := newTestOIDC(t, mp, users, store)
	ids := newFakeIDTokenStore()
	o.idTokens = ids
	r := newTestRouter(o)

	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode=ac-lt&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	authURL, _ := url.Parse(w.Header().Get("Location"))
	state := authURL.Query().Get("state")
	mp.PrepCode("code-lt", "sub-LT", authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/callback?state="+state+"&code=code-lt", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if ids.get("u-lt") == "" {
		t.Errorf("expected id_token stored for uid u-lt after successful callback")
	}
}

// buildEndSessionURL 直接单测:EndSessionURL override 生效;已带 query 的端点要保留;
// id_token_hint / post_logout_redirect_uri 中的特殊字符要正确转义。
func TestBuildEndSessionURL_OverrideAndEscaping(t *testing.T) {
	ids := newFakeIDTokenStore()
	ids.tokens["u"] = "tok with space&amp"
	o := &OIDC{
		Log:      log.NewTLog("OIDC-test"),
		idTokens: ids,
		cfg: &Config{Provider: ProviderConfig{
			EndSessionURL:         "https://idp.example.com/end?foo=bar",
			PostLogoutRedirectURI: "https://app.example.com/login?next=/x y",
		}},
	}
	got := o.buildEndSessionURL(context.Background(), "u")
	if got == "" {
		t.Fatal("buildEndSessionURL returned empty")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("foo") != "bar" {
		t.Errorf("pre-existing query foo=bar lost: %q", got)
	}
	if q.Get("id_token_hint") != "tok with space&amp" {
		t.Errorf("id_token_hint not round-tripped: %q", q.Get("id_token_hint"))
	}
	if q.Get("post_logout_redirect_uri") != "https://app.example.com/login?next=/x y" {
		t.Errorf("post_logout_redirect_uri not round-tripped: %q", q.Get("post_logout_redirect_uri"))
	}
}
