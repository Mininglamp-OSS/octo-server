package workspace

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

const workspaceColumns = "id, workspace_id, space_id, name, description, logo, owner_uid, status, created_at, updated_at"
const workspaceMemberColumns = "workspace_id, uid, role, status, granted_by, created_at, updated_at"

type DB struct {
	ctx     *config.Context
	session *dbr.Session
}

func NewDB(ctx *config.Context) *DB {
	if ctx == nil {
		return &DB{}
	}
	return &DB{ctx: ctx, session: ctx.DB()}
}

func (d *DB) ensureSession() error {
	if d == nil || d.session == nil {
		return fmt.Errorf("workspace: database session unavailable: %w", ErrDependencyUnavailable)
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (d *DB) queryWorkspace(workspaceID string) (*workspaceModel, error) {
	if err := d.ensureSession(); err != nil {
		return nil, err
	}
	if workspaceID == "" {
		return nil, nil
	}
	var rows []*workspaceModel
	_, err := d.session.SelectBySql(
		"SELECT "+workspaceColumns+" FROM `octo_workspace` WHERE workspace_id = ? LIMIT 1",
		workspaceID,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("workspace: query workspace: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (d *DB) queryActiveWorkspace(workspaceID string) (*workspaceModel, error) {
	row, err := d.queryWorkspace(workspaceID)
	if err != nil || row == nil {
		return row, err
	}
	if row.Status != WorkspaceStatusActive {
		return nil, nil
	}
	return row, nil
}

func (d *DB) querySpaceActive(spaceID string) (bool, error) {
	if err := d.ensureSession(); err != nil {
		return false, err
	}
	if spaceID == "" {
		return false, nil
	}
	var status int
	rows, err := d.session.SelectBySql(
		"SELECT status FROM `space` WHERE space_id = ? LIMIT 1", spaceID,
	).Load(&status)
	if err != nil {
		return false, fmt.Errorf("workspace: query space status: %w", err)
	}
	return rows > 0 && status == 1, nil
}

func (d *DB) querySpaceMemberActive(uid, spaceID string) (bool, error) {
	if err := d.ensureSession(); err != nil {
		return false, err
	}
	if uid == "" || spaceID == "" {
		return false, nil
	}
	var found []int
	_, err := d.session.SelectBySql(
		"SELECT 1 FROM `space_member` WHERE uid = ? AND space_id = ? AND status = 1 LIMIT 1",
		uid, spaceID,
	).Load(&found)
	if err != nil {
		return false, fmt.Errorf("workspace: query space member: %w", err)
	}
	return len(found) > 0, nil
}

func (d *DB) queryUserEligible(uid string) (bool, error) {
	if err := d.ensureSession(); err != nil {
		return false, err
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return false, nil
	}
	var rows []*userEligibilityModel
	_, err := d.session.SelectBySql(
		"SELECT uid, status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid = ? LIMIT 1",
		uid,
	).Load(&rows)
	if err != nil {
		return false, fmt.Errorf("workspace: query user eligibility: %w", err)
	}
	if len(rows) == 0 || rows[0] == nil {
		return false, nil
	}
	return rows[0].Status == 1 && rows[0].IsDestroy != 2, nil
}

func (d *DB) queryWorkspaceMember(workspaceID, uid string) (*workspaceMemberModel, error) {
	if err := d.ensureSession(); err != nil {
		return nil, err
	}
	if workspaceID == "" || uid == "" {
		return nil, nil
	}
	var rows []*workspaceMemberModel
	_, err := d.session.SelectBySql(
		"SELECT "+workspaceMemberColumns+" FROM `octo_workspace_member` WHERE workspace_id = ? AND uid = ? LIMIT 1",
		workspaceID, uid,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("workspace: query workspace member: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (d *DB) countActiveWorkspaceMembers(workspaceID string) (int64, error) {
	if err := d.ensureSession(); err != nil {
		return 0, err
	}
	var count int64
	if err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_workspace_member` WHERE workspace_id = ? AND status = 1",
		workspaceID,
	).LoadOne(&count); err != nil {
		return 0, fmt.Errorf("workspace: count members: %w", err)
	}
	return count, nil
}

func (d *DB) insertWorkspaceTx(tx *dbr.Tx, row *workspaceModel) error {
	if row == nil {
		return fmt.Errorf("workspace: insert nil workspace: %w", ErrRequestInvalid)
	}
	_, err := tx.InsertInto("octo_workspace").
		Columns("workspace_id", "space_id", "name", "description", "logo", "owner_uid", "status", "created_at", "updated_at").
		Values(row.WorkspaceID, row.SpaceID, row.Name, row.Description, row.Logo, row.OwnerUID, row.Status, row.CreatedAt, row.UpdatedAt).
		Exec()
	if err != nil {
		return fmt.Errorf("workspace: insert workspace: %w", err)
	}
	return nil
}

func (d *DB) insertMemberTx(tx *dbr.Tx, row *workspaceMemberModel) error {
	if row == nil {
		return fmt.Errorf("workspace: insert nil member: %w", ErrRequestInvalid)
	}
	_, err := tx.InsertInto("octo_workspace_member").
		Columns("workspace_id", "uid", "role", "status", "granted_by", "created_at", "updated_at").
		Values(row.WorkspaceID, row.UID, row.Role, row.Status, row.GrantedBy, row.CreatedAt, row.UpdatedAt).
		Exec()
	if err != nil {
		return fmt.Errorf("workspace: insert member: %w", err)
	}
	return nil
}

func (d *DB) updateWorkspaceTx(tx *dbr.Tx, workspaceID string, req UpdateRequest, now time.Time) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace: update empty workspace: %w", ErrRequestInvalid)
	}
	stmt := tx.Update("octo_workspace").Where("workspace_id = ? AND status = 1", workspaceID)
	changed := false
	if req.Name != nil {
		stmt = stmt.Set("name", *req.Name)
		changed = true
	}
	if req.Description != nil {
		stmt = stmt.Set("description", *req.Description)
		changed = true
	}
	if req.Logo != nil {
		stmt = stmt.Set("logo", *req.Logo)
		changed = true
	}
	if !changed {
		return nil
	}
	if _, err := stmt.Set("updated_at", now).Exec(); err != nil {
		return fmt.Errorf("workspace: update workspace: %w", err)
	}
	return nil
}

func (d *DB) archiveWorkspaceTx(tx *dbr.Tx, workspaceID string, now time.Time) error {
	if _, err := tx.Update("octo_workspace").
		Set("status", WorkspaceStatusArchived).
		Set("updated_at", now).
		Where("workspace_id = ? AND status = 1", workspaceID).
		Exec(); err != nil {
		return fmt.Errorf("workspace: archive workspace: %w", err)
	}
	return nil
}

// admitMemberTx inserts or reactivates a member. An already-active row is
// untouched; callers use the returned changed flag for idempotent responses.
func (d *DB) admitMemberTx(tx *dbr.Tx, row *workspaceMemberModel) (bool, error) {
	if row == nil {
		return false, fmt.Errorf("workspace: admit nil member: %w", ErrRequestInvalid)
	}
	res, err := tx.InsertBySql(
		"INSERT INTO `octo_workspace_member` (workspace_id, uid, role, status, granted_by, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE "+
			"role = IF(status = 0, VALUES(role), role), "+
			"granted_by = IF(status = 0, VALUES(granted_by), granted_by), "+
			"updated_at = IF(status = 0, VALUES(updated_at), updated_at), "+
			"status = 1",
		row.WorkspaceID, row.UID, row.Role, row.Status, row.GrantedBy, row.CreatedAt, row.UpdatedAt,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("workspace: admit member: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("workspace: admit member affected rows: %w", err)
	}
	return affected > 0, nil
}

func (d *DB) updateMemberRoleTx(tx *dbr.Tx, workspaceID, uid string, role int, now time.Time) error {
	if _, err := tx.Update("octo_workspace_member").
		Set("role", role).
		Set("updated_at", now).
		Where("workspace_id = ? AND uid = ? AND status = 1", workspaceID, uid).
		Exec(); err != nil {
		return fmt.Errorf("workspace: update member role: %w", err)
	}
	return nil
}

func (d *DB) deactivateMemberTx(tx *dbr.Tx, workspaceID, uid string, now time.Time) error {
	if _, err := tx.Update("octo_workspace_member").
		Set("status", MemberStatusInactive).
		Set("updated_at", now).
		Where("workspace_id = ? AND uid = ? AND status = 1", workspaceID, uid).
		Exec(); err != nil {
		return fmt.Errorf("workspace: deactivate member: %w", err)
	}
	return nil
}

func (d *DB) transferOwnerTx(tx *dbr.Tx, workspaceID, oldOwner, newOwner string, now time.Time) error {
	if _, err := tx.Update("octo_workspace_member").
		Set("role", MemberRoleAdmin).
		Set("updated_at", now).
		Where("workspace_id = ? AND uid IN (?, ?) AND status = 1", workspaceID, oldOwner, newOwner).
		Exec(); err != nil {
		return fmt.Errorf("workspace: update owner member roles: %w", err)
	}
	if _, err := tx.Update("octo_workspace").
		Set("owner_uid", newOwner).
		Set("updated_at", now).
		Where("workspace_id = ? AND status = 1 AND owner_uid = ?", workspaceID, oldOwner).
		Exec(); err != nil {
		return fmt.Errorf("workspace: transfer owner: %w", err)
	}
	return nil
}

func (d *DB) lockSpaceSeatsTx(tx *dbr.Tx, spaceIDs, uids []string, allActive bool) (map[string]bool, error) {
	held := make(map[string]bool)
	spaceIDs = uniqueSorted(spaceIDs)
	uids = uniqueSorted(uids)
	if len(spaceIDs) == 0 {
		return held, nil
	}
	args := make([]interface{}, 0, len(spaceIDs)+len(uids))
	spacePH := placeholders(len(spaceIDs))
	for _, spaceID := range spaceIDs {
		args = append(args, spaceID)
	}
	query := "SELECT space_id, uid FROM `space_member` WHERE space_id IN (" + spacePH + ") AND status = 1"
	if !allActive {
		if len(uids) == 0 {
			return held, nil
		}
		query += " AND uid IN (" + placeholders(len(uids)) + ")"
		for _, uid := range uids {
			args = append(args, uid)
		}
	}
	query += " FOR SHARE"
	var rows []struct {
		SpaceID string `db:"space_id"`
		UID     string `db:"uid"`
	}
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock space seats: %w", err)
	}
	for _, row := range rows {
		held[row.SpaceID+"\x00"+row.UID] = true
	}
	return held, nil
}

func (d *DB) lockSpacesTx(tx *dbr.Tx, spaceIDs []string) (map[string]bool, error) {
	spaceIDs = uniqueSorted(spaceIDs)
	active := make(map[string]bool, len(spaceIDs))
	if len(spaceIDs) == 0 {
		return active, nil
	}
	var rows []struct {
		SpaceID string `db:"space_id"`
	}
	query := "SELECT space_id FROM `space` WHERE space_id IN (" + placeholders(len(spaceIDs)) + ") AND status = 1 ORDER BY space_id FOR UPDATE"
	args := make([]interface{}, len(spaceIDs))
	for i, spaceID := range spaceIDs {
		args[i] = spaceID
	}
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock spaces: %w", err)
	}
	for _, row := range rows {
		active[row.SpaceID] = true
	}
	return active, nil
}

func (d *DB) lockWorkspaceRowsTx(tx *dbr.Tx, workspaceIDs []string) (map[string]*workspaceModel, error) {
	workspaceIDs = uniqueSorted(workspaceIDs)
	rowsByID := make(map[string]*workspaceModel, len(workspaceIDs))
	if len(workspaceIDs) == 0 {
		return rowsByID, nil
	}
	query := "SELECT " + workspaceColumns + " FROM `octo_workspace` WHERE workspace_id IN (" + placeholders(len(workspaceIDs)) + ") AND status = 1 ORDER BY workspace_id FOR UPDATE"
	args := make([]interface{}, len(workspaceIDs))
	for i, workspaceID := range workspaceIDs {
		args[i] = workspaceID
	}
	var rows []*workspaceModel
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock workspace rows: %w", err)
	}
	for _, row := range rows {
		rowsByID[row.WorkspaceID] = row
	}
	return rowsByID, nil
}

func (d *DB) lockMemberRowsTx(tx *dbr.Tx, workspaceIDs, uids []string, allActive bool) (map[string]*workspaceMemberModel, error) {
	workspaceIDs = uniqueSorted(workspaceIDs)
	uids = uniqueSorted(uids)
	rowsByKey := make(map[string]*workspaceMemberModel)
	if len(workspaceIDs) == 0 {
		return rowsByKey, nil
	}
	args := make([]interface{}, 0, len(workspaceIDs)+len(uids))
	for _, workspaceID := range workspaceIDs {
		args = append(args, workspaceID)
	}
	query := "SELECT " + workspaceMemberColumns + " FROM `octo_workspace_member` WHERE workspace_id IN (" + placeholders(len(workspaceIDs)) + ")"
	if !allActive {
		if len(uids) == 0 {
			return rowsByKey, nil
		}
		query += " AND uid IN (" + placeholders(len(uids)) + ")"
		for _, uid := range uids {
			args = append(args, uid)
		}
	} else {
		query += " AND status = 1"
	}
	query += " ORDER BY workspace_id, uid FOR UPDATE"
	var rows []*workspaceMemberModel
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock member rows: %w", err)
	}
	for _, row := range rows {
		rowsByKey[row.WorkspaceID+"\x00"+row.UID] = row
	}
	return rowsByKey, nil
}

type workspaceListModel struct {
	ID            int64     `db:"id"`
	WorkspaceID   string    `db:"workspace_id"`
	SpaceID       string    `db:"space_id"`
	Name          string    `db:"name"`
	Description   string    `db:"description"`
	Logo          string    `db:"logo"`
	OwnerUID      string    `db:"owner_uid"`
	Status        int       `db:"status"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
	MemberCount   int64     `db:"member_count"`
	WorkspaceRole string    `db:"workspace_role"`
	MyStorageRole int       `db:"my_storage_role"`
}

func (d *DB) listWorkspaces(spaceID, actorUID, keyword string, page Page) (*Pagination[Workspace], error) {
	result := &Pagination[Workspace]{List: make([]Workspace, 0)}
	if err := d.ensureSession(); err != nil {
		return nil, err
	}
	where := "w.space_id = ? AND w.status = 1 AND wm.uid = ? AND wm.status = 1"
	args := []interface{}{spaceID, actorUID}
	if keyword != "" {
		where += " AND w.name LIKE ?"
		args = append(args, "%"+keyword+"%")
	}
	if err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_workspace` w INNER JOIN `octo_workspace_member` wm ON wm.workspace_id = w.workspace_id WHERE "+where,
		args...,
	).LoadOne(&result.Count); err != nil {
		return nil, fmt.Errorf("workspace: count visible workspaces: %w", err)
	}
	offset, limit, ok := pageWindow(page)
	if !ok {
		return result, nil
	}
	query := "SELECT w.id, w.workspace_id, w.space_id, w.name, w.description, w.logo, w.owner_uid, w.status, w.created_at, w.updated_at, " +
		"(SELECT COUNT(*) FROM `octo_workspace_member` wm2 WHERE wm2.workspace_id = w.workspace_id AND wm2.status = 1) AS member_count, " +
		"CASE WHEN w.owner_uid = wm.uid THEN 'owner' WHEN wm.role = 1 THEN 'admin' ELSE 'member' END AS workspace_role, " +
		"wm.role AS my_storage_role FROM `octo_workspace` w INNER JOIN `octo_workspace_member` wm ON wm.workspace_id = w.workspace_id WHERE " + where +
		" ORDER BY w.created_at ASC, w.workspace_id ASC LIMIT ? OFFSET ?"
	pageArgs := append(append([]interface{}{}, args...), limit, offset)
	var rows []*workspaceListModel
	if _, err := d.session.SelectBySql(query, pageArgs...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: list visible workspaces: %w", err)
	}
	for _, row := range rows {
		if row.MyStorageRole != MemberRoleMember && row.MyStorageRole != MemberRoleAdmin {
			return nil, fmt.Errorf("workspace: invalid member role in list: %w", ErrForbidden)
		}
		result.List = append(result.List, Workspace{
			WorkspaceID:   row.WorkspaceID,
			Name:          row.Name,
			Description:   row.Description,
			Logo:          row.Logo,
			OwnerUID:      row.OwnerUID,
			SpaceID:       row.SpaceID,
			MemberCount:   row.MemberCount,
			Status:        row.Status,
			WorkspaceRole: row.WorkspaceRole,
			CreatedAt:     formatWorkspaceTime(row.CreatedAt),
			UpdatedAt:     formatWorkspaceTime(row.UpdatedAt),
		})
	}
	return result, nil
}

func pageWindow(page Page) (offset int64, limit int64, ok bool) {
	index := page.Index
	if index <= 0 {
		index = 1
	}
	size := page.Size
	if size <= 0 {
		size = 15
	}
	if size > 200 {
		size = 200
	}
	limit = int64(size)
	idx := uint64(index - 1)
	if idx > math.MaxUint64/uint64(size) {
		return 0, limit, false
	}
	off := idx * uint64(size)
	if off > math.MaxInt64 {
		return 0, limit, false
	}
	return int64(off), limit, true
}

func (d *DB) listMembers(workspaceID string, filter MemberFilter, page Page) (*Pagination[Member], error) {
	result := &Pagination[Member]{List: make([]Member, 0)}
	if err := d.ensureSession(); err != nil {
		return nil, err
	}
	if filter.Status != MemberStatusInactive && filter.Status != MemberStatusActive {
		filter.Status = MemberStatusActive
	}
	where := "wm.workspace_id = ? AND wm.status = ?"
	args := []interface{}{workspaceID, filter.Status}
	roleWhere, roleArgs := memberRolePredicate(filter.Roles)
	if roleWhere != "" {
		where += " AND (" + roleWhere + ")"
		args = append(args, roleArgs...)
	}
	countQuery := "SELECT COUNT(*) FROM `octo_workspace_member` wm INNER JOIN `octo_workspace` w ON w.workspace_id = wm.workspace_id WHERE w.workspace_id = ? AND w.status = 1 AND " + strings.TrimPrefix(where, "wm.workspace_id = ? AND ")
	countArgs := append([]interface{}{workspaceID}, args[1:]...)
	if err := d.session.SelectBySql(countQuery, countArgs...).LoadOne(&result.Count); err != nil {
		return nil, fmt.Errorf("workspace: count members: %w", err)
	}
	offset, limit, ok := pageWindow(page)
	if !ok {
		return result, nil
	}
	query := "SELECT wm.uid, wm.role, wm.status, wm.granted_by, wm.created_at, wm.updated_at, w.owner_uid FROM `octo_workspace_member` wm INNER JOIN `octo_workspace` w ON w.workspace_id = wm.workspace_id WHERE " + where + " AND w.status = 1 ORDER BY wm.created_at ASC, wm.uid ASC LIMIT ? OFFSET ?"
	pageArgs := append(append([]interface{}{}, args...), limit, offset)
	var rows []*memberListModel
	if _, err := d.session.SelectBySql(query, pageArgs...).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: list members: %w", err)
	}
	names, err := d.queryMemberNames(rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		role := projectedRole(row.OwnerUID, row.UID, row.Role)
		if role == "" {
			return nil, fmt.Errorf("workspace: invalid member role in list: %w", ErrForbidden)
		}
		result.List = append(result.List, Member{
			UID:           row.UID,
			Name:          names[row.UID],
			WorkspaceRole: role,
			Status:        row.Status,
			CreatedAt:     formatWorkspaceTime(row.CreatedAt),
			GrantedBy:     row.GrantedBy,
		})
	}
	return result, nil
}

type memberListModel struct {
	UID       string    `db:"uid"`
	Role      int       `db:"role"`
	Status    int       `db:"status"`
	GrantedBy string    `db:"granted_by"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
	OwnerUID  string    `db:"owner_uid"`
}

func memberRolePredicate(roles []string) (string, []interface{}) {
	clean := make([]string, 0, len(roles))
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			continue
		}
		if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
			continue
		}
		seen[role] = true
		clean = append(clean, role)
	}
	if len(clean) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(clean))
	args := make([]interface{}, 0, len(clean))
	for _, role := range clean {
		switch role {
		case WorkspaceRoleOwner:
			parts = append(parts, "wm.uid = w.owner_uid")
		case WorkspaceRoleAdmin:
			parts = append(parts, "wm.uid <> w.owner_uid AND wm.role = ?")
			args = append(args, MemberRoleAdmin)
		case WorkspaceRoleMember:
			parts = append(parts, "wm.uid <> w.owner_uid AND wm.role = ?")
			args = append(args, MemberRoleMember)
		}
	}
	return strings.Join(parts, " OR "), args
}

func (d *DB) queryMemberNames(rows []*memberListModel) (map[string]string, error) {
	names := make(map[string]string, len(rows))
	if len(rows) == 0 {
		return names, nil
	}
	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		uids = append(uids, row.UID)
	}
	var users []*memberNameModel
	args := make([]interface{}, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	if _, err := d.session.SelectBySql(
		"SELECT uid, IFNULL(name, '') AS name FROM `user` WHERE uid IN ("+placeholders(len(uids))+")",
		args...,
	).Load(&users); err != nil {
		return nil, fmt.Errorf("workspace: query member names: %w", err)
	}
	for _, user := range users {
		names[user.UID] = user.Name
	}
	return names, nil
}

func (d *DB) queryMemberPublic(workspaceID, uid, ownerUID string) (*Member, error) {
	row, err := d.queryWorkspaceMember(workspaceID, uid)
	if err != nil || row == nil {
		return nil, err
	}
	names, err := d.queryMemberNames([]*memberListModel{{UID: uid}})
	if err != nil {
		return nil, err
	}
	role := projectedRole(ownerUID, row.UID, row.Role)
	if role == "" {
		return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
	}
	return &Member{
		UID:           row.UID,
		Name:          names[uid],
		WorkspaceRole: role,
		Status:        row.Status,
		CreatedAt:     formatWorkspaceTime(row.CreatedAt),
		GrantedBy:     row.GrantedBy,
	}, nil
}

func projectedRole(ownerUID, uid string, storageRole int) string {
	if ownerUID != "" && ownerUID == uid {
		return WorkspaceRoleOwner
	}
	if storageRole == MemberRoleAdmin {
		return WorkspaceRoleAdmin
	}
	if storageRole == MemberRoleMember {
		return WorkspaceRoleMember
	}
	return ""
}

func (d *DB) queryActiveWorkspaceMembers(workspaceID string) ([]*workspaceMemberModel, error) {
	if err := d.ensureSession(); err != nil {
		return nil, err
	}
	var rows []*workspaceMemberModel
	if _, err := d.session.SelectBySql(
		"SELECT "+workspaceMemberColumns+" FROM `octo_workspace_member` WHERE workspace_id = ? AND status = 1 ORDER BY uid",
		workspaceID,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: query active members: %w", err)
	}
	return rows, nil
}

func (d *DB) queryWorkspaceLocationsTx(tx *dbr.Tx, workspaceIDs []string) (map[string]*workspaceModel, error) {
	locations := make(map[string]*workspaceModel, len(workspaceIDs))
	workspaceIDs = uniqueSorted(workspaceIDs)
	if len(workspaceIDs) == 0 {
		return locations, nil
	}
	args := make([]interface{}, len(workspaceIDs))
	for i, workspaceID := range workspaceIDs {
		args[i] = workspaceID
	}
	var rows []*workspaceModel
	if _, err := tx.SelectBySql(
		"SELECT "+workspaceColumns+" FROM `octo_workspace` WHERE workspace_id IN ("+placeholders(len(workspaceIDs))+")",
		args...,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: resolve workspace locations: %w", err)
	}
	for _, row := range rows {
		locations[row.WorkspaceID] = row
	}
	return locations, nil
}

func (d *DB) queryWorkspaceMemberTx(tx *dbr.Tx, workspaceID, uid string) (*workspaceMemberModel, error) {
	var rows []*workspaceMemberModel
	if _, err := tx.SelectBySql(
		"SELECT "+workspaceMemberColumns+" FROM `octo_workspace_member` WHERE workspace_id = ? AND uid = ? FOR UPDATE",
		workspaceID, uid,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock member: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (d *DB) queryLockedActiveMembersForWorkspace(locked map[string]*workspaceMemberModel, workspaceID string) ([]*workspaceMemberModel, error) {
	rows := make([]*workspaceMemberModel, 0)
	for key, row := range locked {
		if strings.HasPrefix(key, workspaceID+"\x00") && row != nil && row.Status == MemberStatusActive {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	return rows, nil
}

func (d *DB) queryWorkspaceLockedTx(tx *dbr.Tx, workspaceID string) (*workspaceModel, error) {
	var rows []*workspaceModel
	if _, err := tx.SelectBySql(
		"SELECT "+workspaceColumns+" FROM `octo_workspace` WHERE workspace_id = ? AND status = 1 FOR UPDATE",
		workspaceID,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock workspace for projection: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (d *DB) countActiveWorkspaceMembersTx(tx *dbr.Tx, workspaceID string) (int64, error) {
	var count int64
	if err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_workspace_member` WHERE workspace_id = ? AND status = 1 FOR UPDATE",
		workspaceID,
	).LoadOne(&count); err != nil {
		return 0, fmt.Errorf("workspace: count members in tx: %w", err)
	}
	return count, nil
}

type userEligibilityModel struct {
	UID       string `db:"uid"`
	Status    int    `db:"status"`
	IsDestroy int    `db:"is_destroy"`
}

func (d *DB) lockEligibleUsersTx(tx *dbr.Tx, uids []string) (map[string]bool, error) {
	uids = uniqueSorted(uids)
	eligible := make(map[string]bool, len(uids))
	if len(uids) == 0 {
		return eligible, nil
	}
	args := make([]interface{}, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	var rows []*userEligibilityModel
	if _, err := tx.SelectBySql(
		"SELECT uid, status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid IN ("+placeholders(len(uids))+") ORDER BY uid FOR SHARE",
		args...,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("workspace: lock user eligibility: %w", err)
	}
	for _, row := range rows {
		if row.Status == 1 && row.IsDestroy != 2 {
			eligible[row.UID] = true
		}
	}
	return eligible, nil
}

func (d *DB) membersPublicFromModels(rows []*workspaceMemberModel, ownerUID string) ([]Member, error) {
	result := make([]Member, 0, len(rows))
	if len(rows) == 0 {
		return result, nil
	}
	nameRows := make([]*memberListModel, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		nameRows = append(nameRows, &memberListModel{UID: row.UID})
	}
	names, err := d.queryMemberNames(nameRows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		role := projectedRole(ownerUID, row.UID, row.Role)
		if role == "" {
			return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
		}
		result = append(result, Member{
			UID:           row.UID,
			Name:          names[row.UID],
			WorkspaceRole: role,
			Status:        row.Status,
			CreatedAt:     formatWorkspaceTime(row.CreatedAt),
			GrantedBy:     row.GrantedBy,
		})
	}
	return result, nil
}
