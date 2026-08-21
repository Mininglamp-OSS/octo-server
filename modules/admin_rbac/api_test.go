package adminrbac

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func testWKContext(t *testing.T, method, path, body string) *wkhttp.Context {
	c, _ := testWKContextWithRecorder(t, method, path, body)
	return c
}

func testWKContextWithRecorder(t *testing.T, method, path, body string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	return &wkhttp.Context{Context: ginContext}, recorder
}

func newMockAPI(t *testing.T) (*API, sqlmock.Sqlmock, func()) {
	api, mock, _, cleanup := newMockAPIWithCache(t)
	return api, mock, cleanup
}

func newMockAPIWithCache(t *testing.T) (*API, sqlmock.Sqlmock, *fakePermissionCache, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	backend := newFakePermissionCache()
	return &API{svc: NewService(NewStore(conn.NewSession(nil)), NewPermissionCache(backend))}, mock, backend, func() {
		_ = rawDB.Close()
	}
}

func setRole(c *wkhttp.Context, role string) {
	c.Set("role", role)
}

func setRouteParams(c *wkhttp.Context, params ...string) {
	for i := 0; i+1 < len(params); i += 2 {
		c.Params = append(c.Params, gin.Param{Key: params[i], Value: params[i+1]})
	}
}

func TestStrictJSONRejectsBusinessResourceFields(t *testing.T) {
	c := testWKContext(t, "POST", "/v1/manager/rbac/roles", `{"role_key":"reader","name":"Reader","resource_id":"group-1"}`)
	var request createRoleRequest
	if err := bindStrictJSON(c, &request); err == nil {
		t.Fatal("bindStrictJSON accepted resource_id")
	}
}

func TestResourceSelectorsAreRejected(t *testing.T) {
	c := testWKContext(t, "GET", "/v1/manager/rbac/users/u1/effective-permissions?space_id=s1", "")
	if err := rejectResourceSelectors(c); err != ErrInvalidScope {
		t.Fatalf("error = %v, want ErrInvalidScope", err)
	}
	for _, key := range []string{"resource", "scope", "member_uid", "app_id", "bot_id"} {
		c := testWKContext(t, "GET", "/v1/manager/rbac/roles?"+key+"=x", "")
		if err := rejectResourceSelectors(c); err != ErrInvalidScope {
			t.Fatalf("%s error = %v, want ErrInvalidScope", key, err)
		}
	}
}

func TestGlobalRoleAllowlistDoesNotIncludeFixedBusinessRoles(t *testing.T) {
	if err := validateGlobalScope("", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := validateGlobalScope("group-1", "", "", ""); err != ErrInvalidScope {
		t.Fatalf("group scope error = %v, want ErrInvalidScope", err)
	}
}

func TestRBACHandlersEnforceManagerAndSuperAdminAllowlist(t *testing.T) {
	api := &API{}

	managerCheck, managerRecorder := testWKContextWithRecorder(t, http.MethodGet, "/v1/manager/rbac/roles", "")
	api.listRoles(managerCheck)
	if managerRecorder.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated role check status = %d, want %d", managerRecorder.Code, http.StatusForbidden)
	}

	adminCreate, adminRecorder := testWKContextWithRecorder(t, http.MethodPost, "/v1/manager/rbac/roles", `{}`)
	setRole(adminCreate, string(wkhttp.Admin))
	api.createRole(adminCreate)
	if adminRecorder.Code != http.StatusForbidden {
		t.Fatalf("admin create status = %d, want %d", adminRecorder.Code, http.StatusForbidden)
	}
}

func TestRBACHandlersRejectNonManagerRolesAcrossAllRoutes(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		set    func(*wkhttp.Context)
	}{
		{name: "list roles", method: http.MethodGet, path: "/v1/manager/rbac/roles", set: func(*wkhttp.Context) {}},
		{name: "create role", method: http.MethodPost, path: "/v1/manager/rbac/roles", set: func(*wkhttp.Context) {}},
		{name: "update role", method: http.MethodPut, path: "/v1/manager/rbac/roles/reader", set: func(c *wkhttp.Context) { setRouteParams(c, "role_key", "reader") }},
		{name: "list role permissions", method: http.MethodGet, path: "/v1/manager/rbac/roles/reader/permissions", set: func(c *wkhttp.Context) { setRouteParams(c, "role_key", "reader") }},
		{name: "grant role permission", method: http.MethodPut, path: "/v1/manager/rbac/roles/reader/permissions/user.read", set: func(c *wkhttp.Context) { setRouteParams(c, "role_key", "reader", "permission_key", "user.read") }},
		{name: "revoke role permission", method: http.MethodDelete, path: "/v1/manager/rbac/roles/reader/permissions/user.read", set: func(c *wkhttp.Context) { setRouteParams(c, "role_key", "reader", "permission_key", "user.read") }},
		{name: "list user roles", method: http.MethodGet, path: "/v1/manager/rbac/users/u1/roles", set: func(c *wkhttp.Context) { setRouteParams(c, "uid", "u1") }},
		{name: "grant user role", method: http.MethodPut, path: "/v1/manager/rbac/users/u1/roles/reader", set: func(c *wkhttp.Context) { setRouteParams(c, "uid", "u1", "role_key", "reader") }},
		{name: "revoke user role", method: http.MethodDelete, path: "/v1/manager/rbac/users/u1/roles/reader", set: func(c *wkhttp.Context) { setRouteParams(c, "uid", "u1", "role_key", "reader") }},
		{name: "effective permissions", method: http.MethodGet, path: "/v1/manager/rbac/users/u1/effective-permissions", set: func(c *wkhttp.Context) { setRouteParams(c, "uid", "u1") }},
	}
	for _, role := range []string{"", "dashboardReader", "marketAdmin"} {
		for _, tc := range cases {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				api, _, cleanup := newMockAPI(t)
				defer cleanup()
				c, recorder := testWKContextWithRecorder(t, tc.method, tc.path, "")
				setRole(c, role)
				tc.set(c)
				invokeRBACRouteForTest(api, tc.name, c)
				if recorder.Code != http.StatusForbidden {
					t.Fatalf("role %q route %s status = %d, want %d", role, tc.name, recorder.Code, http.StatusForbidden)
				}
			})
		}
	}
}

func invokeRBACRouteForTest(api *API, name string, c *wkhttp.Context) {
	switch name {
	case "list roles":
		api.listRoles(c)
	case "create role":
		api.createRole(c)
	case "update role":
		api.updateRole(c)
	case "list role permissions":
		api.listRolePermissions(c)
	case "grant role permission":
		api.grantRolePermission(c)
	case "revoke role permission":
		api.revokeRolePermission(c)
	case "list user roles":
		api.listUserRoles(c)
	case "grant user role":
		api.grantUserRole(c)
	case "revoke user role":
		api.revokeUserRole(c)
	case "effective permissions":
		api.effectivePermissions(c)
	}
}

func TestCreateRoleReturnsOnlyAfterDatabaseCommit(t *testing.T) {
	api, mock, cleanup := newMockAPI(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO .*admin_rbac_role").
		WillReturnResult(sqlmock.NewResult(9, 1))
	c, recorder := testWKContextWithRecorder(t, http.MethodPost, "/v1/manager/rbac/roles", `{"role_key":"reader","name":"Reader","description":"read-only"}`)
	setRole(c, string(wkhttp.SuperAdmin))
	api.createRole(c)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"role_key":"reader"`) {
		t.Fatalf("create response = (%d, %s), want committed role", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestUpdateRoleReturnsCommittedState(t *testing.T) {
	api, mock, cleanup := newMockAPI(t)
	defer cleanup()

	now := time.Now()
	roleRows := func(name string) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "reader", name, "", activeStatus, 3, now, now)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='reader' FOR UPDATE").WillReturnRows(roleRows("Reader"))
	mock.ExpectExec("UPDATE .*admin_rbac_role").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE .*role_key='reader'").WillReturnRows(roleRows("Updated Reader"))

	c, recorder := testWKContextWithRecorder(t, http.MethodPut, "/v1/manager/rbac/roles/reader", `{"name":"Updated Reader","description":"updated"}`)
	setRole(c, string(wkhttp.SuperAdmin))
	setRouteParams(c, "role_key", "reader")
	api.updateRole(c)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"Updated Reader"`) {
		t.Fatalf("update response = (%d, %s), want committed state", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestRolePermissionMutationRejectsUnknownPermissionAndResourceBody(t *testing.T) {
	api := &API{}

	unknown, unknownRecorder := testWKContextWithRecorder(t, http.MethodPut, "/v1/manager/rbac/roles/reader/permissions/unknown.permission", "")
	setRole(unknown, string(wkhttp.SuperAdmin))
	setRouteParams(unknown, "role_key", "reader", "permission_key", "unknown.permission")
	api.grantRolePermission(unknown)
	if unknownRecorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown permission status = %d, want %d", unknownRecorder.Code, http.StatusBadRequest)
	}

	resource, resourceRecorder := testWKContextWithRecorder(t, http.MethodPut, "/v1/manager/rbac/roles/reader/permissions/user.read", `{"resource_id":"group-1"}`)
	setRole(resource, string(wkhttp.SuperAdmin))
	setRouteParams(resource, "role_key", "reader", "permission_key", "user.read")
	api.grantRolePermission(resource)
	if resourceRecorder.Code != http.StatusBadRequest {
		t.Fatalf("resource body status = %d, want %d", resourceRecorder.Code, http.StatusBadRequest)
	}
}

func TestRolePermissionTransactionFailureNeverReturnsSuccess(t *testing.T) {
	api, mock, cleanup := newMockAPI(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='reader' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "reader", "Reader", "", activeStatus, 3, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_role_permission").WillReturnError(errors.New("write failed"))
	mock.ExpectRollback()

	c, recorder := testWKContextWithRecorder(t, http.MethodPut, "/v1/manager/rbac/roles/reader/permissions/user.read", "")
	setRole(c, string(wkhttp.SuperAdmin))
	setRouteParams(c, "role_key", "reader", "permission_key", "user.read")
	api.grantRolePermission(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("transaction failure status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestCommittedRolePermissionWithCacheDeleteFailureReturnsObservableError(t *testing.T) {
	api, mock, backend, cleanup := newMockAPIWithCache(t)
	defer cleanup()
	backend.deleteErr = errors.New("redis unavailable")

	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='reader' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "reader", "Reader", "", activeStatus, 3, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_role_permission").WillReturnResult(sqlmock.NewResult(11, 1))
	mock.ExpectExec("UPDATE .*admin_rbac_role").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	c, recorder := testWKContextWithRecorder(t, http.MethodPut, "/v1/manager/rbac/roles/reader/permissions/user.read", "")
	setRole(c, string(wkhttp.SuperAdmin))
	setRouteParams(c, "role_key", "reader", "permission_key", "user.read")
	api.grantRolePermission(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("cache deletion failure status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "redis unavailable") {
		t.Fatalf("cache deletion failure leaked backend detail: %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestEffectivePermissionsReturnsRoleVersions(t *testing.T) {
	api, mock, cleanup := newMockAPI(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='u1'").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .*authorization_version.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_key", "authorization_version"}).AddRow("reader", 4))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_id", "role_key", "role_status", "authorization_version", "permission_key"}).
			AddRow(7, "reader", activeStatus, 4, "user.read"))

	c, recorder := testWKContextWithRecorder(t, http.MethodGet, "/v1/manager/rbac/users/u1/effective-permissions", "")
	setRole(c, string(wkhttp.Admin))
	setRouteParams(c, "uid", "u1")
	api.effectivePermissions(c)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"user.read"`) || !strings.Contains(body, `"authorization_version":4`) {
		t.Fatalf("effective response = (%d, %s), want permission and role version", recorder.Code, body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestRBACRouteRegistersGlobalResourcesOnly(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	(&API{ctx: ctx}).Route(route)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/manager/rbac/roles"},
		{http.MethodPost, "/v1/manager/rbac/roles"},
		{http.MethodPut, "/v1/manager/rbac/roles/reader"},
		{http.MethodGet, "/v1/manager/rbac/roles/reader/permissions"},
		{http.MethodPut, "/v1/manager/rbac/roles/reader/permissions/user.read"},
		{http.MethodDelete, "/v1/manager/rbac/roles/reader/permissions/user.read"},
		{http.MethodGet, "/v1/manager/rbac/users/u1/roles"},
		{http.MethodPut, "/v1/manager/rbac/users/u1/roles/reader"},
		{http.MethodDelete, "/v1/manager/rbac/users/u1/roles/reader"},
		{http.MethodGet, "/v1/manager/rbac/users/u1/effective-permissions"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		route.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want %d from AuthMiddleware", tc.method, tc.path, recorder.Code, http.StatusUnauthorized)
		}
	}

	for _, path := range []string{
		"/v1/groups/g1/members",
		"/v1/space/s1/permissions",
		"/v1/robot/r1/permissions",
		"/v1/manager/rbac/roles/reader/audit",
		"/v1/manager/rbac/users/u1/role-history",
	} {
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("out-of-scope path %s status = %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}

}
