package user

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetManagerUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	t.Cleanup(func() { _ = rds.Close() })
	keys, err := rds.Keys("ratelimit:uid:*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, rds.Del(keys...).Err())
	}
}

func newManagerRouteOnly(t *testing.T) (*wkhttp.WKHttp, *config.Context, *Manager) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	ctx.SetHttpRoute(route)
	require.NoError(t, testutil.CleanAllTables(ctx))
	resetManagerUIDRateLimit(t, ctx)
	m := NewManager(ctx)
	m.Route(route)
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	return route, ctx, m
}

type managerTestAccount struct {
	uid       string
	username  string
	name      string
	shortNo   string
	password  string
	role      string
	status    int
	robot     int
	isDestroy int
}

func insertManagerTestAccount(t *testing.T, ctx *config.Context, account managerTestAccount) {
	t.Helper()
	if account.username == "" {
		account.username = account.uid
	}
	if account.name == "" {
		account.name = account.uid
	}
	if account.shortNo == "" {
		account.shortNo = account.uid
	}
	_, err := NewDB(ctx).session.InsertInto("user").Columns(
		"uid", "username", "name", "short_no", "password", "role", "status", "robot", "is_destroy",
	).Values(
		account.uid, account.username, account.name, account.shortNo, account.password,
		account.role, account.status, account.robot, account.isDestroy,
	).Exec()
	require.NoError(t, err)
}

func TestManagerLoginAllowsDashboardReader(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	password := "reader-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	insertManagerTestAccount(t, ctx, managerTestAccount{
		uid: "dashboard-reader-login", name: "Dashboard Reader", password: hash,
		role: appauth.ManagerRoleDashboardReader, status: StatusEnable.Int(),
	})

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

func TestManagerLoginRejectsUnavailableManagerAccounts(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const password = "unavailable-manager-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	tests := []struct {
		name      string
		role      string
		status    int
		robot     int
		isDestroy int
	}{
		{name: "disabled reader", role: appauth.ManagerRoleDashboardReader, status: StatusDisable.Int()},
		{name: "destroying reader", role: appauth.ManagerRoleDashboardReader, status: StatusEnable.Int(), isDestroy: IsDestroyApplying},
		{name: "destroyed reader", role: appauth.ManagerRoleDashboardReader, status: StatusEnable.Int(), isDestroy: IsDestroyDone},
		{name: "robot reader", role: appauth.ManagerRoleDashboardReader, status: StatusEnable.Int(), robot: 1},
		{name: "disabled admin", role: string(wkhttp.Admin), status: StatusDisable.Int()},
	}

	for i, tt := range tests {
		username := fmt.Sprintf("unavailable-manager-%d", i)
		insertManagerTestAccount(t, ctx, managerTestAccount{
			uid: username, name: tt.name, shortNo: fmt.Sprintf("unavailable-%d", i), password: hash,
			role: tt.role, status: tt.status, robot: tt.robot, isDestroy: tt.isDestroy,
		})
		body, err := json.Marshal(map[string]string{"username": username, "password": password})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader(body))
		route.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", tt.name, w.Body.String())
		assert.Contains(t, w.Body.String(), "err.server.user.manager_permission_required")
	}
}

func TestLiftBanDisabledManagerRevokesHTTPManagerSessions(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "disable-manager-session-revoke-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	tests := []struct {
		name       string
		role       string
		status     int
		wantRevoke bool
	}{
		{name: "dashboard reader", role: appauth.ManagerRoleDashboardReader, status: StatusDisable.Int(), wantRevoke: true},
		{name: "admin", role: string(wkhttp.Admin), status: StatusDisable.Int(), wantRevoke: true},
		{name: "super admin", role: string(wkhttp.SuperAdmin), status: StatusDisable.Int(), wantRevoke: true},
		{name: "normal user", status: StatusDisable.Int()},
		{name: "enabled admin", role: string(wkhttp.Admin), status: StatusEnable.Int()},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid := fmt.Sprintf("disable-manager-session-%d", i)
			insertManagerTestAccount(t, ctx, managerTestAccount{
				uid: uid, name: tt.name, shortNo: fmt.Sprintf("disable-manager-%d", i),
				role: tt.role, status: tt.status,
			})
			require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+uid, tt.role))
			tokens := make(map[config.DeviceFlag]string)
			for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
				token := fmt.Sprintf("disable-manager-%d-%d", i, flag.Uint8())
				uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, uid)
				require.NoError(t, ctx.Cache().Set(uidTokenKey, token))
				require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+token,
					uid+"@target@"+tt.role))
				tokens[flag] = token
			}

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut,
				fmt.Sprintf("/v1/manager/user/liftban/%s/%d", uid, tt.status), nil)
			req.Header.Set("token", callerToken)
			route.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			cachedRole, err := ctx.Cache().Get(RoleCacheKeyPrefix + uid)
			require.NoError(t, err)
			if tt.wantRevoke {
				assert.Empty(t, cachedRole)
			} else {
				assert.Equal(t, tt.role, cachedRole)
			}
			for flag, token := range tokens {
				uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, uid)
				uidToken, err := ctx.Cache().Get(uidTokenKey)
				require.NoError(t, err)
				payload, err := ctx.Cache().Get(cacheCfg.TokenCachePrefix + token)
				require.NoError(t, err)
				if tt.wantRevoke {
					assert.Empty(t, uidToken, "device reverse mapping must be revoked for flag %d", flag.Uint8())
					assert.Empty(t, payload, "token payload must be revoked for flag %d", flag.Uint8())
				} else {
					assert.Equal(t, token, uidToken)
					assert.NotEmpty(t, payload)
				}
			}
		})
	}
}

func TestLiftBanPersistsDisabledStatusBeforeRevokingManagerSessions(t *testing.T) {
	source, err := os.ReadFile("api_manager.go")
	require.NoError(t, err)
	bodyStart := strings.Index(string(source), "func (m *Manager) liftBanUser")
	require.NotEqual(t, -1, bodyStart)
	bodyEnd := strings.Index(string(source)[bodyStart:], "func (m *Manager) revokeManagerSessionsForDisabledAccount")
	require.NotEqual(t, -1, bodyEnd)
	body := string(source)[bodyStart : bodyStart+bodyEnd]

	statusWrite := strings.Index(body, `UpdateUsersWithField("status"`)
	sessionRevoke := strings.Index(body, "revokeManagerSessionsForDisabledAccount")
	require.NotEqual(t, -1, statusWrite)
	require.NotEqual(t, -1, sessionRevoke)
	require.Less(t, statusWrite, sessionRevoke,
		"status must become disabled before session revocation closes the concurrent-login window")
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
		Status:   StatusEnable.Int(),
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
		Status:   StatusEnable.Int(),
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

func TestDeleteAdminRejectsDashboardReaderWithoutRevokingSessions(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "delete-admin-reader-caller"
		targetUID   = "delete-admin-reader-target"
		targetToken = "delete-admin-reader-target-app-token"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      targetUID,
		Username: targetUID,
		Name:     "Dashboard Reader Delete Guard",
		ShortNo:  "delete-reader",
		Role:     appauth.ManagerRoleDashboardReader,
		Status:   StatusEnable.Int(),
	}))
	uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, config.APP, targetUID)
	require.NoError(t, ctx.Cache().Set(uidTokenKey, targetToken))
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+targetToken,
		targetUID+"@target@"+appauth.ManagerRoleDashboardReader))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/manager/user/admin?uid="+targetUID, nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.user.not_admin_account")
	target, err := NewDB(ctx).QueryByUID(targetUID)
	require.NoError(t, err)
	require.NotNil(t, target)
	assert.Equal(t, appauth.ManagerRoleDashboardReader, target.Role)
	payload, err := ctx.Cache().Get(cacheCfg.TokenCachePrefix + targetToken)
	require.NoError(t, err)
	assert.NotEmpty(t, payload, "rejecting delete-admin must not log out a dashboard reader")
	uidToken, err := ctx.Cache().Get(uidTokenKey)
	require.NoError(t, err)
	assert.Equal(t, targetToken, uidToken)
	deleted, err := newManagerDB(ctx).deleteUserWithUIDAndRole(targetUID, string(wkhttp.Admin))
	require.NoError(t, err)
	assert.False(t, deleted, "role-qualified delete must report a zero-row race")
}

func TestDeleteAdminDeletesAdminAndRevokesSessions(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "delete-admin-success-caller"
		targetUID   = "delete-admin-success-target"
		targetToken = "delete-admin-success-target-app-token"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: targetUID, Username: targetUID, Name: "Admin Delete Target",
		ShortNo: "delete-admin", Role: string(wkhttp.Admin), Status: StatusEnable.Int(),
	}))
	uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, config.APP, targetUID)
	require.NoError(t, ctx.Cache().Set(uidTokenKey, targetToken))
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+targetToken,
		targetUID+"@target@"+string(wkhttp.Admin)))
	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, string(wkhttp.Admin)))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/manager/user/admin?uid="+targetUID, nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	target, err := NewDB(ctx).QueryByUID(targetUID)
	require.NoError(t, err)
	assert.Nil(t, target)
	for _, key := range []string{
		RoleCacheKeyPrefix + targetUID,
		cacheCfg.TokenCachePrefix + targetToken,
		uidTokenKey,
	} {
		value, err := ctx.Cache().Get(key)
		require.NoError(t, err)
		assert.Empty(t, value, "delete-admin must clear %s", key)
	}
}

func TestManagerDashboardReaderList(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "dashboard-reader-list-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "listed-reader", Username: "listed-reader", Name: "Listed Reader",
		ShortNo: "listed-reader", Role: appauth.ManagerRoleDashboardReader, Status: StatusEnable.Int(),
	}))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "unlisted-admin", Username: "unlisted-admin", Name: "Unlisted Admin",
		ShortNo: "unlisted-admin", Role: string(wkhttp.Admin), Status: StatusEnable.Int(),
	}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/manager/user/dashboard-read", nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"))
	var list []struct {
		UID      string `json:"uid"`
		Username string `json:"username"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "listed-reader", list[0].UID)
	assert.Equal(t, "listed-reader", list[0].Username)
}

func TestManagerDashboardReaderListRequiresSuperAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "dashboard-reader-list-admin-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"admin-uid@admin@"+string(wkhttp.Admin)))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/manager/user/dashboard-read", nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "err.shared.auth.forbidden")
}

func TestManagerDashboardReaderNoopRevokePreservesNormalSessions(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const (
		callerToken = "dashboard-reader-noop-revoke-caller"
		targetUID   = "dashboard-reader-noop-revoke-normal"
		targetToken = "dashboard-reader-noop-revoke-app-token"
	)
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: targetUID, Username: targetUID, Name: "Normal User", ShortNo: "noop-normal", Status: StatusEnable.Int(),
	}))
	uidTokenKey := fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, config.APP, targetUID)
	require.NoError(t, ctx.Cache().Set(uidTokenKey, targetToken))
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+targetToken, targetUID+"@normal"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/manager/user/"+targetUID+"/dashboard-read", nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	payload, err := ctx.Cache().Get(cacheCfg.TokenCachePrefix + targetToken)
	require.NoError(t, err)
	assert.NotEmpty(t, payload, "revoking an absent role must not log out a normal user")
	uidToken, err := ctx.Cache().Get(uidTokenKey)
	require.NoError(t, err)
	assert.Equal(t, targetToken, uidToken)
}

func TestManagerDashboardReaderGrantRejectsIneligibleAccounts(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "dashboard-reader-ineligible-caller"
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
			uid := "dashboard-reader-ineligible-" + tt.name
			require.NoError(t, NewDB(ctx).Insert(&Model{
				UID: uid, Username: uid, Name: tt.name, ShortNo: fmt.Sprintf("ineligible-%d", i), Status: tt.status,
				Robot: tt.robot, IsDestroy: tt.isDestroy,
			}))
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut,
				"/v1/manager/user/"+uid+"/dashboard-read", nil)
			req.Header.Set("token", callerToken)
			route.ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "err.server.user.dashboard_reader_target_ineligible")
			role, err := NewDB(ctx).QueryRoleByUID(uid)
			require.NoError(t, err)
			assert.Empty(t, role)
		})
	}
}

func TestDashboardReaderRoleConflictCode(t *testing.T) {
	code, ok := codes.Lookup("err.server.user.manager_role_changed")
	require.True(t, ok)
	assert.Equal(t, http.StatusConflict, code.HTTPStatus)
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
