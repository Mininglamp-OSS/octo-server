package authtree

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRouter builds a bare wkhttp engine plus a group to mount contributions on.
func newRouter() (*wkhttp.WKHttp, *wkhttp.RouterGroup) {
	r := wkhttp.New()
	return r, r.Group("/v1/tree")
}

func okHandler(body string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) { c.String(http.StatusOK, body) }
}

// stampHandler records that a middleware ran, so tests can prove the tree's own
// middleware is prefixed ahead of the module's handler.
func stampHandler(header, value string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		c.Writer.Header().Set(header, value)
		c.Next()
	}
}

func get(t *testing.T, r *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// A contribution registered before the tree mounts must still be installed —
// this is the ordering the module.Setup loop actually produces today.
func TestAddBeforeMountIsInstalled(t *testing.T) {
	r, group := newRouter()

	Add(TreeUserKey, r, Route{Method: http.MethodGet, Path: "/early", Handler: okHandler("early")})
	Mount(TreeUserKey, r, MountOn(group, stampHandler("X-Tree", "user_key")))

	w := get(t, r, "/v1/tree/early")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "early", w.Body.String())
	assert.Equal(t, "user_key", w.Header().Get("X-Tree"), "tree middleware must run before the handler")
}

// ...and one arriving after the mount must be installed immediately rather than
// silently dropped, so the wiring does not depend on package init order.
func TestAddAfterMountIsInstalled(t *testing.T) {
	r, group := newRouter()

	Mount(TreeUserKey, r, MountOn(group, stampHandler("X-Tree", "user_key")))
	Add(TreeUserKey, r, Route{Method: http.MethodGet, Path: "/late", Handler: okHandler("late")})

	w := get(t, r, "/v1/tree/late")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "late", w.Body.String())
	assert.Equal(t, "user_key", w.Header().Get("X-Tree"))
}

// Route-local middleware travels with the contribution and runs after the tree's.
func TestRouteMiddlewareRunsAfterTreeMiddleware(t *testing.T) {
	r, group := newRouter()

	var order []string
	Add(TreeUserKey, r, Route{
		Method: http.MethodGet,
		Path:   "/ordered",
		Middlewares: []wkhttp.HandlerFunc{func(c *wkhttp.Context) {
			order = append(order, "route")
			c.Next()
		}},
		Handler: func(c *wkhttp.Context) {
			order = append(order, "handler")
			c.String(http.StatusOK, "ok")
		},
	})
	Mount(TreeUserKey, r, MountOn(group, func(c *wkhttp.Context) {
		order = append(order, "tree")
		c.Next()
	}))

	require.Equal(t, http.StatusOK, get(t, r, "/v1/tree/ordered").Code)
	assert.Equal(t, []string{"tree", "route", "handler"}, order)
}

// Each engine keeps its own wiring. Without this, a second module.Setup (every
// testutil.NewTestServer builds a fresh engine) would re-register routes into the
// previous engine and gin would panic on the duplicate.
func TestRoutersDoNotCrossWire(t *testing.T) {
	r1, g1 := newRouter()
	r2, g2 := newRouter()

	Add(TreeUserKey, r1, Route{Method: http.MethodGet, Path: "/only-first", Handler: okHandler("first")})
	Mount(TreeUserKey, r1, MountOn(g1))

	// Same tree, different engine: r1's contribution must not leak in, and
	// mounting r2 must not re-touch r1.
	Add(TreeUserKey, r2, Route{Method: http.MethodGet, Path: "/only-second", Handler: okHandler("second")})
	Mount(TreeUserKey, r2, MountOn(g2))

	assert.Equal(t, http.StatusOK, get(t, r1, "/v1/tree/only-first").Code)
	assert.Equal(t, http.StatusNotFound, get(t, r1, "/v1/tree/only-second").Code)
	assert.Equal(t, http.StatusOK, get(t, r2, "/v1/tree/only-second").Code)
	assert.Equal(t, http.StatusNotFound, get(t, r2, "/v1/tree/only-first").Code)
}

// Trees are independent: a bot-token contribution must not appear on the user-key
// group even when both are mounted on the same engine.
func TestTreesAreIndependent(t *testing.T) {
	r := wkhttp.New()
	userGroup := r.Group("/v1/user")
	botGroup := r.Group("/v1/bot")

	Add(TreeUserKey, r, Route{Method: http.MethodGet, Path: "/shared", Handler: okHandler("user")})
	Add(TreeBotToken, r, Route{Method: http.MethodGet, Path: "/shared", Handler: okHandler("bot")})
	Mount(TreeUserKey, r, MountOn(userGroup))
	Mount(TreeBotToken, r, MountOn(botGroup))

	assert.Equal(t, "user", get(t, r, "/v1/user/shared").Body.String())
	assert.Equal(t, "bot", get(t, r, "/v1/bot/shared").Body.String())
}

// A verb wkhttp.RouterGroup cannot register is a wiring bug; it must fail at
// startup rather than 404 in production.
func TestUnsupportedMethodPanics(t *testing.T) {
	_, group := newRouter()
	mounter := MountOn(group)

	assert.PanicsWithValue(t,
		`authtree: unsupported method "PATCH" for path "/patch"`,
		func() {
			mounter(Route{Method: http.MethodPatch, Path: "/patch", Handler: okHandler("x")})
		})
}

func TestBoundSpaceID(t *testing.T) {
	r := wkhttp.New()
	group := r.Group("/v1/tree")

	Add(TreeUserKey, r, Route{Method: http.MethodGet, Path: "/space", Handler: func(c *wkhttp.Context) {
		c.String(http.StatusOK, BoundSpaceID(c))
	}})
	Mount(TreeUserKey, r, MountOn(group, func(c *wkhttp.Context) {
		if c.Query("bind") != "" {
			c.Set(CtxKeySpaceID, c.Query("bind"))
		}
		c.Next()
	}))

	assert.Equal(t, "sp_1", get(t, r, "/v1/tree/space?bind=sp_1").Body.String())
	assert.Equal(t, "", get(t, r, "/v1/tree/space").Body.String(),
		"an unbound credential must report no Space rather than a stale one")
}
