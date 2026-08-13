package msgextraseq

// Operator activation surface for #627 (PR-3). Preflight is read-only and
// recommends a safe cutover floor; Activate flips the DB-authoritative state row
// legacy→transactional under an exclusive lock that drains in-flight writers.
// Neither runs in a request path — they are invoked by the `app cutover
// msgextra` operator command.
//
// The transactional scaffolding of the flip (FOR UPDATE CAS, idempotency,
// floor bounds, pinned-connection lock-wait timeout) lives in pkg/cutover,
// shared with #697. What stays here is what is specific to this domain: which
// evidence bounds the floor (message_extra maxima, legacy GenSeq boundaries,
// Redis sync cursors), that the evidence is recomputed UNDER the drain
// barrier, and the sentinel errors this package's callers already match on.
//
// There is deliberately no online "deactivate": rolling back to legacy cannot be
// done safely by a single DB flip. octo-lib's GenSeq HiLo allocator caches its
// block in process memory, so a replica that cached a pre-activation legacy block
// would resume issuing versions below the transactional high-water the instant
// mode flips back — re-opening the exact skip window #627 closes — and a DB-only
// change cannot invalidate that in-memory cache. Rollback is therefore a
// documented, maintenance-window coordinated procedure (drain writes → raise the
// legacy seq boundaries above the transactional max → restart every replica →
// flip mode); see docs/msgextra-cutover-runbook.md §6.
//
// The cutover floor must be at least every version already handed out or validly
// cached as a client sync cursor (otherwise post-cutover writes could remain
// below a cursor and invisible), and at most MaxCutoverFloor (or every
// reservation would fail ErrOverflow). The issued ceiling is the max persisted
// message_extra.version / legacy GenSeq block boundary. Redis
// messageExtraVersion:* values within that ceiling are evidence; malformed,
// negative, or above-issued values cannot be trusted as server-issued cursors,
// are counted as poisoned, and are excluded. Upgraded sync handlers repair those
// per-channel cache entries on read. Activate recomputes and enforces the evidence
// under the operational drain so stale/out-of-range input is never trusted.

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"github.com/gocraft/dbr/v2"
)

// activationLockWaitTimeoutSeconds keeps an accidental pre-drain activation
// from queueing behind a long-running writer and stalling later writers behind
// the pending exclusive state-row lock. The runbook still requires an explicit
// write drain; this is a fail-fast backstop, not a substitute for that drain.
const activationLockWaitTimeoutSeconds = 3

// ErrStateRowMissing is returned when the singleton state row is absent (the
// migration seeds it; a missing row means the schema is not in place).
var ErrStateRowMissing = errors.New("msgextraseq: allocator state row missing (run the migration first)")

// ErrFloorTooLow is returned by Activate when the requested cutover floor is
// below the maximum version already observed, which would risk reissuing a
// version at or below an existing one.
var ErrFloorTooLow = errors.New("msgextraseq: cutover floor is below the observed max version")

// ErrFloorTooHigh is returned by Activate when the requested cutover floor leaves
// no headroom below maxSafeInteger, which would make every subsequent reservation
// fail with ErrOverflow (a write outage). The floor must leave at least one full
// MaxReserveCount batch of room.
var ErrFloorTooHigh = errors.New("msgextraseq: cutover floor leaves no headroom below 2^53-1")

// PreflightResult reports the evidence behind a recommended cutover floor and
// the current allocator state. It is produced by a read-only Preflight.
type PreflightResult struct {
	// CurrentMode/CurrentFloor/CurrentEpoch are the live state row values.
	CurrentMode  int
	CurrentFloor int64
	CurrentEpoch uint64
	// MaxMessageExtraVersion is MAX(message_extra.version) across all channels.
	MaxMessageExtraVersion int64
	// MaxLegacySeqBoundary is MAX(seq.min_seq) across the messageExtra GenSeq
	// keys — the upper bound on versions the legacy HiLo allocator has handed out.
	MaxLegacySeqBoundary int64
	// MaxRedisCursor is the largest valid cached messageExtraVersion:* hash value.
	// RedisCursorKeyCount/RedisCursorFieldCount are aggregate visit counts; key
	// and field names are deliberately never surfaced because they contain user,
	// source, and channel identifiers.
	MaxRedisCursor        int64
	RedisCursorKeyCount   int64
	RedisCursorFieldCount int64
	// InvalidRedisCursorFieldCount counts malformed, negative, or above-issued
	// cursors. They cannot be trusted as server-issued and are excluded from floor
	// evidence; upgraded sync handlers repair the per-channel cache when next read.
	InvalidRedisCursorFieldCount int64
	// RecommendedFloor is the max of the three maxima: the smallest floor that
	// cannot reissue an already-used version or sit below a cached sync cursor.
	RecommendedFloor int64
}

// CurrentState reads the live allocator state row — no locks, no evidence
// scans, no writes. It is the cheap read behind `app cutover msgextra status`;
// Preflight is the full-evidence version.
//
// It returns this package's State, not pkg/cutover's: the shared control plane
// is an implementation detail of the flip, and callers already spell this
// package's field names (CutoverFloor, not Floor).
func (s *Store) CurrentState(ctx context.Context) (State, error) {
	st, err := cutover.ReadState(ctx, s.ctx.DB(), StateTable)
	if err != nil {
		switch {
		case errors.Is(err, cutover.ErrStateTableMissing):
			return State{}, ErrStateTableMissing
		case errors.Is(err, cutover.ErrStateMissing):
			return State{}, ErrStateRowMissing
		default:
			return State{}, fmt.Errorf("msgextraseq: read state: %w", err)
		}
	}
	return State{Mode: st.Mode, Epoch: st.Epoch, CutoverFloor: st.Floor}, nil
}

// ErrStateTableMissing is the subset of ErrStateRowMissing where the table
// itself is absent, and it matters here more than in the sibling domain.
//
// This allocator's runtime treats the two OPPOSITELY:
//
//   - missing ROW: readStateForShare maps dbr.ErrNotFound to legacy, so writes
//     keep flowing on the pre-cutover allocator.
//   - missing TABLE (MySQL 1146): readStateForShare has no case for it, the
//     error propagates, and EVERY message_extra write fails closed.
//
// So an operator surface must be able to say which one it found. It wraps
// ErrStateRowMissing, so callers that only care that the authority is absent
// keep matching on that.
var ErrStateTableMissing = fmt.Errorf("%w: the table itself does not exist", ErrStateRowMissing)

// Preflight reads (no locks, no writes) the maxima that bound already-issued
// versions and reports a safe cutover floor plus the current state. It never
// mutates anything, so it is safe to run against production at any time.
//
// ctx bounds the MySQL reads. It does NOT bound the Redis cursor scan: the
// client library takes no per-command context, so a scan already in flight runs
// to completion. That residue is why the operator command's interrupt handling
// has a second stage.
func (s *Store) Preflight(ctx context.Context) (PreflightResult, error) {
	var res PreflightResult

	state, err := cutover.ReadState(ctx, s.ctx.DB(), StateTable)
	if err != nil {
		if errors.Is(err, cutover.ErrStateMissing) {
			return PreflightResult{}, ErrStateRowMissing
		}
		return PreflightResult{}, fmt.Errorf("msgextraseq: preflight read state: %w", err)
	}
	res.CurrentMode = state.Mode
	res.CurrentEpoch = state.Epoch
	res.CurrentFloor = state.Floor

	maxVersion, maxSeq, err := s.observedMaxima(ctx, s.ctx.DB())
	if err != nil {
		return PreflightResult{}, err
	}
	res.MaxMessageExtraVersion = maxVersion
	res.MaxLegacySeqBoundary = maxSeq
	issuedCeiling := maxVersion
	if maxSeq > issuedCeiling {
		issuedCeiling = maxSeq
	}
	redisEvidence, err := s.observeRedisCursorEvidence(issuedCeiling)
	if err != nil {
		return PreflightResult{}, err
	}
	res.MaxRedisCursor = redisEvidence.maxCursor
	res.RedisCursorKeyCount = redisEvidence.keyCount
	res.RedisCursorFieldCount = redisEvidence.fieldCount
	res.InvalidRedisCursorFieldCount = redisEvidence.invalidFieldCount
	res.RecommendedFloor = maxVersion
	if maxSeq > res.RecommendedFloor {
		res.RecommendedFloor = maxSeq
	}
	if redisEvidence.maxCursor > res.RecommendedFloor {
		res.RecommendedFloor = redisEvidence.maxCursor
	}
	return res, nil
}

// Activate flips the allocator from legacy to transactional under an exclusive
// lock on the state row. The FOR UPDATE is the drain barrier: it waits for every
// in-flight writer (each holds the state row FOR SHARE until it commits) to
// finish under legacy, then no new writer can proceed until this commits — so the
// maxima it recomputes under the lock are final. floor must be >= that max or the
// flip is refused (ErrFloorTooLow). Returns flipped=false with a nil error when
// the allocator is already transactional (idempotent).
func (s *Store) Activate(ctx context.Context, floor int64) (bool, error) {
	flipped, newEpoch, err := cutover.Flip(ctx, s.ctx.DB(), cutover.FlipSpec{
		Table:    StateTable,
		Floor:    floor,
		MaxFloor: MaxCutoverFloor,
		// Recompute every source under the drain barrier so a concurrently-
		// committing legacy writer cannot have raised the DB maxima after the
		// caller's Preflight. The runbook also drains /message/extra/sync while
		// this Redis scan runs; unlike writers, cursor updates do not take the
		// MySQL state-row lock.
		Observe: func(tx *dbr.Tx) (int64, error) {
			return s.observeUnderDrainBarrier(ctx, tx)
		},
		LockWaitTimeoutSeconds: activationLockWaitTimeoutSeconds,
	})
	// flipped is checked BEFORE err on purpose. Releasing the pinned connection
	// can fail after the row is committed, and Flip joins that failure into err;
	// reporting flipped=false there would tell the operator the cutover did not
	// happen while the database says it did, and would skip the metric updates
	// that describe the allocator now in force. See cutover.Flip's contract.
	if flipped {
		setAllocatorModeGauge(ModeTransactional)
		metricCutoverFloor.Set(float64(floor))
		metricAllocatorEpoch.Set(float64(newEpoch))
		return true, err
	}
	if err != nil {
		var floorErr *cutover.FloorError
		switch {
		case errors.Is(err, cutover.ErrStateMissing):
			return false, ErrStateRowMissing
		case errors.Is(err, cutover.ErrUnknownMode):
			return false, fmt.Errorf("%w: %v", ErrUnknownMode, err)
		case errors.As(err, &floorErr) && floorErr.TooHigh:
			// Upper bound: reserveTransactional rejects cur > maxSafeInteger-count,
			// so a floor without at least one full batch of headroom would activate
			// cleanly and then fail every reservation with ErrOverflow (a write
			// outage). Rejected before any state change.
			return false, fmt.Errorf("%w: floor=%d max=%d", ErrFloorTooHigh, floorErr.Floor, floorErr.Max)
		case errors.As(err, &floorErr):
			return false, fmt.Errorf("%w: floor=%d observed max=%d", ErrFloorTooLow, floorErr.Floor, floorErr.Observed)
		case errors.Is(err, cutover.ErrFlipInvariant):
			metricInvariantViolationTotal.Inc()
			return false, ErrInvariantViolation
		default:
			return false, err
		}
	}
	return false, nil // already transactional — idempotent
}

// observeUnderDrainBarrier recomputes the issued ceiling with the state row
// held FOR UPDATE: the MySQL maxima are final because every writer holds the
// row FOR SHARE until commit, and the Redis cursor scan is authoritative only
// under the runbook's /message/extra/sync drain.
func (s *Store) observeUnderDrainBarrier(ctx context.Context, tx *dbr.Tx) (int64, error) {
	maxVersion, maxSeq, err := s.observedMaxima(ctx, tx)
	if err != nil {
		return 0, err
	}
	observed := maxVersion
	if maxSeq > observed {
		observed = maxSeq
	}
	redisEvidence, err := s.observeRedisCursorEvidence(observed)
	if err != nil {
		return 0, err
	}
	if redisEvidence.maxCursor > observed {
		observed = redisEvidence.maxCursor
	}
	return observed, nil
}

// observedMaxima returns MAX(message_extra.version) and the max legacy GenSeq
// block boundary (MAX(seq.min_seq) over the messageExtra keys). Both aggregates
// always return one row, so a missing row is impossible; any driver error other
// than that is surfaced. The querier is the caller's tx (under the drain barrier)
// or a plain session (read-only Preflight).
func (s *Store) observedMaxima(ctx context.Context, q dbr.SessionRunner) (maxVersion, maxSeq int64, err error) {
	if err = q.SelectBySql(
		"SELECT COALESCE(MAX(`version`),0) FROM `message_extra`",
	).LoadOneContext(ctx, &maxVersion); err != nil {
		return 0, 0, fmt.Errorf("msgextraseq: observe max message_extra version: %w", err)
	}
	legacyPrefix := fmt.Sprintf("seq:%s:%%", common.MessageExtraSeqKey)
	if err = q.SelectBySql(
		"SELECT COALESCE(MAX(`min_seq`),0) FROM `seq` WHERE `key` LIKE ?",
		legacyPrefix,
	).LoadOneContext(ctx, &maxSeq); err != nil {
		return 0, 0, fmt.Errorf("msgextraseq: observe max legacy seq: %w", err)
	}
	return maxVersion, maxSeq, nil
}
