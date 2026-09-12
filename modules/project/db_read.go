package project

import (
	"fmt"
	"strings"

	"github.com/gocraft/dbr/v2"
)

const projectReadModelColumns = "p.id, p.project_id, p.space_id, p.name, p.description, p.logo, p.creator, " +
	"p.discoverability, p.max_members, p.member_epoch, p.collaboration_role_epoch, p.status, " +
	"p.all_member_group_no, p.created_at, p.updated_at"
const projectReadMemberColumns = "pm.project_id, pm.uid, pm.space_id, pm.role, pm.status, pm.removing, " +
	"pm.invite_uid, pm.created_at, COALESCE(pm.joined_at, pm.created_at) AS joined_at, pm.updated_at"

const projectReadMemberJoin = "FROM `octo_project_member` pm INNER JOIN `octo_project` p " +
	"ON p.project_id = pm.project_id AND p.space_id = pm.space_id"

const projectReadMemberWhere = "p.status = ? AND pm.project_id = ? AND pm.space_id = p.space_id " +
	"AND pm.status = ? AND pm.removing = 0"

func projectReadListPredicate(spaceID, uid, keyword string) (string, []interface{}) {
	where := "p.space_id = ? AND p.status = ? AND pm.space_id = p.space_id AND pm.uid = ? AND pm.status = ? AND pm.removing = 0"
	args := []interface{}{spaceID, StatusNormal, uid, MemberStatusActive}
	if keyword != "" {
		where += " AND p.name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'"
		args = append(args, projectLikePattern(keyword))
	}
	return where, args
}

func (d *DB) ensureReadTx(tx *dbr.Tx) error {
	if d == nil || d.session == nil {
		return fmt.Errorf("project: database session unavailable")
	}
	if tx == nil {
		return fmt.Errorf("project: read transaction unavailable")
	}
	return nil
}

func (d *DB) queryReadUserEligibleTx(tx *dbr.Tx, uid string) (bool, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return false, err
	}
	var rows []struct {
		Status    int `db:"status"`
		IsDestroy int `db:"is_destroy"`
	}
	if _, err := tx.SelectBySql(
		"SELECT status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid = ? LIMIT 1",
		uid,
	).Load(&rows); err != nil {
		return false, fmt.Errorf("project: read user eligibility: %w", err)
	}
	return len(rows) > 0 && rows[0].Status == 1 && rows[0].IsDestroy != 2, nil
}

func (d *DB) queryReadSpaceActiveTx(tx *dbr.Tx, spaceID string) (bool, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return false, err
	}
	var rows []int
	if _, err := tx.SelectBySql(
		"SELECT status FROM `space` WHERE space_id = ? LIMIT 1", spaceID,
	).Load(&rows); err != nil {
		return false, fmt.Errorf("project: read space status: %w", err)
	}
	return len(rows) > 0 && rows[0] == 1, nil
}

func (d *DB) queryReadSpaceMemberTx(tx *dbr.Tx, spaceID, uid string) (int, bool, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return roleNonMember, false, err
	}
	var rows []struct {
		Role int `db:"role"`
	}
	if _, err := tx.SelectBySql(
		"SELECT role FROM `space_member` WHERE space_id = ? AND uid = ? AND status = 1 LIMIT 1",
		spaceID, uid,
	).Load(&rows); err != nil {
		return roleNonMember, false, fmt.Errorf("project: read space member: %w", err)
	}
	if len(rows) == 0 {
		return roleNonMember, false, nil
	}
	return rows[0].Role, true, nil
}

func (d *DB) queryProjectReadTx(tx *dbr.Tx, projectID string) (*Model, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, nil
	}
	var rows []*Model
	if _, err := tx.SelectBySql(
		"SELECT "+projectReadModelColumns+" FROM `octo_project` p WHERE p.project_id = ? LIMIT 1",
		projectID,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: read project: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (d *DB) queryProjectReadMemberRoleTx(tx *dbr.Tx, projectID, spaceID, uid string) (int, bool, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return roleNonMember, false, err
	}
	var rows []struct {
		Role int `db:"role"`
	}
	if _, err := tx.SelectBySql(
		"SELECT pm.role FROM `octo_project_member` pm INNER JOIN `octo_project` p "+
			"ON p.project_id = pm.project_id AND p.space_id = pm.space_id "+
			"WHERE pm.project_id = ? AND pm.space_id = ? AND pm.uid = ? AND pm.status = ? AND pm.removing = 0 LIMIT 1",
		projectID, spaceID, uid, MemberStatusActive,
	).Load(&rows); err != nil {
		return roleNonMember, false, fmt.Errorf("project: read project member role: %w", err)
	}
	if len(rows) == 0 {
		return roleNonMember, false, nil
	}
	return rows[0].Role, true, nil
}

func (d *DB) listProjectsReadTx(tx *dbr.Tx, spaceID, uid, keyword string, page projectReadPage) (*projectReadListResult, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	where, args := projectReadListPredicate(spaceID, uid, keyword)
	var total int64
	if err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` p INNER JOIN `octo_project_member` pm ON "+
			"pm.project_id = p.project_id WHERE "+where,
		args...,
	).LoadOne(&total); err != nil {
		return nil, fmt.Errorf("project: count readable projects: %w", err)
	}

	pageArgs := []interface{}{uid}
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, page.Limit, page.Offset)
	var rows []*projectReadListRow
	_, err := tx.SelectBySql(
		"SELECT "+projectReadModelColumns+", pm.role AS my_role, IFNULL(s.pinned, 0) AS pinned "+
			"FROM `octo_project` p INNER JOIN `octo_project_member` pm ON "+
			"pm.project_id = p.project_id "+
			"LEFT JOIN `octo_project_user_setting` s ON s.project_id = p.project_id AND s.uid = ? "+
			"WHERE "+where+" ORDER BY IFNULL(s.pinned, 0) DESC, s.pinned_at DESC, p.id DESC LIMIT ? OFFSET ?",
		pageArgs...,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list readable projects: %w", err)
	}
	return &projectReadListResult{Rows: rows, Total: total}, nil
}

func (d *DB) queryProjectPinnedReadTx(tx *dbr.Tx, projectID, uid string) (bool, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return false, err
	}
	var rows []int
	if _, err := tx.SelectBySql(
		"SELECT pinned FROM `octo_project_user_setting` WHERE project_id = ? AND uid = ? LIMIT 1",
		projectID, uid,
	).Load(&rows); err != nil {
		return false, fmt.Errorf("project: read project pin: %w", err)
	}
	return len(rows) > 0 && rows[0] == 1, nil
}

func (d *DB) listProjectMembersReadTx(tx *dbr.Tx, projectID string, page projectReadPage) (*projectReadMembersResult, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	var total int64
	if err := tx.SelectBySql(
		"SELECT COUNT(*) "+projectReadMemberJoin+" WHERE "+projectReadMemberWhere,
		StatusNormal, projectID, MemberStatusActive,
	).LoadOne(&total); err != nil {
		return nil, fmt.Errorf("project: count readable members: %w", err)
	}
	var members []*projectReadMemberRow
	if _, err := tx.SelectBySql(
		"SELECT "+projectReadMemberColumns+" "+projectReadMemberJoin+
			" WHERE "+projectReadMemberWhere+
			" ORDER BY pm.role DESC, pm.created_at ASC, pm.uid ASC LIMIT ? OFFSET ?",
		StatusNormal, projectID, MemberStatusActive, page.Limit, page.Offset,
	).Load(&members); err != nil {
		return nil, fmt.Errorf("project: list readable members: %w", err)
	}
	if err := d.populateProjectMemberReadTx(tx, members); err != nil {
		return nil, err
	}
	return &projectReadMembersResult{Rows: members, Total: total}, nil
}

func (d *DB) queryProjectMemberReadTx(tx *dbr.Tx, projectID, uid string) (*projectReadMemberRow, error) {
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	projectID = strings.TrimSpace(projectID)
	uid = strings.TrimSpace(uid)
	if projectID == "" || uid == "" {
		return nil, nil
	}
	var members []*projectReadMemberRow
	if _, err := tx.SelectBySql(
		"SELECT "+projectReadMemberColumns+" "+projectReadMemberJoin+
			" WHERE "+projectReadMemberWhere+" AND pm.uid = ? LIMIT 1",
		StatusNormal, projectID, MemberStatusActive, uid,
	).Load(&members); err != nil {
		return nil, fmt.Errorf("project: read project member: %w", err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	if err := d.populateProjectMemberReadTx(tx, members); err != nil {
		return nil, err
	}
	return members[0], nil
}

func (d *DB) populateProjectMemberReadTx(tx *dbr.Tx, members []*projectReadMemberRow) error {
	if err := d.ensureReadTx(tx); err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	uids := make([]string, 0, len(members))
	for _, member := range members {
		uids = append(uids, member.UID)
	}
	var users []struct {
		UID   string `db:"uid"`
		Name  string `db:"name"`
		Robot int    `db:"robot"`
	}
	if _, err := tx.SelectBySql(
		"SELECT uid, IFNULL(name, '') AS name, IFNULL(robot, 0) AS robot FROM `user` WHERE uid IN ?",
		uids,
	).Load(&users); err != nil {
		return fmt.Errorf("project: read member users: %w", err)
	}
	usersByUID := make(map[string]struct {
		Name  string
		Robot int
	}, len(users))
	robotUIDs := make([]string, 0, len(users))
	for _, user := range users {
		usersByUID[user.UID] = struct {
			Name  string
			Robot int
		}{Name: user.Name, Robot: user.Robot}
		if user.Robot == 1 {
			robotUIDs = append(robotUIDs, user.UID)
		}
	}
	var robots []struct {
		RobotID    string `db:"robot_id"`
		CreatorUID string `db:"creator_uid"`
	}
	if len(robotUIDs) > 0 {
		if _, err := tx.SelectBySql(
			"SELECT robot_id, IFNULL(creator_uid, '') AS creator_uid FROM `robot` WHERE robot_id IN ? AND status = 1",
			robotUIDs,
		).Load(&robots); err != nil {
			return fmt.Errorf("project: read member robots: %w", err)
		}
	}
	ownersByRobot := make(map[string]string, len(robots))
	for _, robot := range robots {
		ownersByRobot[robot.RobotID] = robot.CreatorUID
	}
	for _, member := range members {
		if user, ok := usersByUID[member.UID]; ok {
			member.Name = user.Name
			member.Robot = user.Robot
		}
		member.OwnerUID = ownersByRobot[member.UID]
	}
	return nil
}

type projectReadSeatCount struct {
	Total  int
	Humans int
	Agents int
}

func (d *DB) projectReadSeatCountsTx(tx *dbr.Tx, projectIDs []string) (map[string]projectReadSeatCount, error) {
	counts := make(map[string]projectReadSeatCount, len(projectIDs))
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	if len(projectIDs) == 0 {
		return counts, nil
	}
	var seats []struct {
		ProjectID string `db:"project_id"`
		UID       string `db:"uid"`
	}
	if _, err := tx.SelectBySql(
		"SELECT pm.project_id, pm.uid FROM `octo_project_member` pm INNER JOIN `octo_project` p "+
			"ON p.project_id = pm.project_id AND p.space_id = pm.space_id "+
			"WHERE pm.project_id IN ? AND p.status = ? AND pm.status = ? AND pm.removing = 0",
		projectIDs, StatusNormal, MemberStatusActive,
	).Load(&seats); err != nil {
		return nil, fmt.Errorf("project: read seats for project list: %w", err)
	}
	if len(seats) == 0 {
		return counts, nil
	}
	uids := make([]string, 0, len(seats))
	seen := make(map[string]struct{}, len(seats))
	for _, seat := range seats {
		count := counts[seat.ProjectID]
		count.Total++
		counts[seat.ProjectID] = count
		if _, ok := seen[seat.UID]; !ok {
			seen[seat.UID] = struct{}{}
			uids = append(uids, seat.UID)
		}
	}
	var bots []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM `user` WHERE uid IN ? AND robot = 1", uids,
	).Load(&bots); err != nil {
		return nil, fmt.Errorf("project: classify seats for project list: %w", err)
	}
	botSet := make(map[string]struct{}, len(bots))
	for _, uid := range bots {
		botSet[uid] = struct{}{}
	}
	for _, seat := range seats {
		count := counts[seat.ProjectID]
		if _, ok := botSet[seat.UID]; ok {
			count.Agents++
		} else {
			count.Humans++
		}
		counts[seat.ProjectID] = count
	}
	return counts, nil
}

func (d *DB) memberCollaborationRolesReadTx(tx *dbr.Tx, projectID string, uids []string) (map[string][]CollaborationRoleResp, error) {
	byUID := make(map[string][]CollaborationRoleResp, len(uids))
	for _, uid := range uids {
		byUID[uid] = []CollaborationRoleResp{}
	}
	if err := d.ensureReadTx(tx); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return byUID, nil
	}
	var rows []collaborationRoleBindingRow
	if _, err := tx.SelectBySql(
		"SELECT b.uid, r.role_id, IFNULL(r.builtin_key, '') AS builtin_key, r.name, r.source "+
			"FROM `octo_project_member_collaboration_role` b INNER JOIN `octo_project_collaboration_role` r "+
			"ON r.project_id = b.project_id AND r.role_id = b.role_id "+
			"WHERE b.project_id = ? AND b.uid IN ? ORDER BY b.uid, CASE r.builtin_key "+
			"WHEN 'product' THEN 1 WHEN 'frontend' THEN 2 WHEN 'backend' THEN 3 WHEN 'hr' THEN 4 ELSE 5 END, "+
			"r.created_at ASC, r.role_id ASC",
		projectID, uids,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: read member collaboration roles: %w", err)
	}
	for _, row := range rows {
		byUID[row.UID] = append(byUID[row.UID], CollaborationRoleResp{
			RoleID: row.RoleID, BuiltinKey: row.BuiltinKey, Name: row.Name, Source: row.Source,
		})
	}
	return byUID, nil
}

func projectLikePattern(keyword string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(keyword)
	return "%" + escaped + "%"
}
