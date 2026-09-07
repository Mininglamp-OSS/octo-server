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
//     equal the default, making the regression invisible. `member_epoch` is
//     likewise absent from the insert list: it takes the DDL default, so the ONLY
//     statement in this package that writes that column is `member_epoch =
//     member_epoch + 1`, which is what makes monotonicity checkable by grep.
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
// type comment for why active_name / is_official / member_epoch are absent.
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
			"discoverability, max_members, member_epoch, status, created_at, updated_at "+
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
			"discoverability, max_members, member_epoch, status, created_at, updated_at "+
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
func (d *DB) bumpMemberEpochTx(tx *dbr.Tx, projectID string, now time.Time) error {
	_ = now // the statement is clock-free now; the parameter stays for call-site stability
	// updated_at is deliberately NOT written here: it is the field a client diffs to decide
	// whether the project's PROFILE changed, and member_epoch already carries the roster
	// signal — writing both made every roster edit churn the profile clock (yujiawei Q8,
	// PR #841 round 1). The status predicate makes the method safe on its own terms instead
	// of by caller convention: a disbanded project's epoch must not move.
	_, err := tx.UpdateBySql(
		"UPDATE octo_project SET member_epoch = member_epoch + 1 "+
			"WHERE project_id = ? AND status = ?",
		projectID, StatusNormal,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: bump member epoch: %w", err)
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

// listVisibleInSpace returns the projects in spaceID that uid may see, newest first, with the
// caller's role attached.
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
	_, err := d.session.SelectBySql(
		"SELECT p.project_id, p.space_id, p.name, p.description, p.logo, p.creator, "+
			"p.discoverability, p.max_members, p.member_epoch, p.status, "+
			"p.created_at, p.updated_at, "+
			"IFNULL(pm.role, ?) AS my_role, "+
			"(SELECT COUNT(*) FROM `octo_project_member` mc "+
			"  WHERE mc.project_id = p.project_id AND mc.status = 1 AND mc.removing = 0) AS member_count "+
			"FROM `octo_project` p "+
			// `removing = 0` on the JOIN as well as on the count: without it a member
			// whose seat is closing keeps my_role, and — worse — keeps
			// `pm.uid IS NOT NULL`, which is the clause that reveals UNLISTED
			// projects. So a departing member would go on seeing projects they are
			// not supposed to be able to enumerate, for the whole cascade window,
			// while the member_count beside them already excluded them.
			"LEFT JOIN `octo_project_member` pm "+
			"  ON pm.project_id = p.project_id AND pm.uid = ? AND pm.status = 1 "+
			"     AND pm.removing = 0 "+
			"WHERE p.space_id = ? AND p.status = ? "+
			"  AND (p.discoverability = ? OR pm.uid IS NOT NULL) "+
			"ORDER BY p.id DESC LIMIT ? OFFSET ?",
		roleNonMember, uid, spaceID, StatusNormal, DiscoverabilitySpaceListed, limit, offset,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: list projects in space: %w", err)
	}
	return rows, nil
}

// listRow carries a project plus the caller-relative fields the list computes.
type listRow struct {
	Model
	MyRole      int `db:"my_role"`
	MemberCount int `db:"member_count"`
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
func (d *DB) lockSpaceRowTx(tx *dbr.Tx, spaceID string) (bool, error) {
	if spaceID == "" {
		return false, nil
	}
	var found []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space` WHERE space_id = ? AND status = 1 FOR UPDATE", spaceID,
	).Load(&found)
	if err != nil {
		return false, fmt.Errorf("project: lock space row: %w", err)
	}
	return len(found) > 0, nil
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
func (d *DB) lockSpaceSeatRowTx(tx *dbr.Tx, spaceID, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}
	var found []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space_member` WHERE uid = ? AND space_id = ? AND status = 1 "+
			"LIMIT 1 FOR SHARE",
		uid, spaceID,
	).Load(&found)
	if err != nil {
		return false, fmt.Errorf("project: lock space seat row: %w", err)
	}
	return len(found) > 0, nil
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
			"pm.created_at, pm.updated_at, IFNULL(u.name, '') AS name "+
			"FROM `octo_project_member` pm "+
			"LEFT JOIN `user` u ON u.uid = pm.uid "+
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
