// Package project exposes read-only Project membership facts that other modules
// need without importing modules/project.
//
// This package must never import modules/project (pinned by
// TestPkgProjectDoesNotImportModulesProject); doing so would put the import
// cycle back.
package project

import (
	"github.com/gocraft/dbr/v2"
)

// Membership is one project's membership fact for a uid.
type Membership struct {
	ProjectID   string `db:"project_id"`
	Role        int    `db:"role"`
	MemberEpoch int64  `db:"member_epoch"`
}

// MembershipsInSpace returns, for the named projects, the ones where uid holds
// an active seat — keyed by project_id. Absent from the map means "not a
// member", for any reason.
//
// One query for the whole batch. This feeds /v1/auth/verify, which every
// subsystem that fronts octo-server calls on EVERY request, so a per-id loop
// would multiply the gateway's database load by the number of projects a
// request happens to mention.
//
// The Space filter is part of the predicate rather than a separate check: a
// project in another Space must be indistinguishable from one that does not
// exist, and the cheapest way to guarantee that is for both to produce the same
// absence rather than two branches that could drift.
//
// `removing = 0` is part of the membership fact: a seat being closed is not a
// member, and a consumer that disagreed would authorize access to a Project
// whose memberships are being torn down.
func MembershipsInSpace(session *dbr.Session, spaceID, uid string, projectIDs []string) (map[string]Membership, error) {
	out := make(map[string]Membership, len(projectIDs))
	if spaceID == "" || uid == "" || len(projectIDs) == 0 {
		return out, nil
	}
	lookup := make([]string, 0, len(projectIDs))
	seen := make(map[string]bool, len(projectIDs))
	for _, pid := range projectIDs {
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		lookup = append(lookup, pid)
	}
	if len(lookup) == 0 {
		return out, nil
	}
	var rows []Membership
	_, err := session.SelectBySql(
		"SELECT pm.project_id, pm.role, p.member_epoch "+
			"FROM `octo_project_member` pm "+
			"INNER JOIN `octo_project` p ON p.project_id = pm.project_id AND p.status = 1 "+
			"WHERE pm.uid = ? AND pm.project_id IN ? AND pm.space_id = ? "+
			"  AND pm.status = 1 AND pm.removing = 0",
		uid, lookup, spaceID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProjectID] = r
	}
	return out, nil
}

// IsAllMemberGroup reports whether groupNo is the active Project's dedicated
// all-member group. The Project pointer is authoritative: a normal group with
// project_id set is intentionally not treated as protected or synchronized.
//
// Read the two sides separately. `octo_project` is pinned to utf8mb4_general_ci
// while production imports of the legacy `group` table may still use
// utf8mb4_0900_ai_ci; comparing their identifiers in one JOIN raises MySQL
// error 1267 and also prevents the group unique index from serving the lookup.
func IsAllMemberGroup(session *dbr.Session, projectID, groupNo string) (bool, error) {
	if session == nil || projectID == "" || groupNo == "" {
		return false, nil
	}

	var pointers []string
	if _, err := session.SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` "+
			"WHERE project_id = ? AND status = 1",
		projectID,
	).Load(&pointers); err != nil {
		return false, err
	}
	if len(pointers) == 0 || pointers[0] != groupNo {
		return false, nil
	}

	var rows []int
	if _, err := session.SelectBySql(
		"SELECT 1 FROM `group` "+
			"WHERE group_no = ? AND status <> 2 AND project_id = ?",
		groupNo, projectID,
	).Load(&rows); err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
