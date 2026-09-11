package project

import (
	"fmt"
	"sort"
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

// spaceSeatRefs records the primary-key identity observed before a write
// transaction begins. It is preparation only; every resolved lock query
// rechecks the id, uid, Space, status, and account rows under its transaction
// locks before a caller treats a seat as eligible.
type spaceSeatRefs map[string]int64

// resolveSpaceSeatIDs prepares exact space_member primary keys without holding
// locks or opening the caller's transaction. A later locking read must
// revalidate the returned identities: a removed or recreated row is deliberately
// reported as absent rather than authorizing from a stale preparation result.
func (d *DB) resolveSpaceSeatIDs(spaceID string, uids []string) (spaceSeatRefs, error) {
	refs := make(spaceSeatRefs, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return refs, nil
	}
	unique := make([]string, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		unique = append(unique, uid)
	}
	if len(unique) == 0 {
		return refs, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]interface{}, 0, len(unique)+1)
	args = append(args, spaceID)
	for _, uid := range unique {
		args = append(args, uid)
	}
	var rows []struct {
		ID  int64  `db:"id"`
		UID string `db:"uid"`
	}
	if _, err := d.session.SelectBySql(
		"SELECT id, uid FROM `space_member` "+
			"WHERE space_id = ? AND uid IN ("+placeholders+") "+
			"ORDER BY id",
		args...,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: resolve space seat ids: %w", err)
	}
	for _, row := range rows {
		refs[row.UID] = row.ID
	}
	return refs, nil
}

// NewDB builds the DAO against the process-wide dbr session.
func NewDB(ctx *config.Context) *DB {
	return &DB{ctx: ctx, session: ctx.DB()}
}

// projectInsertColumns is the write-side column list for octo_project. The
// generated active_name column and non-Project-managed fields are absent.
var projectInsertColumns = []string{
	"project_id", "space_id", "name", "description", "logo", "creator",
	"discoverability", "max_members", "status",
	"created_at", "updated_at",
	// join_mode is deliberately absent: the column exists with its DDL default (1) and
	// nothing above the storage layer touches it until the P2 join path lands. See the
	// JoinMode constants in model.go.
}

// ---------- project ----------

// insertProjectTx writes one project row inside tx. Project names are labels,
// so this DAO intentionally performs no name uniqueness check.
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
// columns it means to change; generated active_name and storage-managed fields
// never appear because setColumns is built from an allow-list.
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
	// having to remember. Today that is one caller (disbandProjectOnce). The Space-removal
	// cascade preserves an Owner seat and closes only non-Owner seats, including agents
	// owned by a departing Owner, so it does not transition the project subsystem here.
	// An earlier version of this comment claimed both paths already routed through here,
	// which a reader would grep for and not find.
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

// lockSpaceSeatTargets orders prepared seat identities by their clustered
// primary key. The preparation query is deliberately outside the write
// transaction; this helper never treats it as authorization.
func lockSpaceSeatTargets(uids []string, refs spaceSeatRefs) []struct {
	uid string
	id  int64
} {
	targets := make([]struct {
		uid string
		id  int64
	}, 0, len(uids))
	seen := make(map[int64]struct{}, len(uids))
	for _, uid := range uids {
		id, ok := refs[uid]
		if !ok || id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, struct {
			uid string
			id  int64
		}{uid: uid, id: id})
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].id < targets[j].id
	})
	return targets
}

// lockSpaceSeatRowsTx takes shared locks on several prepared space_member rows
// in clustered-primary-key order, without joining space. It is used before a
// create takes the exclusive Space lock, so the first locking read must not
// open a consistent-read view through the space table.
//
// resolveSpaceSeatIDs supplies exact row identities before the transaction
// starts. The id IN range is therefore bounded to the requested seats and
// FORCE INDEX (PRIMARY) makes the lock acquisition path the same ascending id
// order as Space disband's space_member scan. The id/uid comparison after the
// locking read rejects a removed/recreated or otherwise mismatched row.
func (d *DB) lockSpaceSeatRowsTx(
	tx *dbr.Tx, spaceID string, uids []string, refs spaceSeatRefs,
) (map[string]bool, error) {
	held := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return held, nil
	}
	targets := lockSpaceSeatTargets(uids, refs)
	if len(targets) == 0 {
		return held, nil
	}
	idPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
	uidPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
	args := make([]interface{}, 0, len(targets)*2+1)
	for _, target := range targets {
		args = append(args, target.id)
	}
	args = append(args, spaceID)
	for _, target := range targets {
		args = append(args, target.uid)
	}
	var found []struct {
		ID  int64  `db:"id"`
		UID string `db:"uid"`
	}
	_, err := tx.SelectBySql(
		"SELECT sm.id, sm.uid FROM `space_member` sm FORCE INDEX (PRIMARY) "+
			"STRAIGHT_JOIN `user` u ON u.uid = sm.uid AND u.status = 1 AND IFNULL(u.is_destroy, 0) <> 2 "+
			"WHERE sm.id IN ("+idPlaceholders+") AND sm.space_id = ? AND sm.uid IN ("+uidPlaceholders+") "+
			"AND sm.status = 1 ORDER BY sm.id ASC FOR SHARE OF sm, u",
		args...,
	).Load(&found)
	if err != nil {
		return nil, fmt.Errorf("project: lock space seat rows: %w", err)
	}
	expected := make(map[int64]string, len(targets))
	for _, target := range targets {
		expected[target.id] = target.uid
	}
	for _, row := range found {
		if expected[row.ID] == row.UID {
			held[row.UID] = true
		}
	}
	return held, nil
}

// lockSpaceSeatsTx is the authorization form of lockSpaceSeatRowsTx. It keeps
// the active-Space predicate and locks the requested membership and account
// rows, but never locks space itself. The exact primary-key probes preserve
// the same space_member -> space -> project lock order used by creation.
func (d *DB) lockSpaceSeatsTx(
	tx *dbr.Tx, spaceID string, uids []string, refs spaceSeatRefs,
) (map[string]bool, error) {
	held := make(map[string]bool, len(uids))
	if spaceID == "" || len(uids) == 0 {
		return held, nil
	}
	targets := lockSpaceSeatTargets(uids, refs)
	if len(targets) == 0 {
		return held, nil
	}
	idPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
	uidPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
	args := make([]interface{}, 0, len(targets)*2+1)
	for _, target := range targets {
		args = append(args, target.id)
	}
	args = append(args, spaceID)
	for _, target := range targets {
		args = append(args, target.uid)
	}
	var found []struct {
		ID  int64  `db:"id"`
		UID string `db:"uid"`
	}
	_, err := tx.SelectBySql(
		"SELECT sm.id, sm.uid FROM `space_member` sm FORCE INDEX (PRIMARY) "+
			"STRAIGHT_JOIN `space` s ON s.space_id = sm.space_id AND s.status = 1 "+
			"STRAIGHT_JOIN `user` u ON u.uid = sm.uid AND u.status = 1 AND IFNULL(u.is_destroy, 0) <> 2 "+
			"WHERE sm.id IN ("+idPlaceholders+") AND sm.space_id = ? AND sm.uid IN ("+uidPlaceholders+") "+
			"AND sm.status = 1 ORDER BY sm.id ASC FOR SHARE OF sm, u",
		args...,
	).Load(&found)
	if err != nil {
		return nil, fmt.Errorf("project: lock space seats in tx: %w", err)
	}
	expected := make(map[int64]string, len(targets))
	for _, target := range targets {
		expected[target.id] = target.uid
	}
	for _, row := range found {
		if expected[row.ID] == row.UID {
			held[row.UID] = true
		}
	}
	return held, nil
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
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, "+
			"COALESCE(joined_at, created_at) AS joined_at, updated_at "+
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
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, "+
			"COALESCE(joined_at, created_at) AS joined_at, updated_at "+
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
// The role is only reset when a seat is inactive or closing, never on a row that
// is already active: re-adding an active admin must not silently demote them.
// `joined_at` follows the same old-state predicate: a first admission receives
// the caller's current time, re-admission refreshes the current round, and an
// active idempotent add leaves the existing round untouched.
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
			"(project_id, uid, space_id, role, status, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE "+
			// -- reads of the OLD status/removing must all precede `status = 1` --
			"  role = IF(status = 0 OR removing = 1, VALUES(role), role), "+
			"  invite_uid = IF(status = 0 OR removing = 1, VALUES(invite_uid), invite_uid), "+
			"  joined_at = IF(status = 0 OR removing = 1, VALUES(joined_at), joined_at), "+
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
		m.CreatedAt, m.JoinedAt, m.UpdatedAt,
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

// deactivateMemberTx closes one non-owner seat and reports whether a row changed.
// The unique Owner identity is retained when the Space seat disappears; auth
// still denies the absent Space seat, and a later transfer remains possible if
// the user rejoins. The status filter makes the cleanup idempotent.
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
		Where("project_id = ? AND uid = ? AND status = ? AND role <> ?", projectID, uid,
			MemberStatusActive, RoleOwner).
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

// deactivateStaleMemberTx closes an active seat after the project itself has
// already been disbanded. A normal Space cascade deliberately preserves an
// Owner row so a later rejoin can transfer ownership, but a disbanded project
// has no remaining authorization surface and must not retain an active seat.
// This path intentionally does not bump member_epoch: disband has already
// invalidated the project's active membership view.
func (d *DB) deactivateStaleMemberTx(tx *dbr.Tx, projectID, uid string, now time.Time) (bool, error) {
	res, err := tx.Update("octo_project_member").
		Set("status", MemberStatusRemoved).
		Set("removing", 0).
		Set("updated_at", now).
		Where("project_id = ? AND uid = ? AND status = ?", projectID, uid,
			MemberStatusActive).
		Exec()
	if err != nil {
		return false, fmt.Errorf("project: deactivate stale member: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: deactivate stale member affected rows: %w", err)
	}
	if affected > 0 {
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

// countActiveOwnersTx counts the project's active owners as a LOCKING read.
//
// `FOR SHARE` is not optional here. Every write path prepares its space-member
// primary keys before BEGIN, then takes the resolved authorization lock through
// lockSpaceSeatsTx. That helper JOINs `space` outside the FOR SHARE list, so a
// plain COUNT(*) after it would still answer from the transaction's old view.
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

// queryActiveProjectIDsForSpaceMember returns up to limit actionable project ids
// for a uid leaving a Space. Non-Owner active seats are directly actionable. An
// Owner row is included only when it has an active non-Owner agent rider owned by
// that uid: the cascade preserves the Owner identity but must still close those
// riders. Owner-only rows are omitted so they cannot consume a page forever.
//
// Bounded on purpose: the Space-removal cascade walks it in pages so one member
// of a thousand projects cannot hold a cleanup lease for the whole walk.
func (d *DB) queryActiveProjectIDsForSpaceMember(spaceID, uid string, limit int) ([]string, error) {
	var ids []string
	_, err := d.session.SelectBySql(
		"SELECT pm.project_id FROM `octo_project_member` pm "+
			"WHERE pm.space_id = ? AND pm.uid = ? AND pm.status = ? AND pm.removing = 0 "+
			"AND (pm.role <> ? OR EXISTS ("+
			"SELECT 1 FROM `octo_project_member` rider INNER JOIN `robot` r "+
			"ON r.robot_id = rider.uid COLLATE utf8mb4_general_ci "+
			"WHERE rider.project_id = pm.project_id AND rider.space_id = pm.space_id "+
			"AND rider.status = ? AND rider.removing = 0 AND rider.role <> ? "+
			"AND r.creator_uid = ? AND r.status = 1)) "+
			"ORDER BY pm.project_id LIMIT ?",
		spaceID, uid, MemberStatusActive, RoleOwner,
		MemberStatusActive, RoleOwner, uid, limit,
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
