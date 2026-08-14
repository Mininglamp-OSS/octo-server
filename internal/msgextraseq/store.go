package msgextraseq

// Package msgextraseq is the shared, transactional message_extra version
// allocator for octo-server (#627). It is a leaf package: it depends only on
// octo-lib (config.Context / GenSeq / DB), common and the pkg/cutover control
// plane (itself a leaf), and imports no modules/* package, so modules/message,
// modules/bot_api, modules/robot and internal/carddispatch can all import it
// without an import cycle.
//
// The allocator is selected at runtime by the DB-authoritative state row
// octo_message_extra_version_state (brief D2/D9). In mode=legacy it delegates to
// the process-local octo-lib GenSeq HiLo allocator; reserveLegacy here will
// become the single allowlisted GenSeq(MessageExtraSeqKey) call site once the
// existing production callers are migrated and the PR-2c source-guard test lands
// (today those callers still call GenSeq directly). In mode=transactional it
// reserves versions from octo_message_extra_channel_seq under a row lock held
// until the caller commits, so commit order == version order and versions are
// unique per storage channel across replicas.
//
// The caller starts and owns the business transaction. ReserveTx never opens or
// commits an independent transaction, and holds every lock it takes until the
// caller commits/rolls back.

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Allocator modes, mirroring octo_message_extra_version_state.mode.
//
// Aliases of the shared constants rather than independent literals: cutover.Flip
// is what writes this column (`SET mode=cutover.ModeActive`), so declaring 0/1
// separately here would let the two drift. If they ever did, a flip would
// succeed and then readStateForShare — comparing against these — would see a
// mode matching neither and fail every message_extra write closed.
const (
	ModeLegacy        = cutover.ModeInactive
	ModeTransactional = cutover.ModeActive
)

// stateSingletonID is the fixed primary key of the single allocator-state row.
const stateSingletonID = cutover.SingletonID

// StateTable is the DB-authoritative allocator-state table (pkg/cutover shape:
// singleton_id / mode / epoch / cutover_floor).
const StateTable = "octo_message_extra_version_state"

// ExpectedModeEnv optionally declares the allocator mode a deployment expects.
// Unset makes no assertion; "legacy"/"transactional" fail closed on mismatch
// (brief D9 read-side guard). Exported for the `app cutover msgextra status`
// operator command, which reports the guard alongside the state row.
const ExpectedModeEnv = "OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE"

// MaxReserveCount bounds a single reservation (brief D3). It matches this
// module's existing chunk magnitude (api_manager.go). Callers that exceed it
// either reject client input or reserve in chunks.
const MaxReserveCount = 1000

// maxSafeInteger is 2^53-1: the largest integer a JavaScript client can
// represent exactly in JSON. Versions must never cross it (brief D3/D9).
const maxSafeInteger int64 = 1<<53 - 1

// MaxCutoverFloor is the largest cutover floor Activate accepts: it leaves at
// least one full MaxReserveCount batch of headroom below maxSafeInteger, so the
// first reservations after activation cannot immediately hit ErrOverflow.
const MaxCutoverFloor int64 = maxSafeInteger - MaxReserveCount

// Sentinel errors. Callers map these to their existing localized error contract;
// the allocator never writes an HTTP response itself (error-handling rule).
var (
	// ErrInvalidCount is returned when count <= 0 or count > MaxReserveCount.
	ErrInvalidCount = errors.New("msgextraseq: count out of range")
	// ErrOverflow is returned when a reservation would cross maxSafeInteger.
	ErrOverflow = errors.New("msgextraseq: version would exceed 2^53-1")
	// ErrUnknownMode is returned when the state row carries an unrecognized mode.
	ErrUnknownMode = errors.New("msgextraseq: unknown allocator mode")
	// ErrExpectedModeMismatch is returned when the resolved mode does not match the
	// deployment's OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE (or that env is malformed).
	ErrExpectedModeMismatch = errors.New("msgextraseq: allocator mode does not match expected")
	// ErrInvariantViolation is returned when a defensive post-write check fails.
	ErrInvariantViolation = errors.New("msgextraseq: allocator invariant violated")
)

// State is the decoded allocator-state row. epoch is surfaced for observability
// only and is never a write-abort condition (brief C1).
type State struct {
	Mode         int
	Epoch        uint64
	CutoverFloor int64
}

// Store is the shared allocator. Construct with New and reuse; it is safe for
// concurrent use (all mutable state lives in MySQL).
type Store struct {
	ctx    *config.Context
	logger interface {
		Error(string, ...zap.Field)
		Warn(string, ...zap.Field)
	}
	// guard is the expected-mode deployment guard, parsed once from
	// ExpectedModeEnv at construction (pkg/cutover semantics: unset asserts
	// nothing, malformed fails closed).
	guard cutover.ExpectedMode
}

// ExpectedModeSpellings is the authoritative set of values ExpectedModeEnv
// accepts, and the mode each one asserts. Anything else is malformed and fails
// closed.
//
// Exported so the operator command reports the guard using the same table the
// allocator enforces it with. A second hand-written copy in the CLI could
// disagree with the running server about whether a value is valid — the guard
// readout would then contradict the thing it is describing.
func ExpectedModeSpellings() map[string]int {
	return map[string]int{
		"legacy":        ModeLegacy,
		"transactional": ModeTransactional,
	}
}

// New builds a Store bound to the given octo-lib context.
func New(ctx *config.Context) *Store {
	return &Store{
		ctx:    ctx,
		logger: liblog.NewTLog("MsgExtraSeq"),
		guard:  cutover.ParseExpectedMode(os.Getenv(ExpectedModeEnv), ExpectedModeSpellings()),
	}
}

// ReserveTx reserves count message_extra versions for the given storage channel
// within the caller's transaction and returns them in assignment order.
//
// The returned slice always has len == count. In transactional mode the values
// are a contiguous ascending range; in legacy mode they are count distinct
// values from GenSeq (contiguity is not guaranteed under concurrency, matching
// pre-task behavior). Returning explicit per-item values — rather than a single
// "first" — keeps callers mode-agnostic and behavior-preserving across the
// Prepare window; see context.yaml.
//
// channelID must be the already-derived storage-channel id (personal chats pass
// the fake channel id). The allocator does no Space/ownership lookup; those
// gates stay in the caller (brief D6, space-isolation rule).
func (s *Store) ReserveTx(tx *dbr.Tx, channelID string, channelType uint8, count int) ([]int64, error) {
	start := time.Now()
	// Observe latency on every path (success, failure, blocked) so failed/blocked
	// reservations are not excluded from the histogram.
	defer func() { metricReserveSeconds.Observe(time.Since(start).Seconds()) }()
	if count <= 0 || count > MaxReserveCount {
		metricReserveTotal.WithLabelValues("failure").Inc()
		return nil, ErrInvalidCount
	}
	metricBatchSize.Observe(float64(count))

	st, err := s.readStateForShare(tx)
	if err != nil {
		metricReserveTotal.WithLabelValues("failure").Inc()
		s.logFailure("state", channelID, channelType, err)
		return nil, err
	}

	var versions []int64
	switch st.Mode {
	case ModeLegacy:
		versions, err = s.reserveLegacy(channelID, count)
	case ModeTransactional:
		versions, err = s.reserveTransactional(tx, channelID, channelType, count, st.CutoverFloor)
	default:
		metricReserveTotal.WithLabelValues("failure").Inc()
		s.logFailure("state", channelID, channelType, fmt.Errorf("%w: %d", ErrUnknownMode, st.Mode))
		return nil, ErrUnknownMode
	}
	if err != nil {
		metricReserveTotal.WithLabelValues("failure").Inc()
		return nil, err
	}

	metricReserveTotal.WithLabelValues("success").Inc()
	return versions, nil
}

// Mode returns the currently effective allocator mode, taking the same shared
// lock on the state row that ReserveTx does (held until the caller commits). It
// lets a caller pick a lock order that matches the mode BEFORE it reserves.
//
// This exists for the card write path (#627 D4): all writers must reserve the
// channel sequence (ReserveTx) before taking any message-row lock so the global
// lock order is uniform (state → channel-seq → message-row) and cannot deadlock.
// Card writers deliberately keep message-row-first in legacy mode (PR#548
// single-process version monotonicity, where GenSeq has no serializing lock), so
// they must know the mode up front to choose the order. Non-card writers are
// already seq-first unconditionally and do not need this.
//
// The double state read (Mode here, then again inside ReserveTx) is harmless: it
// is the same row under a shared lock the caller already holds.
func (s *Store) Mode(tx *dbr.Tx) (int, error) {
	st, err := s.readStateForShare(tx)
	if err != nil {
		return 0, err
	}
	return st.Mode, nil
}

// ObserveReserveRetry records that a caller retried its whole transaction after a
// retriable lock error (MySQL 1213 deadlock / 1205 lock-wait timeout). The
// allocator itself only emits success/failure; retry is a caller-side signal
// because only the caller owns the transaction it re-runs (metrics.go
// reserve_total result=retry, brief D8).
func (s *Store) ObserveReserveRetry() {
	metricReserveTotal.WithLabelValues("retry").Inc()
}

// readStateForShare loads the singleton allocator-state row under a shared lock
// held until the caller's transaction ends. The shared lock is the drain
// barrier for cutover: it is compatible across writers, but the exclusive
// cutover flip waits for all in-flight writers (brief D3/D9). A missing row
// defaults to legacy (the safe pre-task/pre-migration default); an unrecognized
// mode value or an unreadable row fails closed; epoch is not validated.
func (s *Store) readStateForShare(tx *dbr.Tx) (State, error) {
	lockStart := time.Now()
	var row struct {
		Mode         int    `db:"mode"`
		Epoch        uint64 `db:"epoch"`
		CutoverFloor int64  `db:"cutover_floor"`
	}
	err := tx.SelectBySql(
		"SELECT `mode`, `epoch`, `cutover_floor` FROM `"+StateTable+"` WHERE `singleton_id`=? FOR SHARE",
		stateSingletonID,
	).LoadOne(&row)
	metricStateLockWaitSeconds.Observe(time.Since(lockStart).Seconds())
	var st State
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			// A missing state row defaults to legacy — the safe pre-task behavior
			// (GenSeq), which is also what a deploy sees before the migration seeds
			// the row, and what test setups leave after CleanAllTables. This is NOT
			// fail-closed by itself; the expected-mode guard below is what makes a
			// lost/deleted row safe once a deployment expects transactional.
			st = State{Mode: ModeLegacy}
		} else {
			return State{}, fmt.Errorf("msgextraseq: read state: %w", err)
		}
	} else {
		if row.Mode != ModeLegacy && row.Mode != ModeTransactional {
			return State{}, fmt.Errorf("%w: %d", ErrUnknownMode, row.Mode)
		}
		st = State{Mode: row.Mode, Epoch: row.Epoch, CutoverFloor: row.CutoverFloor}
	}
	// Expected-mode guard (brief D9 read side): a deployment declares the mode it
	// expects via OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE. If the resolved mode
	// (including a missing row → legacy) does not match, fail closed. This is the
	// durable safety net that stops a lost/deleted state row from silently
	// re-enabling the legacy allocator after a transactional cutover. Unset (the
	// default, and what tests use) makes no assertion, preserving the legacy
	// default for pre-migration deploys and CleanAllTables-based test setups.
	if err := s.assertExpectedMode(st.Mode); err != nil {
		return State{}, err
	}
	// Refresh observability gauges from the resolved state.
	setAllocatorModeGauge(st.Mode)
	metricAllocatorEpoch.Set(float64(st.Epoch))
	metricCutoverFloor.Set(float64(st.CutoverFloor))
	return st, nil
}

// assertExpectedMode enforces the optional OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE
// deployment guard. Unset → no assertion. A malformed value, or a resolved mode
// that differs from the expectation, fails closed.
func (s *Store) assertExpectedMode(mode int) error {
	switch s.guard.Check(mode) {
	case cutover.GuardMalformed:
		return fmt.Errorf("%w: malformed %s", ErrExpectedModeMismatch, ExpectedModeEnv)
	case cutover.GuardMismatch:
		expected, _ := s.guard.Expected()
		return fmt.Errorf("%w: expected %d, resolved %d", ErrExpectedModeMismatch, expected, mode)
	default:
		return nil
	}
}

// reserveLegacy delegates to the process-local octo-lib GenSeq allocator. After
// the writer cutover this becomes the single allowlisted GenSeq(MessageExtraSeqKey)
// call site, enforced by the PR-2c source-guard test. count GenSeq calls yield
// count distinct values, matching pre-task per-message behavior.
func (s *Store) reserveLegacy(channelID string, count int) ([]int64, error) {
	key := fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, channelID)
	versions := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		v, err := s.ctx.GenSeq(key)
		if err != nil {
			return nil, fmt.Errorf("msgextraseq: legacy GenSeq: %w", err)
		}
		versions = append(versions, v)
	}
	return versions, nil
}

// reserveTransactional locks the per-channel sequence row FOR UPDATE, raises it
// to at least the cutover floor, advances it by count, and returns the
// contiguous range. The lock is held until the caller commits (brief D3/D4).
func (s *Store) reserveTransactional(tx *dbr.Tx, channelID string, channelType uint8, count int, floor int64) ([]int64, error) {
	cur, err := s.lockSequenceRow(tx, channelID, channelType, floor)
	if err != nil {
		return nil, err
	}
	// Per-allocation floor raise: makes a later (higher) cutover safe even when
	// the per-channel row survived an emergency legacy interval (brief D3).
	if cur < floor {
		cur = floor
	}
	// Overflow guard before any mutation (brief D3): preserve exact JS cursors.
	if cur > maxSafeInteger-int64(count) {
		s.logFailure("reserve", channelID, channelType, ErrOverflow)
		return nil, ErrOverflow
	}
	next := cur + int64(count)

	res, err := tx.UpdateBySql(
		"UPDATE `octo_message_extra_channel_seq` SET `last_version`=? WHERE `channel_id`=? AND `channel_type`=?",
		next, channelID, channelType,
	).Exec()
	if err != nil {
		s.logFailure("write", channelID, channelType, err)
		return nil, fmt.Errorf("msgextraseq: advance sequence: %w", err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		// The row we locked vanished — should be impossible under the lock.
		metricInvariantViolationTotal.Inc()
		s.logFailure("write", channelID, channelType, ErrInvariantViolation)
		return nil, ErrInvariantViolation
	}

	versions := make([]int64, count)
	for i := 0; i < count; i++ {
		versions[i] = cur + int64(i) + 1
	}
	return versions, nil
}

// lockSequenceRow returns the current last_version for the channel with the
// sequence row locked FOR UPDATE. If the row does not exist it is initialized
// concurrency-safely (brief D3): a race-safe upsert materializes it at the
// bootstrap baseline, then the locking read below locks an already-existing row.
//
// Ordering matters for deadlock-freedom: we must NEVER issue SELECT ... FOR
// UPDATE against a not-yet-existing row. Under REPEATABLE READ that takes a gap
// lock, and two concurrent first-use initializers holding compatible gap locks
// deadlock (MySQL 1213) on the subsequent insert. So a non-locking existence
// read gates the materialize step, and the FOR UPDATE only ever targets a row
// that already exists (a record lock — no gap, no first-use deadlock).
func (s *Store) lockSequenceRow(tx *dbr.Tx, channelID string, channelType uint8, floor int64) (int64, error) {
	if _, found, err := s.selectSequence(tx, channelID, channelType); err != nil {
		s.logFailure("lock", channelID, channelType, err)
		return 0, err
	} else if !found {
		// First use: compute the bootstrap baseline and materialize the row with a
		// race-safe upsert. INSERT ... ON DUPLICATE KEY UPDATE serializes concurrent
		// initializers on the row/insert-intention lock (they wait, they do not
		// deadlock) and never leaves a duplicate initializer on a private baseline.
		baseline, berr := s.computeBootstrap(tx, channelID, channelType, floor)
		if berr != nil {
			s.logFailure("initialize", channelID, channelType, berr)
			return 0, berr
		}
		if _, ierr := tx.InsertBySql(
			"INSERT INTO `octo_message_extra_channel_seq` (`channel_id`,`channel_type`,`last_version`) VALUES (?,?,?) ON DUPLICATE KEY UPDATE `last_version`=`last_version`",
			channelID, channelType, baseline,
		).Exec(); ierr != nil {
			s.logFailure("initialize", channelID, channelType, ierr)
			return 0, fmt.Errorf("msgextraseq: initialize sequence: %w", ierr)
		}
	}
	// The row now exists (pre-existing or just materialized): lock it. Time only
	// the FOR UPDATE wait here — starting the clock before the existence read /
	// bootstrap / insert above would fold that unrelated work into the reported
	// lock-wait signal (reviewer note on PR #644).
	lockStart := time.Now()
	cur, found, err := s.selectSequenceForUpdate(tx, channelID, channelType)
	metricLockWaitSeconds.Observe(time.Since(lockStart).Seconds())
	if err != nil {
		s.logFailure("lock", channelID, channelType, err)
		return 0, err
	}
	if !found {
		// Must exist after the upsert; a vanished row is an invariant violation.
		metricInvariantViolationTotal.Inc()
		s.logFailure("initialize", channelID, channelType, ErrInvariantViolation)
		return 0, ErrInvariantViolation
	}
	return cur, nil
}

// selectSequence reads the channel sequence row WITHOUT locking (no FOR UPDATE),
// so a missing row does not take a gap lock. found is false (nil error) when the
// row does not exist.
func (s *Store) selectSequence(tx *dbr.Tx, channelID string, channelType uint8) (int64, bool, error) {
	var row struct {
		LastVersion int64 `db:"last_version"`
	}
	err := tx.SelectBySql(
		"SELECT `last_version` FROM `octo_message_extra_channel_seq` WHERE `channel_id`=? AND `channel_type`=?",
		channelID, channelType,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("msgextraseq: read sequence: %w", err)
	}
	return row.LastVersion, true, nil
}

// selectSequenceForUpdate reads and locks the channel sequence row. found is
// false (with nil error) when the row does not yet exist.
func (s *Store) selectSequenceForUpdate(tx *dbr.Tx, channelID string, channelType uint8) (int64, bool, error) {
	var row struct {
		LastVersion int64 `db:"last_version"`
	}
	err := tx.SelectBySql(
		"SELECT `last_version` FROM `octo_message_extra_channel_seq` WHERE `channel_id`=? AND `channel_type`=? FOR UPDATE",
		channelID, channelType,
	).LoadOne(&row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("msgextraseq: lock sequence: %w", err)
	}
	return row.LastVersion, true, nil
}

// computeBootstrap derives the first-use baseline as the maximum of the cutover
// floor, the current maximum message_extra.version for the channel, and the
// legacy octo-lib seq.min_seq for the corresponding MessageExtraSeqKey channel
// key (brief D3). The legacy value is evidence, not a safety boundary; the floor
// is the authoritative one.
func (s *Store) computeBootstrap(tx *dbr.Tx, channelID string, channelType uint8, floor int64) (int64, error) {
	baseline := floor

	var maxExtra int64
	if err := tx.SelectBySql(
		"SELECT COALESCE(MAX(`version`),0) FROM `message_extra` WHERE `channel_id`=? AND `channel_type`=?",
		channelID, channelType,
	).LoadOne(&maxExtra); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return 0, fmt.Errorf("msgextraseq: bootstrap max message_extra: %w", err)
	}
	if maxExtra > baseline {
		baseline = maxExtra
	}

	// Legacy seq key has no channel_type: seq.key = "seq:messageExtra:<channelID>".
	legacyKey := fmt.Sprintf("seq:%s:%s", common.MessageExtraSeqKey, channelID)
	var legacyMin int64
	if err := tx.SelectBySql(
		"SELECT COALESCE(`min_seq`,0) FROM `seq` WHERE `key`=?",
		legacyKey,
	).LoadOne(&legacyMin); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return 0, fmt.Errorf("msgextraseq: bootstrap legacy seq: %w", err)
	}
	if legacyMin > baseline {
		baseline = legacyMin
	}
	return baseline, nil
}

// logFailure emits a redacted structured failure log (brief D8): channel key,
// stage, and MySQL error number only — never message/card content or payloads.
func (s *Store) logFailure(stage, channelID string, channelType uint8, err error) {
	s.logger.Error("message_extra version allocation failed",
		zap.String("stage", stage),
		zap.String("channel_id", channelID),
		zap.Uint8("channel_type", channelType),
		zap.Uint16("mysql_errno", mysqlErrno(err)),
		zap.Error(err),
	)
}

// mysqlErrno best-effort extracts the MySQL error number for logging; 0 when the
// error is not a driver error.
func mysqlErrno(err error) uint16 {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number
	}
	return 0
}
