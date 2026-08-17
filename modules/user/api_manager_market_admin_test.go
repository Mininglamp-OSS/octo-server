package user

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The marketAdmin routes share setFixedManagerRole with dashboardReader, whose
// HTTP behaviour is already pinned in api_manager_dashboard_reader_test.go.
// These tests exist because the shared path is now the write side of a
// privilege boundary for two roles (soon three), and "covered via the other
// role's tests" stops being true the moment someone refactors it. They pin what
// is specific to this role: the three route registrations, the SuperAdmin gate
// on them, the role-scoped listing, and the role-agnostic ineligible code.

func TestManagerLoginAllowsMarketAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	password := "market-admin-password"
	hashed, err := HashPassword(password)
	require.NoError(t, err)
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      "market-admin-login",
		Username: "market-admin-login",
		Name:     "Market Admin",
		ShortNo:  "market-admin-login",
		Password: hashed,
		Role:     appauth.ManagerRoleMarketAdmin,
		Status:   StatusEnable.Int(),
	}))

	body, err := json.Marshal(map[string]string{
		"username": "market-admin-login",
		"password": password,
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader(body))
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"role":"`+appauth.ManagerRoleMarketAdmin+`"`)
}

// TestManagerMe_MarketAdminGetsOnlyMarketCaps is the wire-level counterpart to
// TestManagerCapabilities_MarketAdmin: the pure function cannot catch a
// regression in the handler's own role gate, and the capability map is the
// contract octo-admin renders from.
func TestManagerMe_MarketAdminGetsOnlyMarketCaps(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	loginAsRole(t, ctx, appauth.ManagerRoleMarketAdmin)
	w := getManagerMe(route)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp meResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, appauth.ManagerRoleMarketAdmin, resp.Role)

	granted := map[string]bool{
		"mcp.read": true, "mcp.write": true,
		"skill.read": true, "skill.write": true,
		"expert.read": true, "expert.write": true,
	}
	for capability, enabled := range resp.Capabilities {
		assert.Equalf(t, granted[capability], enabled,
			"marketAdmin capability %q over the wire", capability)
	}
	assert.False(t, resp.Capabilities["dashboard.read"], "marketAdmin is not a dashboard reader")
	assert.False(t, resp.Capabilities["users.read"], "marketAdmin holds no console power outside the market")
	assert.False(t, resp.Capabilities["system_setting"], "marketAdmin holds no console power outside the market")
}

func TestManagerMarketAdminGrantAndRevoke(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "market-admin-grant-caller"
		targetUID   = "market-admin-target"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      targetUID,
		Username: targetUID,
		Name:     "Market Admin Target",
		ShortNo:  targetUID,
		Status:   StatusEnable.Int(),
	}))

	doRoleRequest := func(method string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/manager/user/"+targetUID+"/market-admin", nil)
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		return w
	}

	granted := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, granted.Code, granted.Body.String())
	assert.Equal(t, "uid", granted.Header().Get("X-RateLimit-Scope"),
		"the shared UID limiter must be mounted after AuthMiddleware on this group")
	role, err := NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Equal(t, appauth.ManagerRoleMarketAdmin, role)
	cached, err := ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "grant must invalidate the target role cache")

	// A stale cache entry must not survive an idempotent retry, or a partial
	// earlier failure keeps the old role live for the TTL.
	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, string(wkhttp.Admin)))
	retried := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "idempotent grant must invalidate a stale role cache")

	revoked := doRoleRequest(http.MethodDelete)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
	role, err = NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Empty(t, role)
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "revoke must invalidate the target role cache")
}

func TestManagerMarketAdminRoutesRequireSuperAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		adminToken = "market-admin-gate-admin-caller"
		targetUID  = "market-admin-gate-target"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+adminToken,
		"admin-uid@admin@"+string(wkhttp.Admin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: targetUID, Username: targetUID, Name: "Gate Target",
		ShortNo: targetUID, Status: StatusEnable.Int(),
	}))

	// admin is a manager-console role but not SuperAdmin: it must not be able to
	// read or mutate who holds the role.
	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "list", method: http.MethodGet, path: "/v1/manager/user/market-admin"},
		{name: "grant", method: http.MethodPut, path: "/v1/manager/user/" + targetUID + "/market-admin"},
		{name: "revoke", method: http.MethodDelete, path: "/v1/manager/user/" + targetUID + "/market-admin"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("token", adminToken)
			route.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "err.shared.auth.forbidden")
		})
	}

	role, err := NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Empty(t, role, "a rejected grant must not mutate the target")
}

func TestManagerMarketAdminListIsRoleScoped(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "market-admin-list-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "listed-market-admin", Username: "listed-market-admin", Name: "Listed Market Admin",
		ShortNo: "listed-market-admin", Role: appauth.ManagerRoleMarketAdmin, Status: StatusEnable.Int(),
	}))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "other-fixed-role", Username: "other-fixed-role", Name: "Other Fixed Role",
		ShortNo: "other-fixed-role", Role: appauth.ManagerRoleDashboardReader, Status: StatusEnable.Int(),
	}))

	get := func(path string) []struct {
		UID string `json:"uid"`
	} {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var list []struct {
			UID string `json:"uid"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
		return list
	}

	// Each listing must show only its own role — the two fixed roles are
	// mutually exclusive and must not bleed into each other's views.
	marketAdmins := get("/v1/manager/user/market-admin")
	require.Len(t, marketAdmins, 1)
	assert.Equal(t, "listed-market-admin", marketAdmins[0].UID)

	readers := get("/v1/manager/user/dashboard-read")
	require.Len(t, readers, 1)
	assert.Equal(t, "other-fixed-role", readers[0].UID)
}

func TestManagerMarketAdminGrantRejectsIneligibleAccounts(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "market-admin-ineligible-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	tests := []struct {
		name      string
		status    int
		robot     int
		isDestroy int
	}{
		{name: "disabled", status: StatusDisable.Int()},
		{name: "robot", status: StatusEnable.Int(), robot: 1},
		{name: "destroying", status: StatusEnable.Int(), isDestroy: IsDestroyApplying},
		{name: "destroyed", status: StatusEnable.Int(), isDestroy: IsDestroyDone},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid := "market-admin-ineligible-" + tt.name
			require.NoError(t, NewDB(ctx).Insert(&Model{
				UID: uid, Username: uid, Name: tt.name,
				ShortNo: fmt.Sprintf("market-ineligible-%d", i), Status: tt.status,
				Robot: tt.robot, IsDestroy: tt.isDestroy,
			}))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/"+uid+"/market-admin", nil)
			req.Header.Set("token", callerToken)
			route.ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			// The role-agnostic code, not the dashboard-specific one.
			assert.Contains(t, w.Body.String(), "err.server.user.manager_role_target_ineligible")
			role, err := NewDB(ctx).QueryRoleByUID(uid)
			require.NoError(t, err)
			assert.Empty(t, role)
		})
	}
}

// TestManagerMarketAdminCannotReachAdminEndpoints is the containment claim the
// whole design rests on: the role is unknown to octo-lib's CheckLoginRole*, so a
// holder passes no admin gate in this service — including the endpoints that
// manage its own role.
func TestManagerMarketAdminCannotReachAdminEndpoints(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const holderToken = "market-admin-holder"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+holderToken,
		"market-uid@market@"+appauth.ManagerRoleMarketAdmin))

	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "user list", method: http.MethodGet, path: "/v1/manager/user/list"},
		{name: "admin list", method: http.MethodGet, path: "/v1/manager/user/admin"},
		{name: "online devices", method: http.MethodGet, path: "/v1/manager/user/online?uid=x"},
		{name: "own role list", method: http.MethodGet, path: "/v1/manager/user/market-admin"},
		{name: "own role grant", method: http.MethodPut, path: "/v1/manager/user/someone/market-admin"},
		{name: "dashboard reader list", method: http.MethodGet, path: "/v1/manager/user/dashboard-read"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("token", holderToken)
			route.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "err.shared.auth.forbidden")
		})
	}
}

// TestIsFixedManagerRole pins the closed set setFixedManagerRole is willing to
// write. superAdmin and "" are the two values that would do real damage if the
// guard were dropped: the first is a promotion, the second a silent demotion.
func TestIsFixedManagerRole(t *testing.T) {
	for _, tt := range []struct {
		role string
		want bool
	}{
		{role: appauth.ManagerRoleDashboardReader, want: true},
		{role: appauth.ManagerRoleMarketAdmin, want: true},
		{role: string(wkhttp.SuperAdmin), want: false},
		{role: string(wkhttp.Admin), want: false},
		{role: "", want: false},
		{role: "future-role", want: false},
	} {
		assert.Equalf(t, tt.want, isFixedManagerRole(tt.role), "isFixedManagerRole(%q)", tt.role)
	}
}
