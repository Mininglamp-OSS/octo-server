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
// # Deliberately aligned with #627, now literally shared
//
// Same shape as `octo_message_extra_version_state`: mode / epoch / cutover_floor, a
// `FOR UPDATE` compare-and-set flip, and a floor validated against observed maxima.
// That shared shape now lives in pkg/cutover; this file keeps only what is
// specific to this domain — the strict floor comparison, the sentinel errors
// callers match on, and the missing-row semantics.
//
// What is **not** shared is #627's `FOR SHARE` drain barrier. It works there
// because every `message_extra` writer is already inside a DB transaction and can
// hold the state row until it commits. A `robotEvent` writer is `INCR` + `ZADD` with
// no transaction at all, so there is nothing to hold a lock until — and wrapping
// each allocation in a transaction just to borrow the barrier would reintroduce the
// per-message DB round trip the allocator exists to avoid.
//
// The consequence is honest and must stay documented: this design cannot drain
// in-flight writers at the flip. It relies on the operator having confirmed that no
// pre-fix replica is running, plus a brief write pause around the flip. See
// docs/botevent-cutover-runbook.md and `app cutover botevent`.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"github.com/gocraft/dbr/v2"
)

const (
	// stateTable is the singleton state row's table.
	stateTable = "octo_bot_event_seq_state"

	// stateSingletonID is the only permitted primary key.
	stateSingletonID = 1

	// StateModeLegacy delegates to GenSeq. Seeded value, so a deploy is inert.
	StateModeLegacy = cutover.ModeInactive

	// StateModeIncr allocates from the Redis counter.
	StateModeIncr = cutover.ModeActive
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
	st, err := cutover.ReadState(deadline, ctx.DB(), stateTable)
	if err != nil {
		if errors.Is(err, cutover.ErrStateMissing) {
			// A pre-migration deploy (missing row or missing table, MySQL 1146):
			// the same answer either way, and it must not be confused with an
			// unreachable authority — a deploy that has not run the migration
			// yet is legitimately legacy, while an unreachable DB is unknown
			// and must not downgrade a process that has already issued counter
			// ids.
			return State{}, ErrStateMissing
		}
		return State{}, fmt.Errorf("botevent: read allocator state: %w", err)
	}
	return State{Mode: st.Mode, Epoch: st.Epoch, CutoverFloor: st.Floor}, nil
}

// Activate flips the state row from legacy to incr under a row lock, validating the
// floor first.
//
// The `FOR UPDATE` (inside cutover.Flip) is **not** a drain barrier — see the file
// header. It only serialises concurrent operators and makes the read-validate-write
// atomic. observedMax is gathered by the operator command BEFORE the lock — with no
// drain barrier there is nothing a locked recompute could pin down — and the floor
// must clear it strictly: a floor at or below the observed maximum puts the first
// activated ids under cursors clients already hold. Returns flipped=false with a
// nil error when the row is already activated, so the operator command is
// idempotent.
func Activate(ctx *config.Context, floor, observedMax int64) (bool, uint64, error) {
	if ctx == nil {
		return false, 0, errors.New("botevent: nil ctx, cannot activate")
	}
	flipped, epoch, err := cutover.Flip(ctx.DB(), cutover.FlipSpec{
		Table:                   stateTable,
		Floor:                   floor,
		FloorMustExceedObserved: true,
		Observe:                 func(*dbr.Tx) (int64, error) { return observedMax, nil },
	})
	if err != nil {
		var floorErr *cutover.FloorError
		switch {
		case errors.Is(err, cutover.ErrStateMissing):
			return false, 0, ErrStateMissing
		case errors.As(err, &floorErr):
			return false, 0, fmt.Errorf("%w: floor=%d observed max=%d", ErrFloorTooLow, floorErr.Floor, floorErr.Observed)
		default:
			return false, 0, fmt.Errorf("botevent: activate: %w", err)
		}
	}
	return flipped, epoch, nil
}
