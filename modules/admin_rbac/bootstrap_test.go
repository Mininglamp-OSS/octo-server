package adminrbac

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestWorkplaceRoleDefinitionsAreExact(t *testing.T) {
	wantSuperAdmin := []string{
		"workplace.app.read",
		"workplace.app.write",
		"workplace.banner.read",
		"workplace.banner.write",
		"workplace.category.read",
		"workplace.category.write",
		"workplace.category_app.reorder",
	}
	wantAdmin := []string{
		"workplace.app.read",
		"workplace.banner.read",
		"workplace.category.read",
		"workplace.category_app.reorder",
	}
	if got := workplaceRolePermissions[WorkplaceSuperAdminRoleKey]; !reflect.DeepEqual(got, wantSuperAdmin) {
		t.Fatalf("superAdmin permissions = %v, want %v", got, wantSuperAdmin)
	}
	if got := workplaceRolePermissions[WorkplaceAdminRoleKey]; !reflect.DeepEqual(got, wantAdmin) {
		t.Fatalf("admin permissions = %v, want %v", got, wantAdmin)
	}
}

func TestWorkplaceRolePermissionMatrix(t *testing.T) {
	checks := []struct {
		name        string
		permissions []string
		allowed     map[string]bool
	}{
		{name: "superAdmin", permissions: workplaceRolePermissions[WorkplaceSuperAdminRoleKey], allowed: map[string]bool{
			"workplace.app.read": true, "workplace.app.write": true,
			"workplace.banner.read": true, "workplace.banner.write": true,
			"workplace.category.read": true, "workplace.category.write": true,
			"workplace.category_app.reorder": true,
		}},
		{name: "admin", permissions: workplaceRolePermissions[WorkplaceAdminRoleKey], allowed: map[string]bool{
			"workplace.app.read": true, "workplace.app.write": false,
			"workplace.banner.read": true, "workplace.banner.write": false,
			"workplace.category.read": true, "workplace.category.write": false,
			"workplace.category_app.reorder": true,
		}},
		{name: "dashboardReader", permissions: nil, allowed: map[string]bool{
			"workplace.app.read": false, "workplace.category_app.reorder": false,
		}},
		{name: "marketAdmin", permissions: nil, allowed: map[string]bool{
			"workplace.app.read": false, "workplace.category_app.reorder": false,
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			result, err := Evaluate("u1", []RoleSnapshot{{
				RoleKey: check.name, Status: activeStatus, Permissions: check.permissions,
			}})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			for permission, want := range check.allowed {
				got, err := allowsEffective(result, permission)
				if err != nil || got != want {
					t.Errorf("allowsEffective(%q) = (%v, %v), want (%v, nil)", permission, got, err, want)
				}
			}
		})
	}
}

func TestBootstrapWorkplaceCreatesRolesIdempotently(t *testing.T) {
	service, mock, backend := newMockService(t)
	mock.ExpectBegin()
	expectBootstrapRoleCreate(mock, WorkplaceSuperAdminRoleKey, 7, workplaceRolePermissions[WorkplaceSuperAdminRoleKey])
	expectBootstrapRoleCreate(mock, WorkplaceAdminRoleKey, 8, workplaceRolePermissions[WorkplaceAdminRoleKey])
	mock.ExpectQuery("SELECT uid,role FROM user WHERE").WillReturnRows(sqlmock.NewRows([]string{"uid", "role"}))
	mock.ExpectCommit()

	if err := service.BootstrapWorkplace(); err != nil {
		t.Fatalf("BootstrapWorkplace: %v", err)
	}
	mock.ExpectBegin()
	expectBootstrapRoleExisting(mock, WorkplaceSuperAdminRoleKey, 7, workplaceRolePermissions[WorkplaceSuperAdminRoleKey])
	expectBootstrapRoleExisting(mock, WorkplaceAdminRoleKey, 8, workplaceRolePermissions[WorkplaceAdminRoleKey])
	mock.ExpectQuery("SELECT uid,role FROM user WHERE").WillReturnRows(sqlmock.NewRows([]string{"uid", "role"}))
	mock.ExpectCommit()
	if err := service.BootstrapWorkplace(); err != nil {
		t.Fatalf("repeat BootstrapWorkplace: %v", err)
	}
	if len(backend.deleted) != 0 {
		t.Fatalf("deleted cache keys = %v, want no user invalidation without bindings", backend.deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestBootstrapWorkplaceRejectsPermissionConflict(t *testing.T) {
	service, mock, _ := newMockService(t)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='workplace\\.super_admin' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, WorkplaceSuperAdminRoleKey, "Workplace super admin", "", activeStatus, 1, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").
		WillReturnRows(sqlmock.NewRows([]string{"permission_key"}).AddRow("workplace.app.read"))
	mock.ExpectRollback()

	if err := service.BootstrapWorkplace(); !errors.Is(err, ErrBootstrapConflict) {
		t.Fatalf("BootstrapWorkplace error = %v, want ErrBootstrapConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestSyncManagerRoleTxRejectsUnavailablePrincipal(t *testing.T) {
	service, mock, _ := newMockService(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid,robot,status,is_destroy FROM user WHERE uid='disabled' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"uid", "robot", "status", "is_destroy"}).AddRow("disabled", 0, 0, 0))
	mock.ExpectRollback()
	tx, err := service.store.session.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := service.SyncManagerRoleTx(tx, "disabled", "admin"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SyncManagerRoleTx error = %v, want ErrUserNotFound", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestSyncManagerRoleTxBindsOnlyMappedWorkplaceRole(t *testing.T) {
	service, mock, _ := newMockService(t)
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT uid,robot,status,is_destroy FROM user WHERE uid='u1' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"uid", "robot", "status", "is_destroy"}).AddRow("u1", 0, activeStatus, 0))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='workplace\\.super_admin' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(7, WorkplaceSuperAdminRoleKey, "Super", "", activeStatus, 1, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_user_role").WillReturnResult(sqlmock.NewResult(11, 1))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='workplace\\.admin' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(8, WorkplaceAdminRoleKey, "Admin", "", activeStatus, 1, now, now))
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_user_role.*").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectCommit()

	tx, err := service.store.session.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	changed, err := service.SyncManagerRoleTx(tx, "u1", "superAdmin")
	if err != nil || !changed {
		t.Fatalf("SyncManagerRoleTx = (%v, %v), want (true, nil)", changed, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func expectBootstrapRoleCreate(mock sqlmock.Sqlmock, roleKey string, roleID int64, permissions []string) {
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='" + roleKeyPatternForSQL(roleKey) + "' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}))
	mock.ExpectExec("INSERT INTO .*admin_rbac_role").WillReturnResult(sqlmock.NewResult(roleID, 1))
	for range permissions {
		mock.ExpectExec("INSERT INTO .*admin_rbac_role_permission").WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

func expectBootstrapRoleExisting(mock sqlmock.Sqlmock, roleKey string, roleID int64, permissions []string) {
	now := time.Now()
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role WHERE role_key='" + roleKeyPatternForSQL(roleKey) + "' FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role_key", "name", "description", "status", "authorization_version", "created_at", "updated_at"}).
			AddRow(roleID, roleKey, roleKey, "", activeStatus, 1, now, now))
	permissionRows := sqlmock.NewRows([]string{"permission_key"})
	for _, permission := range permissions {
		permissionRows.AddRow(permission)
	}
	mock.ExpectQuery("SELECT .*FROM .*admin_rbac_role_permission.*").WillReturnRows(permissionRows)
}

func roleKeyPatternForSQL(roleKey string) string {
	result := ""
	for _, r := range roleKey {
		if r == '.' {
			result += `\.`
		} else {
			result += string(r)
		}
	}
	return result
}
