package workplace

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	adminrbac "github.com/Mininglamp-OSS/octo-server/modules/admin_rbac"
	"github.com/Mininglamp-OSS/octo-server/pkg/log"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestRequirePermissionAllowsOnlyEffectiveRBACPermission(t *testing.T) {
	manager, mock, cleanup := newPermissionTestManager(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='u1'").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .*authorization_version.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_key", "authorization_version"}).AddRow("workplace.admin", 1))
	mock.ExpectQuery("SELECT .*COALESCE.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_id", "role_key", "role_status", "authorization_version", "permission_key"}).
			AddRow(7, "workplace.admin", 1, 1, adminrbac.WorkplacePermissionAppRead))

	c, _ := permissionTestContext(t)
	c.Set("uid", "u1")
	if !manager.requirePermission(c, adminrbac.WorkplacePermissionAppRead) {
		t.Fatal("requirePermission denied an effective RBAC permission")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestRequirePermissionMapsDenyAndValidationFailuresToForbidden(t *testing.T) {
	manager, mock, cleanup := newPermissionTestManager(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='u1'").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .*authorization_version.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_key", "authorization_version"}).AddRow("workplace.admin", 1))
	mock.ExpectQuery("SELECT .*COALESCE.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_id", "role_key", "role_status", "authorization_version", "permission_key"}).
			AddRow(7, "workplace.admin", 1, 1, adminrbac.WorkplacePermissionAppRead))

	c, recorder := permissionTestContext(t)
	c.Set("uid", "u1")
	if manager.requirePermission(c, adminrbac.WorkplacePermissionAppWrite) {
		t.Fatal("requirePermission allowed a missing RBAC permission")
	}
	if !strings.Contains(recorder.Body.String(), "You do not have permission") {
		t.Fatalf("deny response = %s, want shared forbidden", recorder.Body.String())
	}

	c, recorder = permissionTestContext(t)
	if manager.requirePermission(c, "unknown.permission") {
		t.Fatal("requirePermission allowed an unknown permission")
	}
	if !strings.Contains(recorder.Body.String(), "You do not have permission") {
		t.Fatalf("invalid permission response = %s, want shared forbidden", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestRequirePermissionMapsStorageFailureToGenericInternal(t *testing.T) {
	manager, mock, cleanup := newPermissionTestManager(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='u1'").WillReturnError(errors.New("database secret: password=hidden"))

	c, recorder := permissionTestContext(t)
	c.Set("uid", "u1")
	if manager.requirePermission(c, adminrbac.WorkplacePermissionAppRead) {
		t.Fatal("requirePermission allowed a storage failure")
	}
	if !strings.Contains(recorder.Body.String(), "Internal server error") {
		t.Fatalf("storage failure response = %s, want shared internal", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "password=hidden") {
		t.Fatalf("response leaked storage error: %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func newPermissionTestManager(t *testing.T) (*manager, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	service := adminrbac.NewService(adminrbac.NewStore(conn.NewSession(nil)), adminrbac.NewPermissionCache(common.NewMemoryCache()))
	return &manager{Log: log.NewTLog("Workplace_permission_test"), rbac: service}, mock, func() { _ = rawDB.Close() }
}

func permissionTestContext(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1/manager/workplace/app", nil)
	return &wkhttp.Context{Context: ginContext}, recorder
}
