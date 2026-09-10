package project

import (
	"fmt"
	"strings"
	"time"

	"github.com/gocraft/dbr/v2"
)

// projectGroupPinTarget is the authoritative group relation projection used by
// the pin write transaction.  It intentionally contains only the fields needed
// to prove the group still belongs to the requested Project and Space.
type projectGroupPinTarget struct {
	GroupNo   string `db:"group_no"`
	SpaceID   string `db:"space_id"`
	ProjectID string `db:"project_id"`
	Status    int    `db:"status"`
}

// lockProjectGroupPinTargetTx re-reads the group relation while holding the
// group row lock.  Project access must be locked first by
// LockGroupProjectAccessTx; keeping this helper narrow makes that lock order
// explicit at the call site and avoids importing modules/group.
func (d *DB) lockProjectGroupPinTargetTx(tx *dbr.Tx, groupNo string) (*projectGroupPinTarget, error) {
	if tx == nil || strings.TrimSpace(groupNo) == "" {
		return nil, fmt.Errorf("%w: empty transaction or group_no", ErrGroupProjectInvalid)
	}
	groupNo = strings.TrimSpace(groupNo)

	var rows []projectGroupPinTarget
	_, err := tx.SelectBySql(
		"SELECT group_no, COALESCE(space_id, '') AS space_id, "+
			"COALESCE(project_id, '') AS project_id, COALESCE(status, 0) AS status "+
			"FROM `group` "+
			"WHERE group_no = ? LIMIT 1 FOR UPDATE",
		groupNo,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("%w: lock group relation: %v", ErrGroupProjectDependency, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// upsertProjectGroupUserSettingTx stores one user's Project-scoped group pin.
// The transaction caller has already revalidated and locked the Project access
// and current group relation.
//
// The duplicate-key update is deliberately state-aware.  A repeated pin keeps
// both pinned_at and updated_at unchanged, while a false-to-true transition
// gets the current server time.  Clearing a pin removes its sort timestamp;
// retaining the row keeps the four-part personal preference identity stable.
func (d *DB) upsertProjectGroupUserSettingTx(
	tx *dbr.Tx,
	spaceID, projectID, groupNo, uid string,
	pinned bool,
) error {
	spaceID = strings.TrimSpace(spaceID)
	projectID = strings.TrimSpace(projectID)
	groupNo = strings.TrimSpace(groupNo)
	uid = strings.TrimSpace(uid)
	if tx == nil || spaceID == "" || projectID == "" || groupNo == "" || uid == "" {
		return fmt.Errorf("%w: invalid group user setting arguments", ErrGroupProjectInvalid)
	}

	now := time.Now().UTC()
	flag := 0
	var pinnedAt interface{}
	if pinned {
		flag = 1
		pinnedAt = now
	}

	_, err := tx.InsertBySql(
		"INSERT INTO octo_project_group_user_setting "+
			"(space_id, project_id, group_no, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE "+
			"pinned_at = CASE "+
			"WHEN VALUES(pinned) = 1 AND pinned = 0 THEN VALUES(pinned_at) "+
			"WHEN VALUES(pinned) = 0 THEN NULL "+
			"ELSE pinned_at END, "+
			"updated_at = CASE "+
			"WHEN VALUES(pinned) <> pinned THEN VALUES(updated_at) "+
			"ELSE updated_at END, "+
			"pinned = VALUES(pinned)",
		spaceID, projectID, groupNo, uid, flag, pinnedAt, now, now,
	).Exec()
	if err != nil {
		return fmt.Errorf("%w: upsert group user setting: %v", ErrGroupProjectDependency, err)
	}
	return nil
}
