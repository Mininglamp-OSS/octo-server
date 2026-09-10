package project

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// DB is the modules/project data-access layer.
//
// Two conventions this file follows deliberately, both because getting them wrong
// is silent:
//
//  1. **Explicit column lists on every write.** Not util.AttrToUnderscore. Two
//     columns must never appear in an INSERT or UPDATE: `active_name` is a STORED
//     generated column (MySQL rejects naming it with error 3105) and `is_official`
//     has no P0 writer by design. A reflective column list derives from struct
//     fields, so it would start writing either one the moment somebody adds the
//     field — and `is_official` would then be written with a value that happens to
//     equal the default, making the regression invisible. `member_epoch` and
//     `collaboration_role_epoch` are likewise absent from the insert list: they
//     take the DDL defaults, so their only writers are atomic `epoch = epoch + 1`
//     statements, which makes monotonicity checkable by grep.
//
//  2. **dbr backtick asymmetry.** Update / InsertInto / DeleteFrom take the bare
//     table name (dbr quotes it); From / Select need manual backticks. Getting it
//     backwards yields error 1064 on a reserved word, and the reconcile scans join
//     `space`, which IS reserved.
type DB struct {
	ctx     *config.Context
	session *dbr.Session
}

// NewDB builds the DAO against the process-wide dbr session.
func NewDB(ctx *config.Context) *DB {
	return &DB{ctx: ctx, session: ctx.DB()}
}

// projectInsertColumns is the write-side column list for octo_project. See the
// type comment for why active_name / is_official / both epoch columns are absent.
var projectInsertColumns = []string{
	"project_id", "space_id", "name", "description", "logo", "creator",
	"discoverability", "max_members", "status",
	"created_at", "updated_at",
	// join_mode is deliberately absent: the column exists with its DDL default (1) and
	// nothing above the storage layer touches it until the P2 join path lands. See the
	// JoinMode constants in model.go.
}

// ---------- project ----------

// insertProjectTx writes one project row inside tx.
//
// A duplicate ACTIVE name surfaces as a MySQL 1062 on uk_octo_project_space_active_name;
// callers map that to ErrProjectNameDuplicated rather than pre-checking, so two
// concurrent creates cannot both pass a check and then both insert.
func (d *DB) insertProjectTx(tx *dbr.Tx, m *Model) error {
	_, err := tx.InsertInto("octo_project").
		Columns(projectInsertColumns...).
		Values(m.ProjectID, m.SpaceID, m.Name, m.Description, m.Logo, m.Creator,
			m.Discoverability, m.MaxMembers, m.Status,
			m.CreatedAt, m.UpdatedAt).
		Exec()
	if err != nil {
		return fmt.Errorf("project: insert project: %w", err)
	}
	return nil
}

// queryByProjectID is the point read behind ProjectMiddleware. It returns
// (nil, nil) when no row exists, and DOES return disbanded rows — the caller
// decides what a disbanded project looks like on the wire, because "disbanded"
// and "never existed" must render identically and that decision belongs at the
// response boundary, not here.
func (d *DB) queryByProjectID(projectID string) (*Model, error) {
	if projectID == "" {
		return nil, nil
	}
	var models []*Model
	_, err := d.session.SelectBySql(
		"SELECT id, project_id, space_id, name, description, logo, creator, "+
			"discoverability, max_members, member_epoch, collaboration_role_epoch, status, all_member_group_no, "+
			"created_at, updated_at "+
			"FROM `octo_project` WHERE project_id = ? LIMIT 1", projectID,
	).Load(&models)
	if err != nil {
		return nil, fmt.Errorf("project: query project: %w", err)
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models[0], nil
}

// lockActiveProjectTx re-reads an ACTIVE project row under a row lock. Every
// membership write starts here, which is what fixes the lock order
// (space -> project -> ... -> octo_project_member) and what makes the epoch bump
// and the membership write a single serialized unit for that project.
//
// Returns (nil, nil) when the project does not exist or is already disbanded.
func (d *DB) lockActiveProjectTx(tx *dbr.Tx, projectID string) (*Model, error) {
	var models []*Model
	_, err := tx.SelectBySql(
		"SELECT id, project_id, space_id, name, description, logo, creator, "+
			"discoverability, max_members, member_epoch, collaboration_role_epoch, status, all_member_group_no, "+
			"created_at, updated_at "+
			"FROM `octo_project` WHERE project_id = ? AND status = ? FOR UPDATE",
		projectID, StatusNormal,
	).Load(&models)
	if err != nil {
		return nil, fmt.Errorf("project: lock project: %w", err)
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models[0], nil
}

// updateProfileTx applies a partial profile update. The caller passes only the
// columns it means to change; active_name / is_official / member_epoch can never
// appear because setColumns is built from an allow-list, not from the payload.
func (d *DB) updateProfileTx(tx *dbr.Tx, projectID string, set map[string]interface{}, now time.Time) error {
	if len(set) == 0 {
		return nil
	}
	stmt := tx.Update("octo_project").Where("project_id = ?", projectID)
	for col, val := range set {
		stmt = stmt.Set(col, val)
	}
	if _, err := stmt.Set("updated_at", now).Exec(); err != nil {
		return fmt.Errorf("project: update project profile: %w", err)
	}
	return nil
}

// disbandProjectTx flips status and deactivates every active member row in the
// same transaction, returning how many seats were closed.
//
// The two writes must be one transaction: a project marked disbanded while its
// member rows stay active is an I1 violation the reconcile job would then report
// forever, since no cleanup job exists for a project disband.
func (d *DB) disbandProjectTx(tx *dbr.Tx, projectID string, now time.Time) (int64, error) {
	if _, err := tx.Update("octo_project").
		Set("status", StatusDisbanded).
		Set("updated_at", now).
		Where("project_id = ? AND status = ?", projectID, StatusNormal).
		Exec(); err != nil {
		return 0, fmt.Errorf("project: disband project: %w", err)
	}
	// `removing` is cleared in the SAME statement that closes the seat.
	//
	// Without it a seat whose two-phase close was still in flight (removing = 1)
	// lands on status = 0 AND removing = 1 — the one combination db_removal.go's
	// state table marks as MUST NOT EXIST, and it is unrecoverable rather than
	// merely wrong: finishMemberRemovalTx is guarded on `status = 1 AND
	// removing = 1`, so nothing can ever match the row again, while
	// scanRemovingStalls has no status filter and alerts on it every tick with no
	// remedy an operator can apply.
	res, err := tx.Update("octo_project_member").
		Set("status", MemberStatusRemoved).
		Set("removing", 0).
		Set("updated_at", now).
		Where("project_id = ? AND status = ?", projectID, MemberStatusActive).
		Exec()
	if err != nil {
		return 0, fmt.Errorf("project: deactivate members on disband: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: disband affected rows: %w", err)
	}
	// Retire the cascade jobs those seats had outstanding, in this transaction.
	// The worker would otherwise claim each one, re-read the member, find
	// removing = 0 and drop it — burning a lease and an attempt per job to
	// discover work that no longer exists. That is precisely what
	// cancelPendingRemovalJobsTx exists to avoid on the re-admission path, and a
	// disband closes every seat at once, so the waste scales with the project.
	if _, err := d.cancelPendingRemovalJobsForProjectTx(tx, projectID, now); err != nil {
		return 0, err
	}
	// Mark this project's subsystem containers reclaimable, in the same transaction.
	//
	// Here rather than in the service layer so that the transition is structural — every
	// present and FUTURE disband path picks it up by construction instead of each caller
	// having to remember. Today that is one caller (disbandProjectOnce); the Space-removal
	// cascade only closes seats and the ownerless case is a recorded, deliberately
	// unresolved end state, so this is where a future cascade would land rather than a
	// junction that already exists. An earlier version of this comment claimed both
	// already routed through here, which a reader would grep for and not find.
	//
	// Teardown stays PULL-based (D9): nothing is sent outbound, and the subsystem learns
	// by asking POST /v1/internal/projects/status.
	//
	// LAST of the two writes in this function, deliberately: octo_project_provisioning is
	// the final lock this transaction acquires. That mattered less when this was the only
	// tail statement; now that P1's removal-job retirement shares the same position, the
	// order is what keeps the declared lock order intact.
	if err := d.markProvisioningDisbandPendingTx(tx, projectID, now); err != nil {
		return 0, err
	}
	return affected, nil
}

// bumpMemberEpochTx increments member_epoch in the caller's transaction.
//
// The statement is `member_epoch = member_epoch + 1` and never an absolute
// assignment. That is the whole monotonicity guarantee: a read-only reconcile
// scan cannot observe monotonicity (it would have to remember the previous value,
// and it runs on every pod), so the property has to hold by construction. A
// source guard test greps this package for any other shape of member_epoch write.
//
// Callers MUST invoke this only when the membership statement actually affected a
// row. An unconditional bump would inflate the epoch on the Space-cascade step's
// no-op reruns — the step is re-executed on every job retry — and break the
// "a no-op write does not change the epoch" rule that clients cache against.
//
// Returns the number of rows the statement matched, because the status guard
// means "no error" and "it happened" are different facts. Most callers can
// ignore it — a no-op on a disbanded project is the intended behaviour there.
// createProjectOnce cannot: a silent zero-row bump would leave a fresh project on
// the reserved absent sentinel while the create response reports 1, which is the
// exact state migration 20260908000002 exists to remove.
func (d *DB) bumpMemberEpochTx(tx *dbr.Tx, projectID string, now time.Time) (int64, error) {
	_ = now // the statement is clock-free now; the parameter stays for call-site stability
	// updated_at is deliberately NOT written here: it is the field a client diffs to decide
	// whether the project's PROFILE changed, and member_epoch already carries the roster
	// signal — writing both made every roster edit churn the profile clock (yujiawei Q8,
	// PR #841 round 1). The status predicate makes the method safe on its own terms instead
	// of by caller convention: a disbanded project's epoch must not move.
	result, err := tx.UpdateBySql(
		"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
			"WHERE project_id = ? AND status = ?",
		projectID, StatusNormal,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: bump member epoch: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: bump member epoch rows: %w", err)
	}
	return affected, nil
}

// absentEpochSentinel is the value the membership integration contract reserves
// for "project does not exist or is not visible".
//
// It is spelled out here rather than written as a bare 0 because the whole point
// of migration 20260908000002 is that this value must never be reachable by a
// real, active project. Naming it makes the two places that care — the repair
// predicate below and the reconcile scan that drives it — obviously the same
// value as the contract's.
const absentEpochSentinel = 0

// repairAbsentSentinelEpoch lifts ONE active project off the absent-sentinel
// value, and reports whether it actually had to.
//
// Why this exists even though migration 20260908000002 already backfilled every
// row: the migration enforces the invariant at ONE INSTANT — the boot that runs
// it. Two windows re-open it afterwards, and neither is hypothetical:
//
//   - Rolling deploy. The first upgraded pod applies the backfill while pods on
//     the old image keep inserting projects at the column default. Those rows
//     hold the sentinel until some unrelated roster write moves them.
//   - Rollback. The migration's Down is a no-op and its ledger row stays, so
//     rolling the binary back restores the zero-inserting create path
//     indefinitely and rolling forward again never re-runs the backfill.
//
// A one-instant invariant is not one the endpoint's fail-closed reasoning can
// rest on, so the scheduled scan turns it into a continuously enforced one. The
// statement is `member_epoch + 1`, the same increment-only shape as every other
// write to this column, so monotonicity survives the repair — this raises an
// epoch, it never assigns one.
//
// The predicate is repeated in full rather than trusting the row the scan read:
// the scan reads outside a transaction, so between the read and this statement
// the project may have been disbanded or had its epoch moved by a real roster
// write. Both cases match no row, and the caller learns that from the returned
// count rather than logging a repair that did not happen.
func (d *DB) repairAbsentSentinelEpoch(projectID string) (int64, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
			"WHERE project_id = ? AND status = ? AND member_epoch = ?",
		projectID, StatusNormal, absentEpochSentinel,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: repair absent-sentinel epoch: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: repair absent-sentinel epoch rows: %w", err)
	}
	return affected, nil
}

// spaceMemberEpochBumpChunk bounds ONE bump statement's IN list.
//
// The enumerated set is bounded by the per-Space project quota (1000 today), so a
// single statement would work — but the quota is a config value and this runs in a
// user-facing transaction, so the statement width is pinned here rather than left
// to whatever the quota becomes. Chunks accumulate locks in the same transaction,
// which is what the correctness argument below requires.
const spaceMemberEpochBumpChunk = 500

// bumpMemberEpochForSpaceMemberTx raises member_epoch on every ACTIVE project in
// spaceID where uid still holds an active seat, in the caller's transaction.
//
// This runs inside the SPACE-REMOVAL transaction, and it is the only thing that
// makes the epoch a complete invalidation channel for that path.
//
// Why it cannot be left to the cascade. Closing the seats is asynchronous: the
// cleanup job may sit in backoff for minutes, and once it exhausts
// removalCleanupMaxAttempts it is terminal and never re-claimed. Until it runs,
// `_verify` already answers member:false (its Space conjunction sees the removal)
// while `epochs` still answers the old value — so a peer holding a grant cached
// under that epoch re-reads it, gets the same number, and its staleness check
// AGREES. The revocation survives for minutes normally and forever when the job
// is abandoned. Bumping here closes that: the epoch moves at the same instant the
// answer does.
//
// # TWO statements, and the split is the whole point
//
// The first version of this was ONE statement:
//
//	UPDATE octo_project p INNER JOIN octo_project_member pm ON ... SET p.member_epoch = ...
//
// which is a lock-order bug that no amount of reading the SQL reveals, because the
// order is not in the SQL — the OPTIMIZER picks the driving table, and it flips
// with cardinality. Measured on MySQL 8.0.33 against this schema:
//
//	3 active projects / 3 seats     -> p driving (ref), pm eq_ref   => project -> member
//	200 active projects / 3 seats   -> pm driving (index_merge)     => member -> project
//
// The second shape is the production one (a Space has many projects; one user sits
// in a few), and it inverts the order this module documents at the top of
// service.go. Both deadlock directions were then reproduced, including the one
// where InnoDB picks the SPACE REMOVAL as its victim:
//
//	(1) HOLDS   octo_project_member PRIMARY  S  (p1,u1) (p2,u1)
//	(1) WAITING octo_project uk_..._project_id X -> p2
//	(2) HOLDS   octo_project uk_..._project_id X -> p2
//	(2) WAITING octo_project_member (p2,u1) X
//	ERROR 1213
//
// A 1213 here rolls the member removal back (that is this step's contract), so
// under contention the REVOCATION FAILS — the exact outcome the step exists to
// prevent.
//
// So: enumerate first, then update. The UPDATE touches only octo_project, which
// takes this step out of the p <-> pm cycle entirely rather than betting on a join
// order. It is not an optimization and must not be folded back into one statement.
//
// # Why the enumeration may be a NON-LOCKING read
//
// Taking S locks on octo_project_member here would re-create the inversion, so the
// enumeration is a plain consistency read. Under REPEATABLE READ that reads the
// transaction's snapshot, and the snapshot can be older than the statement — which
// would matter if a project seat could be created for this uid after the snapshot
// and still be live after this transaction commits. It cannot, and the argument has
// exactly two legs:
//
//  1. Every seat admission locks the target's space_member row FIRST — one
//     statement, `FOR SHARE OF sm` (lockSpaceSeatsTx), before it touches
//     octo_project or octo_project_member. The removal transaction holds that row
//     under FOR UPDATE by the time this step runs. So an admission that has not
//     committed is BLOCKED, and when it unblocks it finds status = 0 and is
//     refused.
//  2. An admission that HAS committed did so before the removal took that X lock,
//     therefore before this transaction's read view was assigned (RR assigns it at
//     the first CONSISTENCY read, and every statement before this one on the
//     removal path is a locking read or a DML). So it is visible here.
//
// Verified, not assumed: with the two transactions interleaved so the admission
// commits while the removal is blocked on space_member, this read returns the seat
// the admission just inserted.
//
// Leg 1 is a property of the OTHER module's write paths, so it is pinned by a test
// rather than by this comment — see TestSpaceMemberEpochBumpSeesConcurrentAdmission
// and the source guard over octo_project_member writers.
//
// Increment-only, like every other writer of this column, so the write-discipline
// guard holds. Idempotent in the sense that matters: a retried removal finds the
// seats already closed by the cascade and enumerates nothing. A retry that lands
// BEFORE the cascade bumps a second time, which costs the peer one extra
// re-verify — the safe direction.
func (d *DB) bumpMemberEpochForSpaceMemberTx(tx *dbr.Tx, spaceID, uid string) error {
	if spaceID == "" || uid == "" {
		return nil
	}

	// Step 1 — enumerate. Non-locking on purpose; see the doc comment.
	var ids []string
	if _, err := tx.SelectBySql(
		"SELECT project_id FROM `octo_project_member` "+
			"WHERE space_id = ? AND uid = ? AND status = ? AND removing = 0 "+
			"ORDER BY project_id",
		spaceID, uid, MemberStatusActive,
	).Load(&ids); err != nil {
		return fmt.Errorf("project: enumerate seats for space member removal: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	// Step 2 — bump, touching octo_project ONLY. space_id and status are kept in
	// the predicate even though project_id is unique: a seat row whose
	// denormalized space_id has drifted must not be able to move another Space's
	// epoch, and a disbanded project's epoch must not move (its answer is already
	// the absent sentinel).
	for start := 0; start < len(ids); start += spaceMemberEpochBumpChunk {
		end := start + spaceMemberEpochBumpChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]interface{}, 0, len(chunk)+2)
		args = append(args, spaceID, StatusNormal)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := tx.UpdateBySql(
			"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
				"WHERE space_id = ? AND status = ? AND project_id IN ("+placeholders+")",
			args...,
		).Exec(); err != nil {
			return fmt.Errorf("project: bump member epoch for space member removal: %w", err)
		}
	}
	return nil
}

// countActiveInSpaceTx counts a Space's active projects inside the create transaction.
// The quota must be counted in the same transaction that inserts, or two
// concurrent creates both pass the check and both land.
func (d *DB) countActiveInSpaceTx(tx *dbr.Tx, spaceID string) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ? AND status = ?",
		spaceID, StatusNormal,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count projects in space (tx): %w", err)
	}
	return count, nil
}

// countActiveByCreatorTx counts a creator's active projects in one Space.
func (d *DB) countActiveByCreatorTx(tx *dbr.Tx, spaceID, creator string) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE space_id = ? AND creator = ? AND status = ?",
		spaceID, creator, StatusNormal,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count projects by creator (tx): %w", err)
	}
	return count, nil
}

// countCreatedInWindowTx counts a creator's projects created in [from, to).
//
// A half-open range on (creator, created_at) rather than DATE(created_at) = ?:
// the function-call form cannot use the index and silently relies on the Go clock
// and the MySQL session clock agreeing. Disbanded projects still count — the cap
// exists to bound creation rate, and create-then-disband would otherwise be a
// free bypass.
func (d *DB) countCreatedInWindowTx(tx *dbr.Tx, creator string, from, to time.Time) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE creator = ? AND created_at >= ? AND created_at < ?",
		creator, from, to,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count same-day creates (tx): %w", err)
	}
	return count, nil
}

// sqlListVisibleInSpace is the statement listVisibleInSpace runs.
//
// A named constant for the same reason sqlListMyProjectGroups is one: the plan
// guard EXPLAINs the string production executes rather than a copy of it, and a
// copy passes forever once the two drift.
const sqlListVisibleInSpace = "SELECT p.project_id, p.space_id, p.name, p.description, p.logo, p.creator, " +
	"p.discoverability, p.max_members, p.member_epoch, p.collaboration_role_epoch, p.status, " +
	// all_member_group_no on the LIST route too. The wire contract defines
	// "" as "no group provisioned", so omitting the column here made every
	// listed project claim it has none — the detail route and the list route
	// disagreeing about the same project, and a client hiding the entry
	// point to a group that exists.
	"p.all_member_group_no, " +
	"p.created_at, p.updated_at, " +
	"IFNULL(pm.role, ?) AS my_role, " +
	// D16's two counts are BOTH computed after the page loads, by
	// fillMemberCounts, out of one roster read. Neither is in this
	// statement, and the reason each left is different:
	//
	//   - member_count was a correlated join into `user` with a COLLATE on
	//     the driving side — twenty `user` probes per page that the
	//     production collation shape turns into twenty scans. PR #855's
	//     eighth review.
	//   - seat_count was a correlated aggregate, which was nearly free
	//     while LIMIT 20 short-circuited the scan and stopped being free
	//     the moment PR-5's ORDER BY made this statement sort every
	//     visible project in the Space: it then ran once per SORTED row,
	//     not once per returned row. Measured on 2000 visible projects,
	//     4000 seats: 1667 subquery loops and 13.5ms with it here, 7.9ms
	//     without. See fillMemberCounts for where it went and ORDER BY
	//     below for what remains.
	//
	// Keeping seat_count here and member_count there would also have kept
	// them under two different read views, which is what the previous round
	// had to document as an inexactness. One read, one arithmetic.
	"IFNULL(s.pinned, 0) AS pinned " +
	"FROM `octo_project` p " +
	// `removing = 0` on the JOIN as well as on the count: without it a member
	// whose seat is closing keeps my_role, and — worse — keeps
	// `pm.uid IS NOT NULL`, which is the clause that reveals UNLISTED
	// projects. So a departing member would go on seeing projects they are
	// not supposed to be able to enumerate, for the whole cascade window,
	// while the member_count beside them already excluded them.
	"LEFT JOIN `octo_project_member` pm " +
	"  ON pm.project_id = p.project_id AND pm.uid = ? AND pm.status = 1 " +
	"     AND pm.removing = 0 " +
	// The caller-specific pin. A LEFT JOIN rather than a correlated
	// subquery because it also drives the ORDER BY, and it costs one
	// equality probe on uk_octo_project_user_setting (project_id, uid) —
	// the same key the upsert is idempotent on, which is why this table
	// needs no second index.
	"LEFT JOIN `octo_project_user_setting` s " +
	// Both sides are octo_project*, i.e. both general_ci. No COLLATE: one
	// between two same-collation columns is not free — an explicit COLLATE
	// has coercibility 0, so the other side is converted per row and its
	// index stops serving the predicate. PR #855 measured that exact cost
	// on this schema.
	"  ON s.project_id = p.project_id AND s.uid = ? " +
	"WHERE p.space_id = ? AND p.status = ? " +
	"  AND (p.discoverability = ? OR pm.uid IS NOT NULL) " +
	// Pinned first, most recently pinned before the rest, then the
	// pre-existing order UNCHANGED. Two properties are load-bearing:
	//
	//   - Totality. p.id is unique, so the three keys together are a total
	//     order however the first two tie. OFFSET pagination silently drops
	//     and duplicates rows across pages under a non-total ORDER BY, and
	//     this list is paginated.
	//
	//     Totality is not stability, and the two are easy to conflate. pinned
	//     and pinned_at are MUTABLE between page requests, so a pin from
	//     another device between page 1 and page 2 still moves rows across the
	//     offset boundary — the same exposure every OFFSET-paginated list in
	//     this module has. Totality only rules out the ordering ITSELF being
	//     the cause.
	//   - IFNULL rather than relying on NULL ordering. An unpinned project
	//     has no row here, so s.pinned is NULL; MySQL sorts NULL lowest, so
	//     plain DESC would happen to be right today. Writing it out means a
	//     reader does not have to know that, and a future port to a database
	//     that orders NULLs the other way does not silently invert the list.
	"ORDER BY IFNULL(s.pinned, 0) DESC, s.pinned_at DESC, p.id DESC " +
	"LIMIT ? OFFSET ?"

// listVisibleInSpace returns the projects in spaceID that uid may see — the caller's
// own pinned ones first, then newest first — with the caller's role attached.
//
// "newest first" alone was true until PR #861 added pinning and left this line
// behind; the ORDER BY below is the authority and now says three keys, not one.
//
// Visibility is one SQL statement rather than a filter in Go so an unlisted project can never
// transit the process boundary: a space_listed project is visible to any Space member, an
// unlisted one only to its own members. Filtering after the fetch would put the decision on the
// response-shaping path, which is where existence oracles come from.
//
// A Space admin gets NO widening here, deliberately. The brief grants them the real payload on
// the DETAIL route only, and scopes "a Space admin can still enumerate project metadata" to the
// P2 admin surface (the endpoint that will own is_official). Widening the P0 user-facing list
// would ship a slice of that P2 capability early, and "unlisted" would stop meaning what it
// says on the one route where users read it.
func (d *DB) listVisibleInSpace(spaceID, uid string, offset, limit int) ([]*listRow, error) {
	var rows []*listRow
	_, err := d.session.SelectBySql(sqlListVisibleInSpace,
		roleNonMember, uid, uid, spaceID, StatusNormal, DiscoverabilitySpaceListed, limit, offset,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list projects in space: %w", err)
	}
	if err := d.fillMemberCounts(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// fillMemberCounts sets SeatCount (every active seat) and HumanCount (the human
// half) on each listed project.
//
// Two single-table reads for the whole page, not one join per card: read the
// active seat rows for the listed projects, then ask `user` which of those uids
// are bots. Neither statement crosses the pinned/legacy schema boundary, so
// neither needs a COLLATE and neither can lose an index to one — which is the
// whole reason the join that used to do this was removed. PR #855s eighth review.
//
// Both counts come from THE SAME roster read, which is what makes the arithmetic
// exact: every uid in it is either a bot or not, so humans + agents == seats by
// construction rather than by two aggregates hoping to agree. seat_count used to
// be computed by the page statement instead, i.e. under a different read view —
// the inexactness the previous round had to document. PR-5 moved it here for a
// second reason, cost: its ORDER BY makes the page statement sort every visible
// project in the Space, so a correlated aggregate in that statement runs once per
// SORTED row rather than once per returned row (measured: 1667 loops on a
// 2000-project Space).
//
// The seats read is per PAGE, not per Space, so it stays proportional to what the
// client asked for.
func (d *DB) fillMemberCounts(rows []*listRow) error {
	if len(rows) == 0 {
		return nil
	}
	projectIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		projectIDs = append(projectIDs, row.ProjectID)
	}

	var seats []struct {
		ProjectID string `db:"project_id"`
		UID       string `db:"uid"`
	}
	if _, err := d.session.SelectBySql(
		"SELECT project_id, uid FROM `octo_project_member` "+
			"WHERE project_id IN ? AND status = ? AND removing = 0",
		projectIDs, MemberStatusActive,
	).Load(&seats); err != nil {
		return fmt.Errorf("project: read seats for list counts: %w", err)
	}
	if len(seats) == 0 {
		return nil
	}

	uidSet := make(map[string]struct{}, len(seats))
	for _, seat := range seats {
		uidSet[seat.UID] = struct{}{}
	}
	uids := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		uids = append(uids, uid)
	}
	var botUIDs []string
	if _, err := d.session.SelectBySql(
		"SELECT uid FROM `user` WHERE uid IN ? AND robot = 1", uids,
	).Load(&botUIDs); err != nil {
		return fmt.Errorf("project: classify seats for list counts: %w", err)
	}
	bots := make(map[string]struct{}, len(botUIDs))
	for _, uid := range botUIDs {
		bots[uid] = struct{}{}
	}

	humans := make(map[string]int, len(rows))
	total := make(map[string]int, len(rows))
	for _, seat := range seats {
		total[seat.ProjectID]++
		if _, isBot := bots[seat.UID]; !isBot {
			humans[seat.ProjectID]++
		}
	}
	for _, row := range rows {
		row.HumanCount = humans[row.ProjectID]
		row.SeatCount = total[row.ProjectID]
	}
	return nil
}

// listRow carries a project plus the caller-relative fields the list computes.
type listRow struct {
	Model
	MyRole int `db:"my_role"`
	// HumanCount counts HUMANS only, the same split the detail route reports — so
	// one number cannot mean two things depending on which endpoint the client
	// called. It becomes `human_member_count` on the wire.
	//
	// Named HumanCount rather than MemberCount because Resp.MemberCount is the
	// TOTAL: two structs one function apart holding a field of the same name and
	// opposite meaning is how the wrong one gets passed, and this pair is one
	// `toResp` argument away from each other.
	//
	// Filled by fillMemberCounts after the page loads, not by the statement: the
	// join that used to produce it crossed into `user` with a COLLATE, once per
	// listed project.
	HumanCount int
	// SeatCount is every active seat, humans and agents together. Agents are the
	// DIFFERENCE rather than a third count: see the query for the measurement
	// behind that choice.
	//
	// Filled by fillMemberCounts out of the same roster read as HumanCount, so
	// the two agree by construction rather than across two read views.
	SeatCount int
	// Pinned is the CALLER's pin, not a property of the project — the same row
	// reads 1 for one user and 0 for the next. It comes from the LEFT JOIN, so an
	// unpinned project reads 0 rather than dropping out of the list.
	Pinned int `db:"pinned"`
}

// AgentCount is the agent half of D16's split, derived from the two counts.
//
// The race this comment used to describe is gone, and the history is worth keeping
// because it decides what the clamp is for. SeatCount was computed by the page
// statement while the human half came from fillMemberCounts afterwards — two
// statements, no enclosing transaction, two read views, so a member added between
// them made `HumanCount > SeatCount` reachable in normal operation and the clamp
// load-bearing. PR #855's tenth review established that. PR-5 then had to move
// seat_count into fillMemberCounts for cost (see the statement), and one roster
// read for both counts removes the race as a side effect: humans + agents == seats
// by construction now, so `human_member_count + agent_member_count == member_count`
// holds on the wire rather than holding usually. (toResp derives member_count from
// the two halves, so that identity is now true by construction on both routes.)
//
// So the clamp is back to being defensive, and stays: it costs nothing, and a
// future edit that gives the two counts different predicates would otherwise put a
// negative agent count on the wire, which a client renders as "-3 agents".
func (r *listRow) AgentCount() int {
	if r.SeatCount <= r.HumanCount {
		return 0
	}
	return r.SeatCount - r.HumanCount
}

// countActiveMembers counts active seats in a project.
func (d *DB) countActiveMembers(projectID string) (int, error) {
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND status = ? AND removing = 0",
		projectID, MemberStatusActive,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count active members: %w", err)
	}
	return count, nil
}

// countActiveMembersTx is countActiveMembers inside a write transaction, so the
// member quota cannot be crossed by two concurrent adds.
// countActiveMembersTx counts active seats as a LOCKING read, for the reason spelled out on
// countActiveOwnersTx. Reproduced consequence: two concurrent adds against max_members=2 each
// counted 1 and both were admitted, leaving 3 — at the 500 default, 501+.
func (d *DB) countActiveMembersTx(tx *dbr.Tx, projectID string) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND status = ? AND removing = 0 FOR SHARE",
		projectID, MemberStatusActive,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count active members (tx): %w", err)
	}
	return count, nil
}

// lockSpaceRowTx takes an exclusive lock on the Space row, serialising every project
// creation in that Space against each other.
//
// This is what makes the creation quotas actually hold. Counting inside the transaction is
// NOT enough and an earlier version of this file claimed otherwise: under REPEATABLE READ a
// plain `SELECT COUNT(*)` is a non-locking consistent read, so two concurrent creates both
// see 999 and both insert, landing on 1001. The count has to be taken behind a lock on a row
// that both transactions must queue on, and the Space row is the only row that exists for
// every create in that Space.
//
// Lock order: `space` comes AFTER space_member and before project
// (space_member -> space -> project -> ... -> octo_project_member). This comment used to
// declare `space` the first position, which is the pre-B-3 order — on the very helper whose
// old position caused that deadlock, so the stale wording could have argued it back in
// (PR #841 round 3, Jerry-Xin P3). The creator's seat lock must already be held when this is
// called; see createProjectOnce for why that direction and not the reverse.
//
// Returns false when the Space does not exist or is not active.
// Returns the space_id the `space` row STORES, not the one the caller sent.
//
// It used to select `1` and discard it, which left every octo_* row this transaction
// writes carrying the request's bytes. Those rows are later matched under
// utf8mb4_general_ci by the epoch step, while `space` and `space_member` are
// utf8mb4_0900_ai_ci in production — so a drifted space_id passes this lock, gets
// denormalised into octo_project / octo_project_member, and is then unreachable from
// the spelling a seat transition resolves out of space_member. Same defect as the uid
// axis, one table up; see modules/space/seatref.go for the measurements.
//
// The row is under an exclusive lock either way, so reading the column costs nothing.
func (d *DB) lockSpaceRowTx(tx *dbr.Tx, spaceID string) (string, bool, error) {
	if spaceID == "" {
		return "", false, nil
	}
	var found []string
	_, err := tx.SelectBySql(
		"SELECT space_id FROM `space` WHERE space_id = ? AND status = 1 FOR UPDATE", spaceID,
	).Load(&found)
	if err != nil {
		return "", false, fmt.Errorf("project: lock space row: %w", err)
	}
	if len(found) == 0 {
		return "", false, nil
	}
	return found[0], true, nil
}

// lockSpaceSeatsTx takes the shared lock on SEVERAL space_member rows in ONE statement and
// reports which of the requested uids hold an active seat in an active Space.
//
// One statement is the whole point, and it is a lock-ORDER fix rather than a round-trip
// optimisation. modules/space's disband takes `space_member WHERE space_id=? AND status=1
// FOR UPDATE` (lockActiveMemberUIDsTx), a range lock acquired ROW BY ROW in index order —
// clustered-key (id) order. A path that locks two seats in two statements holds the first while
// waiting for the second, so whenever the second row precedes the first in id order the cycle
// with that scan closes and InnoDB reports Error 1213. Reproduced on MySQL 8.0.33 against the
// real addOneMember, with the DISBAND SCAN as the victim — and disband is a step of the
// member-removal security cascade.
//
// Sorting the uids ascending does NOT fix this: the disband scan orders by id, not by uid, so
// any order this module chooses can still oppose it. Handing InnoDB one `uid IN (...)` predicate
// lets it acquire the rows in ITS scan order, which is the same order the disband scan uses —
// there is then no "second row" being waited for while the first is held.
//
// The predicate is CheckMembership's (space_member.status = 1 AND space.status = 1), and it keeps
// the JOIN onto `space` because callers here do not lock the `space` row, so the JOIN is their
// only activeness check. Deliberately NOT CheckMembershipForCleanup's relaxed variant: this is an
// authorization decision and a banned Space must never pass one. (The reconcile scans ask the
// opposite question and correctly use the relaxed form — see queryI1ViolationPage.)
//
// ACCOUNT liveness is NOT in this statement, and that is a correction rather than an omission.
// It was briefly a third `INNER JOIN user u ON u.uid = sm.uid AND u.status = 1 ...` — the shape
// both existing precedents use (modules/user.authVerifyAPIKey,
// modules/bot_provision.assertSpaceMember), and collation-safe here since space_member, space and
// user all drifted to utf8mb4_0900_ai_ci together. It was still wrong, because it takes the
// LOCK-ORDER argument above away:
//
//	EXPLAIN, MySQL 8.0.33, real schema, ~3400 members in the Space, ANALYZEd, 200 uids:
//	  WITH the user join:  s=const, u=range(uid), sm=eq_ref   <- `user` DRIVES
//	  WITHOUT it:          s=const, sm=range(spacemember_spaceid_uid)
//
// Scoped honestly, because a first measurement of this overstated it: the flip depends on DATA
// DISTRIBUTION, not only on row count, and it needs the real table's `spacemember_uid` index to
// be present (a hand-written fixture omitting it plans differently). So this is "the join CAN
// take the driving position", not "it always does" — which is enough, because the lock-order
// argument requires that it never can.
//
// MySQL propagates `u.uid = sm.uid` with `sm.uid IN (...)` into `u.uid IN (...)`, making a `user`
// PK range a cheap driving candidate — and once `user` drives, space_member rows are locked one
// eq_ref at a time in `user`-PK order rather than in InnoDB's own scan order for one IN predicate.
// That is precisely the property the paragraphs above depend on to stay out of the row-order cycle
// with the disband scan, and precisely the class round 7 diagnosed in the single-statement epoch
// bump ("the order is not in the SQL at all: the optimizer picks the driving table"). Reviewers
// flagged it as a cardinality-dependent flip; measured, it is worse than a flip — `user` drives at
// BOTH cardinalities.
//
// So account liveness moved OUT of the locking statement into pkg/user.ActiveAccounts, a separate
// single-table read the callers run alongside it. That also dissolves the read-view question this
// statement would otherwise raise, and it makes both paths symmetric: the READ path already had to
// use a separate query, being rooted at an octo_* table where `JOIN user` is error 1267 in
// production and green in CI.
//
// TestSeatLockStatementHasNoUserJoin pins the statement so the join cannot come back.
// (This used to name TestSeatLockStatementLetsInnoDBChooseTheRowOrder, deleted in round 9
// for being distribution-dependent: it asserted an EXPLAIN plan, which the optimizer is
// free to change on different data. The surviving guard asserts the STATEMENT instead.)
//
// The read view that JOIN opens is no longer load-bearing, because every aggregate that authorises
// a write is now a locking read (see countActiveOwnersTx).
//
// Why a lock at all, rather than calling pkg/space.CheckMembership: that function takes a
// *dbr.Session, so it runs on a DIFFERENT pooled connection in its own implicit transaction. A
// read there proves nothing about the state at COMMIT time — a Space removal committing in between
// yields a project seat with no Space seat, closed only later by the asynchronous cascade.
//
// `FOR SHARE OF sm` locks space_member only, never `space`. Locking `space` too would put a shared
// lock on the row Space disband updates, inventing a contention path this module has no reason to
// create. Needs MySQL 8.0.1+ for the OF clause; verified on 8.0.33.
//
// vs. Space MEMBER REMOVAL: it takes FOR UPDATE on the same space_member row, so it blocks here
// until this transaction commits; the cascade step reads space_member without a lock, so there is
// no cycle with it either.
//
// Returns the set of uids that DO hold a seat. Callers decide which absence means what, since
// actor and target absences carry different sentinels.
// lockSpaceSeatRowsTx is lockSpaceSeatRowTx for a SET of uids: a shared lock on
// each one's space_member row, taken in ONE statement, WITHOUT joining `space`.
//
// # Why not lockSpaceSeatsTx
//
// Because that one JOINs `space`, and createProject cannot afford it. A table
// outside the `FOR SHARE OF` list is read as a CONSISTENT read, which OPENS the
// transaction's read view — and in createProject this is the first statement, so
// every creation quota counted after it would be answered from a snapshot taken
// before the `space` row lock. That is not hypothetical: six concurrent creates
// all passed MaxPerSpace=1 when the single-uid path had this shape, which is why
// lockSpaceSeatRowTx exists at all and why
// TestCreateDoesNotTakeItsSpaceSeatLockThroughAJoin pins it. That guard caught
// this function's absence — the agent seats were being locked through the
// JOINing helper, reopening exactly the defect P0 closed.
//
// Nothing is lost by dropping the JOIN: createProject re-checks the Space's
// activeness under the exclusive `space` lock immediately afterwards, which is
// the authoritative check anyway.
//
// One statement rather than one per uid: the round-trips inside the transaction
// then do not grow with the number of agents, and the rows are taken as a single
// deterministic set instead of one at a time in caller-controlled order, which is
// a deadlock shape.
func (d *DB) lockSpaceSeatRowsTx(tx *dbr.Tx, spaceID string, uids []string) (map[string]bool, error) {
	held := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return held, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	args := make([]interface{}, 0, len(uids)+1)
	args = append(args, spaceID)
	for _, uid := range uids {
		args = append(args, uid)
	}
	var found []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM `space_member` "+
			"WHERE space_id = ? AND uid IN ("+placeholders+") AND status = 1 "+
			"ORDER BY uid FOR SHARE",
		args...,
	).Load(&found)
	if err != nil {
		return nil, fmt.Errorf("project: lock space seat rows: %w", err)
	}
	for _, uid := range found {
		held[uid] = true
	}
	return held, nil
}

func (d *DB) lockSpaceSeatsTx(tx *dbr.Tx, spaceID string, uids []string) (map[string]bool, error) {
	held := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return held, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	args := make([]interface{}, 0, len(uids)+1)
	args = append(args, spaceID)
	for _, uid := range uids {
		args = append(args, uid)
	}
	var found []string
	_, err := tx.SelectBySql(
		"SELECT sm.uid FROM `space_member` sm "+
			"INNER JOIN `space` s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.space_id = ? AND sm.uid IN ("+placeholders+") AND sm.status = 1 "+
			"FOR SHARE OF sm",
		args...,
	).Load(&found)
	if err != nil {
		return nil, fmt.Errorf("project: lock space seats in tx: %w", err)
	}
	for _, uid := range found {
		held[uid] = true
	}
	return held, nil
}

// lockSpaceSeatRowTx takes the shared lock on uid's space_member row and nothing else — no
// JOIN onto `space`.
//
// It exists for createProject, and the missing JOIN is the entire point. `FOR SHARE OF sm`
// locks sm, but a table NOT named in the OF list is read as a CONSISTENT read, and a
// consistent read is what OPENS the transaction's read view. Put this statement first and
// every later plain SELECT — including all three creation-quota counts — is answered from a
// snapshot taken before the `space` row lock was acquired, so two concurrent creates both
// count 0 and both insert. That is not a subtle degradation of the quota, it removes it:
// reproduced as six concurrent creates all succeeding against MaxPerSpace=1.
//
// Verified directly on MySQL 8.0.33: `SELECT ... FROM sm JOIN sp ... FOR SHARE OF sm`
// followed by `SELECT COUNT(*)` does NOT see a row another session committed in between,
// while the same pair without the JOIN does.
//
// Dropping the JOIN loses nothing here. The caller takes `space ... FOR UPDATE` immediately
// afterwards and refuses anything but status = 1, so Space activeness is checked more
// strongly than the JOIN checked it — under an exclusive lock rather than in a snapshot.
// Callers that do NOT lock the `space` row must keep using
// lockSpaceSeatsTx, whose JOIN is their only activeness check.
// Returns the uid the `space_member` row STORES, for the same reason lockSpaceRowTx
// returns the stored space_id: the caller goes on to WRITE that identifier into octo_*
// tables, which the epoch step later matches under a stricter collation. Selecting `1`
// and discarding the column left createProject denormalising the request's spelling.
func (d *DB) lockSpaceSeatRowTx(tx *dbr.Tx, spaceID, uid string) (string, bool, error) {
	if spaceID == "" || uid == "" {
		return "", false, nil
	}
	var found []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM `space_member` WHERE uid = ? AND space_id = ? AND status = 1 "+
			"LIMIT 1 FOR SHARE",
		uid, spaceID,
	).Load(&found)
	if err != nil {
		return "", false, fmt.Errorf("project: lock space seat row: %w", err)
	}
	if len(found) == 0 {
		return "", false, nil
	}
	return found[0], true, nil
}

// checkSpaceSeatForCleanupTx answers "does uid still hold their Space seat, so cleanup must
// SKIP?" inside the caller's transaction, taking a shared lock on the space_member row.
//
// This is the CLEANUP predicate, not the authorization one, and the difference is
// load-bearing: it accepts a banned Space (status <> 0) because membership there is real, so
// a ban must not tear members out of their projects. It matches
// pkg/space.CheckMembershipForCleanup exactly — the two layers have to answer the same
// question or the outer gate short-circuits a different predicate than the inner one.
//
// The status literal is spelled out rather than importing modules/space's constant, for the
// same reason pkg/space does it: the import would be a cycle.
//
// The shared lock is what closes the rejoin race. The cascade's outer gate checks membership
// once, outside any transaction; between that check and this seat being closed the user can
// rejoin the Space, and closing their seat then destroys a membership that is legitimate
// again. Holding S on the space_member row means a concurrent rejoin (which takes X on it)
// cannot commit inside the window.
func (d *DB) checkSpaceSeatForCleanupTx(tx *dbr.Tx, spaceID, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var found []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space_member` sm "+
			"INNER JOIN `space` s ON s.space_id = sm.space_id AND s.status <> 0 "+
			"WHERE sm.uid = ? AND sm.space_id = ? AND sm.status = 1 LIMIT 1 FOR SHARE OF sm",
		uid, spaceID,
	).Load(&found)
	if err != nil {
		return false, fmt.Errorf("project: check space seat for cleanup in tx: %w", err)
	}
	return len(found) > 0, nil
}

// ---------- members ----------

// queryMember reads one member row regardless of status. (nil, nil) when absent.
func (d *DB) queryMember(projectID, uid string) (*MemberModel, error) {
	var rows []*MemberModel
	_, err := d.session.SelectBySql(
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at "+
			"FROM `octo_project_member` WHERE project_id = ? AND uid = ? LIMIT 1",
		projectID, uid,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query member: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// queryMemberTx reads one member row under a row lock, inside the caller's
// transaction. Every role/removal decision reads through this rather than through
// the unlocked variant: an unlocked read followed by a conditional write is how a
// concurrent role change turns "an admin may not demote an owner" into a race.
func (d *DB) queryMemberTx(tx *dbr.Tx, projectID, uid string) (*MemberModel, error) {
	var rows []*MemberModel
	_, err := tx.SelectBySql(
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at "+
			"FROM `octo_project_member` WHERE project_id = ? AND uid = ? FOR UPDATE",
		projectID, uid,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: lock member: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// admitMemberTx creates or reactivates a seat and reports whether anything
// changed.
//
// changed=false means the uid already held an active seat with this role, i.e. a
// no-op — and the caller must then NOT bump member_epoch. MySQL's affected-rows
// for INSERT .. ON DUPLICATE KEY UPDATE is 1 for an insert, 2 for a real update
// and 0 when the update would set every column to its current value, so 0 is
// exactly "nothing changed". Testing `!= 1` instead would classify every
// reactivation as a no-op.
//
// The role is only reset on reactivation, never on a row that is already active:
// re-adding a member who happens to be an admin must not silently demote them.
//
// ⚠️ ASSIGNMENT ORDER IS LOAD-BEARING. MySQL evaluates ON DUPLICATE KEY UPDATE
// assignments left to right, and a column read on the right-hand side sees the value
// written by any PRECEDING assignment in the same statement. So every clause that tests
// the OLD status must come before `status = 1`. An earlier draft put
// `updated_at = IF(status = 0, ...)` after it: `status` was already 1 by then, the
// condition was permanently false, and reactivating a member silently kept the
// updated_at from when they were first removed. Verified on MySQL 8.0.33 — with the
// clause after `status = 1` the timestamp does not move; before it, it does.
func (d *DB) admitMemberTx(tx *dbr.Tx, m *MemberModel) (bool, error) {
	res, err := tx.InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE "+
			// -- reads of the OLD status must all precede `status = 1` --
			"  role = IF(status = 0, VALUES(role), role), "+
			"  invite_uid = IF(status = 0, VALUES(invite_uid), invite_uid), "+
			"  updated_at = IF(status = 0 OR removing = 1, VALUES(updated_at), updated_at), "+
			// -- from here on `status` reads as 1 --
			"  space_id = VALUES(space_id), "+
			// D4 — re-admission CANCELS an in-flight cascade rather than being
			// rejected. Clearing `removing` here is the cancellation: the worker
			// re-reads this row under lock before each batch and stops when it
			// finds 0. Rejecting instead would make an unrelated admin action fail
			// for as long as the cascade takes, and a cascade can legitimately run
			// long. The caller must also mark the outstanding job cancelled — see
			// cancelPendingRemovalJobsTx — so the queue does not keep a row that
			// will never do anything.
			"  removing = 0, "+
			"  status = 1",
		m.ProjectID, m.UID, m.SpaceID, m.Role, MemberStatusActive, m.InviteUID,
		m.CreatedAt, m.UpdatedAt,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: admit member: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: admit member affected rows: %w", err)
	}
	return affected > 0, nil
}

// deactivateMemberTx closes one seat and reports whether a row actually changed.
// The status filter is what makes the Space-cascade step idempotent: a rerun finds
// no active row, affects zero rows, and therefore does not bump the epoch again.
func (d *DB) deactivateMemberTx(tx *dbr.Tx, projectID, uid string, now time.Time) (bool, error) {
	// `removing` is cleared here for the same reason disbandProjectTx clears it,
	// and this is the path that actually reaches the state: the Space cascade
	// closes a project seat for a uid leaving the Space, and it can land on a seat
	// whose project-side removal is still in flight. Leaving removing = 1 on a
	// status = 0 row makes finishMemberRemovalTx permanently unable to match it,
	// so the seat stalls forever and the stall scan alerts with nothing to do.
	res, err := tx.Update("octo_project_member").
		Set("status", MemberStatusRemoved).
		Set("removing", 0).
		Set("updated_at", now).
		Where("project_id = ? AND uid = ? AND status = ?", projectID, uid, MemberStatusActive).
		Exec()
	if err != nil {
		return false, fmt.Errorf("project: deactivate member: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: deactivate member affected rows: %w", err)
	}
	if affected > 0 {
		// Same reasoning as the disband path: the seat is closed, so any job still
		// queued for it has no work left. Retiring it here rather than letting the
		// worker discover that keeps a lease and an attempt from being spent on a
		// no-op.
		if _, err := d.cancelPendingRemovalJobsTx(tx, projectID, uid, now); err != nil {
			return false, err
		}
	}
	return affected > 0, nil
}

// updateMemberRoleTx sets a role and reports whether it changed. The `role <> ?`
// guard is what makes "setting the role a member already has" a no-op that leaves
// the epoch alone.
func (d *DB) updateMemberRoleTx(tx *dbr.Tx, projectID, uid string, role int, now time.Time) (bool, error) {
	res, err := tx.Update("octo_project_member").
		Set("role", role).
		Set("updated_at", now).
		Where("project_id = ? AND uid = ? AND status = ? AND role <> ?",
			projectID, uid, MemberStatusActive, role).
		Exec()
	if err != nil {
		return false, fmt.Errorf("project: update member role: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: update member role affected rows: %w", err)
	}
	return affected > 0, nil
}

// countActiveOwnersTx counts active owners, used by the last-owner guard.
// countActiveOwnersTx counts the project's active owners as a LOCKING read.
//
// `FOR SHARE` is not optional here, and the reason is the same read-view trap
// lockSpaceSeatRowTx exists for. Every caller's transaction opens with
// lockSpaceSeatsTx, which JOINs `space` — a table outside the `FOR SHARE OF`
// list, therefore a CONSISTENT read, therefore the statement that opens the read view. A plain
// COUNT(*) after it is answered from a snapshot taken BEFORE lockActiveProjectTx, so the
// project row lock does not protect this count at all.
//
// The consequence is not a soft one. Reproduced on MySQL 8.0.33: two owners leaving
// concurrently each read 2, each pass "you are not the last owner", and the project ends with
// ZERO owners — a state P0 cannot repair (role change and disband are owner-only) and no
// reconcile scan detects. What made it invisible is that each transaction re-reads its OWN
// membership row FOR UPDATE and so sees fresh data; only the aggregate authorising the write
// was stale.
//
// A locking read is a current read, so it sees committed changes regardless of the read view.
// The added lock scope is negligible: every caller already holds the project row's exclusive
// lock, so the rows counted here cannot be written by anyone else anyway.
func (d *DB) countActiveOwnersTx(tx *dbr.Tx, projectID string) (int, error) {
	var count int
	err := tx.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND status = ? AND role = ? AND removing = 0 FOR SHARE",
		projectID, MemberStatusActive, RoleOwner,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count owners (tx): %w", err)
	}
	return count, nil
}

// listMembers returns the roster joined to `user` for display names, paged.
//
// LEFT JOIN, not INNER: a member whose user row is missing must still appear, or
// the roster silently disagrees with the member count and with what the removal
// endpoints accept.
func (d *DB) listMembers(projectID string, offset, limit int) ([]*memberRosterModel, error) {
	var rows []*memberRosterModel
	_, err := d.session.SelectBySql(
		"SELECT pm.project_id, pm.uid, pm.space_id, pm.role, pm.status, pm.invite_uid, "+
			"pm.created_at, pm.updated_at, IFNULL(u.name, '') AS name, "+
			// D16 — 名册要能区分人和分身，并给出分身的所有者，客户端才能像通讯录
			// 那样把分身挂在人下面。两个 JOIN 都是 LEFT：没有 user 行的成员必须仍
			// 然出现（见下面的注释），而 robot 行对每一个真人都不存在。
			//
			// robot 的 COLLATE 是必须的：`robot` 与 `user` 都是未声明 COLLATE 的
			// 老表（生产库 utf8mb4_0900_ai_ci），octo_project_member 明确是
			// general_ci，隐式比较在生产上报 1267 而在 CI 上一路绿灯。
			// u.uid 那个 JOIN 是既有代码，未加 COLLATE。上一版这里写着"它比较的是
			// 两张老表（user / octo_project_member）"——**这句是错的**，而且与上面
			// 两行自相矛盾：octo_project_member 由它自己的迁移 pin 成 general_ci。
			// 所以那个比较是 general_ci ⟷ 0900_ai_ci，在生产上会报 1267，本函数
			// 在转换落地之前根本执行不了（PR #855 第五轮 review 实测确认）。
			//
			// 不在本次改它：那是 P0 留下的、跟着排序规则转换一起走的既有账，
			// 改它等于顺带改动一条已在生产跑着的查询计划。但它是 D16 的名册接口，
			// 开关一开就是用户可见的，所以"转换已落地（或这个 JOIN 已 pin）"是
			// 上线前的硬门槛，记在 open_verification 里。
			"IFNULL(u.robot, 0) AS robot, IFNULL(r.creator_uid, '') AS owner_uid "+
			"FROM `octo_project_member` pm "+
			"LEFT JOIN `user` u ON u.uid = pm.uid "+
			"LEFT JOIN `robot` r ON r.robot_id = pm.uid COLLATE utf8mb4_general_ci AND r.status = 1 "+
			"WHERE pm.project_id = ? AND pm.status = ? AND pm.removing = 0 "+
			"ORDER BY pm.role DESC, pm.created_at ASC LIMIT ? OFFSET ?",
		projectID, MemberStatusActive, limit, offset,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list members: %w", err)
	}
	return rows, nil
}

// queryActiveProjectIDsForSpaceMember returns up to limit active project ids the
// uid still holds a seat in, within one Space. Bounded on purpose: the
// Space-removal cascade walks it in pages so one member of a thousand projects
// cannot hold a cleanup lease for the whole walk.
func (d *DB) queryActiveProjectIDsForSpaceMember(spaceID, uid string, limit int) ([]string, error) {
	var ids []string
	_, err := d.session.SelectBySql(
		"SELECT project_id FROM `octo_project_member` "+
			"WHERE space_id = ? AND uid = ? AND status = ? AND removing = 0 "+
			"ORDER BY project_id LIMIT ?",
		spaceID, uid, MemberStatusActive, limit,
	).Load(&ids)
	if err != nil {
		return nil, fmt.Errorf("project: query active projects of space member: %w", err)
	}
	return ids, nil
}

// queryProjectIDsForSpaceMemberPage returns up to limit project ids in one Space
// where the uid has a member row, ACTIVE OR NOT, ordered by project_id and starting
// strictly after afterProjectID.
//
// The status filter is deliberately absent, which is the whole difference from
// queryActiveProjectIDsForSpaceMember. Its caller is the post-cleanup owner
// convergence (registerAllMemberGroupOwnerFinalizer), which runs AFTER the cascade
// has closed this member's seats — so filtering on status = active would return the
// empty set and converge nothing. Filtering on role = owner would be wrong for a
// second reason: the group's creator is not guaranteed to be a project owner (the
// sync deliberately leaves a former owner in place when the project has none), so a
// departing non-owner can still be the creator the group cascade hands over.
//
// Keyset paging rather than the cascade's "just take the next page" trick: that one
// works because closing a seat removes the row from its own result set, and this
// query has no such filter, so LIMIT alone would re-read page one forever. That same
// absence is why a spent page budget here does NOT ask for a retry — see
// convergeAllMemberGroupOwners.
//
// Measured plan (MySQL 8.0.46, 10k octo_project_member rows over 200 uids x 50 Spaces,
// after ANALYZE):
//
//	ref idx_octo_project_member_space_uid  key_len=324  ref=const,const  rows=50
//	Extra: Using where; Using filesort
//
// The index seeks straight to the (space_id, uid) range, which is the part that has to
// be right. The filesort is expected and accepted rather than overlooked: the index is
// (space_id, uid, status), so inside that range rows are ordered by `status` and then by
// the primary key, never by project_id — and constraining `status` is exactly what this
// query must not do. What bounds the cost is the set itself: one row per project this
// member has ever been in, sorted in memory. Recorded because every other new statement
// in this change carries its plan, and a reader would notice this one did not.
// PR #855s eleventh review, the nit.
func (d *DB) queryProjectIDsForSpaceMemberPage(spaceID, uid, afterProjectID string, limit int) ([]string, error) {
	var ids []string
	_, err := d.session.SelectBySql(
		"SELECT project_id FROM `octo_project_member` "+
			"WHERE space_id = ? AND uid = ? AND project_id > ? "+
			"ORDER BY project_id LIMIT ?",
		spaceID, uid, afterProjectID, limit,
	).Load(&ids)
	if err != nil {
		return nil, fmt.Errorf("project: query projects of space member: %w", err)
	}
	return ids, nil
}

// queryIsOfficialFlags reads is_official for the D6 guard test.
func (d *DB) queryIsOfficialFlags(spaceID string) ([]*officialFlagModel, error) {
	var rows []*officialFlagModel
	_, err := d.session.SelectBySql(
		"SELECT project_id, is_official FROM `octo_project` WHERE space_id = ?", spaceID,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query is_official: %w", err)
	}
	return rows, nil
}
