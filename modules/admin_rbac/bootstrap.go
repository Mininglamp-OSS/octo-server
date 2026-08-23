package adminrbac

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
	"github.com/gocraft/dbr/v2"
)

const (
	WorkplaceSuperAdminRoleKey = "workplace.super_admin"
	WorkplaceAdminRoleKey      = "workplace.admin"

	// Exported aliases keep permission identifiers owned by the RBAC package.
	// Callers bind a fixed server-side permission without importing the generated
	// static contract into runtime handler packages.
	WorkplacePermissionAppRead            = authz.PermissionWorkplaceAppRead
	WorkplacePermissionAppWrite           = authz.PermissionWorkplaceAppWrite
	WorkplacePermissionBannerRead         = authz.PermissionWorkplaceBannerRead
	WorkplacePermissionBannerWrite        = authz.PermissionWorkplaceBannerWrite
	WorkplacePermissionCategoryRead       = authz.PermissionWorkplaceCategoryRead
	WorkplacePermissionCategoryWrite      = authz.PermissionWorkplaceCategoryWrite
	WorkplacePermissionCategoryAppReorder = authz.PermissionWorkplaceCategoryAppReorder
)

var workplaceRolePermissions = map[string][]string{
	WorkplaceSuperAdminRoleKey: {
		authz.PermissionWorkplaceAppRead,
		authz.PermissionWorkplaceAppWrite,
		authz.PermissionWorkplaceBannerRead,
		authz.PermissionWorkplaceBannerWrite,
		authz.PermissionWorkplaceCategoryRead,
		authz.PermissionWorkplaceCategoryWrite,
		authz.PermissionWorkplaceCategoryAppReorder,
	},
	WorkplaceAdminRoleKey: {
		authz.PermissionWorkplaceAppRead,
		authz.PermissionWorkplaceBannerRead,
		authz.PermissionWorkplaceCategoryRead,
		authz.PermissionWorkplaceCategoryAppReorder,
	},
}

type workplaceRoleDefinition struct {
	Key         string
	Name        string
	Description string
}

var workplaceRoleDefinitions = []workplaceRoleDefinition{
	{Key: WorkplaceSuperAdminRoleKey, Name: "Workplace super admin", Description: "全部 Workplace 全局管理权限"},
	{Key: WorkplaceAdminRoleKey, Name: "Workplace admin", Description: "Workplace 读取和分类应用排序权限"},
}

// BootstrapWorkplace creates or validates the two built-in Workplace roles
// and synchronizes currently available legacy admin identities. It is
// intentionally transactional and has no runtime feature flag: a conflicting
// pre-existing role stops the release from using this migration.
func (s *Service) BootstrapWorkplace() error {
	tx, err := s.store.session.Begin()
	if err != nil {
		return wrapStoreError("begin Workplace bootstrap", err)
	}
	defer tx.RollbackUnlessCommitted()

	for _, definition := range workplaceRoleDefinitions {
		if _, err := s.ensureWorkplaceRoleTx(tx, definition); err != nil {
			return err
		}
	}

	var users []struct {
		UID  string `db:"uid"`
		Role string `db:"role"`
	}
	if _, err := tx.Select("uid,role").From("user").
		Where("robot=0 AND status=? AND is_destroy=0 AND role IN (?,?)", activeStatus, "admin", "superAdmin").
		Load(&users); err != nil {
		return wrapStoreError("load Workplace bootstrap users", err)
	}

	changedUIDs := make([]string, 0, len(users))
	for _, user := range users {
		changed, err := s.syncManagerRoleTx(tx, user.UID, user.Role)
		if err != nil {
			return err
		}
		if changed {
			changedUIDs = append(changedUIDs, user.UID)
		}
	}

	if err := tx.Commit(); err != nil {
		return wrapStoreError("commit Workplace bootstrap", err)
	}
	for _, uid := range changedUIDs {
		if err := s.InvalidateUser(uid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ensureWorkplaceRoleTx(tx *dbr.Tx, definition workplaceRoleDefinition) (*Role, error) {
	role, err := s.store.roleByKeyTx(tx, definition.Key, true)
	if errors.Is(err, ErrRoleNotFound) {
		result, insertErr := tx.InsertInto("admin_rbac_role").
			Columns("role_key", "name", "description", "status", "authorization_version").
			Values(definition.Key, definition.Name, definition.Description, activeStatus, 1).Exec()
		if insertErr != nil {
			return nil, wrapStoreError("create Workplace role", insertErr)
		}
		id, insertErr := result.LastInsertId()
		if insertErr != nil {
			return nil, wrapStoreError("read Workplace role id", insertErr)
		}
		role = &Role{ID: id, RoleKey: definition.Key, Name: definition.Name, Description: definition.Description, Status: activeStatus, AuthorizationVersion: 1}
		for _, permissionKey := range workplaceRolePermissions[definition.Key] {
			if _, insertErr := tx.InsertInto("admin_rbac_role_permission").
				Columns("role_id", "permission_key").Values(role.ID, permissionKey).Exec(); insertErr != nil {
				return nil, wrapStoreError("create Workplace role permission", insertErr)
			}
		}
		return role, nil
	}
	if err != nil {
		return nil, wrapStoreError("load Workplace role", err)
	}
	if role.Status != activeStatus {
		return nil, fmt.Errorf("%w: role %s is disabled", ErrBootstrapConflict, definition.Key)
	}

	var permissions []string
	if _, err := tx.Select("permission_key").From("admin_rbac_role_permission").
		Where("role_id=?", role.ID).OrderAsc("permission_key").Load(&permissions); err != nil {
		return nil, wrapStoreError("load Workplace role permissions", err)
	}
	want := append([]string(nil), workplaceRolePermissions[definition.Key]...)
	sort.Strings(want)
	if len(permissions) != len(want) {
		return nil, fmt.Errorf("%w: role %s permission count mismatch", ErrBootstrapConflict, definition.Key)
	}
	for i := range permissions {
		if permissions[i] != want[i] {
			return nil, fmt.Errorf("%w: role %s permission mismatch", ErrBootstrapConflict, definition.Key)
		}
	}
	return role, nil
}

// SyncManagerRoleTx synchronizes one legacy system role inside the caller's
// transaction. It only owns the two built-in Workplace bindings and preserves
// unrelated custom RBAC roles.
func (s *Service) SyncManagerRoleTx(tx *dbr.Tx, uid, legacyRole string) (bool, error) {
	return s.syncManagerRoleTx(tx, uid, legacyRole)
}

func (s *Service) syncManagerRoleTx(tx *dbr.Tx, uid, legacyRole string) (bool, error) {
	if uid == "" {
		return false, ErrInvalidRequest
	}
	if legacyRole == "admin" || legacyRole == "superAdmin" {
		exists, err := s.store.userExistsTx(tx, uid)
		if err != nil {
			return false, wrapStoreError("check Workplace role user", err)
		}
		if !exists {
			return false, ErrUserNotFound
		}
	}

	target := ""
	if legacyRole == "admin" {
		target = WorkplaceAdminRoleKey
	} else if legacyRole == "superAdmin" {
		target = WorkplaceSuperAdminRoleKey
	}
	changed := false
	for _, definition := range workplaceRoleDefinitions {
		role, err := s.store.roleByKeyTx(tx, definition.Key, true)
		if err != nil {
			return false, err
		}
		if definition.Key == target {
			valueChanged, err := ensureUserRoleBindingTx(tx, uid, role.ID)
			if err != nil {
				return false, err
			}
			changed = changed || valueChanged
		} else {
			valueChanged, err := deleteUserRoleBindingTx(tx, uid, role.ID)
			if err != nil {
				return false, err
			}
			changed = changed || valueChanged
		}
	}
	return changed, nil
}

// RevokeManagerRolesTx removes only built-in Workplace bindings for a user.
func (s *Service) RevokeManagerRolesTx(tx *dbr.Tx, uid string) (bool, error) {
	if uid == "" {
		return false, ErrInvalidRequest
	}
	changed := false
	for _, definition := range workplaceRoleDefinitions {
		role, err := s.store.roleByKeyTx(tx, definition.Key, true)
		if errors.Is(err, ErrRoleNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		valueChanged, err := deleteUserRoleBindingTx(tx, uid, role.ID)
		if err != nil {
			return false, err
		}
		changed = changed || valueChanged
	}
	return changed, nil
}

// InvalidateUser exposes the post-commit user effective-permission cache
// deletion needed by identity mutation coordinators.
func (s *Service) InvalidateUser(uid string) error {
	return s.invalidateUser(uid)
}

func ensureUserRoleBindingTx(tx *dbr.Tx, uid string, roleID int64) (bool, error) {
	var binding struct {
		ID     int64 `db:"id"`
		Status int   `db:"status"`
	}
	if _, err := tx.Select("id,status").From("admin_rbac_user_role").
		Where("uid=? AND role_id=?", uid, roleID).Load(&binding); err != nil {
		return false, wrapStoreError("load Workplace user role binding", err)
	}
	if binding.ID == 0 {
		if _, err := tx.InsertInto("admin_rbac_user_role").Columns("uid", "role_id", "status").
			Values(uid, roleID, activeStatus).Exec(); err != nil {
			return false, wrapStoreError("create Workplace user role binding", err)
		}
		return true, nil
	}
	if binding.Status == activeStatus {
		return false, nil
	}
	if _, err := tx.Update("admin_rbac_user_role").Set("status", activeStatus).Where("id=?", binding.ID).Exec(); err != nil {
		return false, wrapStoreError("restore Workplace user role binding", err)
	}
	return true, nil
}

func deleteUserRoleBindingTx(tx *dbr.Tx, uid string, roleID int64) (bool, error) {
	var binding struct {
		ID int64 `db:"id"`
	}
	if _, err := tx.Select("id").From("admin_rbac_user_role").
		Where("uid=? AND role_id=?", uid, roleID).Load(&binding); err != nil {
		return false, wrapStoreError("load Workplace user role binding for revoke", err)
	}
	if binding.ID == 0 {
		return false, nil
	}
	if _, err := tx.DeleteFrom("admin_rbac_user_role").Where("id=?", binding.ID).Exec(); err != nil {
		return false, wrapStoreError("delete Workplace user role binding", err)
	}
	return true, nil
}
