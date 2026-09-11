package group

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/gocraft/dbr/v2"
)

// GroupWorkspace is the deliberately restricted projection used by the
// group↔Workspace relation endpoints. It must stay separate from GroupResp:
// relation metadata never belongs in ordinary group responses.
type GroupWorkspace struct {
	GroupNo     string  `json:"group_no" db:"group_no"`
	Name        string  `json:"name" db:"name"`
	WorkspaceID *string `json:"workspace_id" db:"workspace_id"`
	LinkedBy    *string `json:"linked_by" db:"workspace_linked_by"`
}

// groupWorkspaceRow is private so SQL columns and authoritative group state
// cannot leak into the restricted public DTO.
type groupWorkspaceRow struct {
	GroupNo           string  `db:"group_no"`
	Name              string  `db:"name"`
	SpaceID           string  `db:"space_id"`
	Status            int     `db:"status"`
	WorkspaceID       *string `db:"workspace_id"`
	WorkspaceLinkedBy *string `db:"workspace_linked_by"`
}

const groupWorkspaceRowColumns = "group_no, name, space_id, status, workspace_id, workspace_linked_by"

func (d *DB) queryGroupWorkspace(groupNo string) (*groupWorkspaceRow, error) {
	var row *groupWorkspaceRow
	_, err := d.session.Select(groupWorkspaceRowColumns).
		From("`group`").Where("group_no=?", groupNo).Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// queryGroupWorkspaceTx reads the relation row from the caller-owned
// transaction. It deliberately does not lock: GET handlers use a repeatable
// read snapshot and must not turn relation reads into write contention.
func (d *DB) queryGroupWorkspaceTx(tx *dbr.Tx, groupNo string) (*groupWorkspaceRow, error) {
	var row *groupWorkspaceRow
	_, err := tx.Select(groupWorkspaceRowColumns).
		From("`group`").Where("group_no=?", groupNo).Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// lockGroupWorkspaceTx locks the current group row. Relation writes use this
// narrow query rather than a broad model rewrite, so concurrent ordinary group
// updates cannot overwrite the relation columns.
func (d *DB) lockGroupWorkspaceTx(groupNo string, tx *dbr.Tx) (*groupWorkspaceRow, error) {
	var row *groupWorkspaceRow
	_, err := tx.Select(groupWorkspaceRowColumns).
		From("`group`").Where("group_no=?", groupNo).Suffix("FOR UPDATE").Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// updateGroupWorkspaceTx is the only relation write primitive. Both paired
// nullable columns are updated together; no group membership, ACL, or native
// group field is touched by this operation.
func (d *DB) updateGroupWorkspaceTx(groupNo string, workspaceID, linkedBy *string, tx *dbr.Tx) error {
	_, err := tx.Update("group").
		Set("workspace_id", workspaceID).
		Set("workspace_linked_by", linkedBy).
		Where("group_no=?", groupNo).Exec()
	return err
}

// lockGroupManagerTx rechecks native group management authority after the
// group row is locked. This intentionally retains the native predicates:
// active/non-deleted, non-blacklisted, non-external owner/admin only.
func (d *DB) lockGroupManagerTx(groupNo, uid string, tx *dbr.Tx) (bool, error) {
	var role int
	err := tx.SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 AND status=? AND is_external=0 AND (role=? OR role=?) FOR UPDATE",
		groupNo, uid, common.GroupMemberStatusNormal, MemberRoleCreator, MemberRoleManager,
	).LoadOne(&role)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return role == MemberRoleCreator || role == MemberRoleManager, nil
}

// queryWorkspaceGroupCountTx counts relation rows using the caller-owned
// repeatable-read snapshot. The filter is shared with queryWorkspaceGroupsTx.
func (d *DB) queryWorkspaceGroupCountTx(tx *dbr.Tx, spaceID, workspaceID, keyword string) (int64, error) {
	builder := tx.Select("COUNT(*)").From("`group`").
		Where("space_id=? AND workspace_id=? AND status<>?", spaceID, workspaceID, GroupStatusDisband)
	if keyword != "" {
		builder = builder.Where("name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'", []byte(workspaceGroupLike(keyword)))
	}
	var count int64
	_, err := builder.Load(&count)
	return count, err
}

// queryWorkspaceGroupCount counts only live group rows currently bound to the
// target Workspace. A disbanded group is not a visible relation, matching the
// native group's liveness convention (disabled is still a live group).
func (d *DB) queryWorkspaceGroupCount(spaceID, workspaceID, keyword string) (int64, error) {
	builder := d.session.Select("COUNT(*)").From("`group`").
		Where("space_id=? AND workspace_id=? AND status<>?", spaceID, workspaceID, GroupStatusDisband)
	if keyword != "" {
		builder = builder.Where("name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'", []byte(workspaceGroupLike(keyword)))
	}
	var count int64
	_, err := builder.Load(&count)
	return count, err
}

// queryWorkspaceGroups returns the restricted metadata list. It deliberately
// does not join group_member: Workspace membership grants relation metadata
// visibility, not native group membership or content access.
func (d *DB) queryWorkspaceGroups(spaceID, workspaceID, keyword string, page workspace.Page) ([]GroupWorkspace, error) {
	offset, ok := workspacePageOffset(page)
	if !ok {
		return []GroupWorkspace{}, nil
	}
	builder := d.session.Select("group_no, name, workspace_id, workspace_linked_by").
		From("`group`").Where("space_id=? AND workspace_id=? AND status<>?", spaceID, workspaceID, GroupStatusDisband)
	if keyword != "" {
		builder = builder.Where("name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'", []byte(workspaceGroupLike(keyword)))
	}
	builder = builder.OrderAsc("group_no").Offset(offset).Limit(uint64(normalizeWorkspacePageSize(page.Size)))
	var rows []groupWorkspaceRow
	_, err := builder.Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make([]GroupWorkspace, 0, len(rows))
	for _, row := range rows {
		if err := validateGroupWorkspacePair(&row); err != nil {
			return nil, err
		}
		result = append(result, groupWorkspaceFromRow(&row))
	}
	return result, nil
}

// queryWorkspaceGroupsTx returns the restricted metadata list from the
// caller-owned repeatable-read snapshot. It deliberately does not join
// group_member: Workspace membership grants relation metadata visibility, not
// native group membership or content access.
func (d *DB) queryWorkspaceGroupsTx(tx *dbr.Tx, spaceID, workspaceID, keyword string, page workspace.Page) ([]GroupWorkspace, error) {
	offset, ok := workspacePageOffset(page)
	if !ok {
		return []GroupWorkspace{}, nil
	}
	builder := tx.Select("group_no, name, workspace_id, workspace_linked_by").
		From("`group`").Where("space_id=? AND workspace_id=? AND status<>?", spaceID, workspaceID, GroupStatusDisband)
	if keyword != "" {
		builder = builder.Where("name LIKE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci ESCAPE '!'", []byte(workspaceGroupLike(keyword)))
	}
	builder = builder.OrderAsc("group_no").Offset(offset).Limit(uint64(normalizeWorkspacePageSize(page.Size)))
	var rows []groupWorkspaceRow
	_, err := builder.Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make([]GroupWorkspace, 0, len(rows))
	for _, row := range rows {
		if err := validateGroupWorkspacePair(&row); err != nil {
			return nil, err
		}
		result = append(result, groupWorkspaceFromRow(&row))
	}
	return result, nil
}

// lockGroupSpaceMemberTx is the transaction-side equivalent of
// space.CheckMembership for an unbound relation DELETE. It explicitly locks
// the actor's membership row first, then the Space row, so the relation
// transaction follows the repository's seat/member -> Space lock order and a
// stale middleware decision cannot authorize a native manager operation.
func (d *DB) lockGroupSpaceMemberTx(spaceID, uid string, tx *dbr.Tx) (bool, error) {
	var memberStatus int
	err := tx.SelectBySql(
		"SELECT status FROM space_member WHERE space_id=? AND uid=? FOR UPDATE",
		spaceID, uid,
	).LoadOne(&memberStatus)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var spaceStatus int
	err = tx.SelectBySql(
		"SELECT status FROM space WHERE space_id=? FOR UPDATE",
		spaceID,
	).LoadOne(&spaceStatus)
	if errors.Is(err, dbr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return spaceStatus == 1 && memberStatus == 1, nil
}

// ExistMemberActiveTx is the read-only transaction equivalent of
// ExistMemberActive. It intentionally uses ordinary SELECT semantics so the
// caller's repeatable-read snapshot governs relation visibility.
func (d *DB) ExistMemberActiveTx(tx *dbr.Tx, uid, groupNo string) (bool, error) {
	var count int64
	_, err := tx.Select("count(*)").From("group_member").
		Where("group_no=? and uid=? and is_deleted=0 and status=?",
			groupNo, uid, common.GroupMemberStatusNormal).
		Load(&count)
	return count > 0, err
}

// queryGroupSpaceAccessTx revalidates the actor's account, Space, and seat
// state from the caller-owned read transaction. GET on an unbound relation
// still runs after the derived-Space middleware, so this check must use the
// same repeatable-read snapshot rather than trusting that earlier decision.
func (d *DB) queryGroupSpaceAccessTx(tx *dbr.Tx, spaceID, uid string) (bool, error) {
	var found []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space_member` sm "+
			"INNER JOIN `space` s ON s.space_id=sm.space_id AND s.status=1 "+
			"INNER JOIN `user` u ON u.uid=sm.uid "+
			"WHERE sm.space_id=? AND sm.uid=? AND sm.status=1 "+
			"AND u.status=1 AND IFNULL(u.is_destroy, 0)<>2 LIMIT 1",
		spaceID, uid,
	).Load(&found)
	return len(found) > 0, err
}

// workspaceGroupLike escapes LIKE metacharacters with '!', matching the
// explicit ESCAPE '!' clauses used by both group relation query paths. The
// query paths bind the result as []byte before CONVERT so dbr's MySQL string
// encoder cannot rewrite a literal backslash in the pattern.
func workspaceGroupLike(keyword string) string {
	return "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(keyword) + "%"
}
