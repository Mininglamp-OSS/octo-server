package cutover

// The one-way compare-and-set flip, extracted from #627's
// internal/msgextraseq.Activate and #697's botevent.Activate, which differed
// only in table name, floor comparison, and whether evidence is recomputed
// under the lock. The FOR UPDATE on the state row serializes concurrent
// operators; whether it is also a writer drain barrier depends on the domain
// (#627's writers hold the row FOR SHARE until commit, so it is; #697's
// writers take no MySQL lock at all, so it is not — that difference lives in
// the domain's runbook, not here).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Sentinel errors. Domains wrap these into their own exported sentinels so
// existing callers and tests keep their errors.Is contracts.
var (
	// ErrFlipInvariant is returned when the mode-conditional UPDATE did not
	// match exactly one row despite the FOR UPDATE lock — the drain barrier
	// was lost or the row changed underneath the lock. Fail instead of
	// committing and reporting a phantom flip.
	ErrFlipInvariant = errors.New("cutover: flip matched an unexpected row count under the state lock")
	// ErrUnknownMode is returned when the locked state row carries a mode
	// that is neither the inactive nor the active value.
	ErrUnknownMode = errors.New("cutover: unknown mode in state row")
)

// FloorError reports a requested floor outside its validated bounds. It is a
// typed error (not a sentinel) so domains can re-render it with their own
// wording and sentinel while keeping the numbers exact.
type FloorError struct {
	// Floor is the requested cutover floor.
	Floor int64
	// Observed is the maximum the evidence produced (set when !TooHigh).
	Observed int64
	// Max is the configured upper bound (set when TooHigh).
	Max int64
	// TooHigh distinguishes "below observed evidence" from "above the
	// domain's safe ceiling".
	TooHigh bool
}

func (e *FloorError) Error() string {
	if e.TooHigh {
		return fmt.Sprintf("cutover: floor=%d exceeds the maximum %d", e.Floor, e.Max)
	}
	return fmt.Sprintf("cutover: floor=%d is below the observed max %d", e.Floor, e.Observed)
}

// FlipSpec parameterizes one activation attempt.
type FlipSpec struct {
	// Table is the domain's singleton state table.
	Table string
	// Floor is the requested cutover floor, validated against Observe's
	// result before any mutation.
	Floor int64
	// MaxFloor, when > 0, is the highest acceptable floor (inclusive). A
	// floor above it returns a FloorError with TooHigh set. Domains use it
	// to keep post-activation allocations away from representation limits
	// (#627: 2^53-1 minus one full batch).
	MaxFloor int64
	// FloorMustExceedObserved selects the lower-bound comparison: true
	// refuses Floor <= observed (#697, where the counter is seeded at the
	// floor), false refuses Floor < observed (#627, where the first reserved
	// version is floor+1, so floor == observed is safe).
	FloorMustExceedObserved bool
	// Observe returns the maximum already-issued value, computed while the
	// state row is held FOR UPDATE. A domain whose writers drain against
	// that lock recomputes its evidence here so a concurrently-committing
	// legacy writer cannot have raised the maxima after the operator's
	// preflight (#627); a domain with no drain barrier returns the value it
	// gathered before the lock (#697). Nil means no observed lower bound.
	Observe func(tx *dbr.Tx) (int64, error)
	// LockWaitTimeoutSeconds, when > 0, pins one connection and applies a
	// session innodb_lock_wait_timeout for the duration of the flip, so an
	// accidental pre-drain activation fails fast instead of queueing behind
	// a long-running writer and stalling later writers behind the pending
	// exclusive lock. The previous value is restored before the connection
	// returns to the pool; if it cannot be restored the connection is
	// discarded rather than pooled with an unknown session setting.
	LockWaitTimeoutSeconds int
}

// Flip performs the one-way inactive→active CAS on the domain's state row.
//
// It locks the singleton FOR UPDATE, returns flipped=false with the current
// epoch when the row is already active (idempotent re-run), refuses a missing
// row (ErrStateMissing), an unrecognized mode (ErrUnknownMode), and a floor
// outside its bounds (*FloorError), then executes a mode-conditional UPDATE
// setting mode/epoch+1/cutover_floor and verifies it matched exactly one row
// (ErrFlipInvariant otherwise). On success it returns the new epoch.
//
// # flipped is authoritative; err alone is not
//
// flipped reports whether the state row was COMMITTED, and callers must consult
// it before concluding from a non-nil error that nothing happened. Releasing the
// pinned connection can fail after the commit (restoring the session lock-wait
// timeout, closing the connection), and that failure is joined into the returned
// error — so `(true, non-nil)` is a real and reachable outcome meaning "the
// cutover happened, and the connection could not be returned to the pool
// cleanly". A caller that checks err first and reports "not flipped" tells the
// operator the opposite of what the database now says, and skips whatever it
// does on success (metrics, the ACTIVATED banner). Check flipped, then err.
//
// The converse has one unavoidable case: if the connection drops between the
// server committing and the client seeing the ack, Commit returns an error for
// a flip that DID happen, and this reports flipped=false. Nothing here can tell
// that apart from a commit that never landed. The recovery is the idempotent
// re-run — a second activate finds the row already active, returns
// flipped=false with no error, and the domain's post-flip steps (botevent's
// mirror publication) still run. Both runbooks say to re-run before concluding
// an activation failed.
//
// ctx bounds every statement, including acquiring the pinned connection and the
// session SET/restore around it. innodb_lock_wait_timeout bounds lock waits, not
// an unresponsive server, so without a deadline here an operator running the
// activation mid-procedure — with writes drained — has no way to abort it.
func Flip(ctx context.Context, db *dbr.Session, spec FlipSpec) (flipped bool, newEpoch uint64, retErr error) {
	if db == nil {
		return false, 0, errors.New("cutover: nil db session, cannot flip")
	}
	tx, cleanup, err := beginFlipTx(ctx, db, spec.LockWaitTimeoutSeconds)
	if err != nil {
		return false, 0, fmt.Errorf("cutover: begin flip: %w", err)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	var locked struct {
		Mode  int    `db:"mode"`
		Epoch uint64 `db:"epoch"`
	}
	count, err := tx.tx.SelectBySql(
		"SELECT `mode`, `epoch` FROM `"+spec.Table+"` WHERE `singleton_id`=? FOR UPDATE",
		SingletonID,
	).LoadContext(ctx, &locked)
	if err != nil {
		// Classify exactly as ReadState does. Both domains switch on these
		// sentinels, so an unclassified 1146 falls through to their raw-error
		// default and tells the operator the authority is unreachable when the
		// truth is that the migration has not run — opposite safe answers once
		// the new era has begun. The shipped commands preflight first and do not
		// reach this today; the primitive is what a third domain copies.
		if isMissingTable(err) {
			return false, 0, ErrStateTableMissing
		}
		return false, 0, fmt.Errorf("cutover: lock %s: %w", spec.Table, err)
	}
	if count == 0 {
		return false, 0, ErrStateMissing
	}
	switch locked.Mode {
	case ModeActive:
		return false, locked.Epoch, nil // already active — idempotent
	case ModeInactive:
		// proceed
	default:
		return false, 0, fmt.Errorf("%w: %d", ErrUnknownMode, locked.Mode)
	}

	if spec.Observe != nil {
		observed, err := spec.Observe(tx.tx)
		if err != nil {
			return false, 0, err
		}
		tooLow := spec.Floor < observed
		if spec.FloorMustExceedObserved {
			tooLow = spec.Floor <= observed
		}
		if tooLow {
			return false, 0, &FloorError{Floor: spec.Floor, Observed: observed}
		}
	}
	if spec.MaxFloor > 0 && spec.Floor > spec.MaxFloor {
		return false, 0, &FloorError{Floor: spec.Floor, Max: spec.MaxFloor, TooHigh: true}
	}

	res, err := tx.tx.UpdateBySql(
		"UPDATE `"+spec.Table+"` SET `mode`=?, `epoch`=?, `cutover_floor`=? WHERE `singleton_id`=? AND `mode`=?",
		ModeActive, locked.Epoch+1, spec.Floor, SingletonID, ModeInactive,
	).ExecContext(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("cutover: flip %s: %w", spec.Table, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("cutover: flip %s rows: %w", spec.Table, err)
	}
	if affected != 1 {
		// We read mode==inactive while holding the row FOR UPDATE, so the
		// mode-conditional UPDATE must match exactly one row. Anything else
		// means the barrier was lost — surface it instead of committing.
		return false, 0, fmt.Errorf("%w: matched %d rows, expected 1", ErrFlipInvariant, affected)
	}
	if err := tx.restoreBeforeCommit(); err != nil {
		return false, 0, err
	}
	if err := tx.tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("cutover: commit flip: %w", err)
	}
	return true, locked.Epoch + 1, nil
}

// flipTx wraps the flip's transaction. When a session lock-wait timeout is
// requested it pins one SQL connection so the operator-only setting can be
// restored before that connection returns to the shared pool.
type flipTx struct {
	tx                      *dbr.Tx
	conn                    *sql.Conn
	ctx                     context.Context
	previousLockWaitTimeout int64
	restored                bool
}

// cleanupRestoreTimeout bounds the compensating restore in cleanup. It is short
// because by then the outcome is already decided and the only question is
// whether this connection can go back in the pool.
const cleanupRestoreTimeout = 2 * time.Second

// beginFlipTx starts the flip transaction. With no lock-wait override it is a
// plain Begin; cleanup only rolls back if not committed.
func beginFlipTx(ctx context.Context, db *dbr.Session, lockWaitSeconds int) (*flipTx, func() error, error) {
	if lockWaitSeconds <= 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		wrapped := &flipTx{tx: tx, restored: true}
		return wrapped, func() error { tx.RollbackUnlessCommitted(); return nil }, nil
	}

	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var previous int64
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&previous); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("read session lock-wait timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = ?", lockWaitSeconds); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("set session lock-wait timeout: %w", err)
	}
	sqlTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		// Detached like cleanup's restore, and for the same reason: the most
		// likely way to arrive here is ctx being cancelled or expiring, and
		// restoring on that dead context would discard a connection a fresh
		// two-second attempt returns to the pool cleanly.
		restoreErr := restoreDetached(ctx, conn, previous)
		closeErr := closeFlipConnection(conn, restoreErr != nil)
		return nil, nil, errors.Join(err, restoreErr, closeErr)
	}
	wrapped := &flipTx{
		tx: &dbr.Tx{
			EventReceiver: db.EventReceiver,
			Dialect:       db.Dialect,
			Tx:            sqlTx,
			Timeout:       db.GetTimeout(),
		},
		conn:                    conn,
		ctx:                     ctx,
		previousLockWaitTimeout: previous,
	}
	return wrapped, wrapped.cleanup, nil
}

// restoreBeforeCommit puts the pooled connection's session setting back before
// the commit. A restore failure rolls the flip back; cleanup then retries the
// restore after rollback and discards the connection if it cannot be restored
// safely.
func (f *flipTx) restoreBeforeCommit() error {
	if f.restored {
		return nil
	}
	if err := restoreLockWaitTimeout(f.ctx, f.tx, f.previousLockWaitTimeout); err != nil {
		return err
	}
	f.restored = true
	return nil
}

func (f *flipTx) cleanup() error {
	f.tx.RollbackUnlessCommitted()
	var restoreErr error
	if !f.restored {
		restoreErr = restoreDetached(f.ctx, f.conn, f.previousLockWaitTimeout)
	}
	closeErr := closeFlipConnection(f.conn, restoreErr != nil)
	return errors.Join(restoreErr, closeErr)
}

// restoreDetached runs the compensating restore on a context detached from the
// caller's.
//
// Cleanup must not inherit a cancellation that is frequently the reason it is
// running: restoring on a dead context fails instantly, which discards a
// connection that a fresh short attempt would have returned to the pool.
func restoreDetached(ctx context.Context, execer contextExecer, previous int64) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupRestoreTimeout)
	defer cancel()
	return restoreLockWaitTimeout(restoreCtx, execer, previous)
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func restoreLockWaitTimeout(ctx context.Context, execer contextExecer, previous int64) error {
	if _, err := execer.ExecContext(
		ctx,
		"SET SESSION innodb_lock_wait_timeout = ?",
		previous,
	); err != nil {
		return fmt.Errorf("cutover: restore session lock-wait timeout: %w", err)
	}
	return nil
}

func closeFlipConnection(conn *sql.Conn, discard bool) error {
	if discard {
		// Returning driver.ErrBadConn prevents database/sql from putting a
		// connection with an unknown session setting back into the pool.
		//
		// It also CLOSES the Conn as a side effect (database/sql calls
		// c.close(err) when the Raw callback returns ErrBadConn), so the Close
		// below always reports sql.ErrConnDone. That is the expected outcome
		// here, not a failure: reporting it would append a fabricated
		// "close failed" line to the one message an operator has to read
		// correctly — the half-done cutover.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			return fmt.Errorf("cutover: close flip connection: %w", err)
		}
		return nil
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("cutover: close flip connection: %w", err)
	}
	return nil
}
