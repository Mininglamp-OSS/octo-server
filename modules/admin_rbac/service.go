package adminrbac

import (
	"fmt"
	"strings"

	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

type Service struct {
	store *Store
	cache *PermissionCache
}

func NewService(store *Store, permissionCache *PermissionCache) *Service {
	return &Service{store: store, cache: permissionCache}
}

func (s *Service) ListRoles() ([]Role, error) {
	return s.store.ListRoles()
}

func (s *Service) svcRole(roleKey string) (*Role, error) {
	roleKey = strings.TrimSpace(roleKey)
	if err := validateRoleKey(roleKey); err != nil {
		return nil, err
	}
	return s.store.GetRole(roleKey)
}

func (s *Service) GetRole(roleKey string) (*Role, error) {
	return s.svcRole(roleKey)
}

func (s *Service) CreateRole(roleKey, name, description string) (*Role, error) {
	roleKey = strings.TrimSpace(roleKey)
	if err := validateRoleKey(roleKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidRequest
	}
	return s.store.createRole(roleKey, strings.TrimSpace(name), strings.TrimSpace(description))
}

func (s *Service) UpdateRole(roleKey, name, description string, status *int) (*Role, error) {
	roleKey = strings.TrimSpace(roleKey)
	if err := validateRoleKey(roleKey); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidRequest
	}
	if status != nil && *status != 0 && *status != activeStatus {
		return nil, ErrInvalidRequest
	}
	tx, err := s.store.session.Begin()
	if err != nil {
		return nil, wrapStoreError("begin role update", err)
	}
	defer tx.RollbackUnlessCommitted()
	role, err := s.store.roleByKeyTx(tx, roleKey, true)
	if err != nil {
		return nil, err
	}
	effectiveStatus := role.Status
	if status != nil {
		effectiveStatus = *status
	}
	statusChanged := effectiveStatus != role.Status
	if !statusChanged {
		_, err = tx.Update("admin_rbac_role").Set("name", name).
			Set("description", strings.TrimSpace(description)).Where("id=?", role.ID).Exec()
	} else {
		_, err = tx.Update("admin_rbac_role").Set("name", name).
			Set("description", strings.TrimSpace(description)).Set("status", effectiveStatus).
			Set("authorization_version", dbr.Expr("authorization_version + 1")).Where("id=?", role.ID).Exec()
	}
	if err != nil {
		return nil, wrapStoreError("update role", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, wrapStoreError("commit role update", err)
	}
	if statusChanged {
		if err := s.invalidateRole(roleKey); err != nil {
			return nil, err
		}
	}
	updated, err := s.store.GetRole(roleKey)
	if err != nil {
		return nil, wrapStoreError("load updated role", err)
	}
	return updated, nil
}

func (s *Service) RolePermissions(roleKey string) ([]string, error) {
	roleKey = strings.TrimSpace(roleKey)
	if err := validateRoleKey(roleKey); err != nil {
		return nil, err
	}
	if _, err := s.store.GetRole(roleKey); err != nil {
		return nil, err
	}
	return s.store.listRolePermissions(roleKey)
}

func (s *Service) ChangeUserRole(uid, roleKey string, bind bool) error {
	uid = strings.TrimSpace(uid)
	roleKey = strings.TrimSpace(roleKey)
	if uid == "" {
		return ErrInvalidRequest
	}
	if err := validateRoleKey(roleKey); err != nil {
		return err
	}
	tx, err := s.store.session.Begin()
	if err != nil {
		return wrapStoreError("begin user role change", err)
	}
	defer tx.RollbackUnlessCommitted()
	if bind {
		exists, err := s.store.userExistsTx(tx, uid)
		if err != nil {
			return wrapStoreError("check user", err)
		}
		if !exists {
			return ErrUserNotFound
		}
	}
	role, err := s.store.roleByKeyTx(tx, roleKey, true)
	if err != nil {
		return err
	}
	if bind && role.Status != activeStatus {
		return ErrRoleDisabled
	}
	var binding struct {
		ID     int64 `db:"id"`
		Status int   `db:"status"`
	}
	_, err = tx.Select("id,status").From("admin_rbac_user_role").
		Where("uid=? AND role_id=?", uid, role.ID).Load(&binding)
	if err != nil {
		return wrapStoreError("load user role binding", err)
	}
	changed := false
	if bind {
		if binding.ID == 0 {
			_, err = tx.InsertInto("admin_rbac_user_role").Columns("uid", "role_id", "status").Values(uid, role.ID, activeStatus).Exec()
			changed = true
		} else if binding.Status != activeStatus {
			_, err = tx.Update("admin_rbac_user_role").Set("status", activeStatus).Where("id=?", binding.ID).Exec()
			changed = true
		}
	} else if binding.ID != 0 {
		_, err = tx.DeleteFrom("admin_rbac_user_role").Where("id=?", binding.ID).Exec()
		changed = true
	}
	if err != nil {
		return wrapStoreError("change user role binding", err)
	}
	if err := tx.Commit(); err != nil {
		return wrapStoreError("commit user role change", err)
	}
	if changed {
		return s.invalidateUser(uid)
	}
	return nil
}

func (s *Service) ChangeRolePermission(roleKey, permissionKey string, bind bool) error {
	roleKey = strings.TrimSpace(roleKey)
	permissionKey = strings.TrimSpace(permissionKey)
	if err := validateRoleKey(roleKey); err != nil {
		return err
	}
	if err := validatePermissionKey(permissionKey); err != nil {
		return err
	}
	tx, err := s.store.session.Begin()
	if err != nil {
		return wrapStoreError("begin role permission change", err)
	}
	defer tx.RollbackUnlessCommitted()
	role, err := s.store.roleByKeyTx(tx, roleKey, true)
	if err != nil {
		return err
	}
	if bind && role.Status != activeStatus {
		return ErrRoleDisabled
	}
	var binding struct {
		ID int64 `db:"id"`
	}
	_, err = tx.Select("id").From("admin_rbac_role_permission").
		Where("role_id=? AND permission_key=?", role.ID, permissionKey).Load(&binding)
	if err != nil {
		return wrapStoreError("load role permission binding", err)
	}
	changed := false
	if bind && binding.ID == 0 {
		_, err = tx.InsertInto("admin_rbac_role_permission").Columns("role_id", "permission_key").Values(role.ID, permissionKey).Exec()
		changed = true
	} else if !bind && binding.ID != 0 {
		_, err = tx.DeleteFrom("admin_rbac_role_permission").Where("id=?", binding.ID).Exec()
		changed = true
	}
	if err != nil {
		return wrapStoreError("change role permission binding", err)
	}
	if changed {
		if _, err = tx.Update("admin_rbac_role").Set("authorization_version", dbr.Expr("authorization_version + 1")).Where("id=?", role.ID).Exec(); err != nil {
			return wrapStoreError("advance role authorization version", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return wrapStoreError("commit role permission change", err)
	}
	if changed {
		return s.invalidateRole(roleKey)
	}
	return nil
}

func (s *Service) UserRoles(uid string) ([]UserRole, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, ErrInvalidRequest
	}
	if exists, err := s.store.UserExists(uid); err != nil {
		return nil, err
	} else if !exists {
		return nil, ErrUserNotFound
	}
	return s.store.listUserRoles(uid)
}

func (s *Service) EffectivePermissions(uid string) (EffectivePermissions, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return EffectivePermissions{}, ErrInvalidRequest
	}
	if exists, err := s.store.UserExists(uid); err != nil {
		return EffectivePermissions{}, err
	} else if !exists {
		return EffectivePermissions{}, ErrUserNotFound
	}
	roleVersions, err := s.store.loadRoleVersions(uid)
	if err != nil {
		return EffectivePermissions{}, wrapStoreError("load effective role versions", err)
	}
	if cached, ok, err := s.cache.Get(uid, roleVersions); err != nil {
		return EffectivePermissions{}, err
	} else if ok {
		return cached, nil
	}
	snapshots, err := s.store.loadEffectiveSnapshot(uid)
	if err != nil {
		return EffectivePermissions{}, wrapStoreError("load effective permission snapshot", err)
	}
	result, err := Evaluate(uid, snapshots)
	if err != nil {
		return EffectivePermissions{}, err
	}
	// Cache population is best effort. A read remains correct on a cache
	// outage because the database snapshot is authoritative.
	_ = s.cache.Set(result)
	return result, nil
}

func (s *Service) invalidateUser(uid string) error {
	if err := s.cache.InvalidateUser(uid); err != nil {
		zap.L().Error("admin rbac cache invalidation failed",
			zap.String("operation", "invalidate_user"),
			zap.String("uid", uid),
			zap.Error(err),
		)
		return fmt.Errorf("%w: user=%s: %v", ErrCacheInvalidation, uid, err)
	}
	return nil
}

func (s *Service) invalidateRole(roleKey string) error {
	if err := s.cache.InvalidateRole(roleKey); err != nil {
		zap.L().Error("admin rbac cache invalidation failed",
			zap.String("operation", "invalidate_role"),
			zap.String("role_key", roleKey),
			zap.Error(err),
		)
		return fmt.Errorf("%w: role=%s: %v", ErrCacheInvalidation, roleKey, err)
	}
	return nil
}
