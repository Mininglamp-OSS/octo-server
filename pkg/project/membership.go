// Package project exposes read-only Project membership and attribution facts
// that other modules need without importing modules/project.
//
// CheckMembership and MemberRole are plain session-runner predicates shared by
// reconcile and authorization reads. ResolveForGroup answers whether a group
// belongs to an active Project in the requested Space, and MembershipsInSpace
// batches the membership facts needed by verification.
//
// This package must never import modules/project (pinned by
// TestPkgProjectDoesNotImportModulesProject); doing so would put the import
// cycle back.
package project

import (
	"github.com/gocraft/dbr/v2"
)

// CheckMembership reports whether uid is an active member of projectID, for
// READ paths such as the reconcile scans and the /v1/auth/verify read contract.
//
// It is session-scoped and carries the `removing = 0` clause, so every
// authorization read in the product answers "is this a member?" the same way
// while a seat is closing. Do NOT use it to gate a write: a session read runs
// outside the caller's transaction and cannot see the state the write will
// commit against.
func CheckMembership(session *dbr.Session, projectID string, uid string) (bool, error) {
	if projectID == "" || uid == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0",
		projectID, uid,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// MemberRole returns uid's role in projectID and whether they hold an active
// seat at all. ok=false means "not an active member", and role is then
// meaningless — callers must check ok before reading role.
//
// Role numbers are octo_project_member.role: 0 = member, 1 = admin, 2 = owner.
// Consumers outside octo-server must NOT be handed these to derive permissions
// from; the verify read contract emits explicit capabilities alongside the role
// for exactly that reason (D11).
// The runner is an interface rather than *dbr.Session so a caller that has
// already opened a transaction can pass its *dbr.Tx. dbr.SessionRunner is the
// repo's existing way of saying "either one" (modules/user/db_manager.go);
// widening to it changes no call site.
func MemberRole(session dbr.SessionRunner, projectID string, uid string) (role int, ok bool, err error) {
	if projectID == "" || uid == "" {
		return 0, false, nil
	}
	var roles []int
	rows, err := session.SelectBySql(
		"SELECT role FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0 LIMIT 1",
		projectID, uid,
	).Load(&roles)
	if err != nil {
		return 0, false, err
	}
	if rows == 0 || len(roles) == 0 {
		return 0, false, nil
	}
	return roles[0], true, nil
}

// ResolveForGroup answers whether a group in spaceID may be attributed to
// projectID: the project must exist, be active, and belong to that same Space.
//
// ok=false covers all three failures — absent, disbanded, and cross-Space — and
// the caller must NOT distinguish them on the wire. Doing so turns "create a
// group" into an oracle: an attacker with a project id they cannot see could
// learn whether it exists and which Space it lives in, from a Space they do have
// access to. The reason belongs in the log.
//
// Deliberately does NOT check whether the caller is a member of the project.
// Caller-specific authorization belongs to the operation's own transaction,
// rather than to this read-only attribution lookup, because a separate read
// could go stale before the write commits.
//
// The status literal is spelled out rather than importing modules/project's
// constant, for the same reason pkg/space spells out space.status: the import
// would be a cycle.
func ResolveForGroup(session *dbr.Session, spaceID, projectID string) (bool, error) {
	if spaceID == "" || projectID == "" {
		return false, nil
	}
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` "+
			"WHERE project_id = ? AND space_id = ? AND status = 1",
		projectID, spaceID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

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
