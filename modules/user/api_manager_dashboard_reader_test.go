package user

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newManagerRouteOnly(t *testing.T) (*wkhttp.WKHttp, *config.Context, *Manager) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	ctx.SetHttpRoute(route)
	require.NoError(t, testutil.CleanAllTables(ctx))
	m := NewManager(ctx)
	m.Route(route)
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	return route, ctx, m
}

func TestManagerLoginAllowsDashboardReader(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	password := "reader-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      "dashboard-reader-login",
		Username: "dashboard-reader-login",
		Name:     "Dashboard Reader",
		Password: hash,
		Role:     appauth.ManagerRoleDashboardReader,
	}))

	body, err := json.Marshal(map[string]string{
		"username": "dashboard-reader-login",
		"password": password,
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader(body))
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"role":"`+appauth.ManagerRoleDashboardReader+`"`)
}

func TestDashboardReaderRoleTransition(t *testing.T) {
	tests := []struct {
		name        string
		currentRole string
		grant       bool
		wantRole    string
		wantChanged bool
		wantAllowed bool
	}{
		{name: "grant normal", grant: true, wantRole: appauth.ManagerRoleDashboardReader, wantChanged: true, wantAllowed: true},
		{name: "downgrade admin", currentRole: string(wkhttp.Admin), grant: true, wantRole: appauth.ManagerRoleDashboardReader, wantChanged: true, wantAllowed: true},
		{name: "grant idempotent", currentRole: appauth.ManagerRoleDashboardReader, grant: true, wantRole: appauth.ManagerRoleDashboardReader, wantAllowed: true},
		{name: "protect superAdmin grant", currentRole: string(wkhttp.SuperAdmin), grant: true},
		{name: "reject unknown grant", currentRole: "future-role", grant: true},
		{name: "revoke reader", currentRole: appauth.ManagerRoleDashboardReader, wantChanged: true, wantAllowed: true},
		{name: "revoke idempotent", wantAllowed: true},
		{name: "do not revoke admin inheritance", currentRole: string(wkhttp.Admin)},
		{name: "protect superAdmin revoke", currentRole: string(wkhttp.SuperAdmin)},
		{name: "reject unknown revoke", currentRole: "future-role"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRole, gotChanged, gotAllowed := dashboardReaderRoleTransition(tt.currentRole, tt.grant)
			assert.Equal(t, tt.wantRole, gotRole)
			assert.Equal(t, tt.wantChanged, gotChanged)
			assert.Equal(t, tt.wantAllowed, gotAllowed)
		})
	}
}

func TestManagerDashboardReaderGrantAndRevoke(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "dashboard-reader-grant-caller"
		targetUID   = "dashboard-reader-target"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      targetUID,
		Username: "dashboard-reader-target",
		Name:     "Dashboard Reader Target",
		Role:     string(wkhttp.Admin),
	}))
	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, string(wkhttp.Admin)))

	doRoleRequest := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/manager/user/"+targetUID+"/dashboard-read", nil)
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		return w
	}

	granted := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, granted.Code, granted.Body.String())
	assert.Equal(t, "uid", granted.Header().Get("X-RateLimit-Scope"))
	role, err := NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Equal(t, appauth.ManagerRoleDashboardReader, role)
	cached, err := ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "grant must invalidate the target role cache")

	// Idempotent retries must still clear a stale cache entry. Otherwise a
	// previous partial failure can leave an admin snapshot active for the TTL.
	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, string(wkhttp.Admin)))
	retriedGrant := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, retriedGrant.Code, retriedGrant.Body.String())
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "idempotent grant must invalidate a stale role cache")

	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, appauth.ManagerRoleDashboardReader))
	revoked := doRoleRequest(http.MethodDelete)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
	role, err = NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Empty(t, role)
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "revoke must invalidate the target role cache")

	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, appauth.ManagerRoleDashboardReader))
	retriedRevoke := doRoleRequest(http.MethodDelete)
	require.Equal(t, http.StatusOK, retriedRevoke.Code, retriedRevoke.Body.String())
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "idempotent revoke must invalidate a stale role cache")

	// Granting from the normal-user empty role is the other supported path.
	normalGrant := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, normalGrant.Code, normalGrant.Body.String())
	role, err = NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Equal(t, appauth.ManagerRoleDashboardReader, role)
}

func TestManagerDashboardReaderRoleChangeRevokesExistingSessions(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "dashboard-reader-session-revoke-caller"
		targetUID   = "dashboard-reader-session-revoke-target"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      targetUID,
		Username: targetUID,
		Name:     "Dashboard Reader Session Revoke Target",
		Role:     string(wkhttp.Admin),
	}))

	seedTargetSessions := func(stage string) map[config.DeviceFlag]string {
		t.Helper()
		tokens := make(map[config.DeviceFlag]string)
		for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
			token := fmt.Sprintf("%s-%d", stage, flag.Uint8())
			uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, targetUID)
			require.NoError(t, ctx.Cache().Set(uidTokenKey, token))
			require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+token,
				targetUID+"@target@"+string(wkhttp.Admin)))
			tokens[flag] = token
		}
		return tokens
	}
	assertSessionsRevoked := func(tokens map[config.DeviceFlag]string) {
		t.Helper()
		for flag, token := range tokens {
			uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, targetUID)
			uidToken, err := ctx.Cache().Get(uidTokenKey)
			require.NoError(t, err)
			assert.Empty(t, uidToken, "device reverse mapping must be revoked for flag %d", flag.Uint8())
			payload, err := ctx.Cache().Get(cacheCfg.TokenCachePrefix + token)
			require.NoError(t, err)
			assert.Empty(t, payload, "token payload must be revoked for flag %d", flag.Uint8())
		}
	}
	request := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/manager/user/"+targetUID+"/dashboard-read", nil)
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		return w
	}

	adminSessions := seedTargetSessions("before-grant")
	granted := request(http.MethodPut)
	require.Equal(t, http.StatusOK, granted.Code, granted.Body.String())
	assertSessionsRevoked(adminSessions)

	readerSessions := seedTargetSessions("before-revoke")
	revoked := request(http.MethodDelete)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
	assertSessionsRevoked(readerSessions)
}

func TestManagerDashboardReaderGrantRequiresSuperAdminAndProtectsSuperAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      "protected-superadmin",
		Username: "protected-superadmin",
		Name:     "Protected SuperAdmin",
		Role:     string(wkhttp.SuperAdmin),
	}))

	request := func(token, tokenRole string) *httptest.ResponseRecorder {
		t.Helper()
		require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+token,
			"caller-uid@caller@"+tokenRole))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut,
			"/v1/manager/user/protected-superadmin/dashboard-read", nil)
		req.Header.Set("token", token)
		route.ServeHTTP(w, req)
		return w
	}

	admin := request("dashboard-reader-admin-caller", string(wkhttp.Admin))
	assert.Equal(t, http.StatusBadRequest, admin.Code)
	assert.Contains(t, admin.Body.String(), "err.shared.auth.forbidden")

	super := request("dashboard-reader-super-caller", string(wkhttp.SuperAdmin))
	assert.Equal(t, http.StatusBadRequest, super.Code)
	assert.Contains(t, super.Body.String(), "err.shared.auth.forbidden")
	role, err := NewDB(ctx).QueryRoleByUID("protected-superadmin")
	require.NoError(t, err)
	assert.Equal(t, string(wkhttp.SuperAdmin), role)
}

func TestManagerDashboardReaderRoleGuardEdges(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)
	cacheCfg := ctx.GetConfig().Cache
	const callerToken = "dashboard-reader-edge-super"
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))

	request := func(method, uid string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/manager/user/"+uid+"/dashboard-read", nil)
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		return w
	}

	missing := request(http.MethodPut, "missing-dashboard-reader-target")
	assert.Equal(t, http.StatusBadRequest, missing.Code)
	assert.Contains(t, missing.Body.String(), "err.server.user.not_found")

	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      "unknown-role-target",
		Username: "unknown-role-target",
		Name:     "Unknown Role",
		ShortNo:  "unknown-role-short",
		Role:     "future-role",
	}))
	unknown := request(http.MethodPut, "unknown-role-target")
	assert.Equal(t, http.StatusBadRequest, unknown.Code)
	assert.Contains(t, unknown.Body.String(), "err.shared.auth.forbidden")

	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      "admin-revoke-target",
		Username: "admin-revoke-target",
		Name:     "Admin Revoke Target",
		ShortNo:  "admin-revoke-short",
		Role:     string(wkhttp.Admin),
	}))
	adminRevoke := request(http.MethodDelete, "admin-revoke-target")
	assert.Equal(t, http.StatusBadRequest, adminRevoke.Code)
	assert.Contains(t, adminRevoke.Body.String(), "err.shared.auth.forbidden")

	updated, err := newManagerDB(ctx).updateUserRole("admin-revoke-target", "", appauth.ManagerRoleDashboardReader)
	require.NoError(t, err)
	assert.False(t, updated, "compare-and-set must reject a stale expected role")
}
