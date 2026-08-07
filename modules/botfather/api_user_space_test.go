package botfather

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardKeyUID is the key owner every case below authenticates as.
const guardKeyUID = "u_guard"

// enforceKeySpaceWithChecker touches no BotFather field but Log, so the tenant
// rules can be exercised against a bare engine with an injected membership
// checker — no MySQL, Redis or IM needed. That matters: the query-cache bug this
// file pins was invisible to build/vet and to every DB-free check in the first
// round, yet it is reproducible in milliseconds here.
func newSpaceGuardRouteWithChecker(boundSpace string, check spacepkg.MembershipChecker) *wkhttp.WKHttp {
	bf := &BotFather{Log: log.NewTLog("uk-space-guard-test")}
	r := wkhttp.New()
	group := r.Group("/v1/user", func(c *wkhttp.Context) {
		// Mirrors authUserAPIKey: the key's owner and its frozen Space both land in
		// the context before the tenant guard runs.
		c.Set("uid", guardKeyUID)
		c.Set("api_key_uid", guardKeyUID)
		if boundSpace != "" {
			c.Set(authtree.CtxKeySpaceID, boundSpace)
		}
		c.Next()
	})
	mount := authtree.MountOn(group, func(c *wkhttp.Context) {
		bf.enforceKeySpaceWithChecker(c, check)
	})

	// echoSpace reports the Space the handler actually observes through gin's
	// query accessor — the same call path user.search takes.
	echoSpace := func(c *wkhttp.Context) { c.String(http.StatusOK, c.Query(spaceIDField)) }
	mount(authtree.Route{Method: http.MethodGet, Path: "/echo", Handler: echoSpace})
	mount(authtree.Route{Method: http.MethodGet, Path: "/space/:space_id/echo", Handler: echoSpace})
	return r
}

// newSpaceGuardRoute is the common case: the key owner is still a live member of
// the bound Space, so only the Space-matching rules are under test.
func newSpaceGuardRoute(boundSpace string) *wkhttp.WKHttp {
	return newSpaceGuardRouteWithChecker(boundSpace, func(string, string) (bool, error) {
		return true, nil
	})
}

func spaceGuardGet(t *testing.T, r *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// §3.1 rule 1 — and the regression pin for the gin query-cache trap.
//
// The middleware must not read space_id through c.Query: that call populates the
// Context's private queryCache, after which rewriting URL.RawQuery is invisible to
// the handler. The handler then sees no space_id at all, user.search falls back to
// its "any shared Space" branch, and the request reaches outside the key's tenant.
// Asserting on what the HANDLER observes (not on what the middleware computed) is
// what makes this test able to catch it.
func TestEnforceKeySpaceInjectsBoundSpaceVisibleToHandler(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	w := spaceGuardGet(t, r, "/v1/user/echo")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_bound", w.Body.String(),
		"handler must observe the injected Space; an empty body means the middleware poisoned gin's query cache")
}

// Injection must not clobber the rest of the query string.
func TestEnforceKeySpacePreservesOtherQueryParams(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	w := spaceGuardGet(t, r, "/v1/user/echo?keyword=alice&limit=5")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_bound", w.Body.String())
}

// §3.1 rule 2 — an explicitly matching Space passes through untouched.
func TestEnforceKeySpaceAllowsMatchingQuerySpace(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	w := spaceGuardGet(t, r, "/v1/user/echo?space_id=sp_bound")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_bound", w.Body.String())
}

// §3.1 rule 3 — a differing Space is refused, whether it arrives as a query param
// or in the path.
func TestEnforceKeySpaceRejectsMismatchedSpace(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	assert.Equal(t, http.StatusForbidden,
		spaceGuardGet(t, r, "/v1/user/echo?space_id=sp_other").Code,
		"query-param Space must not override the key's tenant")
	assert.Equal(t, http.StatusForbidden,
		spaceGuardGet(t, r, "/v1/user/space/sp_other/echo").Code,
		"path Space must not override the key's tenant")
}

// A mismatched Space is refused before the membership query runs: the 403 path
// must not become a way to make one unauthorized request cost a DB round-trip.
func TestEnforceKeySpaceRejectsMismatchWithoutQueryingMembership(t *testing.T) {
	called := false
	r := newSpaceGuardRouteWithChecker("sp_bound", func(string, string) (bool, error) {
		called = true
		return true, nil
	})

	require.Equal(t, http.StatusForbidden, spaceGuardGet(t, r, "/v1/user/echo?space_id=sp_other").Code)
	assert.False(t, called, "cross-Space requests must be rejected without a membership lookup")
}

// The path parameter decides which Space is checked, and the guard then pins the
// query to that same value — so a handler reading either source can only ever see
// the one Space the middleware verified.
func TestEnforceKeySpacePathParamTakesPrecedenceAndPinsQuery(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	w := spaceGuardGet(t, r, "/v1/user/space/sp_bound/echo?space_id=sp_other")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_bound", w.Body.String(),
		"a conflicting query param must be overwritten, not left for the handler to read")
}

// A key with no bound Space carries no tenant to enforce: nothing is injected,
// nothing is refused, and there is no membership to look up — matching how
// /v1/user/bots* already behaves on empty bindings.
func TestEnforceKeySpaceUnboundKeyPassesThrough(t *testing.T) {
	called := false
	r := newSpaceGuardRouteWithChecker("", func(string, string) (bool, error) {
		called = true
		return true, nil
	})

	w := spaceGuardGet(t, r, "/v1/user/echo")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "", w.Body.String(), "no binding means no injection")

	w = spaceGuardGet(t, r, "/v1/user/echo?space_id=sp_anything")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_anything", w.Body.String(), "no binding means nothing to conflict with")

	assert.False(t, called, "an unbound key has no Space membership to verify")
}

// F1 / YUJ-58 — the key's owner must still be a live member of the bound Space.
//
// authUserAPIKey only checks key status, integration-client enablement and user
// active; it never re-reads membership. Without this gate a key issued while its
// owner was a member stays trusted after the owner is removed, and because rule 1
// forces user.search onto its "filter by space_id" branch — which validates the
// TARGET's membership and never the caller's — the removed member could still
// probe the Space's current members. A live human session is refused in the same
// situation by HaveCommonSpace.
func TestEnforceKeySpaceRefusesRevokedMembership(t *testing.T) {
	var gotSpace, gotUID string
	r := newSpaceGuardRouteWithChecker("sp_bound", func(spaceID, uid string) (bool, error) {
		gotSpace, gotUID = spaceID, uid
		return false, nil
	})

	// Refused on every shape: injected, explicitly matching, and via the path.
	for _, path := range []string{
		"/v1/user/echo",
		"/v1/user/echo?space_id=sp_bound",
		"/v1/user/space/sp_bound/echo",
	} {
		w := spaceGuardGet(t, r, path)
		assert.Equal(t, http.StatusForbidden, w.Code,
			"a key whose owner left the bound Space must be refused on %s: %s", path, w.Body.String())
	}
	assert.Equal(t, "sp_bound", gotSpace, "membership must be checked against the key's bound Space")
	assert.Equal(t, guardKeyUID, gotUID, "membership must be checked for the key's owner")
}

// A failed membership lookup is fail-closed: the request must not proceed on an
// unverified tenant just because the database was unreachable.
func TestEnforceKeySpaceFailsClosedOnCheckerError(t *testing.T) {
	r := newSpaceGuardRouteWithChecker("sp_bound", func(string, string) (bool, error) {
		return false, errors.New("db down")
	})

	w := spaceGuardGet(t, r, "/v1/user/echo")
	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "db down",
		"the internal cause must stay in the logs, not in the response")
}
