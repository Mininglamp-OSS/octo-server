package oidc

// api_exchange_test.go — tests for POST /v1/auth/oidc/<id>/exchange.
//
// The exchange endpoint is the non-browser client SSO entry: the client has already
// completed SSO with the IdP and holds an IdP access_token; it POSTs that token
// to us and gets back our own session token. Unlike /callback (which is a
// browser redirect that hands the result off via ThirdAuthcode polling),
// /exchange returns the session JSON directly.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gin-gonic/gin"
)

// newTestOIDCForExchange builds an OIDC instance wired with a caller-controlled
// fake AuthProvider. It bypasses New() so no real Discovery / config is needed
// — these are unit tests for the handler, not the provider implementation.
func newTestOIDCForExchange(t *testing.T, prov AuthProvider, users *fakeUserLookup, store *fakeIdentityStore) *OIDC {
	t.Helper()
	cfg := &Config{
		Enabled: true,
		Provider: ProviderConfig{
			ID:           "sso",
			Name:         "SSO",
			Issuer:       prov.Issuer(),
			ClientID:     "test-cid",
			RedirectURI:  "https://app.example.com/callback",
			AllowNewUser: true,
			Scopes:       []string{"read"},
		},
	}
	return &OIDC{
		Log:        log.NewTLog("OIDC-test"),
		cfg:        cfg,
		provider:   prov,
		service:    newService(cfg.Provider, store, users),
		store:      store,
		stateStore: newMemoryStateStore(),
		authcode:   newFakeAuthcode(),
		audit:      newFakeAudit(),
	}
}

// postExchange fires POST /exchange against a gin engine that mounts only the
// exchange handler. Mirrors newTestRouter: handler takes *wkhttp.Context, so
// tests wrap gin.Context via wrapWk (defined in api_test.go).
func postExchange(o *OIDC, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/exchange", func(c *gin.Context) { o.exchange(wrapWk(c)) })
	req := httptest.NewRequest(http.MethodPost, "/exchange", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- fakeProvider ----------------------------------------------------------
//
// Captures the TokenSet passed to Identity and returns pre-programmed
// claims/errors.

type fakeProvider struct {
	issuer     string
	caps       ProviderCapabilities
	identityFn func(ctx context.Context, tok *TokenSet) (*IdentityClaims, error)
	lastTok    *TokenSet
	callN      int64
}

func (f *fakeProvider) Kind() ProviderKind                 { return KindOAuth2 }
func (f *fakeProvider) Capabilities() ProviderCapabilities { return f.caps }
func (f *fakeProvider) Issuer() string                     { return f.issuer }
func (f *fakeProvider) AuthCodeURL(p AuthCodeParams) (string, error) {
	return "", errors.New("not used")
}
func (f *fakeProvider) Exchange(ctx context.Context, code, cv string) (*TokenSet, error) {
	return nil, errors.New("not used")
}
func (f *fakeProvider) LogoutURL(ctx context.Context, hint LogoutHint) (string, bool) {
	return "", false
}

// IdentityFromClientCredential 复用 Identity —— fake 不区分凭据来源,只区分
// "被问了几次"和"返回什么",这两件事对两个入口是一样的。
func (f *fakeProvider) IdentityFromClientCredential(ctx context.Context, raw string) (*IdentityClaims, error) {
	return f.Identity(ctx, &TokenSet{AccessToken: raw})
}

func (f *fakeProvider) Identity(ctx context.Context, tok *TokenSet) (*IdentityClaims, error) {
	atomic.AddInt64(&f.callN, 1)
	f.lastTok = tok
	if f.identityFn != nil {
		return f.identityFn(ctx, tok)
	}
	return &IdentityClaims{Issuer: f.issuer, Subject: "default-sub", Name: "Default", Email: "d@e.com"}, nil
}

var _ AuthProvider = (*fakeProvider)(nil)

func defaultExchangeProvider() *fakeProvider {
	return &fakeProvider{issuer: "test-idp", caps: ProviderCapabilities{UpstreamLogout: true}}
}

// ---- exchange fakes --------------------------------------------------------
//
// The package already has fakeUserLookup / fakeIdentityStore in service_test.go
// (with a loginResp / loginErr hook). These helpers configure them for the
// greenfield happy path (AllowNewUser=true, CreateUser=true, a valid
// LoginRespJSON so we can assert the token comes back).

func newExchangeUserFake() *fakeUserLookup {
	return &fakeUserLookup{
		usersByEmail: map[string][]string{},
		usersByPhone: map[string][]string{},
		loginResp: &IssueSessionResp{
			UID:           "uid-alice",
			IsNewUser:     true,
			LoginRespJSON: `{"token":"sess-alice","uid":"uid-alice"}`,
		},
	}
}

// ---- RED tests (expect compile failure or 404 until handler exists) --------

func TestExchange_HandlerExists(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForExchange(t, defaultExchangeProvider(), users, store)

	w := postExchange(o, `{"access_token":"good"}`)
	// Before implementation this is 404; after it must be 200 for a valid token.
	if w.Code == http.StatusNotFound {
		t.Fatalf("POST /exchange returned 404 — handler is not mounted yet")
	}
}

// ---- request validation ----------------------------------------------------

func TestExchange_MissingBody(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForExchange(t, defaultExchangeProvider(), users, store)

	w := postExchange(o, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty body: want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExchange_BadJSON(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForExchange(t, defaultExchangeProvider(), users, store)

	w := postExchange(o, "{not-json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExchange_BlankAccessToken(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	o := newTestOIDCForExchange(t, defaultExchangeProvider(), users, store)

	cases := []string{`{}`, `{"access_token":""}`, `{"access_token":"   "}`}
	for _, body := range cases {
		w := postExchange(o, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: want 400, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

// ---- anti-enumeration: IdP failures collapse to a single generic 401 -------

func TestExchange_IdPRejectsToken(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	prov := defaultExchangeProvider()
	prov.identityFn = func(_ context.Context, _ *TokenSet) (*IdentityClaims, error) {
		return nil, ErrIdentityUntrusted
	}
	o := newTestOIDCForExchange(t, prov, users, store)

	w := postExchange(o, `{"access_token":"bad"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("IdP reject: want 401, got %d: %s", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt64(&prov.callN); n != 1 {
		t.Errorf("Identity called %d times, want 1 (no retry storm)", n)
	}
	// Body must not leak the upstream error verbatim (which could carry PII or
	// help token probing). We don't pin the exact message here — the i18n
	// envelope carries a generic code; just ensure no word from the wrapped
	// upstream error leaks out as-is.
	if strings.Contains(w.Body.String(), "identity could not be established") {
		t.Errorf("response body leaks upstream error text: %s", w.Body.String())
	}
}

// ---- happy path returns session JSON ---------------------------------------

func TestExchange_HappyPathReturnsSession(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	prov := defaultExchangeProvider()
	prov.identityFn = func(_ context.Context, tok *TokenSet) (*IdentityClaims, error) {
		if tok.AccessToken != "good-token" {
			t.Errorf("Identity got access_token=%q, want %q", tok.AccessToken, "good-token")
		}
		return &IdentityClaims{
			Issuer:  prov.issuer,
			Subject: "alice-sub",
			Name:    "Alice",
			Email:   "alice@example.com",
		}, nil
	}
	o := newTestOIDCForExchange(t, prov, users, store)

	w := postExchange(o, `{"access_token":"good-token","flag":0}`)
	if w.Code != http.StatusOK {
		t.Fatalf("happy: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Status string          `json:"status"`
		UID    string          `json:"uid"`
		Login  json.RawMessage `json:"login_resp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v, body=%s", err, w.Body.String())
	}
	if resp.Status != "ok" {
		t.Errorf("status=%q, want ok", resp.Status)
	}
	if resp.UID == "" {
		t.Error("uid empty, want non-empty")
	}
	// login_resp is the serialized JSON string from user.IssueSession.
	var inner string
	if err := json.Unmarshal(resp.Login, &inner); err != nil {
		t.Fatalf("login_resp not a JSON string: %s", resp.Login)
	}
	if !strings.Contains(inner, `"token"`) {
		t.Errorf("login_resp inner JSON missing token: %s", inner)
	}
}

// ---- public route (no AuthMiddleware) --------------------------------------

func TestExchange_RouteIsPublic(t *testing.T) {
	users := newExchangeUserFake()
	store := newFakeIdentityStore()
	prov := defaultExchangeProvider()
	o := newTestOIDCForExchange(t, prov, users, store)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	pub := r.Group("/v1/auth/oidc/" + o.cfg.Provider.ID)
	pub.POST("/exchange", func(c *gin.Context) { o.exchange(wrapWk(c)) })

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/oidc/sso/exchange",
		strings.NewReader(`{"access_token":"good"}`))
	req.Header.Set("Content-Type", "application/json")
	// No `token` header at all — must still reach the handler (return 200).
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized && strings.Contains(w.Body.String(), "token_missing") {
		t.Fatal("exchange is behind AuthMiddleware (got token_missing); must be public")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on unauthenticated POST, got %d: %s", w.Code, w.Body.String())
	}
}
