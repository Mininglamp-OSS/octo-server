package botfather

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enforceKeySpace and rejectSpaceMismatch touch no BotFather field, so the tenant
// rules can be exercised against a bare engine — no MySQL, Redis or IM needed.
// That matters: the query-cache bug this file pins was invisible to build/vet and
// to every DB-free check in the first round, yet it is reproducible in
// milliseconds here.
func newSpaceGuardRoute(boundSpace string) *wkhttp.WKHttp {
	bf := &BotFather{}
	r := wkhttp.New()
	group := r.Group("/v1/user", func(c *wkhttp.Context) {
		if boundSpace != "" {
			c.Set(authtree.CtxKeySpaceID, boundSpace)
		}
		c.Next()
	})
	mount := authtree.MountOn(group, bf.enforceKeySpace())

	// echoSpace reports the Space the handler actually observes through gin's
	// query accessor — the same call path user.search takes.
	echoSpace := func(c *wkhttp.Context) { c.String(http.StatusOK, c.Query(spaceIDField)) }
	mount(authtree.Route{Method: http.MethodGet, Path: "/echo", Handler: echoSpace})
	mount(authtree.Route{Method: http.MethodGet, Path: "/space/:space_id/echo", Handler: echoSpace})
	return r
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

// The path parameter wins over the query string, so a matching path plus a
// mismatched query param is still allowed — the route's own Space is the one the
// handler acts on, and it agrees with the binding.
func TestEnforceKeySpacePathParamTakesPrecedence(t *testing.T) {
	r := newSpaceGuardRoute("sp_bound")

	assert.Equal(t, http.StatusOK,
		spaceGuardGet(t, r, "/v1/user/space/sp_bound/echo?space_id=sp_other").Code)
}

// A key with no bound Space carries no tenant to enforce: nothing is injected and
// nothing is refused, matching how /v1/user/bots* already behaves on empty bindings.
func TestEnforceKeySpaceUnboundKeyPassesThrough(t *testing.T) {
	r := newSpaceGuardRoute("")

	w := spaceGuardGet(t, r, "/v1/user/echo")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "", w.Body.String(), "no binding means no injection")

	w = spaceGuardGet(t, r, "/v1/user/echo?space_id=sp_anything")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "sp_anything", w.Body.String(), "no binding means nothing to conflict with")
}
