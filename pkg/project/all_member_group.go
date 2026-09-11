package project

import (
	"errors"

	"github.com/gocraft/dbr/v2"
)

// ErrAdmittedButNotSubscribed marks the case where the native group-member row
// committed but the broker subscription did not. Callers must not report this
// as a failed membership write: the durable local state is already admitted.
var ErrAdmittedButNotSubscribed = errors.New("project: admitted to all-member group but not subscribed")

// CheckMembership reports whether uid currently holds an active Project seat.
// A seat being closed (removing = 1) is not active for cascade decisions.
func CheckMembership(session *dbr.Session, projectID, uid string) (bool, error) {
	if session == nil || projectID == "" || uid == "" {
		return false, nil
	}
	var rows []int
	_, err := session.SelectBySql(
		"SELECT 1 FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0 LIMIT 1",
		projectID, uid,
	).Load(&rows)
	return len(rows) > 0, err
}

// MemberRole returns an active Project role for uid. ok=false means that the
// seat is absent, removed, or currently closing.
func MemberRole(session dbr.SessionRunner, projectID, uid string) (role int, ok bool, err error) {
	if session == nil || projectID == "" || uid == "" {
		return 0, false, nil
	}
	var rows []int
	_, err = session.SelectBySql(
		"SELECT role FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0 LIMIT 1",
		projectID, uid,
	).Load(&rows)
	if err != nil || len(rows) == 0 {
		return 0, false, err
	}
	return rows[0], true, nil
}

// PickActiveOwner returns the longest-standing active Project owner. An
// ownerless Project is a normal intermediate state and returns "" without an
// error; callers must not promote a non-owner as a substitute.
func PickActiveOwner(session dbr.SessionRunner, projectID string) (string, error) {
	if session == nil || projectID == "" {
		return "", nil
	}
	var rows []string
	_, err := session.SelectBySql(
		"SELECT uid FROM `octo_project_member` "+
			"WHERE project_id = ? AND role = 2 AND status = 1 AND removing = 0 "+
			"ORDER BY COALESCE(joined_at, created_at) ASC, created_at ASC, uid ASC LIMIT 1",
		projectID,
	).Load(&rows)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0], nil
}
