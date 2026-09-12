package project

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gocraft/dbr/v2"
)

// Data access for octo_project_provisioning — the transactional outbox and
// local Project-to-target accounting. Fleet uses container_id as its remote
// idempotency key; Drive maps project_id to its own remote space and never
// treats this local container_id as a Drive id or URL.
//
// Two conventions carried over from db.go, both silent when wrong:
//
//  1. Every time value is Go's UTC clock. Never CURRENT_TIMESTAMP(3) and never
//     NOW(): next_attempt_at is COMPARED against a Go time at claim time, so a
//     MySQL session timezone on the write side and a Go UTC clock on the read side
//     would make new rows unclaimable for a whole timezone offset. modules/space
//     shipped a metric broken exactly that way.
//  2. dbr backtick asymmetry — Update / InsertInto / DeleteFrom take the bare table
//     name, From / Select need manual backticks. Everything here is raw *BySql, so
//     the backticks are written out.

// enqueueProvisioningTx writes one outbox row per enabled target, inside the
// caller's transaction.
//
// This is the whole point of Shape S: enqueueing in the SAME transaction as the
// project INSERT is the only construction under which "project exists ⟹ job
// exists" is true. internal/cardactiondispatch's Redis queue cannot do it — a
// different store means an enqueue can be lost after the commit or orphaned before
// it.
//
// Fail-closed. An enqueue error aborts the create. The alternative (commit the
// project, log the enqueue failure) produces a project whose containers nothing
// will ever provision and nothing will ever notice, which is the failure mode the
// outbox exists to remove.
//
// LOCK ORDER: this INSERT is the LAST statement of the create transaction, after
// octo_project_member. The disband path also touches this table last (see
// markProvisioningDisbandPendingTx, called from disbandProjectTx after both member
// and project writes), and the worker's claim transaction touches ONLY this table.
// So every transaction that holds a lock here acquires it after everything else,
// and no cycle is introduced into the order db.go documents
// (space_member -> space -> project -> ... -> octo_project_member ->
// octo_project_provisioning).
func (d *DB) enqueueProvisioningTx(tx *dbr.Tx, projectID, spaceID string, targets []provisionTarget, now time.Time) error {
	if len(targets) == 0 {
		return nil
	}
	if projectID == "" || spaceID == "" {
		return errors.New("project: provisioning enqueue requires project_id and space_id")
	}
	placeholders := make([]string, 0, len(targets))
	args := make([]interface{}, 0, len(targets)*7)
	for _, target := range targets {
		containerID, err := newContainerID(target.Name)
		if err != nil {
			// A container id we could not randomize must never be written; see
			// newContainerID for why there is no fallback.
			return errors.Join(errProvisioningEnqueueFailed, err)
		}
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			projectID, spaceID, target.Name, containerID,
			provisionStatusPending, now, now)
	}
	sql := "INSERT INTO `octo_project_provisioning` " +
		"(project_id, space_id, target, container_id, status, next_attempt_at, created_at) VALUES " +
		strings.Join(placeholders, ",")
	if _, err := tx.InsertBySql(sql, args...).Exec(); err != nil {
		// A duplicate key on the local uk_octo_project_provisioning_container
		// index is the one failure whose MySQL message embeds container_id.
		// That value is a Fleet wire key; Drive keeps the column only as an
		// opaque internal task key and its request uses project_id instead.
		// The caller logs this chain, so replace database text with a fixed
		// summary rather than allowing a capability to reach an error string.
		if isDuplicateKeyErr(err) {
			return errors.Join(errProvisioningEnqueueFailed,
				errors.New("project: provisioning row already exists for this project and target"))
		}
		// errors.Join, not fmt.Errorf: the caller classifies on
		// errProvisioningEnqueueFailed for the metric label while the underlying cause
		// stays in the chain for the log line. errors.As still reaches the
		// *mysql.MySQLError underneath, so retryOnLockConflict keeps retrying 1205/1213.
		return errors.Join(errProvisioningEnqueueFailed, err)
	}
	return nil
}

// provisioningProject is the current authoritative identity needed by Drive.
// The outbox row intentionally carries only the enqueue-time SpaceID; a Drive
// retry must re-read the Project name and active human Owner instead of trusting
// stale creator data or a client-supplied UID.
type provisioningProject struct {
	ProjectID string `db:"project_id"`
	SpaceID   string `db:"space_id"`
	Name      string `db:"name"`
	OwnerUID  string `db:"owner_uid"`
}

// queryProvisioningProject returns the active Project and its current human
// Owner for a Drive delivery. A missing row means the Project is disbanded,
// its Space seat is no longer active, or its Owner is no longer a valid human;
// callers must not send an internal create request in that case.
func (d *DB) queryProvisioningProject(projectID string) (*provisioningProject, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, nil
	}
	var rows []provisioningProject
	_, err := d.session.SelectBySql(
		"SELECT p.project_id, p.space_id, p.name, pm.uid AS owner_uid "+
			"FROM `octo_project` p "+
			"INNER JOIN `octo_project_member` pm "+
			"ON pm.project_id = p.project_id AND pm.space_id = p.space_id "+
			"INNER JOIN `space_member` sm "+
			"ON sm.space_id = p.space_id AND sm.uid = pm.uid "+
			"INNER JOIN `user` u ON u.uid = pm.uid "+
			"WHERE p.project_id = ? AND p.status = ? "+
			"AND pm.status = ? AND pm.removing = 0 AND pm.role = ? "+
			"AND sm.status = 1 AND u.robot = 0 AND u.status = 1 "+
			"AND COALESCE(u.is_destroy, 0) <> 2 LIMIT 1",
		projectID, StatusNormal, MemberStatusActive, RoleOwner,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query provisioning project: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// markProvisioningDisbandPendingTx moves every non-terminal row of a project to
// disband_pending, inside the disband transaction.
//
// Called from disbandProjectTx rather than from the service layer on purpose:
// putting the transition in the DAO makes "any project disband marks its
// containers reclaimable" structural. On this base the DAO has exactly one
// caller (disbandProjectOnce). A Space-removal cascade preserves the active
// human Owner and closes non-Owner seats/riders; it does not disband projects
// or transition their provisioning rows. Any future Space-level project-disband
// path must call this DAO explicitly.
//
// Abandoned rows move too. They record "we never confirmed a container", but
// we may have created one and failed to record it, so they are exactly as
// reclaimable as a ready row. last_error survives, so the failure history stays
// readable.
func (d *DB) markProvisioningDisbandPendingTx(tx *dbr.Tx, projectID string, now time.Time) error {
	if projectID == "" {
		return errors.New("project: provisioning disband requires project_id")
	}
	if _, err := tx.UpdateBySql(
		"UPDATE `octo_project_provisioning` "+
			"SET status = ?, finished_at = ?, lease_owner = '', lease_until = NULL "+
			"WHERE project_id = ? AND status <> ?",
		provisionStatusDisbandPending, now, projectID, provisionStatusDisbandPending,
	).Exec(); err != nil {
		return fmt.Errorf("project: mark provisioning disband pending: %w", err)
	}
	return nil
}

// claimProvisioningJob claims one due, unleased row under a lease.
//
// SKIP LOCKED lets replicas advance in parallel without blocking each other; the
// lease makes a claimed row exclusively one executor's for its duration. Returns
// (nil, nil) when there is nothing to do.
//
// owner MUST be unique per claim (see newProvisioningClaimOwner). A process-level
// constant would make the `AND lease_owner = ?` guard true for two goroutines in
// the same process at once, and the lease would be decorative.
//
// The `target IN ?` filter is what makes turning a target OFF non-destructive: rows
// already enqueued for it stop being claimed instead of burning their retry budget
// against a destination nobody is listening on, so they survive the switch being
// flipped back. They stay visible as `pending` in provisioning_rows, which is the
// honest reading of "a target has work and is disabled".
//
// now must be UTC, the same clock the write side uses.
func (d *DB) claimProvisioningJob(owner string, targets []string, maxAttempts uint32, now time.Time) (*provisioningJob, error) {
	if owner == "" {
		return nil, errors.New("project: provisioning claim owner required")
	}
	if len(targets) == 0 {
		return nil, nil
	}
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin provisioning claim: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var job provisioningJob
	err = tx.SelectBySql(
		"SELECT id, project_id, space_id, target, container_id, attempts "+
			"FROM `octo_project_provisioning` "+
			"WHERE status = ? AND target IN ? AND attempts < ? AND next_attempt_at <= ? "+
			"AND (lease_until IS NULL OR lease_until <= ?) "+
			// No ORDER BY id, deliberately. With it the optimizer prefers a PRIMARY
			// scan taking the first match over idx_octo_project_provisioning_pending
			// (target, status, next_attempt_at, lease_until); terminal rows are never
			// deleted before their retention expires and cluster at low ids, so the scan
			// length would grow with deployment age. FIFO is not a guarantee here anyway —
			// SKIP LOCKED already makes multi-replica pick order non-deterministic.
			"LIMIT 1 FOR UPDATE SKIP LOCKED",
		provisionStatusPending, targets, maxAttempts, now, now,
	).LoadOne(&job)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("project: select provisioning job: %w", err)
	}

	// attempts is incremented AT CLAIM, and the `attempts < max` bound is enforced
	// AT CLAIM too. Both because the release path only runs when a job finished and
	// reported an error: a pod killed by OOM or eviction mid-job never reaches it,
	// so the row stays pending with attempts unchanged. Without the claim-side
	// bound, a job that reliably kills the process gets re-claimed forever, and each
	// re-claim also consumes one slot of the batch and pushes a healthy row out of
	// that tick. With it, such a row stops being claimable — which is why
	// abandonExhaustedProvisioningJobs then has to exist to push it to a terminal
	// state instead of leaving a zombie that is neither retried nor reported.
	result, err := tx.UpdateBySql(
		"UPDATE `octo_project_provisioning` SET lease_owner = ?, lease_until = ?, attempts = attempts + 1 "+
			"WHERE id = ? AND status = ?",
		owner, now.Add(provisioningLease), job.ID, provisionStatusPending,
	).Exec()
	if err != nil {
		return nil, fmt.Errorf("project: claim provisioning job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, errors.New("project: invalid provisioning claim result")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit provisioning claim: %w", err)
	}
	// The SELECT read the pre-increment value; align it so the caller's "how many
	// tries so far" test does not have to know about the offset.
	job.Attempts++
	return &job, nil
}

// finishProvisioningJob writes a terminal status, requiring the lease. Returns
// false when the lease has changed hands, which the caller treats as "another
// replica took over" rather than as an error.
//
// attempts is not touched: it was counted at claim, so an abandoned row's attempts
// equals the configured maximum and ops can select on that threshold directly.
func (d *DB) finishProvisioningJob(id uint64, owner string, status uint8, note string, now time.Time) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE `octo_project_provisioning` "+
			"SET status = ?, finished_at = ?, lease_owner = '', lease_until = NULL, last_error = ? "+
			"WHERE id = ? AND status = ? AND lease_owner = ?",
		status, now, truncateProvisioningError(note), id, provisionStatusPending, owner,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: finish provisioning job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: read provisioning finish result: %w", err)
	}
	return affected == 1, nil
}

// releaseProvisioningJob records a failure and schedules the retry.
//
// attempts is NOT incremented here — the claim already did it. Incrementing in both
// places doubles the count, which halves both the backoff schedule and the
// give-up threshold.
func (d *DB) releaseProvisioningJob(id uint64, owner string, attempts uint32, lastError string, now time.Time) error {
	next := now.Add(provisioningRetryDelay(attempts))
	result, err := d.session.UpdateBySql(
		"UPDATE `octo_project_provisioning` "+
			"SET next_attempt_at = ?, lease_owner = '', lease_until = NULL, last_error = ? "+
			"WHERE id = ? AND status = ? AND lease_owner = ?",
		next, truncateProvisioningError(lastError), id, provisionStatusPending, owner,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: release provisioning job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errProvisioningLeaseLost
	}
	return nil
}

// abandonExhaustedProvisioningJobs pushes rows that used up their budget and whose
// lease has long expired into `abandoned`, returning how many moved.
//
// This is the out-of-process complement to the release path's own transition: that
// one only runs when a job finished and returned an error, so a hard-killed pod
// leaves the row pending forever — and the claim-side attempts bound means nothing
// will pick it up again.
//
// The grace period is a FULL lease, not merely "the lease has expired". A job on its
// final attempt has attempts == max while its lease is still legitimately held, and
// judging on attempts alone would write a terminal state under a running executor;
// its own finish would then land on nothing and leave only a misleading
// "lease changed hands" line.
//
// The lease predicate is `IS NULL OR <= grace`, and the NULL arm is the fix for a real
// zombie rather than defensive breadth. releaseProvisioningJob NULLs lease_until on every
// retry, so the resting state of a retrying row is (pending, attempts = k, lease_until
// NULL). Require IS NOT NULL here and that row is unsweepable; claiming needs
// attempts < max, so if max is ever observed as m <= k the row is unclaimable too — it
// then sits pending forever with no retry, no terminal state, no gauge and no log, and
// the only visible symptom is a rising `pending` count that the runbook tells the
// on-call means "the target is not responding". Reachable by lowering
// OCTO_PROJECT_PROVISION_MAX_ATTEMPTS during an incident, and (before parseMaxAttempts
// existed) by an out-of-range value casting to uint32(0). Exactly the state the
// claim-side bound and this sweep are documented to prevent.
//
// The NULL arm is safe only because max >= 1 is now GUARANTEED at config load
// (parseMaxAttempts): a never-claimed row has attempts = 0, so `attempts >= max`
// excludes it. With max = 0 this sweep would abandon the whole table on its first tick,
// so that validation is a prerequisite of this predicate, not an unrelated tidy-up.
//
// A NULL lease also means nobody is executing the row: the claim writes lease_until in
// the same committed transaction that takes the row, and release/finish only NULL it
// once the job is done. So there is no executor to write underneath.
//
// Two statements, not one UPDATE ... WHERE: `attempts` is in no index, so the only
// available access path is `status`, and under REPEATABLE READ a single UPDATE would
// take next-key locks over the whole status = pending range. LIMIT bounds rows
// CHANGED, not rows LOCKED. modules/space measured the consequence of the one-statement
// form: a concurrent, non-conflicting enqueue INSERT hit ERROR 1205, and enqueue is
// inside the fail-closed create transaction — so a backlog made project creation fail
// at random. A non-locking SELECT plus a primary-key UPDATE locks only the named rows.
//
// No extra locking is needed against the claim path WITHIN one replica: claiming requires
// attempts < max and this requires attempts >= max, so the two predicates cannot select
// the same row. Across replicas mid-rollout they can disagree about max, which is why the
// UPDATE below re-checks the predicates instead of trusting the SELECT.
//
// targets carries the SAME `target IN ?` filter as claimProvisioningJob, for the same
// reason: turning a target off has to be non-destructive. Without it, narrowing
// OCTO_PROJECT_PROVISION_TARGETS to fleet still let this sweep drive drive's parked rows
// to terminal `abandoned` — from which there is no automatic re-drive — so flipping the
// switch back would NOT resume them, contradicting the promise the rollback runbook makes.
// Parked rows stay `pending` and stay visible in provisioning_rows instead.
func (d *DB) abandonExhaustedProvisioningJobs(targets []string, maxAttempts uint32, now time.Time, limit int) (int64, error) {
	if limit <= 0 || len(targets) == 0 {
		return 0, nil
	}
	var ids []uint64
	if _, err := d.session.SelectBySql(
		"SELECT id FROM `octo_project_provisioning` "+
			"WHERE target IN ? AND status = ? AND attempts >= ? "+
			"AND (lease_until IS NULL OR lease_until <= ?) "+
			"LIMIT ?",
		targets, provisionStatusPending, maxAttempts, now.Add(-provisioningLease), limit,
	).Load(&ids); err != nil {
		return 0, fmt.Errorf("project: select exhausted provisioning jobs: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// The UPDATE re-checks EVERY predicate the SELECT used, not just id and status.
	//
	// Dropping them looked safe under the argument that claiming (attempts < max) and
	// sweeping (attempts >= max) cannot select the same row — but that holds only while
	// every replica agrees on max, and during a rolling change of
	// OCTO_PROJECT_PROVISION_MAX_ATTEMPTS they do not. An old replica (max=5) selects an
	// expired 5-attempt row; before its UPDATE lands, a new replica (max=12) claims it
	// and starts the outbound call; the old UPDATE then matches on id + status alone,
	// clears the fresh lease and writes `abandoned` while the call is in flight.
	// Re-checking is one clause and removes the whole class.
	// last_error is APPENDED to, not overwritten.
	//
	// Overwriting it with a constant destroyed the only durable per-row evidence of why
	// provisioning failed — and it did so exactly on the path this sweep exists for, where
	// the executing pod was killed and there is no log line to fall back on either. The
	// sweep's own note matters (it says "nobody released this row", which is different from
	// a normal give-up), so both are kept: the row keeps whatever the last release recorded
	// and gains the sweep marker. CONCAT is bounded by LEFT(...) at the column width, so a
	// long history cannot make the UPDATE fail on a strict-mode length error — which would
	// leave the row pending and unsweepable, reintroducing the zombie one layer up.
	result, err := d.session.UpdateBySql(
		"UPDATE `octo_project_provisioning` "+
			"SET status = ?, finished_at = ?, lease_owner = '', lease_until = NULL, "+
			"    last_error = LEFT(CONCAT(IF(last_error = '', '', CONCAT(last_error, ' | ')), ?), 255) "+
			"WHERE id IN ? AND target IN ? AND status = ? AND attempts >= ? "+
			"AND (lease_until IS NULL OR lease_until <= ?)",
		provisionStatusAbandoned, now, "sweep: no executor released this row", ids, targets,
		provisionStatusPending, maxAttempts, now.Add(-provisioningLease),
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: sweep exhausted provisioning jobs: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: read provisioning sweep result: %w", err)
	}
	return affected, nil
}

// requeueAbandonedProvisioningJobs moves `abandoned` rows back to `pending` so the
// worker retries them. This is the manual re-drive the module has always said it
// needed and never had: without it, one project whose ensure call exhausted its
// retry budget is permanently invisible to the peer, with no recovery path.
//
// Scoped to ONE project on purpose, and there is no all-projects mode. Exhaustion
// usually means the peer was genuinely down, so a blanket re-drive of every parked
// row would re-issue the same doomed calls and walk the whole backlog to
// `abandoned` a second time — the operator has to name what they believe is fixed.
//
// attempts is RESET to 0, which is what makes the retry budget mean "attempts since
// someone decided to retry" rather than "attempts ever". Leaving it at max would
// have the sweep re-abandon the row on its next tick without a single outbound call.
//
// container_id is deliberately NOT reissued. Fleet retries need the same remote
// idempotency key, while Drive keeps this local opaque task key unchanged for
// row identity and uses project_id for remote duplicate detection. A fresh
// local key on either path would make the prior attempt's result unaccounted.
//
// last_error is APPENDED to, matching abandonExhaustedProvisioningJobs: while the row
// is parked, the reason provisioning gave up is the operator's durable evidence, and
// the requeue marker distinguishes "gave up once" from "gave up, was retried, gave up
// again". LEFT(...) bounds it at the column width for the same reason as there.
//
// Scope of that claim, precisely: it holds while the row is pending or abandoned, NOT
// after a successful re-drive. finishProvisioningJob and releaseProvisioningJob both
// REPLACE last_error (pre-existing behaviour), and the success path passes "" — so a
// rescued row that reaches `ready` carries no trace of the rescue. That is the right
// default for a column whose job is "why is this row not done", and the audit question
// ("was this project ever rescued") is not one a mutable status column can answer;
// the boot log line is. Recorded here because the appended history looks durable and
// is not.
func (d *DB) requeueAbandonedProvisioningJobs(projectID string, targets []string, now time.Time) (int64, error) {
	if strings.TrimSpace(projectID) == "" || len(targets) == 0 {
		return 0, nil
	}
	// SELECT then primary-key UPDATE, not a ranged UPDATE: the same ERROR 1205 that
	// abandonExhaustedProvisioningJobs records — a ranged UPDATE here takes gap locks
	// that collide with the enqueue INSERT inside the fail-closed create transaction,
	// i.e. a backlog would make project creation fail at random.
	var ids []uint64
	if _, err := d.session.SelectBySql(
		"SELECT id FROM `octo_project_provisioning` "+
			"WHERE project_id = ? AND target IN ? AND status = ?",
		projectID, targets, provisionStatusAbandoned,
	).Load(&ids); err != nil {
		return 0, fmt.Errorf("project: select abandoned provisioning jobs: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// The UPDATE re-checks status, so two concurrent requeues (two pods reading the same
	// env value) cannot both move the same row: the second matches nothing. That is what
	// makes this safe to run more than once rather than merely unlikely to be.
	//
	// It re-checks ONLY status, where abandonExhaustedProvisioningJobs deliberately
	// re-checks every predicate its SELECT used. The asymmetry is intentional, not an
	// oversight: that function's extra predicates guard against replicas disagreeing about
	// max_attempts mid-rollout, i.e. against a value that CHANGES under the row. Here the
	// dropped predicates are project_id and target, which no statement in this package
	// ever updates — uk_octo_project_provisioning_target makes them the row's identity, so
	// re-checking them could not fail. status is the only column another actor can move.
	result, err := d.session.UpdateBySql(
		"UPDATE `octo_project_provisioning` "+
			"SET status = ?, attempts = 0, next_attempt_at = ?, "+
			"    lease_owner = '', lease_until = NULL, finished_at = NULL, "+
			"    last_error = LEFT(CONCAT(IF(last_error = '', '', CONCAT(last_error, ' | ')), ?), 255) "+
			"WHERE id IN ? AND status = ?",
		provisionStatusPending, now, "requeued by operator", ids, provisionStatusAbandoned,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: requeue abandoned provisioning jobs: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: read provisioning requeue result: %w", err)
	}
	return affected, nil
}

// provisioningRetention is how long a disband_pending row is kept before purge.
//
// Only disband_pending is ever purged. `ready` rows are kept indefinitely on
// purpose: they are what makes "which projects were ever provisioned into drive"
// answerable locally, which is what reclaim accounting needs (D12). `abandoned` rows
// are kept because they are the only durable record that provisioning gave up.
//
// Long, because it is a reclaim window: the subsystem side learns about a disband by
// POLLING POST /v1/internal/projects/status, so deleting our row before every
// consumer has swept would drop the local accounting for a container that still
// exists on the other side.
const provisioningRetention = 90 * 24 * time.Hour

// purgeFinishedProvisioningJobs deletes disband_pending rows past the retention
// window, bounded per call so one DELETE cannot lock a large range.
//
// targets is the set whose reclaim consumer the operator has DECLARED LIVE
// (ProvisioningConfig.ReclaimTargets), and the `target IN ?` predicate is load-bearing,
// not a filter for tidiness. This is the only irreversible statement in the slice, and
// deletion authority is per subsystem: fleet's consumer shipping says nothing about
// drive's, and without this clause the first subsystem to ship one would silently license
// deleting the other's reclaim records — after which its containers can never be reclaimed
// (the D9 answer becomes permanently `unknown`, indistinguishable from "outside your
// grant"). Empty set means delete nothing, which is the default state.
//
// idx_octo_project_provisioning_finished leads with (target, status) so this DELETE is an
// index range and not a scan that examines the other target's rows. That column order is
// not for this statement's speed — the eligible set is tiny — but because the same missing
// leading `target` made the CLAIM path starve one target: see the index's own comment in
// the migration, and TestBothTargetsAdvanceInTheSameTick.
func (d *DB) purgeFinishedProvisioningJobs(targets []string, before time.Time, limit int) (int64, error) {
	if limit <= 0 || len(targets) == 0 {
		return 0, nil
	}
	// DeleteBySql, not UpdateBySql. modules/space's equivalent sends its DELETE through
	// UpdateBySql and it works, but dbr has the right builder and this repo already uses
	// it (modules/conversation_ext) — a DELETE spelled as an update is a thing every
	// future reader has to stop and re-read.
	result, err := d.session.DeleteBySql(
		"DELETE FROM `octo_project_provisioning` "+
			"WHERE target IN ? AND status = ? AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?",
		targets, provisionStatusDisbandPending, before, limit,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: purge provisioning jobs: %w", err)
	}
	return result.RowsAffected()
}

// provisioningCount is one (target, status) bucket.
type provisioningCount struct {
	Target string `db:"target"`
	Status uint8  `db:"status"`
	Rows   int64  `db:"rows"`
}

// countProvisioningByTargetStatus aggregates the table for the gauges.
//
// A whole-table GROUP BY, which is why it runs on the sparse metrics tick rather than the
// claim tick. What is bounded by design is the OUTPUT — two targets times four statuses,
// so at most eight rows and eight label combinations — and not the rows examined: only
// disband_pending is ever purged, so the table grows with every project ever created.
func (d *DB) countProvisioningByTargetStatus() ([]provisioningCount, error) {
	var rows []provisioningCount
	if _, err := d.session.SelectBySql(
		"SELECT target, status, COUNT(*) AS `rows` FROM `octo_project_provisioning` GROUP BY target, status",
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: count provisioning rows: %w", err)
	}
	return rows, nil
}

// truncateProvisioningError makes a failure summary safe for last_error.
//
// Both steps are required. Any invalid byte in a utf8mb4 column makes strict mode
// reject the entire UPDATE, so attempts would not advance and next_attempt_at would
// not move: the function meant to protect the retry would break the backoff, and the
// row would be re-claimed every lease period and never reach a terminal state.
//
//  1. Sanitize the whole string. An error can carry invalid bytes anywhere in it (a
//     proxy's error page, a raw response fragment), so repairing only a truncated
//     tail is not enough.
//  2. Then cut on a rune boundary. Cutting at a byte offset can leave 1-3 trailing
//     bytes of a 4-byte rune, and DecodeLastRuneInString reports (RuneError, 1) for
//     each dangling byte — so a single one-byte trim still leaves half a rune. The
//     loop runs at most three times because the input is already valid.
//
// The column is 255 CHARACTERS and this cuts at 255 BYTES, so it can never overflow.
func truncateProvisioningError(s string) string {
	const max = 255
	cleaned := strings.ToValidUTF8(s, "")
	if len(cleaned) <= max {
		return cleaned
	}
	truncated := cleaned[:max]
	for len(truncated) > 0 {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError || size > 1 { // U+FFFD itself is valid (size 3) and must not be trimmed
			break
		}
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}
