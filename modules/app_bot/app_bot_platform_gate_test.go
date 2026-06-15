package app_bot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPlatformGateRoute mounts the AppBot routes with `role` baked into the
// token. The harness wires no RoleResolver, so the token role is authoritative
// (same approach as newApplyBotRateLimitTestRoute). DB is sqlmock — the gate is
// the first thing each handler does, so the role tests never reach a query.
func newPlatformGateRouteWithMock(t *testing.T, role string) (*wkhttp.WKHttp, sqlmock.Sqlmock, func()) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)

	val := testutil.UID + "@test"
	if role != "" {
		val += "@" + role
	}
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, val))

	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}

	route := wkhttp.New()
	ab := &AppBot{
		ctx: ctx,
		db:  &appBotDB{ctx: ctx, session: conn.NewSession(nil)},
		Log: log.NewTLog("AppBotPlatformGateTest"),
	}
	ab.Route(route)
	return route, mock, func() { _ = rawDB.Close() }
}

func newPlatformGateRoute(t *testing.T, role string) (*wkhttp.WKHttp, func()) {
	t.Helper()
	route, _, cleanup := newPlatformGateRouteWithMock(t, role)
	return route, cleanup
}

// TestBotInRouteScope pins the cross-tenant scope boundary: the platform route
// (spaceID=="") admits only platform bots, the space route only its own space's
// bots. The false case on row 2 is the IDOR guard — a platform-route caller must
// not reach a space-scoped bot.
func TestBotInRouteScope(t *testing.T) {
	platform := &appBotModel{Scope: "platform"}
	spaceX := &appBotModel{Scope: "space", SpaceID: "X"}

	assert.True(t, botInRouteScope(platform, ""), "platform route admits a platform bot")
	assert.False(t, botInRouteScope(spaceX, ""), "platform route must NOT admit a space bot (cross-tenant IDOR guard)")
	assert.True(t, botInRouteScope(spaceX, "X"), "space route admits its own space's bot")
	assert.False(t, botInRouteScope(spaceX, "Y"), "space route must NOT admit another space's bot")
	assert.False(t, botInRouteScope(platform, "X"), "space route must NOT admit a platform bot")
}

// TestPlatformAppBot_SpaceBotViaPlatformRouteRejected is the regression test for
// the cross-tenant token-exposure path: an admin hitting the platform
// reveal-token route with a SPACE bot's (global) id must get 404, and the space
// bot's raw token must never be returned.
func TestPlatformAppBot_SpaceBotViaPlatformRouteRejected(t *testing.T) {
	route, mock, cleanup := newPlatformGateRouteWithMock(t, string(wkhttp.Admin))
	defer cleanup()

	mock.ExpectQuery("app_bot").WithArgs("space-bot-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "scope", "space_id", "status", "token"}).
			AddRow("space-bot-1", "space", "X", 1, "super-secret-space-token"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/app_bot/space-bot-1/token/reveal", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "platform route must not reach a space bot")
	assert.NotContains(t, w.Body.String(), "super-secret-space-token",
		"a space bot's token must never be revealed via the platform route")
}

// TestPlatformAppBot_AdminAllowed pins the loosening: platform bot management is
// delegated to admin (operations). An admin must pass the /v1/admin/app_bot gate
// (previously superAdmin-only) — proven by reaching request validation rather
// than the forbidden envelope.
func TestPlatformAppBot_AdminAllowed(t *testing.T) {
	route, cleanup := newPlatformGateRoute(t, string(wkhttp.Admin))
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/app_bot", strings.NewReader("not-json"))
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	body := w.Body.String()
	// admin reaches request validation (proves the gate let it through), now the
	// localized "Invalid request." envelope — and NOT a forbidden envelope.
	assert.Contains(t, body, "Invalid request", "admin must pass the gate and reach handler validation")
	assert.NotContains(t, body, "You do not have permission", "admin must not be forbidden")
}

// TestPlatformAppBot_PlainUserForbiddenLocalized pins both the gate (a user with
// no system role is rejected) and the i18n fix (the forbidden response is the
// localized shared envelope, not the raw wkhttp framework string that the legacy
// c.ResponseError(err) leaked).
func TestPlatformAppBot_PlainUserForbiddenLocalized(t *testing.T) {
	route, cleanup := newPlatformGateRoute(t, "") // no system role
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/app_bot", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	body := w.Body.String()
	// i18n fix: the forbidden response is now the localized shared-forbidden
	// message rendered from the registered code (en-US source here), NOT the raw
	// unlocalized wkhttp framework string the legacy c.ResponseError(err) leaked.
	assert.NotContains(t, body, "该用户无权执行此操作", "must not leak the raw wkhttp framework string")
	assert.Contains(t, body, "permission", "renders the localized shared-forbidden message")
}

// TestPlatformAppBot_NotFoundLocalized pins the correct-semantics migration of
// the not-found path: an admin querying a missing bot gets a real 404 with the
// localized "Bot not found." envelope (was a raw c.JSON(404, {"msg":"bot not
// found"})).
func TestPlatformAppBot_NotFoundLocalized(t *testing.T) {
	route, cleanup := newPlatformGateRoute(t, string(wkhttp.Admin))
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/app_bot/does-not-exist", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "missing bot returns a real 404")
	assert.Contains(t, w.Body.String(), "Bot not found", "localized not-found message")
}

// TestSpaceAppBot_ForbiddenLocalized pins the space-scoped guard migration: a
// caller who is not a space admin (here a system admin, which is independent of
// space role) is rejected with the localized shared-forbidden envelope at a
// preserved real 403 — not the raw "no permission: requires space admin" string
// the legacy c.AbortWithStatusJSON leaked.
func TestSpaceAppBot_ForbiddenLocalized(t *testing.T) {
	route, cleanup := newPlatformGateRoute(t, string(wkhttp.Admin))
	defer cleanup()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/space/space-x/app_bot", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, http.StatusForbidden, w.Code, "space forbidden preserves the real 403")
	assert.NotContains(t, body, "no permission: requires space admin", "raw checkSpaceAdmin string must be gone")
	assert.Contains(t, body, "You do not have permission", "localized shared-forbidden message")
}
