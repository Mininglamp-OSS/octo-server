package user

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	adminrbac "github.com/Mininglamp-OSS/octo-server/modules/admin_rbac"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func TestAdminIdentityChangesSynchronizeWorkplaceBinding(t *testing.T) {
	route, ctx, manager := newManagerRouteOnly(t)
	callerToken := "admin-rbac-sync-caller"
	cacheCfg := ctx.GetConfig().Cache
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))

	body, err := json.Marshal(map[string]string{
		"login_name": "rbac-sync-admin",
		"name":       "RBAC Sync Admin",
		"password":   "Strong!123",
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/user/admin", bytes.NewReader(body))
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var created Model
	_, err = ctx.DB().Select("*").From("user").Where("username=?", "rbac-sync-admin").Load(&created)
	require.NoError(t, err)
	require.NotEmpty(t, created.UID)
	assertWorkplaceBindingCount(t, ctx, created.UID, 1)
	require.NoError(t, manager.deleteAdminUser(context.Background(), created.UID))
	assertWorkplaceBindingCount(t, ctx, created.UID, 0)
	deleted, err := manager.userDB.QueryByUID(created.UID)
	require.NoError(t, err)
	require.Nil(t, deleted)

	downgradeUID := "rbac-sync-downgrade"
	require.NoError(t, manager.userDB.Insert(&Model{
		UID: downgradeUID, Username: downgradeUID, Name: "RBAC Sync Downgrade",
		Role: string(wkhttp.Admin), Status: StatusEnable.Int(),
	}))
	require.NoError(t, manager.rbac.ChangeUserRole(downgradeUID, adminrbac.WorkplaceAdminRoleKey, true))

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/v1/manager/user/%s/dashboard-read", downgradeUID), nil)
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assertWorkplaceBindingCount(t, ctx, downgradeUID, 0)
}

func assertWorkplaceBindingCount(t *testing.T, ctx interface{ DB() *dbr.Session }, uid string, want int64) {
	t.Helper()
	var count int64
	_, err := ctx.DB().Select("COUNT(*)").From("admin_rbac_user_role").Where("uid=? AND status=1", uid).Load(&count)
	require.NoError(t, err)
	require.Equal(t, want, count)
}
