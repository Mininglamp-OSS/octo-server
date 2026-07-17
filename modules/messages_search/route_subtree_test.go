package messages_search

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/searchbackend"
	"github.com/gin-gonic/gin"
)

// YUJ-49 (#B) — the entry-wiring layer that exposes the shared _search* handler
// to the bot / uk route trees, resolving the correct principal at the route
// edge. These tests cover the exported principal resolvers (三态区分) and
// MountSubtree (endpoint coverage + the searchRateLimiter → audit → backendGate
// chain fronted by a caller-supplied resolver).

func newResolveCtx(t *testing.T) *wkhttp.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/x", nil)
	return &wkhttp.Context{Context: gc}
}

func TestAuthenticateUserBot_UserBotAppBotAndMissing(t *testing.T) {
	// User Bot: subject = botUID, no space, blacklist not consulted, all
	// metering/audit keyed on botUID.
	c := newResolveCtx(t)
	c.Set(ctxKeyRobotID, "bot_1")
	p, err := AuthenticateUserBot(c)
	if err != nil {
		t.Fatalf("user bot: unexpected err %v", err)
	}
	if p.Kind() != principalKindUserBot {
		t.Fatalf("kind = %v, want user_bot", p.Kind())
	}
	if p.SubjectUID() != "bot_1" || p.RateLimitKey() != "bot_1" || p.AuditBotUID() != "bot_1" {
		t.Fatalf("subject/ratelimit/audit must all be botUID, got %+v", p)
	}
	if p.SpaceID() != "" {
		t.Fatalf("bot spaceID must be empty, got %q", p.SpaceID())
	}
	if p.BlacklistPolicy() != blacklistNone {
		t.Fatalf("bot blacklist policy must be none, got %v", p.BlacklistPolicy())
	}
	if p.AuditGrantorUID() != "" {
		t.Fatalf("bot audit grantor must be empty, got %q", p.AuditGrantorUID())
	}

	// App Bot: explicitly denied (一期不做 App Bot，决策五).
	appc := newResolveCtx(t)
	appc.Set(ctxKeyRobotID, "app_1")
	appc.Set(ctxKeyBotKind, botKindApp)
	if _, err := AuthenticateUserBot(appc); !errors.Is(err, ErrAppBotSearchDenied) {
		t.Fatalf("app bot must be denied, got %v", err)
	}

	// No robot_id in context → unauthenticated (fail-closed).
	if _, err := AuthenticateUserBot(newResolveCtx(t)); !errors.Is(err, ErrPrincipalUnauthenticated) {
		t.Fatalf("missing robot_id must be unauthenticated, got %v", err)
	}
}

func TestAuthenticateUK_DirectRealUser(t *testing.T) {
	c := newResolveCtx(t)
	c.Set(ctxKeyAPIKeyUID, "u_9")
	c.Set(ctxKeyAPIKeySpaceID, "sp_1")
	p, err := AuthenticateUK(c)
	if err != nil {
		t.Fatalf("uk: unexpected err %v", err)
	}
	if p.Kind() != principalKindUK {
		t.Fatalf("kind = %v, want uk", p.Kind())
	}
	if p.SubjectUID() != "u_9" || p.RateLimitKey() != "u_9" {
		t.Fatalf("uk subject/ratelimit must be key UID, got %+v", p)
	}
	if p.SpaceID() != "sp_1" {
		t.Fatalf("uk spaceID must be api_key_space_id, got %q", p.SpaceID())
	}
	if p.BlacklistPolicy() != blacklistRealUserBidirectional {
		t.Fatalf("uk blacklist must be real-user bidirectional, got %v", p.BlacklistPolicy())
	}
	if p.AuditBotUID() != "" || p.AuditGrantorUID() != "" {
		t.Fatalf("uk has no bot/grantor audit fields, got %+v", p)
	}

	if _, err := AuthenticateUK(newResolveCtx(t)); !errors.Is(err, ErrPrincipalUnauthenticated) {
		t.Fatalf("missing api_key_uid must be unauthenticated, got %v", err)
	}
}

func TestNewOBOPrincipal_GrantorSubjectBotMetering(t *testing.T) {
	p, err := NewOBOPrincipal("bot_1", "grantor_2", "sp_3")
	if err != nil {
		t.Fatalf("obo: unexpected err %v", err)
	}
	if p.Kind() != principalKindOBO {
		t.Fatalf("kind = %v, want obo", p.Kind())
	}
	if p.SubjectUID() != "grantor_2" {
		t.Fatalf("obo subject must be grantor, got %q", p.SubjectUID())
	}
	if p.RateLimitKey() != "bot_1" {
		t.Fatalf("obo rate-limit key must be botUID (防单 bot 打爆), got %q", p.RateLimitKey())
	}
	if p.AuditBotUID() != "bot_1" || p.AuditGrantorUID() != "grantor_2" {
		t.Fatalf("obo audit must record bot+grantor, got %+v", p)
	}
	if p.SpaceID() != "sp_3" {
		t.Fatalf("obo spaceID must follow grantor, got %q", p.SpaceID())
	}
	if p.BlacklistPolicy() != blacklistRealUserBidirectional {
		t.Fatalf("obo blacklist must be real-user bidirectional, got %v", p.BlacklistPolicy())
	}

	// Self-grant and empty inputs fail closed (与 checkOBO 一致).
	if _, err := NewOBOPrincipal("x", "x", ""); !errors.Is(err, ErrPrincipalUnauthenticated) {
		t.Fatalf("self-grant must fail closed, got %v", err)
	}
	if _, err := NewOBOPrincipal("bot", "", ""); !errors.Is(err, ErrPrincipalUnauthenticated) {
		t.Fatalf("empty grantor must fail closed, got %v", err)
	}
}

func TestSetPrincipalAndPrincipalKind(t *testing.T) {
	c := newResolveCtx(t)
	if got := PrincipalKind(c); got != "" {
		t.Fatalf("no principal yet, PrincipalKind = %q, want empty", got)
	}
	SetPrincipal(c, userBotPrincipal{botUID: "b"})
	if got := PrincipalKind(c); got != "user_bot" {
		t.Fatalf("PrincipalKind = %q, want user_bot", got)
	}
}

// newSubtreeTestHandler builds a Handler wired just enough to exercise the
// MountSubtree chain (searchRateLimiter needs a limiter; backendGate needs a
// mode; audit needs a Log). ESServe is left false so backendGate aborts every
// request with SEARCH_DISABLED before any real search handler (which would need
// ES/MySQL) runs — that is exactly what proves the route is mounted and the
// chain is reached.
func newSubtreeTestHandler() *Handler {
	return &Handler{
		Log:     log.NewTLog("route-subtree-test"),
		limiter: newUIDLimiter(5, 20),
		mode:    searchbackend.Mode{ESServe: false, Declared: searchbackend.DeclaredDisabled},
	}
}

func TestMountSubtree_EndpointsAndChain(t *testing.T) {
	h := newSubtreeTestHandler()
	r := wkhttp.New()

	var resolved int
	front := func(c *wkhttp.Context) {
		resolved++
		SetPrincipal(c, userBotPrincipal{botUID: "bot_meter"})
		c.Next()
	}
	h.MountSubtree(r, "/v1/bot/messages", front)

	// Every registered _search* endpoint must be reachable under the new prefix
	// (chain reaches backendGate → SEARCH_DISABLED, i.e. NOT a 404).
	endpoints := []string{
		"/v1/bot/messages/_search",
		"/v1/bot/messages/_search_media",
		"/v1/bot/messages/_search_files",
		"/v1/bot/messages/_search_all",
		"/v1/bot/messages/_search_around",
		"/v1/bot/messages/_search_global_messages",
		"/v1/bot/messages/_search_global_files",
		"/v1/bot/messages/_search_global_groups",
	}
	if len(endpoints) != len(routeMounters) {
		t.Fatalf("test endpoint list (%d) drifted from routeMounters (%d); update the list",
			len(endpoints), len(routeMounters))
	}
	for _, path := range endpoints {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s: not mounted (404) — MountSubtree must register it under the prefix", path)
		}
		body := strings.ToLower(rec.Body.String())
		if !strings.Contains(body, "disabled") && !strings.Contains(rec.Body.String(), "未启用") &&
			!strings.Contains(body, "not enabled") {
			t.Fatalf("%s: expected SEARCH_DISABLED (chain reached backendGate), got %q", path, rec.Body.String())
		}
	}
	if resolved != len(endpoints) {
		t.Fatalf("front resolver ran %d times, want %d (must run before the chain on every endpoint)",
			resolved, len(endpoints))
	}

	// A sibling path NOT registered must 404 — the subtree only mounts the
	// _search* set, nothing broader.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bot/messages/not_a_search", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered path must 404, got %d", rec.Code)
	}
}

func TestMountSubtree_FrontAbortShortCircuits(t *testing.T) {
	h := newSubtreeTestHandler()
	r := wkhttp.New()

	reached := false
	front := func(c *wkhttp.Context) {
		// Simulate an auth/authorization failure at the route edge.
		c.JSON(http.StatusUnauthorized, gin.H{"msg": "denied"})
		c.Abort()
	}
	// Wrap a probe as the last "front" middleware to detect leak-through.
	probe := func(c *wkhttp.Context) {
		reached = true
		c.Next()
	}
	h.MountSubtree(r, "/v1/bot/messages", front, probe)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/bot/messages/_search", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("front abort must short-circuit with its own status, got %d", rec.Code)
	}
	if reached {
		t.Fatalf("downstream middleware must not run after front Abort")
	}
}
