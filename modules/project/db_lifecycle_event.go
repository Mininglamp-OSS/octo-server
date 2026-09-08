package project

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// SQL for the project lifecycle outbox (O4).
//
// Every statement here mirrors its counterpart in db_removal.go, including the
// claim shape. Where the two differ, the difference is commented — the point of
// copying a reviewed pattern is that a reader can diff them.

// lifecycleEventInsertColumns is the write-side allow-list. status / attempts /
// lease_owner take their DDL defaults; last_error and finished_at are written
// only by the worker.
var lifecycleEventInsertColumns = []string{
	"event_id", "event_type", "project_id", "space_id",
	"project_version", "payload", "occurred_at",
	"next_attempt_at", "created_at",
}

// insertLifecycleEventTx writes one outbox row inside the caller's transaction.
//
// next_attempt_at is `now`, not a delay: the row becomes claimable the instant
// the business transaction commits, and until then no worker can see it because
// it is not committed. That is the outbox invariant in one line.
func (d *DB) insertLifecycleEventTx(tx *dbr.Tx, row lifecycleEventRow, now time.Time) error {
	_, err := tx.InsertInto("octo_project_lifecycle_event").
		Columns(lifecycleEventInsertColumns...).
		Values(row.EventID, row.EventType, row.ProjectID, row.SpaceID,
			row.ProjectVersion, string(row.Payload), row.OccurredAt,
			now, now).
		Exec()
	if err != nil {
		return fmt.Errorf("project: insert lifecycle event: %w", err)
	}
	return nil
}

// claimLifecycleEvents leases a batch of due rows.
//
// Same shape as claimRemovalJobs and for the same reasons: the SELECT reads the
// FULL row under FOR UPDATE SKIP LOCKED, so there is no second non-locking read
// afterwards (this module has a source guard against that shape), and SKIP
// LOCKED lets several pods poll one queue without contending.
//
// ORDER BY next_attempt_at, id — the id tiebreak is not cosmetic. Events for one
// project must be delivered in the order they were written, and many are
// enqueued in the same millisecond by the same transaction; without the
// tiebreak, two events sharing a timestamp could be claimed in either order, and
// the consumer would receive a rename after the archive that superseded it.
func (d *DB) claimLifecycleEvents(owner string, limit int, now time.Time, lease time.Duration) ([]lifecycleEventRow, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: claim lifecycle events begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var rows []lifecycleEventRow
	_, err = tx.SelectBySql(
		"SELECT id, event_id, event_type, project_id, space_id, project_version, "+
			"payload, occurred_at, attempts "+
			"FROM `octo_project_lifecycle_event` "+
			"WHERE status = ? AND next_attempt_at <= ? "+
			"  AND (lease_until IS NULL OR lease_until <= ?) "+
			"ORDER BY next_attempt_at ASC, id ASC LIMIT ? FOR UPDATE SKIP LOCKED",
		lifecycleEventPending, now, now, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: claim lifecycle events select: %w", err)
	}
	if len(rows) == 0 {
		return nil, tx.Commit()
	}

	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
		// attempts is incremented by the UPDATE below; reflect it here so the
		// backoff and abandon decisions use the value that is actually stored.
		rows[i].Attempts++
	}
	if _, err = tx.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` "+
			"SET lease_owner = ?, lease_until = ?, attempts = attempts + 1 WHERE id IN ?",
		owner, now.Add(lease), ids,
	).Exec(); err != nil {
		return nil, fmt.Errorf("project: claim lifecycle events lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: claim lifecycle events commit: %w", err)
	}
	return rows, nil
}

// heartbeatLifecycleEventLeases extends the lease of every row in one claimed batch.
//
// The whole batch, not just the in-flight row: one claim leases up to a batch at
// once and the worker sends them in sequence, so heartbeating only the current
// one lets the tail expire while this worker still intends to send it. Another
// pod then claims those rows and delivers the same events concurrently. The
// consumer would deduplicate on event_id, but the two workers would race each
// other for the same lease on completion and one would log a false ownership
// loss on every batch.
func (d *DB) heartbeatLifecycleEventLeases(ids []int64, owner string, until time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.session.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` SET lease_until = ? "+
			"WHERE id IN ? AND lease_owner = ? AND status = ?",
		until, ids, owner, lifecycleEventPending,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: heartbeat lifecycle event leases: %w", err)
	}
	return nil
}

// completeLifecycleEvent marks a row terminal, and reports whether THIS worker still
// held the lease.
//
// The lease_owner predicate is what makes a lost lease visible instead of
// silent: a worker that stalled past its lease, had its rows re-claimed, and
// then finished anyway must not overwrite the new owner's result. A false return
// means the write was refused, and the caller says so rather than logging a
// completion that did not happen.
func (d *DB) completeLifecycleEvent(id int64, owner string, status int, lastErr string, now time.Time) (bool, error) {
	res, err := d.session.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` "+
			"SET status = ?, finished_at = ?, last_error = ?, lease_owner = '', lease_until = NULL "+
			"WHERE id = ? AND lease_owner = ? AND status = ?",
		status, now, truncateError(lastErr), id, owner, lifecycleEventPending,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: complete lifecycle event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: complete lifecycle event rows: %w", err)
	}
	return affected > 0, nil
}

// rescheduleLifecycleEvent returns a row to the queue with a later attempt time.
func (d *DB) rescheduleLifecycleEvent(id int64, owner string, nextAttempt time.Time, lastErr string) (bool, error) {
	res, err := d.session.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` "+
			"SET next_attempt_at = ?, last_error = ?, lease_owner = '', lease_until = NULL "+
			"WHERE id = ? AND lease_owner = ? AND status = ?",
		nextAttempt, truncateError(lastErr), id, owner, lifecycleEventPending,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: reschedule lifecycle event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: reschedule lifecycle event rows: %w", err)
	}
	return affected > 0, nil
}

// countPendingLifecycleEvents backs the backlog gauge.
func (d *DB) countPendingLifecycleEvents() (int, error) {
	var n int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_lifecycle_event` WHERE status = ?", lifecycleEventPending,
	).LoadOne(&n)
	if err != nil {
		return 0, fmt.Errorf("project: count pending lifecycle events: %w", err)
	}
	return n, nil
}

// oldestPendingLifecycleEventAge backs the staleness gauge, which is the one an
// alert should actually fire on.
//
// A backlog COUNT cannot distinguish a healthy burst from a stuck queue — both
// look like "many rows". Age can: an undelivered revocation that is an hour old
// is a member still executing after being removed, regardless of how many
// siblings it has.
//
// The age is computed in Go against a UTC column rather than with MySQL
// TIMESTAMPDIFF, because this repository has already shipped a gauge that
// compared a Go UTC clock against a session-timezone column and read minus
// 28799 seconds.
func (d *DB) oldestPendingLifecycleEventAge(now time.Time) (time.Duration, error) {
	var oldest []time.Time
	_, err := d.session.SelectBySql(
		"SELECT created_at FROM `octo_project_lifecycle_event` "+
			"WHERE status = ? ORDER BY created_at ASC LIMIT 1", lifecycleEventPending,
	).Load(&oldest)
	if err != nil {
		return 0, fmt.Errorf("project: oldest pending lifecycle event: %w", err)
	}
	if len(oldest) == 0 {
		return 0, nil
	}
	age := now.UTC().Sub(oldest[0].UTC())
	if age < 0 {
		// Clock skew between pods, not a negative age. Report zero rather than a
		// nonsense negative on a dashboard.
		return 0, nil
	}
	return age, nil
}

// purgeFinishedLifecycleEvents drains terminal rows past the retention window.
//
// Bounded per statement and DRAINED by the caller looping, rather than a fixed
// number per tick: a fixed cap below the arrival rate is not a slower purge, it
// is no purge, and the table grows without bound.
func (d *DB) purgeFinishedLifecycleEvents(before time.Time, batch int) (int64, error) {
	res, err := d.session.DeleteBySql(
		"DELETE FROM `octo_project_lifecycle_event` "+
			"WHERE status <> ? AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?",
		lifecycleEventPending, before, batch,
	).Exec()
	if err != nil {
		return 0, fmt.Errorf("project: purge lifecycle events: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("project: purge lifecycle events rows: %w", err)
	}
	return affected, nil
}

// truncateError bounds what reaches last_error.
//
// The column is VARCHAR(255) and the value is written from a failure path that
// can carry a peer response body. Truncating here rather than at each call site
// means no call site can forget, and the migration's rule that this column holds
// a low-cardinality summary stays enforceable.
func truncateError(s string) string {
	const max = 255
	if len(s) <= max {
		return s
	}
	return s[:max]
}
