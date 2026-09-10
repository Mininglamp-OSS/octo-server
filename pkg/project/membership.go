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
	"errors"
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/user"
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
// The runner is an interface rather than *dbr.Session so a caller that has
// already opened a transaction can pass its *dbr.Tx. That is not a convenience:
// modules/group's all-member owner sync holds FOR UPDATE locks on the group's
// group_member rows while it asks this question, and asking it on a pooled
// connection reads a DIFFERENT snapshot than the one its writes will land in —
// the role can change inside that window and the sync only re-fires on a project
// owner change. dbr.SessionRunner is the repo's existing way of saying "either
// one" (modules/group/bot_ownership.go, modules/user/db_manager.go), and widening
// to it changes no call site.
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

// AbsentEpochSentinel is the value the membership integration contract reserves
// for "this project does not exist or is not visible".
//
// It is a value in the same domain as a real epoch, which is what makes
// ErrLiveProjectOnAbsentSentinel necessary.
const AbsentEpochSentinel = 0

// ErrLiveProjectOnAbsentSentinel is returned when an ACTIVE project is found
// holding the reserved value, i.e. when the answer these functions would give is
// indistinguishable from "gone" for a project that is not.
//
// Callers MUST fail the request rather than serve the value. See
// ProjectEpochsInSpace for why the row can exist at all and why the read layer
// is where it has to be refused.
var ErrLiveProjectOnAbsentSentinel = errors.New(
	"project: an active project holds the reserved absent-epoch sentinel")

// SentinelAnomalyError carries WHICH project tripped the refusal, so a caller can
// repair that one row instead of waiting for the reconcile cursor to reach it.
//
// The wait is the reason this type exists. The rollout document told operators the
// scan repairs such a row "within one rotation", but scanEpochSanity walks a bounded
// page budget per tick behind a PERSISTED cursor — and a row inserted by a
// not-yet-upgraded pod gets the highest id, so it is reached only when the cursor
// completes its current pass. On a large octo_project that is hours, and every
// request naming that id returns 500 for the whole window because the refusal is
// per-batch. RepairAbsentSentinelEpoch closes that to one indexed UPDATE.
type SentinelAnomalyError struct {
	ProjectID string
}

func (e *SentinelAnomalyError) Error() string {
	return ErrLiveProjectOnAbsentSentinel.Error() + ": " + e.ProjectID
}

// Unwrap keeps errors.Is(err, ErrLiveProjectOnAbsentSentinel) true, which is what
// every existing caller branches on.
func (e *SentinelAnomalyError) Unwrap() error { return ErrLiveProjectOnAbsentSentinel }

// RepairAbsentSentinelEpoch lifts ONE active project off the reserved sentinel.
//
// The `member_epoch = 0` predicate is its own CAS, so N replicas racing to repair
// the same row produce exactly one increment; and `status = 1` keeps it off
// disbanded rows, whose 0 is the correct answer. Increment-only, like every other
// writer of this column.
//
// Reports whether a row was changed, so a caller can tell "repaired, retry is worth
// it" from "someone else already did, or the row is not actually anomalous".
//
// This is a duplicate of modules/project's own repair statement, deliberately: that
// one is a private method on a module the read layer's consumers do not import, and
// the whole point here is that the endpoint that HIT the anomaly can fix it. The
// shared statement shape is pinned by the write-discipline guard, which scans for
// exactly this increment form.
func RepairAbsentSentinelEpoch(session *dbr.Session, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	result, err := session.UpdateBySql(
		"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
			"WHERE project_id = ? AND status = 1 AND member_epoch = ?",
		projectID, AbsentEpochSentinel,
	).Exec()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// FoldID normalizes an identifier for MATCHING, and it is the only fold in this
// package — the read functions below key their answers by it so no caller has to
// re-derive the rule.
//
// # Why a fold is needed at all
//
// octo_project and octo_project_member are pinned to utf8mb4_general_ci, which is
// case-INSENSITIVE. `project_id IN (?)` therefore matches a stored `abc` when the
// caller sends `ABC`, and the row comes back spelled `abc`. Keying an answer off
// the spelling the DATABASE returned means a caller looking up its own `ABC`
// finds nothing — and an absent key is the "does not exist" / member:false
// sentinel, so a real member of a live project would read as denied while the SQL
// had matched perfectly.
//
// # ASCII-only, and this time actually
//
// This was strings.ToLower, described in a comment as "ASCII-only folding" and as
// "the STRICTER of the two, so the disagreement costs an answer of absent rather
// than a wrong positive". All three claims were false: strings.ToLower is UNICODE
// case mapping, and it is LOOSER than utf8mb4_general_ci. Measured on MySQL 8.0.33:
//
//	Go: strings.ToLower("\u212A") == "k"  -> true   (KELVIN SIGN)
//	Go: strings.ToLower("\u212B") == "å"  -> true   (ANGSTROM SIGN)
//	MySQL: (_utf8mb4 0xE284AA) COLLATE utf8mb4_general_ci = 'k'  -> 0
//	MySQL: (_utf8mb4 0xE284AB) COLLATE utf8mb4_general_ci = 'å'  -> 0
//
// So the fold merged identifiers the database keeps apart, and on _verify that is
// fail-OPEN: send uids ["k", "\u212A"], SQL matches only the real row `k`, both
// requested ids fold to "k", and the id the database never matched is served as a
// member WITH a role.
//
// A byte-level ASCII fold instead. Non-ASCII bytes are left untouched, which makes
// this strictly COARSER-than-nothing and strictly FINER than either collation
// (general_ci also folds accents; 0900_ai_ci folds accents and the compatibility
// characters above). Finer means a disagreement can only cost an "absent" answer,
// never a wrong positive — the direction the comment always claimed.
//
// Two spellings cannot collide into one wrong answer either: project_id and
// (project_id, uid) are UNIQUE under general_ci, whose equivalence classes are a
// superset of this fold's, so the database cannot hold two rows that this folds
// together.
//
// Rejecting non-canonical case at the boundary was the alternative and it is
// worse: it would break a caller holding a legitimately re-cased id, and nothing
// here has the standing to declare one spelling canonical — existing ids predate
// the UUID format and nothing validates their shape.
func FoldID(id string) string {
	b := []byte(id)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// FoldedHas reports whether want is present in a set keyed by the spelling the
// DATABASE returned.
//
// Every caller that intersects two of those sets needs this, and open-coding it is how
// one of the sites ends up exact-match: `space_member`, `user` and `octo_project_member`
// all compare `uid` case-insensitively, so a row can come back spelled differently from
// the uid that was asked for, and an exact lookup then drops a live, seated member. The
// direction is fail-closed — a real caller refused — which is the shape this package
// already treats as a defect on the read path.
//
// Folding the SET rather than the needle, because the set is the side with the
// database's spelling and there may be many of them.
func FoldedHas(set map[string]bool, want string) bool {
	_, ok := FoldedLookup(set, want)
	return ok
}

// FoldedLookup is FoldedHas that also returns the DATABASE's spelling.
//
// Callers that go on to WRITE the uid into another table need this rather than the
// boolean: the two tables do not share a collation (octo_* are pinned
// utf8mb4_general_ci, the dump-imported legacy tables are utf8mb4_0900_ai_ci in
// production), so a row written with the caller's spelling can be unreachable from a
// query that resolved the same person through the other table. Storing what the
// database returned keeps the two byte sequences identical instead of relying on a
// collation to bridge them.
//
// The miss direction is unchanged and still fail-closed: FoldID is ASCII-only, so a
// spelling that only the looser collation considers equal does not match here and the
// caller refuses. That refusal is what keeps non-canonical spellings out of the octo_*
// tables today; returning the canonical one is what stops that from being the only
// thing keeping them out.
func FoldedLookup(set map[string]bool, want string) (string, bool) {
	if set[want] {
		return want, true
	}
	folded := FoldID(want)
	for key, ok := range set {
		if ok && FoldID(key) == folded {
			return key, true
		}
	}
	return "", false
}

// ProjectEpochsInSpace returns member_epoch for each named ACTIVE project in
// spaceID, keyed by FoldID(project_id) — NOT by the spelling the caller sent and
// NOT by the spelling the database returned. Callers look up with FoldID too; see
// FoldID for why neither raw spelling works.
//
// Absent from the map means the project does not exist, is disbanded, or lives
// in another Space. The caller maps all three to epoch 0 — one indistinguishable
// answer, for the same reason MembershipsInSpace folds them together: telling
// them apart would let a caller probe which Space a project id lives in.
//
// `status = 1` is what makes project disband converge without a separate event:
// a disbanded project drops out of this result, the caller reads 0, and an
// authorization snapshot taken against the old epoch stops matching.
//
// # The PARENT Space is part of the answer, and it is checked last
//
// A Space ban flips `space.status` and touches nothing else — no octo_project
// write, no epoch bump, and the member-removal cascade deliberately skips banned
// Spaces so an unban can restore them. A Space disband is worse: nothing
// disbands the projects of a disbanded Space, so their rows stay `status = 1`
// forever.
//
// Without the parent check these two functions DISAGREE, and the disagreement
// lands on the channel the peer uses to invalidate:
//
//	ban:   ProjectMemberships flips to member:false (its Space conjunction sees
//	       the ban) while this function keeps answering the same epoch E. The
//	       peer re-checks, E == E, its check AGREES, and the cached grant
//	       survives the ban — unbounded.
//	unban: a peer that re-verified during the ban cached member:false under E.
//	       The unban restores the answer, the epoch is still E, the check agrees
//	       again, and every member of every project in that Space stays denied.
//
// The unban direction was introduced by adding the Space conjunction to
// ProjectMemberships alone: before that both functions ignored Space status and
// at least agreed with each other. One predicate is not allowed to know
// something the invalidation channel does not.
//
// Checked LAST, after the project rows, for the same reason ProjectMemberships
// reads its Space half last: this read can only ever REMOVE projects from the
// answer, so the freshest data arriving here is the fail-closed direction. A ban
// committing between the two reads yields "rows, then inactive" — everything
// drops, the peer reads 0, re-verifies. The other order would yield "active,
// then rows" and serve a live epoch for a Space that is already banned.
//
// A single-row lookup on `space` rather than a JOIN, deliberately: every project
// in one call shares one space_id, so the join would buy nothing, and
// `octo_project` pins utf8mb4_general_ci while `space` is a 2019 table that
// inherits the server default — measured as utf8mb4_0900_ai_ci in production. An
// implicit cross-schema comparison is MySQL error 1267 THERE while passing in
// CI, which on a fail-closed endpoint means the peer is denied everything and
// the lane that would have caught it is green. See modules/project/reconcile_p1.go
// for the repository's full account of that trap.
//
// # An ACTIVE project on the sentinel is refused, not served
//
// That whole scheme rests on a live project never holding 0, and it is enforced
// by three writers in modules/project — creation bumps the epoch, a migration
// lifted the existing rows, and the reconcile scan repairs regressions. The
// first two are one-shots, so there is a window: a not-yet-upgraded pod mid
// rollout, or a rolled-back binary, inserts at the column default again, and the
// scan closes that only on its next rotation.
//
// Serving such a row is NOT merely an availability problem, which an earlier
// version of this reasoning claimed. It is the stale-grant direction, reachable
// in that window and permanent once it happens:
//
//  1. an old pod creates P at member_epoch 0, status 1;
//  2. the peer verifies, is told epoch 0, and caches a positive grant at 0;
//  3. P is disbanded before the scan reaches it — disband bumps the epoch and
//     then flips status, so the row becomes epoch 1, status 0;
//  4. the peer re-reads: the status filter drops P, and the answer is 0;
//  5. 0 == 0, so the staleness check AGREES and the grant never expires.
//
// The repair scan cannot fix this after the fact: its predicate needs
// status = 1, and by step 3 the row is disbanded forever.
//
// So the sentinel is refused at the read layer. The window then costs
// availability (the peer gets a 500 and retries, and the scan repairs the row
// within one rotation) instead of costing a permanent grant, which is the trade
// this whole module makes everywhere else. It also removes the rollback
// runbook's dependency on the reconcile loop still being enabled: with the loop
// off, the endpoint refuses instead of silently handing out the collision.
func ProjectEpochsInSpace(session dbr.SessionRunner, spaceID string, projectIDs []string) (map[string]int64, error) {
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
	if len(rows) == 0 {
		return out, nil
	}

	// The parent Space, last. See the doc comment: this read can only narrow the
	// answer, and an inactive Space folds its projects into the same "does not
	// exist" answer as every other absent case.
	spaceActive, err := space.IsActiveSpace(session, spaceID)
	if err != nil {
		return nil, err
	}
	if !spaceActive {
		return out, nil
	}

	for _, r := range rows {
		// Every row here is status = 1 by the predicate above, so an epoch on the
		// sentinel is a live project wearing the value that means "gone".
		if r.MemberEpoch == AbsentEpochSentinel {
			return nil, &SentinelAnomalyError{ProjectID: r.ProjectID}
		}
		out[FoldID(r.ProjectID)] = r.MemberEpoch
	}
	return out, nil
}

// ProjectMemberships answers, for ONE project, which of the named uids hold an
// active seat — the mirror image of MembershipsInSpace, which answers one uid
// across many projects.
//
// Returns epoch 0 and an empty map when the project is not an active project of
// spaceID. The roles map is keyed by FoldID(uid), like ProjectEpochsInSpace keys
// its own answer and for the same reason — see FoldID.
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
// # Already-cached grants: closed elsewhere, not here
//
// This conjunction fixes FRESH answers only. A grant the consumer cached BEFORE
// the removal is invalidated by the epoch moving, and that bump is NOT in this
// function — it happens in the Space-removal transaction itself
// (modules/project.bumpMemberEpochForSpaceMemberTx, registered as a synchronous
// tx step).
//
// It has to be there rather than here for a reason worth stating: closing the
// seats is asynchronous, the cleanup job can sit in backoff for minutes, and it
// has a terminal abandoned state after which nothing re-claims it. An
// invalidation signal that moves when the seat closes is therefore not bounded at
// all. Moving it at removal commit is what makes the peer contract's "epoch
// agreement means the cache is still good" true for this path.
// membershipTearHook runs immediately after ProjectMemberships' epoch read.
//
// It exists so a test can commit a Space ban at EXACTLY the point where this function
// used to tear — between the epoch stamp and the narrowing reads — and assert the
// answer it produces. Without it the interleaving is a race and the test would be
// probabilistic, which on this branch has repeatedly meant "green for the wrong
// reason".
//
// Unexported, so nothing outside this package can set it, and nil in every binary but
// the one running this package's tests. The cost on the request path is one nil
// comparison.
var membershipTearHook func()

// SetMembershipTearHookForTest installs the hook above and returns a function that
// removes it.
//
// Exported because the engine harness for this package's statements lives in
// modules/project — pkg/project is a predicate package with no database of its own,
// which is why its statements had no engine lane until that file was written. Refuses
// outside a test binary for the same reason modules/space's removal seams do: "nothing
// calls it" is a property of the current tree, not a boundary.
func SetMembershipTearHookForTest(fn func()) (restore func(), err error) {
	if !testing.Testing() {
		return nil, errors.New("project: the membership tear hook is test-only")
	}
	membershipTearHook = fn
	return func() { membershipTearHook = nil }, nil
}

func ProjectMemberships(session *dbr.Session, spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	roles := make(map[string]int, len(uids))
	if spaceID == "" || projectID == "" {
		return 0, roles, nil
	}

	// ONE SNAPSHOT for every step below, and it is a correctness requirement rather
	// than a tidiness one.
	//
	// These steps used to run on the plain session, so each read saw its own instant.
	// A Space ban committing between step 1 and step 3 produced an answer that was
	// internally inconsistent in the one direction that has no bound: step 1 stamped
	// the live epoch E, step 3's `INNER JOIN space ... AND s.status = 1` observed the
	// ban and emptied the answer, and the response went out as member:false beside E.
	// The peer keys its cached DECISION on (uid, project, member_epoch); unbanning
	// writes only the `space` row — no seat, no bump — so the epoch channel answers E
	// again, agreement holds, and the denial never expires.
	//
	// Round 5's read-time fold closes the STEADY-state directions (banned folds to the
	// absent sentinel, so 0 != E; unbanned gives E != 0). It cannot close an answer
	// torn ACROSS the ban commit, because that answer carries both halves.
	//
	// Read-only, no locks, four point reads. MySQL 8's default REPEATABLE READ
	// establishes the view at the first read in the transaction and holds it for the
	// rest — measured on 8.0.33 rather than assumed: with a ban committed by another
	// connection between two reads, the in-transaction reader still sees status = 1
	// while a plain-session reader sees 0.
	//
	// The answer this produces is a point-in-time one: a ban that commits after the
	// first read belongs to the NEXT answer. The peer learns about it from the epoch
	// channel, whose IsActiveSpace fold turns the project absent and breaks agreement
	// — which is a bound, and the torn denial had none.
	tx, err := session.Begin()
	if err != nil {
		return 0, nil, err
	}
	// Rollback rather than commit: nothing here writes, and rolling back releases the
	// read view just as commit would.
	defer tx.RollbackUnlessCommitted()

	// Step 1 — epoch + existence, in that order. See the doc comment.
	epochs, err := ProjectEpochsInSpace(tx, spaceID, []string{projectID})
	if membershipTearHook != nil {
		membershipTearHook()
	}
	if err != nil {
		return 0, nil, err
	}
	// Folded lookup, because that is how ProjectEpochsInSpace keys its answer. This
	// was `epochs[projectID]` against a map keyed by the DATABASE's spelling, so a
	// caller sending `P-ABC` for a stored `p-abc` matched in SQL, missed here, and
	// got served member_epoch 0 with every uid member:false. Fail-closed, but the
	// handler-level fold could not reach it — the function returned first.
	epoch, active := epochs[FoldID(projectID)]
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
	_, err = tx.SelectBySql(
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
	inSpace, err := space.ActiveMembers(tx, spaceID, seated)
	if err != nil {
		return 0, nil, err
	}
	// The accent half stays open, deliberately: 0900_ai_ci is accent-INSENSITIVE, so
	// a legacy table could match a uid whose accents differ where this fold will not.
	// That residue is fail-closed (a member reads as absent and re-verifies) and is
	// unreachable for generated uids, which are ASCII. Closing it properly would mean
	// carrying a collation table in Go — see FoldID.
	inSpaceFolded := make(map[string]bool, len(inSpace))
	for uid, active := range inSpace {
		if active {
			inSpaceFolded[FoldID(uid)] = true
		}
	}

	// Step 4 — the ACCOUNT half. Last, for the third time and the same reason: it
	// can only ever REMOVE uids, so the freshest data arriving here is the
	// fail-closed direction.
	//
	// A Space seat is not the whole answer either. A super-admin ban writes ONLY the
	// `user` row — modules/user.liftBanUser revokes sessions, kicks devices and bans
	// the IM channel, and touches neither space_member nor octo_project_member — and
	// account destroy cascades no membership removal at all. So a banned or destroyed
	// uid keeps both membership rows at status = 1, and without this step it was
	// served to the peer as a member WITH its real role.
	//
	// The ban's own session revocation cannot close that, because this endpoint's
	// subject presents NOTHING: it is a peer control plane asking about a third party
	// it holds no token for, which is the same argument that put the Space half here.
	// The repository has fixed this identical class twice on other tokenless paths
	// (modules/user.authVerifyAPIKey, modules/bot_provision.assertSpaceMember).
	//
	// A SEPARATE query, not a JOIN onto step 2's SQL, and that is not a preference:
	// `user` is one of the dump-imported tables sitting at utf8mb4_0900_ai_ci in
	// production while octo_project_member is migration-created general_ci, so
	// `JOIN user u ON u.uid = pm.uid` is error 1267 THERE and green in CI. Measured.
	// See pkg/user.ActiveAccounts.
	//
	// Only the uids that survived the Space half are asked about, so the batch keeps
	// shrinking rather than being re-derived from the request.
	stillSeated := make([]string, 0, len(rows))
	for _, r := range rows {
		if inSpaceFolded[FoldID(r.UID)] {
			stillSeated = append(stillSeated, r.UID)
		}
	}
	liveAccounts, err := user.ActiveAccounts(tx, stillSeated)
	if err != nil {
		return 0, nil, err
	}
	liveFolded := make(map[string]bool, len(liveAccounts))
	for uid, live := range liveAccounts {
		if live {
			liveFolded[FoldID(uid)] = true
		}
	}

	// Two maps keyed by TWO DIFFERENT databases' spellings, joined here in Go:
	// octo_project_member pins utf8mb4_general_ci while space / space_member / user
	// are 2019 tables inheriting the server default — measured as utf8mb4_0900_ai_ci
	// in production. An exact-match lookup across that seam drops a real member whose
	// rows differ in case, so both sides are folded.
	for _, r := range rows {
		folded := FoldID(r.UID)
		if !inSpaceFolded[folded] || !liveFolded[folded] {
			continue
		}
		roles[folded] = r.Role
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
