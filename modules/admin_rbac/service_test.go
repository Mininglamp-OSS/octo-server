package adminrbac

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock, *fakePermissionCache) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	backend := newFakePermissionCache()
	return NewService(NewStore(conn.NewSession(nil)), NewPermissionCache(backend)), mock, backend
}

func TestChangeRolePermissionAdvancesVersionAfterSuccessfulCommit(t *testing.T) {
	service, mock, _ := newMockService(t)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='writer' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "writer", "Writer", "", activeStatus, 3, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_role_permission").WillReturnResult(sqlmock.NewResult(11, 1))
	mock.ExpectExec("UPDATE .*admin_rbac_role").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := service.ChangeRolePermission("writer", "user.read", true); err != nil {
		t.Fatalf("ChangeRolePermission: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestChangeRolePermissionIsIdempotentWithoutVersionUpdate(t *testing.T) {
	service, mock, _ := newMockService(t)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='writer' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "writer", "Writer", "", activeStatus, 3, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(11))
	mock.ExpectCommit()

	if err := service.ChangeRolePermission("writer", "user.read", true); err != nil {
		t.Fatalf("ChangeRolePermission: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestChangeUserRoleRollsBackOnBindingWriteFailure(t *testing.T) {
	service, mock, _ := newMockService(t)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid.*'u1'").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='writer' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, "writer", "Writer", "", activeStatus, 3, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_user_role.*").WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_user_role").
		WillReturnError(errors.New("write failed"))
	mock.ExpectRollback()

	if err := service.ChangeUserRole("u1", "writer", true); err == nil {
		t.Fatal("ChangeUserRole succeeded, want rollback error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestMutationValidationFailsBeforeOpeningTransaction(t *testing.T) {
	service, mock, _ := newMockService(t)
	if err := service.ChangeRolePermission("writer", "unknown.permission", true); !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("error = %v, want ErrInvalidPermission", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

func TestLoadEffectiveSnapshotTreatsNullPermissionAsEmpty(t *testing.T) {
	service, mock, _ := newMockService(t)

	mock.ExpectQuery("SELECT .*COALESCE.*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"role_id", "role_key", "role_status", "authorization_version", "permission_key"}).
			AddRow(7, "empty", activeStatus, 1, nil))

	snapshots, err := service.store.loadEffectiveSnapshot("u1")
	if err != nil {
		t.Fatalf("loadEffectiveSnapshot: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].RoleKey != "empty" || len(snapshots[0].Permissions) != 0 {
		t.Fatalf("snapshots = %+v, want one empty-permission role", snapshots)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestChangeUserRoleRejectsInactiveUser(t *testing.T) {
	service, mock, _ := newMockService(t)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='disabled'.*status=1.*is_destroy").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectRollback()

	if err := service.ChangeUserRole("disabled", "writer", true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("ChangeUserRole error = %v, want ErrUserNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestEffectivePermissionsRejectsInactiveUser(t *testing.T) {
	service, mock, _ := newMockService(t)

	mock.ExpectQuery("SELECT COUNT.*FROM .*user.*uid='disabled'.*status=1.*is_destroy").
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))

	if _, err := service.EffectivePermissions("disabled"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("EffectivePermissions error = %v, want ErrUserNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestUpdateRoleInvalidatesBeforeReadback(t *testing.T) {
	service, mock, backend := newMockService(t)
	now := time.Now()
	roleRows := sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
		AddRow(7, "reader", "Reader", "", activeStatus, 3, now, now)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='reader' FOR UPDATE").WillReturnRows(roleRows)
	mock.ExpectExec("UPDATE .*admin_rbac_role").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE .*role_key='reader'").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}))

	if _, err := service.UpdateRole("reader", "Reader", "", 0); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("UpdateRole error = %v, want post-commit readback error", err)
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != service.cache.roleKey("reader") {
		t.Fatalf("deleted cache keys = %v, want role cache invalidated before readback", backend.deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestCacheInvalidationLogContainsOperationAndRole(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	previous := zap.ReplaceGlobals(zap.New(core))
	defer previous()

	service, _, backend := newMockService(t)
	backend.deleteErr = errors.New("redis unavailable")
	if err := service.invalidateRole("reader"); !errors.Is(err, ErrCacheInvalidation) {
		t.Fatalf("invalidateRole error = %v, want ErrCacheInvalidation", err)
	}
	if logs.Len() != 1 {
		t.Fatalf("log entries = %d, want one structured error", logs.Len())
	}
	fields := logs.All()[0].ContextMap()
	if fields["operation"] != "invalidate_role" || fields["role_key"] != "reader" || fields["error"] != "redis unavailable" {
		t.Fatalf("log fields = %#v, want operation, role_key and error", fields)
	}
}
