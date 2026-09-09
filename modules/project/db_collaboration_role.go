package project

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type collaborationRoleBindingRow struct {
	UID        string `db:"uid"`
	RoleID     string `db:"role_id"`
	BuiltinKey string `db:"builtin_key"`
	Name       string `db:"name"`
	Source     string `db:"source"`
}

type collaborationRoleProjectRow struct {
	ID        int64  `db:"id"`
	ProjectID string `db:"project_id"`
}

type collaborationRoleIntegrityRow struct {
	ProjectID string `db:"project_id"`
	UID       string `db:"uid"`
	RoleID    string `db:"role_id"`
	Violating bool   `db:"violating"`
}

func (d *DB) seedBuiltinCollaborationRolesTx(tx *dbr.Tx, projectID string, now time.Time) (bool, error) {
	var existingKeys []string
	_, err := tx.SelectBySql(
		"SELECT builtin_key FROM `octo_project_collaboration_role` "+
			"WHERE project_id = ? AND source = ? FOR UPDATE",
		projectID, CollaborationRoleSourceBuiltin,
	).Load(&existingKeys)
	if err != nil {
		return false, fmt.Errorf("project: lock builtin collaboration roles: %w", err)
	}
	existing := make(map[string]struct{}, len(existingKeys))
	for _, key := range existingKeys {
		existing[key] = struct{}{}
	}
	changed := false
	for _, role := range builtinCollaborationRoles {
		if _, ok := existing[role.Key]; ok {
			continue
		}
		_, err := tx.InsertBySql(
			"INSERT INTO `octo_project_collaboration_role` "+
				"(role_id, project_id, builtin_key, name, normalized_name, source, creator_uid, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)",
			util.GenerUUID(), projectID, role.Key, role.Name,
			normalizeCollaborationRoleName(role.Name), CollaborationRoleSourceBuiltin, now, now,
		).Exec()
		if err != nil {
			return false, fmt.Errorf("project: seed builtin collaboration role %s: %w", role.Key, err)
		}
		changed = true
	}
	return changed, nil
}

func (d *DB) collaborationRoleBackfillPage(afterID int64, limit int) ([]collaborationRoleProjectRow, error) {
	var projects []collaborationRoleProjectRow
	_, err := d.session.SelectBySql(
		"SELECT id, project_id FROM `octo_project` "+
			"WHERE status = ? AND id > ? ORDER BY id LIMIT ?",
		StatusNormal, afterID, limit,
	).Load(&projects)
	if err != nil {
		return nil, fmt.Errorf("project: query collaboration role backfill page: %w", err)
	}
	return projects, nil
}

// collaborationRoleIntegrityPage inspects at most limit association rows. The
// LIMIT belongs to the primary-key-ordered base page, before any joins or
// violation predicate, so a healthy table cannot turn this maintenance query
// into a recurring full-table scan.
func (d *DB) collaborationRoleIntegrityPage(
	cursorProjectID, cursorUID, cursorRoleID string, limit int,
) ([]*collaborationRoleIntegrityRow, error) {
	var rows []*collaborationRoleIntegrityRow
	_, err := d.session.SelectBySql(
		"SELECT b.project_id, b.uid, b.role_id, "+
			"(p.project_id IS NOT NULL AND p.status = ? AND "+
			"(r.role_id IS NULL OR m.uid IS NULL OR m.status <> ? OR m.removing <> 0)) AS violating "+
			"FROM ("+
			"SELECT project_id, uid, role_id "+
			"FROM `octo_project_member_collaboration_role` "+
			"WHERE (project_id, uid, role_id) > (?, ?, ?) "+
			"ORDER BY project_id, uid, role_id LIMIT ?"+
			") b "+
			"LEFT JOIN `octo_project` p ON p.project_id = b.project_id "+
			"LEFT JOIN `octo_project_collaboration_role` r "+
			"ON r.project_id = b.project_id AND r.role_id = b.role_id "+
			"LEFT JOIN `octo_project_member` m "+
			"ON m.project_id = b.project_id AND m.uid = b.uid "+
			"ORDER BY b.project_id, b.uid, b.role_id",
		StatusNormal, MemberStatusActive,
		cursorProjectID, cursorUID, cursorRoleID, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query collaboration role integrity page: %w", err)
	}
	return rows, nil
}

func (d *DB) collaborationRoleCatalog(projectID string) ([]CollaborationRoleModel, int64, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("project: begin collaboration role catalog read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var epochs []int64
	_, err = tx.SelectBySql(
		"SELECT collaboration_role_epoch FROM `octo_project` WHERE project_id = ? AND status = ?",
		projectID, StatusNormal,
	).Load(&epochs)
	if err != nil {
		return nil, 0, fmt.Errorf("project: query collaboration role epoch: %w", err)
	}
	if len(epochs) == 0 {
		return nil, 0, errProjectGone
	}

	var roles []CollaborationRoleModel
	_, err = tx.SelectBySql(
		"SELECT role_id, project_id, IFNULL(builtin_key, '') AS builtin_key, name, "+
			"normalized_name, source, creator_uid, created_at, updated_at "+
			"FROM `octo_project_collaboration_role` WHERE project_id = ? "+
			"ORDER BY CASE builtin_key "+
			"WHEN 'product' THEN 1 WHEN 'frontend' THEN 2 WHEN 'backend' THEN 3 WHEN 'hr' THEN 4 ELSE 5 END, "+
			"created_at ASC, role_id ASC",
		projectID,
	).Load(&roles)
	if err != nil {
		return nil, 0, fmt.Errorf("project: list collaboration roles: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("project: commit collaboration role catalog read: %w", err)
	}
	if roles == nil {
		roles = []CollaborationRoleModel{}
	}
	return roles, epochs[0], nil
}

func (d *DB) countCollaborationRolesTx(tx *dbr.Tx, projectID string) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_collaboration_role` WHERE project_id = ? FOR SHARE",
		projectID,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count collaboration roles: %w", err)
	}
	return count, nil
}

func (d *DB) insertCustomCollaborationRoleTx(tx *dbr.Tx, role *CollaborationRoleModel) error {
	_, err := tx.InsertBySql(
		"INSERT INTO `octo_project_collaboration_role` "+
			"(role_id, project_id, builtin_key, name, normalized_name, source, creator_uid, created_at, updated_at) "+
			"VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?)",
		role.RoleID, role.ProjectID, role.Name, role.NormalizedName, role.Source,
		role.CreatorUID, role.CreatedAt, role.UpdatedAt,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: insert custom collaboration role: %w", err)
	}
	return nil
}

func (d *DB) lockCollaborationRoleTx(tx *dbr.Tx, projectID, roleID string) (*CollaborationRoleModel, error) {
	var roles []*CollaborationRoleModel
	_, err := tx.SelectBySql(
		"SELECT role_id, project_id, IFNULL(builtin_key, '') AS builtin_key, name, "+
			"normalized_name, source, creator_uid, created_at, updated_at "+
			"FROM `octo_project_collaboration_role` "+
			"WHERE project_id = ? AND role_id = ? FOR UPDATE",
		projectID, roleID,
	).Load(&roles)
	if err != nil {
		return nil, fmt.Errorf("project: lock collaboration role: %w", err)
	}
	if len(roles) == 0 {
		return nil, nil
	}
	return roles[0], nil
}

func (d *DB) updateCollaborationRoleTx(
	tx *dbr.Tx, projectID, roleID, name, normalizedName string, now time.Time,
) error {
	_, err := tx.Update("octo_project_collaboration_role").
		Set("name", name).
		Set("normalized_name", normalizedName).
		Set("updated_at", now).
		Where("project_id = ? AND role_id = ? AND source = ?",
			projectID, roleID, CollaborationRoleSourceCustom).
		Exec()
	if err != nil {
		return fmt.Errorf("project: update collaboration role: %w", err)
	}
	return nil
}

func (d *DB) deleteCollaborationRoleTx(tx *dbr.Tx, projectID, roleID string) (bool, error) {
	if _, err := tx.DeleteFrom("octo_project_member_collaboration_role").
		Where("project_id = ? AND role_id = ?", projectID, roleID).
		Exec(); err != nil {
		return false, fmt.Errorf("project: delete collaboration role bindings: %w", err)
	}
	res, err := tx.DeleteFrom("octo_project_collaboration_role").
		Where("project_id = ? AND role_id = ? AND source = ?",
			projectID, roleID, CollaborationRoleSourceCustom).
		Exec()
	if err != nil {
		return false, fmt.Errorf("project: delete collaboration role: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: delete collaboration role affected rows: %w", err)
	}
	return affected > 0, nil
}

func (d *DB) lockCollaborationRoleIDsTx(
	tx *dbr.Tx, projectID string, roleIDs []string,
) (map[string]struct{}, error) {
	roles := make(map[string]struct{}, len(roleIDs))
	if len(roleIDs) == 0 {
		return roles, nil
	}
	var found []string
	_, err := tx.SelectBySql(
		"SELECT role_id FROM `octo_project_collaboration_role` "+
			"WHERE project_id = ? AND role_id IN ? FOR SHARE",
		projectID, roleIDs,
	).Load(&found)
	if err != nil {
		return nil, fmt.Errorf("project: validate collaboration role ids: %w", err)
	}
	for _, roleID := range found {
		roles[roleID] = struct{}{}
	}
	return roles, nil
}

func (d *DB) replaceMemberCollaborationRolesTx(
	tx *dbr.Tx, projectID, uid string, roleIDs []string, now time.Time,
) (bool, error) {
	var current []string
	_, err := tx.SelectBySql(
		"SELECT role_id FROM `octo_project_member_collaboration_role` "+
			"WHERE project_id = ? AND uid = ? ORDER BY role_id FOR UPDATE",
		projectID, uid,
	).Load(&current)
	if err != nil {
		return false, fmt.Errorf("project: lock member collaboration roles: %w", err)
	}
	if sameStringSet(current, roleIDs) {
		return false, nil
	}
	if _, err := tx.DeleteFrom("octo_project_member_collaboration_role").
		Where("project_id = ? AND uid = ?", projectID, uid).
		Exec(); err != nil {
		return false, fmt.Errorf("project: clear member collaboration roles: %w", err)
	}
	for _, roleID := range roleIDs {
		_, err := tx.InsertInto("octo_project_member_collaboration_role").
			Columns("project_id", "uid", "role_id", "created_at").
			Values(projectID, uid, roleID, now).
			Exec()
		if err != nil {
			return false, fmt.Errorf("project: bind member collaboration role: %w", err)
		}
	}
	return true, nil
}

func (d *DB) deleteMemberCollaborationRolesTx(
	tx *dbr.Tx, projectID string, uids []string,
) (bool, error) {
	if len(uids) == 0 {
		return false, nil
	}
	res, err := tx.DeleteFrom("octo_project_member_collaboration_role").
		Where("project_id = ? AND uid IN ?", projectID, uids).
		Exec()
	if err != nil {
		return false, fmt.Errorf("project: clear closing member collaboration roles: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: clear closing member collaboration roles affected rows: %w", err)
	}
	return affected > 0, nil
}

func (d *DB) bumpCollaborationRoleEpochTx(tx *dbr.Tx, projectID string) error {
	_, err := tx.UpdateBySql(
		"UPDATE `octo_project` SET collaboration_role_epoch = collaboration_role_epoch + 1 "+
			"WHERE project_id = ? AND status = ?",
		projectID, StatusNormal,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: bump collaboration role epoch: %w", err)
	}
	return nil
}

func (d *DB) memberCollaborationRoles(
	projectID string, uids []string,
) (map[string][]CollaborationRoleResp, error) {
	byUID := make(map[string][]CollaborationRoleResp, len(uids))
	for _, uid := range uids {
		byUID[uid] = []CollaborationRoleResp{}
	}
	if len(uids) == 0 {
		return byUID, nil
	}
	var rows []collaborationRoleBindingRow
	_, err := d.session.SelectBySql(
		"SELECT b.uid, r.role_id, IFNULL(r.builtin_key, '') AS builtin_key, r.name, r.source "+
			"FROM `octo_project_member_collaboration_role` b "+
			"INNER JOIN `octo_project_collaboration_role` r "+
			"ON r.project_id = b.project_id AND r.role_id = b.role_id "+
			"WHERE b.project_id = ? AND b.uid IN ? "+
			"ORDER BY b.uid, CASE r.builtin_key "+
			"WHEN 'product' THEN 1 WHEN 'frontend' THEN 2 WHEN 'backend' THEN 3 WHEN 'hr' THEN 4 ELSE 5 END, "+
			"r.created_at ASC, r.role_id ASC",
		projectID, uids,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list member collaboration roles: %w", err)
	}
	for _, row := range rows {
		byUID[row.UID] = append(byUID[row.UID], CollaborationRoleResp{
			RoleID: row.RoleID, BuiltinKey: row.BuiltinKey, Name: row.Name, Source: row.Source,
		})
	}
	return byUID, nil
}
