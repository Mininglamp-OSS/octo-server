package botevent

// #697: the DB-authoritative state for the bot event id allocator.
//
// # Why the state is not in Redis
//
// The allocator itself must stay in Redis: `saveRobotMessage` allocates inside a
// `msgSem` slot and cannot afford a DB round trip per message (GenSeq amortised one
// over 1000 ids; a DB sequence would be one per id).
//
// The *activation state* is a different question, and review found the answer was
// wrong. Production runs `appendonly no` with only RDB snapshots, so a Redis-only
// mode key regresses with the snapshot. Losing it dropped the allocator back to
// legacy `GenSeq` — whose ids sit **below** everything the counter had already
// issued, so new events land under live consumer cursors and are permanently
// invisible. That is #697 mirrored, caused by its own fix.
//
// So the authority lives here, in a table that does not share Redis's recovery
// domain, and the Redis key is only a **mirror**. When the mirror is missing the
// allocator reads this row and, if activated, rebuilds the mirror and re-seeds
// rather than degrading.
//
// # Deliberately aligned with #627, deliberately not a copy
//
// Same shape as `octo_message_extra_version_state`: mode / epoch / cutover_floor, a
// `FOR UPDATE` compare-and-set flip, and a floor validated against observed maxima.
//
// What is **not** reused is that design's `FOR SHARE` drain barrier. It works there
// because every `message_extra` writer is already inside a DB transaction and can
// hold the state row until it commits. A `robotEvent` writer is `INCR` + `ZADD` with
// no transaction at all, so there is nothing to hold a lock until — and wrapping
// each allocation in a transaction just to borrow the barrier would reintroduce the
// per-message DB round trip the allocator exists to avoid.
//
// The consequence is honest and must stay documented: this design cannot drain
// in-flight writers at the flip. It relies on the operator having confirmed that no
// pre-fix replica is running, plus a brief write pause around the flip. See
// tools/botevent-seq.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/go-sql-driver/mysql"
)

const (
	// stateTable is the singleton state row's table.
	stateTable = "octo_bot_event_seq_state"

	// stateSingletonID is the only permitted primary key.
	stateSingletonID = 1

	// StateModeLegacy delegates to GenSeq. Seeded value, so a deploy is inert.
	StateModeLegacy = 0

	// StateModeIncr allocates from the Redis counter.
	StateModeIncr = 1
)

// ErrFloorTooLow mirrors msgextraseq: a cutover floor at or below what has already
// been issued would let post-activation ids land under a live consumer cursor.
var ErrFloorTooLow = errors.New("botevent: cutover floor is below the observed maximum")

// ErrStateMissing means the migration has not run. Treated as legacy by readers so
// a pre-migration deploy behaves like the old binary, and as a hard error by the
// operator tool so activation cannot be attempted blind.
var ErrStateMissing = errors.New("botevent: allocator state row is missing")

// State is the authoritative allocator state.
type State struct {
	Mode         int
	Epoch        uint64
	CutoverFloor int64
}

// Activated reports whether the counter is the authoritative allocator.
func (s State) Activated() bool { return s.Mode == StateModeIncr }

// ReadState reads the singleton row with no deadline. For the operator tool and
// tests; the allocator uses ReadStateContext so a stalled MySQL cannot hold a msgSem
// slot open (review P1-1).
func ReadState(ctx *config.Context) (State, error) {
	return ReadStateContext(context.Background(), ctx)
}

// ReadStateContext reads the singleton row under the caller's deadline.
//
// A missing row returns ErrStateMissing rather than a zero State, so callers choose
// what it means: readers treat it as legacy (which is what a pre-migration deploy
// is), while the operator tool refuses to flip. Callers must distinguish it from
// every other error — "the table is not there yet" and "the authority is
// unreachable" have opposite safe answers once the counter era has begun.
func ReadStateContext(deadline context.Context, ctx *config.Context) (State, error) {
	if ctx == nil {
		return State{}, errors.New("botevent: nil ctx, cannot read allocator state")
	}
	var row struct {
		Mode         int    `db:"mode"`
		Epoch        uint64 `db:"epoch"`
		CutoverFloor int64  `db:"cutover_floor"`
	}
	count, err := ctx.DB().SelectBySql(
		"SELECT `mode`, `epoch`, `cutover_floor` FROM `"+stateTable+"` WHERE `singleton_id`=?",
		stateSingletonID).LoadContext(deadline, &row)
	if err != nil {
		if isMissingTable(err) {
			// A pre-migration deploy: the same answer as a missing row, and it must not
			// be confused with an unreachable authority.
			return State{}, ErrStateMissing
		}
		return State{}, fmt.Errorf("botevent: read allocator state: %w", err)
	}
	if count == 0 {
		return State{}, ErrStateMissing
	}
	return State{Mode: row.Mode, Epoch: row.Epoch, CutoverFloor: row.CutoverFloor}, nil
}

// isMissingTable reports whether err is MySQL's "table doesn't exist" (1146).
//
// Matched on the driver's error number rather than the message so it survives a
// server locale change. It matters because a deploy that has not run the migration
// yet, and a deploy whose DB is unreachable, are the same *error* to dbr and
// opposite *states* to the allocator: one is legitimately legacy, the other is
// unknown and must not downgrade a process that has already issued counter ids.
func isMissingTable(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1146
}

// Activate flips the state row from legacy to incr under a row lock, validating the
// floor first.
//
// The `FOR UPDATE` here is **not** a drain barrier — see the file header. It only
// serialises concurrent operators and makes the read-validate-write atomic. Returns
// flipped=false with a nil error when the row is already activated, so the tool is
// idempotent.
func Activate(ctx *config.Context, floor, observedMax int64) (bool, uint64, error) {
	if ctx == nil {
		return false, 0, errors.New("botevent: nil ctx, cannot activate")
	}
	tx, err := ctx.DB().Begin()
	if err != nil {
		return false, 0, fmt.Errorf("botevent: begin activation tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var locked struct {
		Mode  int    `db:"mode"`
		Epoch uint64 `db:"epoch"`
	}
	count, err := tx.SelectBySql(
		"SELECT `mode`, `epoch` FROM `"+stateTable+"` WHERE `singleton_id`=? FOR UPDATE",
		stateSingletonID).Load(&locked)
	if err != nil {
		return false, 0, fmt.Errorf("botevent: lock allocator state: %w", err)
	}
	if count == 0 {
		return false, 0, ErrStateMissing
	}
	if locked.Mode == StateModeIncr {
		return false, locked.Epoch, nil
	}
	// The floor must clear everything legacy could still hand out. Refusing here is
	// the difference between an activation and an outage: a floor below the observed
	// maximum puts the first activated ids under cursors clients already hold.
	if floor <= observedMax {
		return false, 0, fmt.Errorf("%w: floor=%d observed max=%d", ErrFloorTooLow, floor, observedMax)
	}
	res, err := tx.UpdateBySql(
		"UPDATE `"+stateTable+"` SET `mode`=?, `epoch`=?, `cutover_floor`=? "+
			"WHERE `singleton_id`=? AND `mode`=?",
		StateModeIncr, locked.Epoch+1, floor, stateSingletonID, StateModeLegacy).Exec()
	if err != nil {
		return false, 0, fmt.Errorf("botevent: flip allocator state: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("botevent: flip allocator state rows: %w", err)
	}
	if affected != 1 {
		// The row was locked FOR UPDATE, so a mode-conditional UPDATE must match
		// exactly one row. Anything else means the row changed underneath the lock.
		return false, 0, fmt.Errorf("botevent: flip matched %d rows, expected 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("botevent: commit activation: %w", err)
	}
	return true, locked.Epoch + 1, nil
}

// stateFloorOrZero returns the recorded cutover floor, or 0 when unreadable.
//
// Used by the seed as one more floor source. Best-effort: an unreadable row must not
// fail an allocation, because the other floor sources (queue ceiling, legacy row,
// durable high-water) already cover the cases this one is a backstop for. Deadlined
// like every other DB read on this path — it runs inside a held msgSem slot.
func stateFloorOrZero(ctx *config.Context) int64 {
	deadline, cancel := context.WithTimeout(context.Background(), authorityTimeout)
	defer cancel()
	st, err := ReadStateContext(deadline, ctx)
	if err != nil {
		return 0
	}
	return st.CutoverFloor
}
