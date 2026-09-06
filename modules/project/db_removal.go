package project

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Project-side member removal — the two-phase seat close (D4) and its outbox (D5).
//
// # Why removal is two-phase
//
// A project seat gates group admission (I2). If removal flipped status to 0 and
// then detached groups asynchronously, there would be a window in which
// octo_project_member says "not a member" while group_member rows for that uid
// still exist in the project's groups — I2 violated, by the removal itself,
// every time.
//
// So the order is inverted: `removing = 1` is set in the SAME transaction that
// begins the removal, `status` stays 1, and the worker flips status only after
// the groups are detached. Every authorization read treats removing = 1 as a
// non-member, so the seat stops granting anything immediately, while the rows
// that have not been cleaned up yet still belong to a member of record.
//
// The states, and what each means to a reader:
//
//	status=1 removing=0  — an ordinary active member
//	status=1 removing=1  — seat closing; NOT a member for any authorization
//	                       purpose; group rows may still exist
//	status=0 removing=0  — removed, cleanup finished
//	status=0 removing=1  — must not exist; the reconcile scan reports it

// removalJobStatus values for octo_project_member_removal_cleanup.status.
const (
	removalJobPending   = 0
	removalJobDone      = 1
	removalJobAbandoned = 2
	// removalJobCancelled marks a job retired by re-admission (D4). It is
	// distinct from done and from abandoned on purpose: neither of those says
	// "this work is no longer wanted", and deleting the row instead would leave
	// no evidence that a cascade was cancelled.
	removalJobCancelled = 3
)

// removalReason values. Low-cardinality; they end up in a metric label.
const (
	removalReasonKicked           = "kicked"
	removalReasonLeft             = "left"
	removalReasonProjectDisbanded = "project_disbanded"
)

// RemovalJob is one unit of project-side cleanup.
type RemovalJob struct {
	ID          int64     `db:"id"`
	ProjectID   string    `db:"project_id"`
	UID         string    `db:"uid"`
	SpaceID     string    `db:"space_id"`
	OperatorUID string    `db:"operator_uid"`
	Reason      string    `db:"reason"`
	Status      int       `db:"status"`
	Attempts    int       `db:"attempts"`
	CreatedAt   time.Time `db:"created_at"`
}

// beginMemberRemovalTx sets removing = 1 on an active seat and reports whether a
// row actually changed.
//
// The `status = active AND removing = 0` predicate is what makes the whole
// removal path idempotent: a second request for a uid already being removed
// affects zero rows, so the caller neither bumps member_epoch again nor enqueues
// a second job. P0 established that rule for deactivateMemberTx and nearly broke
// it; the same discipline applies here.
func (d *DB) beginMemberRemovalTx(tx *dbr.Tx, projectID, uid string, now time.Time) (bool, error) {
	res, err := tx.UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 1, updated_at = ? "+
			"WHERE project_id = ? AND uid = ? AND status = ? AND removing = 0",
		now, projectID, uid, MemberStatusActive,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: begin member removal: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: begin member removal affected rows: %w", err)
	}
	return affected > 0, nil
}

// finishMemberRemovalTx closes the seat for good: status 0, removing cleared.
//
// Guarded on removing = 1 so it can only ever complete a removal that this
// module started. A job whose member was re-admitted in the meantime finds
// removing = 0, affects zero rows, and reports false — which is how the worker
// learns its work was cancelled rather than by being told.
func (d *DB) finishMemberRemovalTx(tx *dbr.Tx, projectID, uid string, now time.Time) (bool, error) {
	res, err := tx.UpdateBySql(
		"UPDATE `octo_project_member` SET status = ?, removing = 0, updated_at = ? "+
			"WHERE project_id = ? AND uid = ? AND status = ? AND removing = 1",
		MemberStatusRemoved, now, projectID, uid, MemberStatusActive,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: finish member removal: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: finish member removal affected rows: %w", err)
	}
	return affected > 0, nil
}

// lockMemberForCascadeTx re-reads the member row under an exclusive lock.
//
// The worker calls this before EVERY batch, not once per job. The job was
// enqueued at removal time and may sit in the queue for minutes; a member
// re-added in that window has removing = 0, and continuing to tear their groups
// down would destroy a membership that is legitimate again. Same shape as P0's
// checkSpaceSeatForCleanupTx re-check inside deactivateSeatForCascade, and as
// cleanupSpaceMemberGroups's.
//
// A cancellation landing mid-batch leaves the member in the project but out of
// some of its groups. That is NOT an invariant violation — the subset relation
// still holds — it is visible in the member lists, and an admin can re-add. Say
// so here, or the next reader will "fix" it into something worse.
func (d *DB) lockMemberForCascadeTx(tx *dbr.Tx, projectID, uid string) (*MemberModel, error) {
	var rows []*MemberModel
	_, err := tx.SelectBySql(
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at "+
			"FROM `octo_project_member` WHERE project_id = ? AND uid = ? FOR UPDATE",
		projectID, uid,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: lock member for cascade: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// enqueueRemovalJobTx writes the outbox row in the same transaction that sets
// removing = 1, so a crash between the two cannot lose the cleanup.
//
// Deliberately NOT unique on (project_id, uid): remove → re-add → remove must
// enqueue a second job. The worker re-reads live state before acting, so a stale
// job resolves to a no-op rather than to damage.
func (d *DB) enqueueRemovalJobTx(tx *dbr.Tx, job RemovalJob, now time.Time) error {
	_, err := tx.InsertBySql(
		"INSERT INTO `octo_project_member_removal_cleanup` "+
			"(project_id, uid, space_id, operator_uid, reason, status, attempts, "+
			" next_attempt_at, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)",
		job.ProjectID, job.UID, job.SpaceID, job.OperatorUID, job.Reason,
		removalJobPending, now, now,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: enqueue removal job: %w", err)
	}
	return nil
}

// cancelPendingRemovalJobsTx retires every outstanding job for (project, uid).
//
// Called from the re-admission path in the transaction that clears `removing`.
// Without it the queue keeps rows the worker will pick up, re-read, find
// cancelled, and drop — burning a lease and an attempt each time, and leaving
// the operator unable to tell a cancelled cascade from a stalled one.
func (d *DB) cancelPendingRemovalJobsTx(tx *dbr.Tx, projectID, uid string, now time.Time) (int64, error) {
	res, err := tx.UpdateBySql(
		"UPDATE `octo_project_member_removal_cleanup` "+
			"SET status = ?, finished_at = ?, lease_owner = '', lease_until = NULL "+
			"WHERE project_id = ? AND uid = ? AND status = ?",
		removalJobCancelled, now, projectID, uid, removalJobPending,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: cancel removal jobs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: cancel removal jobs affected rows: %w", err)
	}
	return affected, nil
}

// claimRemovalJobs leases up to limit pending jobs for owner.
//
// Two statements rather than one because MySQL cannot UPDATE a table it selects
// from in a subquery; the SELECT takes FOR UPDATE SKIP LOCKED so two workers
// never contend for the same rows, which is the same shape
// space_member_removal_cleanup's worker uses.
func (d *DB) claimRemovalJobs(owner string, limit int, now time.Time, lease time.Duration) ([]RemovalJob, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: claim removal jobs begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// The claim reads the FULL row, not just the id, so there is no second
	// SELECT after the UPDATE.
	//
	// That is not a micro-optimization. A plain SELECT later in this transaction
	// would be a CONSISTENT read, and modules/project has a source guard —
	// TestNoWriteAuthorisingAggregateIsANonLockingRead — forbidding exactly that
	// shape after P0 shipped it and it cost a project with zero owners and a
	// bypassable member cap. Reading everything under the same FOR UPDATE SKIP
	// LOCKED removes the question instead of arguing about whether this
	// particular instance happens to be safe.
	//
	// SKIP LOCKED is what lets several pods poll the same queue without
	// contending: a row another worker already claimed is skipped, not waited on.
	var jobs []RemovalJob
	_, err = tx.SelectBySql(
		"SELECT id, project_id, uid, space_id, operator_uid, reason, status, attempts, created_at "+
			"FROM `octo_project_member_removal_cleanup` "+
			"WHERE status = ? AND next_attempt_at <= ? "+
			"  AND (lease_until IS NULL OR lease_until <= ?) "+
			"ORDER BY next_attempt_at ASC LIMIT ? FOR UPDATE SKIP LOCKED",
		removalJobPending, now, now, limit,
	).Load(&jobs)
	if err != nil {
		return nil, fmt.Errorf("project: claim removal jobs select: %w", err)
	}
	if len(jobs) == 0 {
		return nil, tx.Commit()
	}

	ids := make([]int64, 0, len(jobs))
	for i := range jobs {
		ids = append(ids, jobs[i].ID)
		// attempts is incremented by the UPDATE below; reflect it in what the
		// caller sees so the backoff and abandon decisions use the value that is
		// actually stored.
		jobs[i].Attempts++
	}

	_, err = tx.UpdateBySql(
		"UPDATE `octo_project_member_removal_cleanup` "+
			"SET lease_owner = ?, lease_until = ?, attempts = attempts + 1 "+
			"WHERE id IN ?",
		owner, now.Add(lease), ids,
	).Exec()
	if err != nil {
		return nil, fmt.Errorf("project: claim removal jobs lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: claim removal jobs commit: %w", err)
	}
	return jobs, nil
}

// heartbeatRemovalLease extends the lease of a job that is still being worked.
//
// #797's open P1 on the Space outbox is the absence of exactly this: without a
// heartbeat, the abandon sweep marks a still-running FINAL attempt as abandoned,
// and the work is then never retried because the row is terminal. A project
// removal fans out over every group in the project, so it is more likely to
// outrun a lease than the Space job that already does.
func (d *DB) heartbeatRemovalLease(id int64, owner string, until time.Time) error {
	_, err := d.session.UpdateBySql(
		"UPDATE `octo_project_member_removal_cleanup` SET lease_until = ? "+
			"WHERE id = ? AND lease_owner = ? AND status = ?",
		until, id, owner, removalJobPending,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: heartbeat removal lease: %w", err)
	}
	return nil
}

// completeRemovalJob marks a job terminal with the given status.
func (d *DB) completeRemovalJob(id int64, status int, lastErr string, now time.Time) error {
	if len(lastErr) > 255 {
		lastErr = lastErr[:255]
	}
	_, err := d.session.UpdateBySql(
		"UPDATE `octo_project_member_removal_cleanup` "+
			"SET status = ?, finished_at = ?, last_error = ?, lease_owner = '', lease_until = NULL "+
			"WHERE id = ?",
		status, now, lastErr, id,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: complete removal job: %w", err)
	}
	return nil
}

// rescheduleRemovalJob puts a failed job back with backoff and releases the lease.
func (d *DB) rescheduleRemovalJob(id int64, nextAttempt time.Time, lastErr string) error {
	if len(lastErr) > 255 {
		lastErr = lastErr[:255]
	}
	_, err := d.session.UpdateBySql(
		"UPDATE `octo_project_member_removal_cleanup` "+
			"SET next_attempt_at = ?, last_error = ?, lease_owner = '', lease_until = NULL "+
			"WHERE id = ?",
		nextAttempt, lastErr, id,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: reschedule removal job: %w", err)
	}
	return nil
}

// purgeFinishedRemovalJobs deletes terminal rows older than the retention
// window, in bounded batches, and reports how many it deleted.
//
// The caller DRAINS by calling until this returns 0, rather than deleting a
// fixed limit per tick. #797 records why: the Space outbox purges 1000 rows an
// hour, which is 24k a day and below realistic churn, so the table grows
// forever and the purge itself gets slower as it does.
func (d *DB) purgeFinishedRemovalJobs(before time.Time, batch int) (int64, error) {
	res, err := d.session.DeleteBySql(
		"DELETE FROM `octo_project_member_removal_cleanup` "+
			"WHERE status <> ? AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?",
		removalJobPending, before, batch,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: purge removal jobs: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: purge removal jobs affected rows: %w", err)
	}
	return affected, nil
}

// countPendingRemovalJobs powers the backlog gauge.
func (d *DB) countPendingRemovalJobs() (int, error) {
	var n int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member_removal_cleanup` WHERE status = ?",
		removalJobPending,
	).LoadOne(&n)
	if err != nil {
		return 0, fmt.Errorf("project: count pending removal jobs: %w", err)
	}
	return n, nil
}
