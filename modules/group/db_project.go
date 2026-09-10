package group

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/gocraft/dbr/v2"
)

// GroupProjectRelation is the restricted relation projection. It intentionally
// contains no native membership, ACL, or chat-content fields.
type GroupProjectRelation struct {
	GroupNo   string  `json:"group_no" db:"group_no"`
	Name      string  `json:"name" db:"name"`
	ProjectID string  `json:"project_id" db:"project_id"`
	LinkedBy  *string `json:"linked_by" db:"project_linked_by"`
}

type groupProjectRelationRow struct {
	GroupNo         string  `db:"group_no"`
	Name            string  `db:"name"`
	SpaceID         string  `db:"space_id"`
	ProjectID       string  `db:"project_id"`
	ProjectLinkedBy *string `db:"project_linked_by"`
	Status          int     `db:"status"`
}

const groupProjectRelationColumns = "group_no, name, space_id, project_id, project_linked_by, status"

func (d *DB) queryGroupProjectRelation(groupNo string) (*groupProjectRelationRow, error) {
	var row *groupProjectRelationRow
	_, err := d.session.Select(groupProjectRelationColumns).
		From("`group`").Where("group_no=?", groupNo).Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	return row, err
}

func (d *DB) queryGroupProjectRelationTx(tx *dbr.Tx, groupNo string) (*groupProjectRelationRow, error) {
	var row *groupProjectRelationRow
	_, err := tx.Select(groupProjectRelationColumns).
		From("`group`").Where("group_no=?", groupNo).Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	return row, err
}

func (d *DB) lockGroupProjectRelationTx(tx *dbr.Tx, groupNo string) (*groupProjectRelationRow, error) {
	var row *groupProjectRelationRow
	_, err := tx.Select(groupProjectRelationColumns).
		From("`group`").Where("group_no=?", groupNo).Suffix("FOR UPDATE").Load(&row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	return row, err
}

// updateGroupProjectRelationTx is the only relation write primitive. The
// project_id and project_linked_by columns are always changed together.
func (d *DB) updateGroupProjectRelationTx(tx *dbr.Tx, groupNo, projectID, linkedBy string) error {
	var projectValue interface{}
	var linkedValue interface{}
	if strings.TrimSpace(projectID) == "" {
		projectValue = ""
		linkedValue = nil
	} else {
		projectValue = strings.TrimSpace(projectID)
		if strings.TrimSpace(linkedBy) == "" {
			linkedValue = nil
		} else {
			linkedValue = strings.TrimSpace(linkedBy)
		}
	}
	_, err := tx.Update("group").
		Set("project_id", projectValue).
		Set("project_linked_by", linkedValue).
		Where("group_no=?", groupNo).Exec()
	return err
}

// lockGroupManagerTx rechecks native group authority after the relation row is
// locked. A Project member without native group management remains forbidden.
func (d *DB) lockGroupManagerTx(tx *dbr.Tx, groupNo, uid string) (bool, error) {
	var role int
	err := tx.SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 AND status=? AND is_external=0 AND (role=? OR role=?) LIMIT 1 FOR UPDATE",
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

// lockGroupSpaceMemberTx authenticates an unbound relation operation using
// the authoritative group Space. The caller must not select a Space from input.
func (d *DB) lockGroupSpaceMemberTx(tx *dbr.Tx, spaceID, uid string) (bool, error) {
	var rows []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space_member` sm INNER JOIN `space` s ON s.space_id=sm.space_id "+
			"WHERE sm.space_id=? AND sm.uid=? AND sm.status=1 AND s.status=1 LIMIT 1 FOR UPDATE",
		spaceID, uid,
	).Load(&rows)
	return len(rows) > 0, err
}

func (d *DB) lockGroupUserEligibleTx(tx *dbr.Tx, uid string) (bool, error) {
	var rows []struct {
		Status    int `db:"status"`
		IsDestroy int `db:"is_destroy"`
	}
	_, err := tx.SelectBySql(
		"SELECT status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid=? LIMIT 1 FOR SHARE",
		uid,
	).Load(&rows)
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].Status == 1 && rows[0].IsDestroy != 2, nil
}

func projectIDFromPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func linkedByFromPointer(value *string) *string {
	if value == nil {
		return nil
	}
	v := strings.TrimSpace(*value)
	if v == "" {
		return nil
	}
	return &v
}

func validateGroupProjectRelation(row *groupProjectRelationRow) error {
	if row == nil {
		return errProjectRelationNotFound
	}
	// An unbound group cannot retain an actor. A historical bound relation may
	// have NULL actor because no safe inference exists; it remains readable.
	if strings.TrimSpace(row.ProjectID) == "" && row.ProjectLinkedBy != nil && strings.TrimSpace(*row.ProjectLinkedBy) != "" {
		return errProjectRelationCorrupt
	}
	return nil
}

func groupProjectRelationFromRow(row *groupProjectRelationRow) GroupProjectRelation {
	if row == nil {
		return GroupProjectRelation{}
	}
	projectID := strings.TrimSpace(row.ProjectID)
	return GroupProjectRelation{
		GroupNo:   row.GroupNo,
		Name:      row.Name,
		ProjectID: projectID,
		LinkedBy:  linkedByFromPointer(row.ProjectLinkedBy),
	}
}

func projectRelationAccessIDs(sourceID, targetID string) []string {
	ids := make([]string, 0, 2)
	if sourceID = strings.TrimSpace(sourceID); sourceID != "" {
		ids = append(ids, sourceID)
	}
	if targetID = strings.TrimSpace(targetID); targetID != "" && targetID != sourceID {
		ids = append(ids, targetID)
	}
	return ids
}

func (d *DB) queryGroupNativeReadAccessTx(tx *dbr.Tx, spaceID, groupNo, uid string) (bool, error) {
	var rows []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM group_member gm INNER JOIN `space_member` sm ON sm.uid=gm.uid "+
			"INNER JOIN `space` s ON s.space_id=sm.space_id "+
			"INNER JOIN `user` u ON u.uid=gm.uid "+
			"WHERE gm.group_no=? AND gm.uid=? AND gm.is_deleted=0 AND gm.status=? "+
			"AND sm.space_id=? AND sm.status=1 AND s.status=1 AND u.status=1 "+
			"AND COALESCE(u.is_destroy, 0)<>2 LIMIT 1",
		groupNo, uid, common.GroupMemberStatusNormal, spaceID,
	).Load(&rows)
	return len(rows) > 0, err
}
