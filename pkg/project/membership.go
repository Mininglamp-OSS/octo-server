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
// # Lock order — what is actually true, and what is not
//
// An earlier version of this comment claimed the module's table order
//
//	space_member -> space -> project -> group -> group_member -> octo_project_member
//
// held here, with octo_project_member "deliberately LAST", and concluded that
// calling this at the point of admission "with the group rows already held"
// could not close a cycle. That was wrong, and PR #846's review reproduced the
// consequence on MySQL 8.0.46. On the primary admission path the group rows are
// NOT held: admitOrRestoreMembersTx calls the gate FIRST and issues the
// group_member upsert SECOND, so the real acquisition order there is
// octo_project_member -> group_member. A11 (the un-blacklist branch) is the
// same. The project-group handover goes the other way — group_member first,
// then this function's shared lock.
//
// Two such transactions do not deadlock, because both take octo_project_member
// SHARED. Three can, because InnoDB will not grant a shared request that is
// queued behind a waiting exclusive one:
//
//	T1 admission     holds S(pm)                    wants X(gm)
//	T2 seat removal                                 wants X(pm)  -> queued behind T1
//	T3 handover      holds X(gm)                    wants S(pm)  -> queued behind T2
//
// T1 -> T3 -> T2 -> T1. InnoDB picks a victim; the cascade job backs off and
// retries and the API call errors, so the consequence is bounded — but it is
// reachable, and the previous comment said it was impossible, which is the part
// that cost something: nobody would look for it in a deadlock log.
//
// # The invariant that DOES hold, and must keep holding
//
//	No path takes an EXCLUSIVE lock on octo_project_member while holding any
//	group_member lock.
//
// Every path was checked against it: the funnel and A11 take S(pm) before any
// group_member row; the handover takes S(pm) — shared, deliberately, so a
// concurrent admission is not serialised behind it — while holding group_member
// rows; modules/project's seat writes take X(pm) holding no group locks at all.
// TestNoExclusiveProjectMemberLockUnderAGroupMemberLock pins it.
//
// Tightening the handover's `FOR SHARE OF pm` to `FOR UPDATE` is what the
// invariant forbids, and it is the change that looks harmless: it would turn
// the three-way cycle above into a plain two-way ABBA between the funnel and
// the handover, on the hottest write path in the module.
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

// ProjectEpochsInSpace returns member_epoch for each named ACTIVE project in
// spaceID, keyed by project_id.
//
// Absent from the map means the project does not exist, is disbanded, or lives
// in another Space. The caller maps all three to epoch 0 — one indistinguishable
// answer, for the same reason MembershipsInSpace folds them together: telling
// them apart would let a caller probe which Space a project id lives in.
//
// `status = 1` is what makes disband converge without a separate event: a
// disbanded project drops out of this result, the caller reads 0, and an
// authorization snapshot taken against the old epoch stops matching.
func ProjectEpochsInSpace(session *dbr.Session, spaceID string, projectIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(projectIDs))
	if spaceID == "" || len(projectIDs) == 0 {
		return out, nil
	}
	lookup := dedupeNonEmpty(projectIDs)
	if len(lookup) == 0 {
		return out, nil
	}
	var rows []struct {
		ProjectID   string `db:"project_id"`
		MemberEpoch int64  `db:"member_epoch"`
	}
	_, err := session.SelectBySql(
		"SELECT project_id, member_epoch FROM `octo_project` "+
			"WHERE space_id = ? AND project_id IN ? AND status = 1",
		spaceID, lookup,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProjectID] = r.MemberEpoch
	}
	return out, nil
}

// ProjectMemberships answers, for ONE project, which of the named uids hold an
// active seat — the mirror image of MembershipsInSpace, which answers one uid
// across many projects.
//
// Returns epoch 0 and an empty map when the project is not an active project of
// spaceID. Same folded answer as ProjectEpochsInSpace, same reason.
//
// # Read order is load-bearing
//
// The epoch is read BEFORE the member rows, and that order must not be swapped.
// A membership write bumps member_epoch in the same transaction, so a consumer
// caches this answer under the epoch and re-checks the epoch later to decide
// whether the cache is still good. The invariant that makes that sound is:
//
//	the returned epoch is never NEWER than the returned membership data.
//
// Read epoch first and a change landing between the two queries yields an OLD
// epoch beside NEW membership: the consumer's next epoch check differs, so it
// re-verifies. One wasted round trip, no stale grant.
//
// Read members first and the same interleaving yields NEW epoch beside OLD
// membership: the consumer caches a stale positive under the current epoch, and
// its epoch check keeps agreeing. A removed member stays authorized until the
// next unrelated bump. That is the bug this ordering exists to prevent, and it
// is invisible in a single-threaded test — see the ordering guard test.
//
// `removing = 0` is here for the same reason as every other predicate in this
// package: a seat being closed is not a member.
//
// # The Space half is conjoined here, and only here
//
// A project seat alone is NOT the answer to "may this uid act in this project".
// The Space→project cascade is ASYNCHRONOUS: when a user is removed from a Space,
// the project seat survives until a background job gets to it, that job's cleanup
// step deactivates the seat directly without the synchronous `removing = 1` phase
// (modules/project/space_member_removal.go), and it gives up after
// removalCleanupMaxAttempts with the row kept but never re-claimed. So `status = 1
// AND removing = 0` on its own reports a removed user as a member for a window
// that is unbounded in the failure case.
//
// Every OTHER caller of that predicate runs downstream of a Space gate — the
// project routes go through spacepkg.CheckMembership in middleware, and group
// admission conjoins spacepkg.ActiveMembers explicitly. This function is the
// first caller with no gate in front of it: its consumer is a peer control plane
// asking about a THIRD party, for whom it holds no token, and no endpoint in this
// repository lets it obtain the Space half itself. "Keep your Space check and
// layer this on top" is advice that consumer cannot act on, so the conjunction
// has to happen server-side.
//
// The predicate comes from spacepkg.ActiveMembers rather than being spelled out
// again, so it cannot drift from CheckMembership's.
//
// MembershipsInSpace is deliberately NOT changed: it answers for the caller's OWN
// token holder on a route that already ran SpaceMiddleware, so its consumer both
// has the Space half and has already applied it.
//
// # What this does NOT close
//
// Only FRESH answers. A grant the consumer cached BEFORE the Space removal keeps
// riding epoch agreement, because a Space removal does not move member_epoch —
// the epoch only bumps when the cascade actually changes a project row. Closing
// that requires either an epoch bump at Space-removal commit time or a hard TTL
// in the peer contract that does not depend on epoch agreement. Neither is in
// this function.
func ProjectMemberships(session *dbr.Session, spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	roles := make(map[string]int, len(uids))
	if spaceID == "" || projectID == "" {
		return 0, roles, nil
	}

	// Step 1 — epoch + existence, in that order. See the doc comment.
	epochs, err := ProjectEpochsInSpace(session, spaceID, []string{projectID})
	if err != nil {
		return 0, nil, err
	}
	epoch, active := epochs[projectID]
	if !active {
		return 0, roles, nil
	}

	lookup := dedupeNonEmpty(uids)
	if len(lookup) == 0 {
		return epoch, roles, nil
	}

	// Step 2 — the seats. project_id + uid is the primary key, so this is a PK
	// range scan. space_id is redundant on the member row and is repeated here as
	// defence in depth: step 1 already established that projectID belongs to
	// spaceID, so this predicate can only ever remove rows whose denormalized
	// copy has drifted — which is a bug we would rather fail closed on.
	var rows []struct {
		UID  string `db:"uid"`
		Role int    `db:"role"`
	}
	_, err = session.SelectBySql(
		"SELECT uid, role FROM `octo_project_member` "+
			"WHERE project_id = ? AND space_id = ? AND uid IN ? "+
			"  AND status = 1 AND removing = 0",
		projectID, spaceID, lookup,
	).Load(&rows)
	if err != nil {
		return 0, nil, err
	}
	if len(rows) == 0 {
		return epoch, roles, nil
	}

	// Step 3 — the Space half. LAST on purpose, and for the same reason step 1 is
	// first: this read is the one that can only ever REMOVE a uid from the answer,
	// so the newest data landing here is the fail-closed direction. Reading it
	// before the seats would let a Space removal committing in between produce a
	// stale positive; reading it after can at worst deny someone who was
	// re-admitted microseconds ago, and they re-verify.
	//
	// Only the uids that actually hold a seat are asked about, so the batch is
	// bounded by the answer rather than by the request.
	seated := make([]string, 0, len(rows))
	for _, r := range rows {
		seated = append(seated, r.UID)
	}
	inSpace, err := space.ActiveMembers(session, spaceID, seated)
	if err != nil {
		return 0, nil, err
	}
	for _, r := range rows {
		if !inSpace[r.UID] {
			continue
		}
		roles[r.UID] = r.Role
	}
	return epoch, roles, nil
}

// dedupeNonEmpty drops empty and repeated ids while preserving first-seen order.
//
// Shared by both batch queries so neither can grow its own copy that forgets the
// empty-string case — an empty id in an IN list matches nothing but still costs a
// bind parameter, and callers reach these functions straight off the wire.
func dedupeNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
