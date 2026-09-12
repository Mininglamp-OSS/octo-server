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
