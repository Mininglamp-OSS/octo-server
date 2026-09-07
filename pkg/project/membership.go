// Package project exposes the Project membership facts that other modules need
// in order to enforce invariant I2, without importing modules/project.
//
// I2 — for a group whose project_id is not the empty sentinel, every active
// group_member row belongs to a uid that is an active member of that project
// (system bots exempted by whitelist).
//
// Why a pkg/ package rather than a method on modules/project's service:
// modules/group needs a PREDICATE, not a module. pkg/space is the precedent —
// CheckMembership, MemberRole: plain functions over a session, no module import,
// no init-order coupling — and it is what lets modules/group and modules/project
// both depend on the same fact without either importing the other. The
// dependency that DOES run module-to-module is the cascade, and it runs the
// other way, reverse-registered: modules/group registers its detach step into
// modules/project, exactly as modules/group and modules/project already register
// steps into modules/space.
//
// This package must never import modules/project (pinned by
// TestPkgProjectDoesNotImportModulesProject); doing so would put the import
// cycle back.
package project

import (
	"sort"

	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// exemptFromMembership reports whether uid is admissible to a project group
// without holding a project seat.
//
// The whitelist is pkg/space's system-bot list — botfather, fileHelper and the
// other platform bots that are added to groups by the platform itself and have
// no user to grant them a project seat. Reusing that list rather than declaring
// a second one is deliberate: two whitelists drift, and the divergence shows up
// as "the file bot silently stopped being added to project groups".
//
// An ORDINARY bot is NOT exempt. A bot that some user invited into a group is
// admitted through the same gate as a person and needs an explicit project seat,
// because otherwise "invite a bot" becomes a way to put a listener inside a
// project group without anyone granting it access.
//
// pkg/space depends only on octo-lib — it imports no octo-server module — so
// taking the list from there adds no coupling this package did not already have.
func exemptFromMembership(uid string) bool {
	return space.IsSystemBot(uid)
}

// AssertMembersInProjectTx answers, inside the caller's transaction, which of
// `uids` may NOT be admitted to a group belonging to `projectID`.
//
// It returns the inadmissible uids, in the order they were given, so the caller
// can name them in an error. An empty result means every uid passed.
//
// projectID == "" means the group is Space-direct and this predicate does not
// apply; callers MUST short-circuit before calling (see C1 — a gate that runs
// and passes is still a latency regression on every group join in the product),
// but passing "" here is answered "everything is admissible" rather than
// panicking, because a predicate that fails open on its own is worse than one
// that is simply not reached.
//
// # Why this takes a *dbr.Tx and locks
//
// The check has to be inside the transaction that commits the admission, or it
// is a TOCTOU with a comment. pkg/space.CheckMembership takes a *dbr.Session, so
// it necessarily runs on a different pooled connection in its own implicit
// transaction: a read there proves nothing about the state at COMMIT time. That
// is a known, tolerated weakness of the Space half of the composite gate —
// tightening it changes behaviour on every group join in the product and is out
// of scope here — but the project half must not copy it. `FOR SHARE` makes a
// concurrent project-seat removal block until this transaction commits.
//
// # Lock order
//
// The module's declared order is
//
//	space_member -> space -> project -> group -> group_member -> octo_project_member
//
// and octo_project_member is deliberately LAST. This function is therefore safe
// to call at the point of admission, with the group rows already held: it only
// ever adds the final link, never an earlier one, so it cannot close a cycle
// against a path that took the same links in the same order. Callers must not
// invert it by locking a project seat and then reaching back for a group row.
//
// uids are sorted before the IN clause so that two concurrent admissions to
// different groups of the same project acquire their locks in the same order.
// Without that, two overlapping batches deadlock on each other in whichever
// order MySQL happens to evaluate them — the same reason RemoveGroupMembers
// sorts its targets by uid.
func AssertMembersInProjectTx(tx *dbr.Tx, projectID string, uids []string) ([]string, error) {
	if projectID == "" || len(uids) == 0 {
		return nil, nil
	}

	// Deduplicate and drop exempt uids before the query. A caller passing the
	// same uid twice must not turn into two lock acquisitions.
	lookup := make([]string, 0, len(uids))
	seen := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if uid == "" || seen[uid] || exemptFromMembership(uid) {
			continue
		}
		seen[uid] = true
		lookup = append(lookup, uid)
	}
	if len(lookup) == 0 {
		return nil, nil
	}
	sort.Strings(lookup)

	// One query for the whole batch (D10). Per-uid checks would put N queries
	// inside the admission transaction and lengthen it in proportion to batch
	// size, and the batch cap is 200.
	//
	// `removing = 1` counts as a non-member here, which is the entire point of
	// that column: the seat is closing, its group rows are being torn down, and
	// admitting into another of the project's groups in the middle of that would
	// race the cascade it is already running.
	var present []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid IN ? AND status = 1 AND removing = 0 "+
			"FOR SHARE",
		projectID, lookup,
	).Load(&present)
	if err != nil {
		return nil, err
	}

	ok := make(map[string]bool, len(present))
	for _, uid := range present {
		ok[uid] = true
	}

	// Report in the caller's original order, deduplicated: an error message that
	// reorders the uids the caller sent is harder to act on.
	missing := make([]string, 0)
	reported := make(map[string]bool, len(lookup))
	for _, uid := range uids {
		if uid == "" || exemptFromMembership(uid) || reported[uid] {
			continue
		}
		if !ok[uid] {
			reported[uid] = true
			missing = append(missing, uid)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	return missing, nil
}

// CheckMembership reports whether uid is an active member of projectID, for
// READ paths that are not committing an admission — the reconcile scans and the
// /v1/auth/verify read contract.
//
// It is the session-scoped sibling of AssertMembersInProjectTx and carries the
// same `removing = 0` clause, so every authorization read in the product answers
// "is this a member?" the same way while a seat is closing. Do NOT use it to
// gate a write: a session read runs outside the caller's transaction and cannot
// see the state the write will commit against.
//
// Unlike the Tx variant this does NOT apply the system-bot exemption. Exemption
// is an admission rule ("may this uid be put in a group of this project"), not a
// membership fact ("is this uid a member of this project"), and a read path that
// reported system bots as project members would put them in member counts and in
// the verify response.
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
func MemberRole(session *dbr.Session, projectID string, uid string) (role int, ok bool, err error) {
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
// That is the admission gate's job, and it happens inside the create
// transaction: the creator is admitted through admitOrRestoreMembersTx like
// every other member, so a non-member creating a project group is refused there,
// under lock, rather than here in a read that could go stale before the commit.
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
// `removing = 0` is here for the same reason it is in every other predicate in
// this package: a seat being closed is not a member, and a consumer that
// disagreed with the admission gate about that would be authorizing access to a
// project whose groups are being torn down.
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
