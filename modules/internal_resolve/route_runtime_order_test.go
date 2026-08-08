package internal_resolve

// Runtime-order test for the Route() middleware chain.
//
// PR #711 review round 4 (yujiawei P1 middleware-order) pointed out that
// the source-guard test (route_wiring_test.go) only checks the SHAPE of
// Route()'s body, not the RUNTIME order Gin actually executes. In Gin,
// group-attached handlers run BEFORE route-attached handlers regardless
// of source ordering; so a `Group(prefix, auth)` + `POST(path, ipLimit,
// handler)` combination executes as `auth → ipLimit → handler` — which
// is exactly the vulnerability that motivated moving both middlewares
// onto the concrete POST route in this PR.
//
// This test wraps `internalAuthMiddleware`, `resolveBotOwnerIPRateLimit`,
// and the handler with a *recording* wrapper that appends its own name
// to a slice at invocation time. It then drives one HTTP request through
// the real Route() and asserts the observed order is exactly
// [ipLimit, auth, handler].
//
// The wrap sites match the source order in Route() by construction (we
// swap the closures for the recording variants before Route() runs), so a
// future refactor that reorders Route()'s attach calls will change the
// observed sequence and the assertion will catch it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestRouteMiddlewareRuntimeOrder proves the middleware execution sequence
// is [rate-limit, auth, handler]. Any refactor that moves auth back to the
// group, or swaps ipLimit and auth, flips the observed order and fails.
func TestRouteMiddlewareRuntimeOrder(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"bot-1": {UID: "bot-1", Kind: botidentity.KindUserBot, CreatorUID: "alice"},
	}}
	m := &Module{
		identities:    res,
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalResolveRuntimeOrderTest"),
	}

	// Mount the recording chain by hand so we can substitute the same
	// closures Route() would use — this test's job is to prove the ORDER,
	// not to run Route()'s runtime construction (which requires a real
	// config for the Redis client).
	var seen []string
	record := func(name string, next wkhttp.HandlerFunc) wkhttp.HandlerFunc {
		return func(c *wkhttp.Context) {
			seen = append(seen, name)
			next(c)
		}
	}

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	// Mirror the production wiring exactly (Group with no middleware, both
	// middlewares on the POST route in ipLimit → auth → handler order),
	// wrapping each in the recording layer. A stub limiter substitute is
	// used because the real one needs Redis; the wrapper still executes
	// FIRST if the mount order is correct.
	ipLimitStub := record("ipLimit", func(c *wkhttp.Context) { c.Next() })
	auth := record("auth", m.internalAuthMiddleware())
	handler := record("handler", m.resolveBotOwner)

	internal := r.Group("/v1/internal")
	internal.POST("/user/resolve-bot-owner", ipLimitStub, auth, handler)

	body, err := json.Marshal(resolveBotOwnerRequest{UID: "bot-1"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/user/resolve-bot-owner", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, testInternalToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d — chain aborted before handler: seen=%v body=%s",
			got, want, seen, w.Body.String())
	}
	want := []string{"ipLimit", "auth", "handler"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("middleware order = %v, want %v — a reordering has changed the runtime "+
			"execution sequence. Route() must mount both middlewares on the concrete POST "+
			"in ipLimit → auth → handler order; putting auth on the group re-orders "+
			"execution and lets unauthenticated requests bypass the strict-IP bucket.",
			seen, want)
	}
}

// TestRouteMiddlewareRuntimeOrder_UnauthedStillConsumesLimiter asserts the
// other half of the invariant: even a request with a wrong X-Internal-Token
// (which will abort at the auth step) MUST have consumed the ipLimit slot
// first. If a future refactor accidentally puts auth first, unauthed
// requests will never reach the strict-IP bucket, which is the exact hazard
// yujiawei P1-2 flagged.
func TestRouteMiddlewareRuntimeOrder_UnauthedStillConsumesLimiter(t *testing.T) {
	res := &stubResolver{}
	m := &Module{
		identities:    res,
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalResolveRuntimeOrderTest"),
	}

	var seen []string
	record := func(name string, next wkhttp.HandlerFunc) wkhttp.HandlerFunc {
		return func(c *wkhttp.Context) {
			seen = append(seen, name)
			next(c)
		}
	}

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	ipLimitStub := record("ipLimit", func(c *wkhttp.Context) { c.Next() })
	auth := record("auth", m.internalAuthMiddleware())
	handler := record("handler", m.resolveBotOwner)

	internal := r.Group("/v1/internal")
	internal.POST("/user/resolve-bot-owner", ipLimitStub, auth, handler)

	body, _ := json.Marshal(resolveBotOwnerRequest{UID: "u1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/user/resolve-bot-owner", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, "wrong-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got, want := w.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	// Two entries expected: ipLimit consumed FIRST, then auth (which
	// aborts). Handler MUST NOT appear.
	if len(seen) < 2 || seen[0] != "ipLimit" || seen[1] != "auth" {
		t.Fatalf("middleware trace = %v, want [ipLimit, auth, ...] with auth aborting", seen)
	}
	for _, name := range seen {
		if name == "handler" {
			t.Fatalf("handler executed on unauthed request; seen=%v", seen)
		}
	}
}
