package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerLoginAllowsDashboardReader(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	require.NoError(t, testutil.CleanAllTables(ctx))

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
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"role":"`+appauth.ManagerRoleDashboardReader+`"`)
}

func TestManagerDashboardReaderGrantAndRevoke(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	require.NoError(t, testutil.CleanAllTables(ctx))

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
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	granted := doRoleRequest(http.MethodPut)
	require.Equal(t, http.StatusOK, granted.Code, granted.Body.String())
	role, err := NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Equal(t, appauth.ManagerRoleDashboardReader, role)
	cached, err := ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "grant must invalidate the target role cache")

	require.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+targetUID, appauth.ManagerRoleDashboardReader))
	revoked := doRoleRequest(http.MethodDelete)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
	role, err = NewDB(ctx).QueryRoleByUID(targetUID)
	require.NoError(t, err)
	assert.Empty(t, role)
	cached, err = ctx.Cache().Get(RoleCacheKeyPrefix + targetUID)
	require.NoError(t, err)
	assert.Empty(t, cached, "revoke must invalidate the target role cache")
}

func TestManagerDashboardReaderGrantRequiresSuperAdminAndProtectsSuperAdmin(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	require.NoError(t, testutil.CleanAllTables(ctx))

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
		s.GetRoute().ServeHTTP(w, req)
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
