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

// queryWorkspaceGroupCount counts only live group rows currently bound to the
// target Workspace. A disbanded group is not a visible relation, matching the
// native group's liveness convention (disabled is still a live group).
func (d *DB) queryWorkspaceGroupCount(spaceID, workspaceID, keyword string) (int64, error) {
	builder := d.session.Select("COUNT(*)").From("`group`").
		Where("space_id=? AND workspace_id=? AND status<>?", spaceID, workspaceID, GroupStatusDisband)
	if keyword != "" {
		builder = builder.Where("name LIKE ?", workspaceGroupLike(keyword))
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
		builder = builder.Where("name LIKE ?", workspaceGroupLike(keyword))
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

func workspaceGroupLike(keyword string) string {
	// Keep '%' and '_' literal. MySQL's default LIKE escape is '\\', and
	// escaping backslashes first prevents input from manufacturing a wildcard.
	escaped := strings.ReplaceAll(keyword, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "%", `\%`)
	escaped = strings.ReplaceAll(escaped, "_", `\_`)
	return "%" + escaped + "%"
}
