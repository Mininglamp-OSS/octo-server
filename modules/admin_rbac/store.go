package adminrbac

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gocraft/dbr/v2"
)

type Store struct {
	session *dbr.Session
}

func NewStore(session *dbr.Session) *Store {
	return &Store{session: session}
}

func (s *Store) ListRoles() ([]Role, error) {
	var roles []Role
	_, err := s.session.Select("id,role_key,name,description,status,authorization_version,created_at,updated_at").
		From("admin_rbac_role").OrderAsc("role_key").Load(&roles)
	return roles, err
}

func (s *Store) GetRole(roleKey string) (*Role, error) {
	var role Role
	_, err := s.session.Select("id,role_key,name,description,status,authorization_version,created_at,updated_at").
		From("admin_rbac_role").Where("role_key=?", roleKey).Load(&role)
	if err != nil {
		return nil, err
	}
	if role.RoleKey == "" {
		return nil, ErrRoleNotFound
	}
	return &role, nil
}

func (s *Store) UserExists(uid string) (bool, error) {
	var count int64
	_, err := s.session.Select("COUNT(*)").From("user").Where("uid=? AND robot=0 AND status=? AND is_destroy=0", uid, activeStatus).Load(&count)
	return count > 0, err
}

func (s *Store) createRole(roleKey, name, description string) (*Role, error) {
	result, err := s.session.InsertInto("admin_rbac_role").
		Columns("role_key", "name", "description", "status", "authorization_version").
		Values(roleKey, name, description, activeStatus, 1).Exec()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return nil, ErrAlreadyExists
		}
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Role{ID: id, RoleKey: roleKey, Name: name, Description: description, Status: activeStatus, AuthorizationVersion: 1}, nil
}

func (s *Store) updateRole(roleKey, name, description string, status int) error {
	result, err := s.session.Update("admin_rbac_role").
		Set("name", name).Set("description", description).Set("status", status).
		Where("role_key=?", roleKey).Exec()
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrRoleNotFound
	}
	return nil
}

func (s *Store) roleByKeyTx(tx *dbr.Tx, roleKey string, forUpdate bool) (*Role, error) {
	query := "SELECT id,role_key,name,description,status,authorization_version,created_at,updated_at FROM admin_rbac_role WHERE role_key=?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var role Role
	_, err := tx.SelectBySql(query, roleKey).Load(&role)
	if err != nil {
		return nil, err
	}
	if role.RoleKey == "" {
		return nil, ErrRoleNotFound
	}
	return &role, nil
}

func (s *Store) userExistsTx(tx *dbr.Tx, uid string) (bool, error) {
	var principal struct {
		UID       string `db:"uid"`
		Robot     int    `db:"robot"`
		Status    int    `db:"status"`
		IsDestroy int    `db:"is_destroy"`
	}
	err := tx.SelectBySql("SELECT uid,robot,status,is_destroy FROM user WHERE uid=? FOR UPDATE", uid).LoadOne(&principal)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return principal.Robot == 0 && principal.Status == activeStatus && principal.IsDestroy == 0, nil
}

func (s *Store) listRolePermissions(roleKey string) ([]string, error) {
	var permissions []string
	_, err := s.session.Select("rp.permission_key").From("admin_rbac_role_permission rp").
		Join("admin_rbac_role r", "r.id=rp.role_id").
		Where("r.role_key=?", roleKey).OrderAsc("rp.permission_key").Load(&permissions)
	return permissions, err
}

func (s *Store) listUserRoles(uid string) ([]UserRole, error) {
	var roles []UserRole
	_, err := s.session.Select("ur.uid,r.role_key,r.name role_name,r.status role_status,ur.status,ur.created_at").
		From("admin_rbac_user_role ur").
		Join("admin_rbac_role r", "r.id=ur.role_id").
		Join("user u", "u.uid=ur.uid AND u.robot=0 AND u.status=1 AND u.is_destroy=0").
		Where("ur.uid=?", uid).OrderAsc("r.role_key").Load(&roles)
	return roles, err
}

func (s *Store) loadEffectiveSnapshot(uid string) ([]RoleSnapshot, error) {
	var rows []rolePermissionRow
	_, err := s.session.Select("r.id role_id,r.role_key,r.status role_status,r.authorization_version,COALESCE(rp.permission_key,'') permission_key").
		From("admin_rbac_user_role ur").
		Join("admin_rbac_role r", "r.id=ur.role_id").
		Join("user u", "u.uid=ur.uid AND u.robot=0 AND u.status=1 AND u.is_destroy=0").
		LeftJoin("admin_rbac_role_permission rp", "rp.role_id=r.id").
		Where("ur.uid=? AND ur.status=?", uid, activeStatus).
		OrderAsc("r.role_key").OrderAsc("rp.permission_key").Load(&rows)
	if err != nil {
		return nil, err
	}
	byRole := make(map[string]*RoleSnapshot, len(rows))
	for _, row := range rows {
		snapshot := byRole[row.RoleKey]
		if snapshot == nil {
			snapshot = &RoleSnapshot{RoleKey: row.RoleKey, AuthorizationVersion: row.AuthorizationVersion, Status: row.RoleStatus}
			byRole[row.RoleKey] = snapshot
		}
		if row.PermissionKey.Valid && row.PermissionKey.String != "" {
			snapshot.Permissions = append(snapshot.Permissions, row.PermissionKey.String)
		}
	}
	result := make([]RoleSnapshot, 0, len(byRole))
	for _, snapshot := range byRole {
		result = append(result, *snapshot)
	}
	return sortRoleSnapshots(result), nil
}

func (s *Store) loadRoleVersions(uid string) ([]RoleVersion, error) {
	var versions []RoleVersion
	_, err := s.session.Select("r.role_key,r.authorization_version").
		From("admin_rbac_user_role ur").
		Join("admin_rbac_role r", "r.id=ur.role_id").
		Join("user u", "u.uid=ur.uid AND u.robot=0 AND u.status=1 AND u.is_destroy=0").
		Where("ur.uid=? AND ur.status=?", uid, activeStatus).
		OrderAsc("r.role_key").Load(&versions)
	return versions, err
}

func sortRoleSnapshots(snapshots []RoleSnapshot) []RoleSnapshot {
	for i := range snapshots {
		for j := i + 1; j < len(snapshots); j++ {
			if snapshots[j].RoleKey < snapshots[i].RoleKey {
				snapshots[i], snapshots[j] = snapshots[j], snapshots[i]
			}
		}
	}
	return snapshots
}

func wrapStoreError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("admin rbac %s: %w", operation, err)
}
