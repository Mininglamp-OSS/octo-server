// Package project exposes read-only Project membership facts that other modules
// need without importing modules/project.
//
// This package must never import modules/project (pinned by
// TestPkgProjectDoesNotImportModulesProject); doing so would put the import
// cycle back.
package project

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/user"
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
	// FOLDED keys, matching ProjectEpochsInSpace two functions down.
	//
	// This used to key by r.ProjectID — the spelling the DATABASE returned — while
	// its one consumer looked the row up with the id the CALLER sent
	// (modules/user/api_project_context.go). project_id compares
	// case-insensitively under either production collation, so a re-cased id
	// matched in SQL, missed in Go, and /v1/auth/verify answered member:false for
	// a real member of a real project.
	//
	// Fail-closed, but a legitimate member refused — and it is the same defect
	// FoldID was introduced for and already fixed in the two sibling readers in
	// this file. Left open here it was the third instance in one file, which is
	// what a shared helper is supposed to make impossible.
	//
	// Callers must fold their needle. They must also keep answering with their own
	// spelling, not this map's key: an answer keyed on a folded id would hand the
	// caller back an identifier they never sent.
	for _, r := range rows {
		out[FoldID(r.ProjectID)] = r
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
// Takes a context because its only production caller runs it in a detached
// goroutine that holds an in-flight marker for this project id. On the process-wide
// session dbr reaches sql.DB with context.Background() and no Timeout, so both the
// pool acquisition and the statement were unbounded — and an unbounded one there does
// not merely leak a goroutine, it never releases the marker, which then rejects EVERY
// later repair attempt for that project for the life of the process. Recovery would
// fall back to the reconcile cursor, the hours-scale path this repair exists to avoid.
//
// Tests may pass context.Background(): they are not holding a marker, and pinning a
// deadline is the caller's job.
func RepairAbsentSentinelEpoch(ctx context.Context, session *dbr.Session, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	result, err := session.UpdateBySql(
		"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
			"WHERE project_id = ? AND status = 1 AND member_epoch = ?",
		projectID, AbsentEpochSentinel,
	).ExecContext(ctx)
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
// availability instead of costing a permanent grant, which is the trade this
// whole module makes everywhere else.
//
// How long that window actually is: the peer gets a 500 and retries, and the
// handler repairs the named row out of band on the way out
// (modules/internal_membership -> RepairAbsentSentinelEpoch), so the practical
// bound is ONE request. Do NOT read it as "the reconcile scan fixes it within one
// rotation" — that claim was wrong and is corrected on SentinelAnomalyError
// above: scanEpochSanity walks a bounded page budget per tick behind a persisted
// cursor, a row written by a not-yet-upgraded pod carries the highest id, and on
// a large octo_project reaching it takes hours. The refusal is also per-BATCH, so
// during that window every request whose batch of 50 contains the id fails, not
// just requests naming it.
//
// It also removes the rollback runbook's dependency on the reconcile loop still
// being enabled: with the loop off, the endpoint refuses instead of silently
// handing out the collision.
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
	// `activated_at IS NOT NULL` is the two-phase create gate (O6, contract
	// section 7). A project whose subsystem container has not been confirmed
	// must be indistinguishable here from one that does not exist, so that
	// nothing can be authorized into a workspace that is not there yet.
	//
	// It belongs in THIS query specifically, not in each endpoint:
	// ProjectMemberships runs this function as its step 1 and returns early when
	// the project is absent from the result, so one predicate closes both
	// inbound endpoints and they cannot drift apart.
	//
	// Rows created before the column existed were backfilled to created_at, so
	// this filter removes nothing that used to be visible.
	_, err := session.SelectBySql(
		"SELECT project_id, member_epoch FROM `octo_project` "+
			"WHERE space_id = ? AND project_id IN ? AND status = 1 "+
			"  AND activated_at IS NOT NULL",
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
// MembershipsInSpace is deliberately NOT changed, and the reason it used to give
// was false. It said the route "already ran SpaceMiddleware", so the consumer held
// the Space half. It does not: modules/user/api.go registers POST /v1/auth/verify
// in a group whose only middleware is the rate limiter, and the request carries a
// caller-supplied space_id.
//
// The exemption survives on a different fact, which is the one to check if this is
// ever revisited: the uid is not caller-supplied. authVerifyToken derives it from
// the PRESENTED TOKEN (tokenValidator.Validate), so every answer this function can
// produce is about the token holder themselves, and the seat predicate means a
// `member: true` can only ever be self-information. A caller naming a Space they
// hold no seat in learns nothing, because they hold no seat in its projects either.
//
// That is why it is exempt from the SPACE conjunction. It is NOT by itself a reason
// to exempt it from the ACTIVATION gate — see the note on that below.
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
//
// This enumeration covers the project, seat and Space axes. It does NOT cover the
// ACCOUNT axis, and read top-to-bottom it used to look as if it did. A super-admin
// ban or an account destroy flips step 4's answer below while moving no epoch —
// the epoch is per PROJECT and the ban is per USER, so there is no per-project row
// for it to bump. Both directions ride epoch agreement unboundedly: a grant cached
// before the ban stays good, and a denial cached during it stays denied until some
// unrelated membership change happens to bump that project. The only bound on that
// axis is a time bound on the consumer's side; it is out of scope for this branch
// and specified in docs/project-membership-cache-bound-proposal.md, and the reason
// OCTO_MEMBERSHIP_INTERNAL_TOKEN stays unset until the peer implements it.
func ProjectMemberships(ctx context.Context, session *dbr.Session, spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	// Read-only, no locks, five point reads — and the isolation level is NAMED here
	// rather than inherited, which is the half of this fix that is easy to leave out.
	//
	// REPEATABLE READ establishes the view at the first read in the transaction and
	// holds it for the rest — measured on 8.0.33 rather than assumed: with a ban
	// committed by another connection between two reads, the in-transaction reader
	// still sees status = 1 while a plain-session reader sees 0.
	//
	// A bare Begin() would take whatever `transaction_isolation` the server happens
	// to carry, and NOTHING in this repository sets it: not pkg/db, not the DSN
	// template, not a boot check. Under READ COMMITTED each statement takes a fresh
	// view, this transaction linearises nothing, and the torn denial comes straight
	// back — silently, on the authorization oracle only, with no error and no log
	// line.
	//
	// Two measurements, kept apart because they were taken by different people and
	// say different things. A reviewer set `transaction_isolation` GLOBALLY to
	// READ-COMMITTED on 8.0.46 and found that with a bare Begin() the two
	// hook-driven cases in modules/project/torn_verify_test.go fail in exactly the
	// original torn shape, and pass with this form. Independently, here: reverting
	// this call to Begin() leaves those two cases GREEN on an RR server — they
	// inherit the ambient level, so they measure the deployment — while
	// TestProjectMembershipsPinsItsOwnIsolationLevel, which runs against a session
	// pinned to READ COMMITTED, fails with member=false beside the live epoch.
	//
	// That is the whole argument for naming the level: the property is not "the
	// engine we happen to test on is REPEATABLE READ".
	//
	// Scoping: `SET TRANSACTION ISOLATION LEVEL` with neither GLOBAL nor SESSION
	// applies to the next transaction only, and go-sql-driver/mysql issues it
	// immediately before START TRANSACTION. So this borrows nothing from the pooled
	// connection and leaves nothing on it for the next borrower.
	//
	// ReadOnly is not decoration: it turns "someone added a write to this path" into
	// a driver error instead of a review question — the same discipline SeatRef
	// applies on the write side.
	//
	// The answer this produces is a point-in-time one: a ban that commits after the
	// first read belongs to the NEXT answer. The peer learns about it from the epoch
	// channel, whose IsActiveSpace fold turns the project absent and breaks agreement
	// — which is a bound, and the torn denial had none.
	// The CALLER's context, not context.Background(), and that matters more since
	// round 16 than it did before it.
	//
	// Before the snapshot fix these five reads borrowed and returned a pooled
	// connection each. Now one transaction holds a connection across all five round
	// trips, and sql.DB.BeginTx is where the wait for that connection happens. With
	// Background() nothing bounds it: a retrying peer at the configured burst can park
	// hundreds of goroutines waiting on a pool octo-lib defaults to 100 connections,
	// and every other module in the process queues behind them. The 15s read deadline
	// on the route bounds the SOCKET, not the pool, so it offers nothing here.
	//
	// Passing the request's context also means a peer that hangs up frees its
	// connection immediately rather than at the end of five queries.
	tx, err := session.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return 0, nil, err
	}
	// Rollback rather than commit: nothing here writes, and rolling back releases the
	// read view just as commit would.
	defer tx.RollbackUnlessCommitted()

	// Bound every statement on this transaction, including the four issued from
	// pkg/project, pkg/space and pkg/user through the SessionRunner they are handed.
	//
	// Tx inherits the session's timeout — which is unset process-wide, i.e. no deadline
	// at all. Setting it here is the one place that reaches all five reads without
	// giving three packages a context parameter they otherwise have no use for. A
	// blocked read holds the connection AND the read view, so an unbounded one is the
	// expensive half of the same hazard.
	//
	// # The guarantee is a property of the CALL SHAPE, not of dbr
	//
	// This used to say "dbr applies runner.GetTimeout() around each query", full stop.
	// That is true of dbr's query()/exec(), which Load and LoadOne go through, and NOT
	// of queryRows(), which discards the runner's timeout on purpose — its own comment
	// says "the context should not be canceled implicitly here". So `.Rows()`,
	// `.Iterate()` and `.IterateContext()` on this transaction are UNBOUNDED, and a
	// future read added in that shape would lose the deadline with no signal.
	//
	// Every read on this path is Load or LoadOne today, so the bound holds at this
	// head. TestVerifyTransactionReadsAreAllTimeoutBearing pins it, because "all five
	// reads are bounded" is otherwise a claim no test can fail.
	//
	// The value is per-STATEMENT, not for the whole transaction; the caller's context
	// bounds the whole. Five point reads on indexed predicates, so this is an outlier
	// cutoff rather than a budget.
	tx.Timeout = membershipReadTimeout

	// Step 1 — epoch + existence, in that order. See the doc comment.
	epochs, err := ProjectEpochsInSpace(tx, spaceID, []string{projectID})
	if hook := membershipTearHook.Load(); hook != nil {
		(*hook)()
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

// membershipTearHook runs immediately after ProjectMemberships' epoch read.
//
// It exists so a test can commit a Space ban at EXACTLY the point where that function
// used to tear — between the epoch stamp and the narrowing reads — and assert the
// answer it produces. Without it the interleaving is a race and the test would be
// probabilistic, which on this branch has repeatedly meant "green for the wrong
// reason".
//
// Unexported, so nothing outside this package can set it, and nil in every binary but
// the one running this package's tests. The cost on the request path is one atomic
// load.
//
// It lives BELOW ProjectMemberships rather than above it, and that is not cosmetic:
// a comment block with no blank line before a declaration is that declaration's doc
// comment. Declared above, this comment swallowed the whole read-order contract into
// the documentation of an unexported var and `go doc ProjectMemberships` printed
// nothing. Inserting a blank line does not fix that — it only detaches the contract
// from everything — so the seam moved instead. TestProjectMembershipsKeepsItsDoc-
// Comment holds the line.
//
// atomic rather than a plain func value because a plain one is a data race the moment
// any test on this path calls t.Parallel(); today none does, which makes it latent
// rather than absent.
var membershipTearHook atomic.Pointer[func()]

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
	membershipTearHook.Store(&fn)
	return func() { membershipTearHook.Store(nil) }, nil
}

// membershipReadTimeout caps ONE statement inside ProjectMemberships' transaction.
//
// Not a budget for the request: each of the five reads is a point lookup on an
// indexed predicate and returns in single-digit milliseconds on a healthy engine.
// It is the cutoff past which a read has stopped being slow and started being stuck,
// and holding a pooled connection plus a read view while stuck is what makes an
// authorization endpoint able to starve the rest of the process.
//
// Worst case behind it is therefore 5 x 5s = 25s, not 20s: ProjectEpochsInSpace issues
// TWO statements, not one — the octo_project select and then space.IsActiveSpace. Worth
// stating in the same breath as the number, next to a route whose own read deadline is
// 15s and whose peer contract declares a cache bound of 1-300s.
const membershipReadTimeout = 5 * time.Second

// EpochsReadTimeout is the same cutoff for the epochs endpoint's transaction.
//
// Exported, and deliberately the SAME constant rather than a second literal:
// modules/internal_membership opens that transaction itself (it owns the store), and
// two independently drifting deadlines on the two halves of one peer contract is how
// the pair stops meaning anything. A named accessor rather than an exported var so it
// cannot be reassigned at runtime.
func EpochsReadTimeout() time.Duration { return membershipReadTimeout }

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
