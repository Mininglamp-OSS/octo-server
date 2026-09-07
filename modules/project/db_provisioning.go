package project

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gocraft/dbr/v2"
)

// Data access for octo_project_provisioning — the transactional outbox that is
// also the project→container mapping (D12: one table, because the row that drives
// the retry is the row that records the outcome).
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
		// A duplicate key on uk_octo_project_provisioning_container is the one failure
		// whose MySQL message embeds the container id verbatim
		// ("Duplicate entry 'octows-...' for key ..."), and the caller logs this chain
		// with zap.Error — so passing it through would put a capability in a log line.
		// It needs a 128-bit collision to happen at all (every attempt mints a fresh id,
		// so a retry never reuses one), but "essentially unreachable" is not the same as
		// "cannot happen", and the zap-field guard cannot see a leak that arrives through
		// an error string. Replaced with a fixed summary; the row is identifiable from
		// project_id, which the caller already logs.
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

// markProvisioningDisbandPendingTx moves every non-terminal row of a project to
// disband_pending, inside the disband transaction.
//
// Called from disbandProjectTx rather than from the service layer on purpose: putting the
// transition in the DAO makes "any disband marks its containers reclaimable" structural,
// so a future disband path inherits it instead of having to remember. On this base that
// DAO function has exactly one caller (disbandProjectOnce) — the Space-removal cascade
// only closes seats and the ownerless case is a recorded, unresolved end state — so the
// claim is about where a future path would land, not about a junction that exists today.
//
// A related gap worth recording where someone will find it: no path disbands a project
// when its SPACE is disbanded, so a project in a dead Space keeps its `ready` rows and
// its containers are never marked reclaimable. D9's pull-based reclaim has no answer for
// that today; it belongs to the Space-cascade work, not to this slice.
//
// abandoned rows move too. They record "we never confirmed a container", but we may
// well have created one and failed to record it, so they are exactly as reclaimable
// as a ready row — and last_error survives, so the failure history is still
// readable. Leaving them behind would also pin the abandoned gauge above zero
// forever for a project that no longer exists, which trains the on-call to ignore
// the one signal that needs a human.
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
			// scan taking the first match over the (status, next_attempt_at,
			// lease_until) index; terminal rows are never deleted before their
			// retention expires and cluster at low ids, so the scan length would grow
			// with deployment age. FIFO is not a guarantee here anyway — SKIP LOCKED
			// already makes multi-replica pick order non-deterministic.
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
