package msgextraseq

// Operator activation surface for #627 (PR-3). Preflight is read-only and
// recommends a safe cutover floor; Activate flips the DB-authoritative state row
// legacy→transactional under an exclusive lock that drains in-flight writers.
// Neither runs in a request path — they are invoked by the tools/msgextra-version
// operator command.
//
// There is deliberately no online "deactivate": rolling back to legacy cannot be
// done safely by a single DB flip. octo-lib's GenSeq HiLo allocator caches its
// block in process memory, so a replica that cached a pre-activation legacy block
// would resume issuing versions below the transactional high-water the instant
// mode flips back — re-opening the exact skip window #627 closes — and a DB-only
// change cannot invalidate that in-memory cache. Rollback is therefore a
// documented, maintenance-window coordinated procedure (drain writes → raise the
// legacy seq boundaries above the transactional max → restart every replica →
// flip mode); see tools/msgextra-version/README.md §5.
//
// The cutover floor must be at least every version already handed out (or the
// transactional sequence could reissue a value ≤ an existing one and re-open the
// skip window) and at most MaxCutoverFloor (or every reservation would fail
// ErrOverflow). The floor evidence is the max persisted message_extra.version and
// the max legacy GenSeq block boundary (seq.min_seq for the messageExtra keys);
// Activate recomputes and enforces both bounds under the drain barrier so a
// stale/out-of-range floor is rejected rather than trusted.

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/gocraft/dbr/v2"
)

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
	// RecommendedFloor is the max of the two maxima: the smallest floor that
	// cannot reissue an already-used version.
	RecommendedFloor int64
}

// Preflight reads (no locks, no writes) the maxima that bound already-issued
// versions and reports a safe cutover floor plus the current state. It never
// mutates anything, so it is safe to run against production at any time.
func (s *Store) Preflight() (PreflightResult, error) {
	var res PreflightResult

	var state struct {
		Mode         int    `db:"mode"`
		Epoch        uint64 `db:"epoch"`
		CutoverFloor int64  `db:"cutover_floor"`
	}
	err := s.ctx.DB().SelectBySql(
		"SELECT `mode`, `epoch`, `cutover_floor` FROM `octo_message_extra_version_state` WHERE `singleton_id`=?",
		stateSingletonID,
	).LoadOne(&state)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return PreflightResult{}, ErrStateRowMissing
		}
		return PreflightResult{}, fmt.Errorf("msgextraseq: preflight read state: %w", err)
	}
	res.CurrentMode = state.Mode
	res.CurrentEpoch = state.Epoch
	res.CurrentFloor = state.CutoverFloor

	maxVersion, maxSeq, err := s.observedMaxima(s.ctx.DB())
	if err != nil {
		return PreflightResult{}, err
	}
	res.MaxMessageExtraVersion = maxVersion
	res.MaxLegacySeqBoundary = maxSeq
	res.RecommendedFloor = maxVersion
	if maxSeq > res.RecommendedFloor {
		res.RecommendedFloor = maxSeq
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
func (s *Store) Activate(floor int64) (bool, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("msgextraseq: activate begin: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	mode, epoch, err := s.lockStateForUpdate(tx)
	if err != nil {
		return false, err
	}
	switch mode {
	case ModeTransactional:
		return false, nil // already active — idempotent
	case ModeLegacy:
		// proceed
	default:
		return false, fmt.Errorf("%w: %d", ErrUnknownMode, mode)
	}

	// Recompute the maxima under the drain barrier so a concurrently-committing
	// legacy writer cannot have raised them after the caller's Preflight.
	maxVersion, maxSeq, err := s.observedMaxima(tx)
	if err != nil {
		return false, err
	}
	observed := maxVersion
	if maxSeq > observed {
		observed = maxSeq
	}
	if floor < observed {
		return false, fmt.Errorf("%w: floor=%d observed max=%d", ErrFloorTooLow, floor, observed)
	}
	// Upper bound: reserveTransactional rejects cur > maxSafeInteger-count, so a
	// floor without at least one full batch of headroom would activate cleanly and
	// then fail every reservation with ErrOverflow (a write outage). Reject it
	// before changing state.
	if floor > MaxCutoverFloor {
		return false, fmt.Errorf("%w: floor=%d max=%d", ErrFloorTooHigh, floor, int64(MaxCutoverFloor))
	}

	res, err := tx.UpdateBySql(
		"UPDATE `octo_message_extra_version_state` SET `mode`=?, `cutover_floor`=?, `epoch`=? WHERE `singleton_id`=? AND `mode`=?",
		ModeTransactional, floor, epoch+1, stateSingletonID, ModeLegacy,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("msgextraseq: activate update: %w", err)
	}
	// Defense-in-depth: we read mode==ModeLegacy while holding the state row
	// FOR UPDATE, so the mode-conditional UPDATE must match exactly one row. A
	// zero-row result means the drain barrier was lost (e.g. a future refactor drops
	// lockStateForUpdate) and a concurrent flip slipped in — surface it instead of
	// committing and reporting a phantom flipped=true. Mirrors reserveTransactional's
	// post-write invariant guard.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		metricInvariantViolationTotal.Inc()
		return false, ErrInvariantViolation
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("msgextraseq: activate commit: %w", err)
	}
	setAllocatorModeGauge(ModeTransactional)
	metricCutoverFloor.Set(float64(floor))
	metricAllocatorEpoch.Set(float64(epoch + 1))
	return true, nil
}

// lockStateForUpdate takes the exclusive drain-barrier lock on the singleton
// state row and returns its mode and epoch.
func (s *Store) lockStateForUpdate(tx *dbr.Tx) (mode int, epoch uint64, err error) {
	var row struct {
		Mode  int    `db:"mode"`
		Epoch uint64 `db:"epoch"`
	}
	err = tx.SelectBySql(
		"SELECT `mode`, `epoch` FROM `octo_message_extra_version_state` WHERE `singleton_id`=? FOR UPDATE",
		stateSingletonID,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return 0, 0, ErrStateRowMissing
		}
		return 0, 0, fmt.Errorf("msgextraseq: lock state: %w", err)
	}
	return row.Mode, row.Epoch, nil
}

// observedMaxima returns MAX(message_extra.version) and the max legacy GenSeq
// block boundary (MAX(seq.min_seq) over the messageExtra keys). Both aggregates
// always return one row, so a missing row is impossible; any driver error other
// than that is surfaced. The querier is the caller's tx (under the drain barrier)
// or a plain session (read-only Preflight).
func (s *Store) observedMaxima(q dbr.SessionRunner) (maxVersion, maxSeq int64, err error) {
	if err = q.SelectBySql(
		"SELECT COALESCE(MAX(`version`),0) FROM `message_extra`",
	).LoadOne(&maxVersion); err != nil {
		return 0, 0, fmt.Errorf("msgextraseq: observe max message_extra version: %w", err)
	}
	legacyPrefix := fmt.Sprintf("seq:%s:%%", common.MessageExtraSeqKey)
	if err = q.SelectBySql(
		"SELECT COALESCE(MAX(`min_seq`),0) FROM `seq` WHERE `key` LIKE ?",
		legacyPrefix,
	).LoadOne(&maxSeq); err != nil {
		return 0, 0, fmt.Errorf("msgextraseq: observe max legacy seq: %w", err)
	}
	return maxVersion, maxSeq, nil
}
