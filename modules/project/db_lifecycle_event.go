package project

import (
	"database/sql"
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
//
// # The NOT EXISTS clause is what actually enforces per-project order
//
// At most ONE pending event per project is claimable: the row is skipped while
// any older pending sibling exists. Everything about per-project ordering rests
// on this clause, and it replaced an in-memory version that did not work.
//
// That earlier attempt blocked a project inside the delivery loop after its
// first failure. It held for the rows in one claim and inverted on the very next
// tick, because the two statements that hand rows back leave the queue in the
// wrong order: the failed head is pushed to now+backoff, while a skipped tail is
// released with next_attempt_at UNTOUCHED at its original enqueue time. So the
// tail sorted FIRST — and during the first backoff the head was not even due, so
// the tail was claimed alone, with an empty blocked map. It was sent, the peer
// answered "unknown project", and a plain 4xx is terminal here: the revocation
// behind a transiently failed creation event was abandoned. The test missed it
// by calling the delivery loop exactly once.
//
// A predicate in the claim has the property the map structurally cannot: it also
// holds ACROSS REPLICAS. One pod holding a leased head does not stop another pod
// from claiming that project's tail, and no amount of per-process state can see
// that.
//
// # Cost, measured rather than assumed
//
// An earlier version of this comment said the cost was bounded by the batch
// size. It is bounded by the BACKLOG size, and the difference is what someone
// will rely on when deciding whether a five-second poll is safe. EXPLAIN ANALYZE
// against this migration's DDL with 50,000 terminal and 505 pending rows: the
// optimizer does not use idx_..._pending, the LIMIT cannot bound the sort input
// because the antijoin sits above it, and all 505 pending rows are read, fully
// sorted, and the subquery evaluated 505 times to return 6. Cost 6871 against
// 71.3 for the same query without the NOT EXISTS — roughly 96x, growing linearly
// with the backlog, and the backlog is the one state in which this runs hot.
//
// The lock footprint is the part that reaches other writers, and it is NOT
// introduced by the predicate: FOR UPDATE combined with a sort locks every row
// the scan reads, not the ones it returns, so the pre-predicate shape holds the
// same ~1000 record locks. performance_schema.data_locks during an open claim at
// that size shows ~1012 locks to claim 6 rows, and a concurrent INSERT for an
// UNRELATED project can hit lock wait timeout — that insert is
// insertLifecycleEventTx, which runs inside the user-facing business
// transaction. What bounds it is that this transaction commits before any HTTP
// call, so the window is the query, milliseconds at this size.
//
// Left as it is rather than optimised, deliberately: the feed is not enabled
// anywhere, the correct shape is a cheap candidate read followed by a narrow
// FOR UPDATE on those ids, and doing that here would rewrite the claim in the
// same commit that is fixing two blockers. Recorded so the next person has the
// numbers instead of the reassurance. An EXPLAIN under a synthetic backlog is
// worth doing before this feed is enabled anywhere.
//
// The alternative — a window function — cannot be combined with FOR UPDATE.
func (d *DB) claimLifecycleEvents(owner string, limit int, now time.Time, lease time.Duration) ([]lifecycleEventRow, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: claim lifecycle events begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var rows []lifecycleEventRow
	_, err = tx.SelectBySql(
		"SELECT e.id, e.event_id, e.event_type, e.project_id, e.space_id, e.project_version, "+
			"e.payload, e.occurred_at, e.attempts "+
			"FROM `octo_project_lifecycle_event` e "+
			"WHERE e.status = ? AND e.next_attempt_at <= ? "+
			"  AND (e.lease_until IS NULL OR e.lease_until <= ?) "+
			"  AND e.attempts < ? "+
			"  AND NOT EXISTS ("+
			"    SELECT 1 FROM `octo_project_lifecycle_event` older "+
			"    WHERE older.project_id = e.project_id AND older.status = ? AND older.id < e.id) "+
			"ORDER BY e.next_attempt_at ASC, e.id ASC LIMIT ? FOR UPDATE SKIP LOCKED",
		lifecycleEventPending, now, now, lifecycleEventMaxAttempts, lifecycleEventPending, limit,
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
func (d *DB) heartbeatLifecycleEventLeases(owner string, until time.Time) error {
	// By OWNER, not by a list of ids captured when the batch was claimed. The
	// per-project drain claims MORE rows after the heartbeat has started, and an
	// id list fixed at claim time would leave exactly those rows unrenewed — the
	// ones this worker is actively walking. lease_owner is unique per claim
	// (workerIdentity), so this can only touch rows this worker holds.
	_, err := d.session.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` SET lease_until = ? "+
			"WHERE lease_owner = ? AND status = ?",
		until, owner, lifecycleEventPending,
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

// releaseUnattemptedLifecycleEvents hands back rows this worker claimed but did
// NOT send, and refunds the attempt the claim charged them.
//
// The refund is the point. claimLifecycleEvents increments attempts for the whole
// batch up front, so a row released without a send would otherwise burn budget
// for work that never happened — and with the per-project ordering rule below,
// a project with a slow head event would retire its own tail after a dozen ticks
// without a single delivery having been tried on it.
//
// Guarded on lease_owner so a row whose lease already changed hands is left
// alone: another worker owns it and its attempt is legitimately spent.
//
// attempts is decremented with a floor, because a value that could go negative
// would read as unsigned in MySQL and become a very large number — the abandon
// check compares against it.
func (d *DB) releaseUnattemptedLifecycleEvents(ids []int64, owner string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := d.session.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` "+
			"SET lease_owner = '', lease_until = NULL, "+
			"    attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END "+
			"WHERE id IN ? AND lease_owner = ? AND status = ?",
		ids, owner, lifecycleEventPending,
	).Exec(); err != nil {
		return fmt.Errorf("project: release unattempted lifecycle events: %w", err)
	}
	return nil
}

// claimNextForProject leases this project's next pending event, if there is one.
//
// The per-project drain (see runLifecycleEventDelivery) is what stops the
// ordering rule from also being a throughput rule. Ordering requires a project's
// events go in SEQUENCE; it does not require one per poll interval, and the
// difference is 200x on the one operation that produces a burst:
// POST /:project_id/members/remove takes up to MemberBatchMax uids and enqueues
// one member_revoked per uid on the SAME project, so a single authorized request
// used to need ~1000 seconds to drain against a perfectly healthy peer — on the
// event class this module treats as a security failure to lose, and past any
// threshold on the age gauge that exists to report exactly that.
//
// Deliberately NOT the batch claim with a bigger limit. This is an index lookup
// on (project_id, id), where the batch claim sorts and antijoins the whole
// pending set; running that repeatedly to drain one project would multiply the
// expensive query rather than the cheap one.
//
// No NOT EXISTS needed: ORDER BY id ASC LIMIT 1 IS the oldest pending sibling,
// so the ordering property is the same one the batch claim spells out.
func (d *DB) claimNextForProject(owner, projectID string, now time.Time, lease time.Duration) (*lifecycleEventRow, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: claim next lifecycle event begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var rows []lifecycleEventRow
	if _, err := tx.SelectBySql(
		"SELECT id, event_id, event_type, project_id, space_id, project_version, "+
			"payload, occurred_at, attempts "+
			"FROM `octo_project_lifecycle_event` "+
			"WHERE project_id = ? AND status = ? AND next_attempt_at <= ? "+
			"  AND (lease_until IS NULL OR lease_until <= ?) AND attempts < ? "+
			"ORDER BY id ASC LIMIT 1 FOR UPDATE SKIP LOCKED",
		projectID, lifecycleEventPending, now, now, lifecycleEventMaxAttempts,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: claim next lifecycle event select: %w", err)
	}
	if len(rows) == 0 {
		return nil, tx.Commit()
	}
	rows[0].Attempts++
	if _, err := tx.UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` "+
			"SET lease_owner = ?, lease_until = ?, attempts = attempts + 1 WHERE id = ?",
		owner, now.Add(lease), rows[0].ID,
	).Exec(); err != nil {
		return nil, fmt.Errorf("project: claim next lifecycle event lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: claim next lifecycle event commit: %w", err)
	}
	return &rows[0], nil
}

// abandonExhaustedLifecycleEvents retires pending rows whose attempt budget is
// already gone.
//
// It is the other half of the claim-time `attempts < max` predicate, and it is
// REQUIRED rather than tidy. The budget used to be evaluated only after Send
// returned, so a worker that died between claiming and completing re-charged an
// attempt on every lease expiry and the row never reached a terminal state.
// Adding the claim-time predicate alone would convert that into something worse:
// an exhausted row becomes unclaimable, and because the claim now refuses any
// project with an older pending sibling, one such row would block its project's
// queue permanently.
//
// So the two go together. The predicate stops budget from being spent on rows
// that cannot use it; this sweep gives those rows the terminal state the
// delivery path would have given them, with the same alerting attached.
//
// Only unleased rows: a leased row belongs to a worker that may be mid-send.
func (d *DB) abandonExhaustedLifecycleEvents(now time.Time, reason string, limit int) ([]lifecycleEventRow, error) {
	var rows []lifecycleEventRow
	if _, err := d.session.SelectBySql(
		"SELECT id, event_id, event_type, project_id, space_id, project_version, "+
			"payload, occurred_at, attempts "+
			"FROM `octo_project_lifecycle_event` "+
			"WHERE status = ? AND attempts >= ? "+
			"  AND (lease_until IS NULL OR lease_until <= ?) "+
			"ORDER BY id LIMIT ?",
		lifecycleEventPending, lifecycleEventMaxAttempts, now, limit,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("project: find exhausted lifecycle events: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// One guarded UPDATE PER ROW, and the row count of each is the answer to "did
	// this pod win this row".
	//
	// A single UPDATE ... WHERE id IN (...) cannot answer that: RowsAffected is a
	// total, and re-reading the rows afterwards cannot separate the ones this pod
	// transitioned from the ones a concurrent sweep did — both write the same
	// terminal state and the same fixed reason. Ten statements per pass at a
	// five-minute cadence is not a cost worth trading that certainty for.
	//
	// It matters because of what the caller does with the answer: it increments
	// the abandoned counter and logs "the peer will never be told" per row. Two
	// replicas whose jittered sweeps overlap would otherwise both alert on the
	// same event, and a row claimed by a delivery worker between the SELECT and
	// the UPDATE would be alerted on while its new owner is mid-send.
	var mine []lifecycleEventRow
	for _, row := range rows {
		// The status, budget and lease predicates are repeated from the SELECT so a
		// row claimed in between is left to the worker that now owns it.
		res, err := d.session.UpdateBySql(
			"UPDATE `octo_project_lifecycle_event` "+
				"SET status = ?, last_error = ?, finished_at = ?, lease_owner = '', lease_until = NULL "+
				"WHERE id = ? AND status = ? AND attempts >= ? "+
				"  AND (lease_until IS NULL OR lease_until <= ?)",
			lifecycleEventAbandoned, truncateError(reason), now,
			row.ID, lifecycleEventPending, lifecycleEventMaxAttempts, now,
		).Exec()
		if err != nil {
			return nil, fmt.Errorf("project: abandon exhausted lifecycle event %d: %w", row.ID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("project: abandon exhausted lifecycle event rows: %w", err)
		}
		if affected > 0 {
			mine = append(mine, row)
		}
	}
	return mine, nil
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
//
// That reasoning was right about the hazard and wrong about which half of it
// this code was exposed to. Computing in Go defends the SQL side; the Go-side
// READ-BACK is where a loc=Local DSN bites, and this function had both halves of
// that wrong — see utcread.go. It scanned into []time.Time, which dbr silently
// fills with the zero time, so the gauge reported about 2.5 million hours on
// every deployment regardless of timezone.
func (d *DB) oldestPendingLifecycleEventAge(now time.Time) (time.Duration, error) {
	var oldest []sql.NullTime
	_, err := d.session.SelectBySql(
		"SELECT created_at FROM `octo_project_lifecycle_event` "+
			"WHERE status = ? ORDER BY created_at ASC LIMIT 1", lifecycleEventPending,
	).Load(&oldest)
	if err != nil {
		return 0, fmt.Errorf("project: oldest pending lifecycle event: %w", err)
	}
	oldestUTC, ok := firstUTCFromColumn(oldest)
	if !ok {
		return 0, nil
	}
	age := now.UTC().Sub(oldestUTC)
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

// truncateError bounds last_error for the lifecycle outbox.
//
// It is truncateProvisioningError — the same 255-byte column, the same package,
// one implementation. This started as a second copy that repaired only its own
// cut, which was the weaker half on the path that carries revocations: a string
// arriving with invalid bytes ALREADY IN IT passed through untouched whenever it
// was under the limit, MySQL strict mode rejects it with 1366, and the write it
// fails is the one marking the event delivered or abandoned — so the row stays
// pending, the lease expires, and the same event is redelivered every tick
// forever. That is the exact loop the rune-boundary fix was made for, reachable
// through the other door.
//
// The duplicate was not reachable today, but only through an invariant nothing
// stated: every current input is either a literal or a code that came through
// json.Unmarshal, which replaces invalid UTF-8 with U+FFFD. One future caller
// passing a raw err.Error() would have reopened it. Sharing the sibling means
// whole-string sanitization cannot be forgotten per call site.
func truncateError(s string) string { return truncateProvisioningError(s) }
