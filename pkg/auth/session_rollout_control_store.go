package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

const (
	rolloutStateTable        = "octo_session_rollout_state"
	rolloutAdvanceTable      = "octo_session_rollout_advance"
	defaultRecoveryMaxPerUID = 20
	rolloutInitializeRetries = 8
)

var ErrRolloutStateUninitialized = errors.New("auth: session rollout state is not initialized")

// RolloutState is the sole durable authority for the session rollout. Redis
// contains session data and scan leases only; losing or restoring Redis cannot
// move this value in either direction.
type RolloutState struct {
	Floor     SessionMode `db:"floor" json:"floor"`
	MaxPerUID int         `db:"max_per_uid" json:"max_per_uid"`
	Version   int64       `db:"version" json:"version"`
	Paused    bool        `db:"paused" json:"paused"`
	UpdatedAt time.Time   `db:"updated_at" json:"updated_at"`
}

// RolloutSeed is used only while taking authority over from the #725 Redis
// record and deprecated environment. Once the singleton exists, Initialize
// may raise it but can never lower it.
type RolloutSeed struct {
	Floor     SessionMode
	MaxPerUID int
	Actor     string
	Source    string
	RedisID   string
}

// ResolveRolloutSeed persists the strictest floor already in effect during
// takeover. A cap already carried by the #725 Redis record remains authoritative;
// the deprecated environment cap is only a fallback for older records without one.
func ResolveRolloutSeed(
	redisFloor, legacyMode SessionMode,
	redisMaxPerUID, legacyMaxPerUID int,
) (RolloutSeed, error) {
	for name, mode := range map[string]SessionMode{"redis floor": redisFloor, "legacy mode": legacyMode} {
		if mode != "" && !mode.valid() {
			return RolloutSeed{}, fmt.Errorf("auth: invalid %s %q", name, mode)
		}
	}
	for name, value := range map[string]int{"redis max_per_uid": redisMaxPerUID, "legacy max_per_uid": legacyMaxPerUID} {
		if value < 0 || value > sessionMaxPerUIDLimit {
			return RolloutSeed{}, fmt.Errorf("auth: invalid %s %d", name, value)
		}
	}
	floor := SessionModeExpand
	if redisFloor.valid() && redisFloor.rank() > floor.rank() {
		floor = redisFloor
	}
	if legacyMode.valid() && legacyMode.rank() > floor.rank() {
		floor = legacyMode
	}
	maxPerUID := redisMaxPerUID
	if maxPerUID <= 0 {
		maxPerUID = legacyMaxPerUID
	}
	if maxPerUID <= 0 {
		maxPerUID = defaultRecoveryMaxPerUID
	}
	return RolloutSeed{Floor: floor, MaxPerUID: maxPerUID}, nil
}

// RolloutControlStore owns the singleton state and its append-only advance
// evidence. All irreversible changes happen in one MySQL transaction.
type RolloutControlStore struct {
	db *dbr.Session
}

func NewRolloutControlStore(db *dbr.Session) *RolloutControlStore {
	if db == nil {
		panic("auth: NewRolloutControlStore requires a non-nil session")
	}
	return &RolloutControlStore{db: db}
}

func (s *RolloutControlStore) Load(ctx context.Context) (RolloutState, error) {
	var state RolloutState
	err := s.db.Select("floor", "max_per_uid", "version", "paused", "updated_at").
		From(rolloutStateTable).
		Where("singleton_id = ?", 1).
		LoadOneContext(ctx, &state)
	if errors.Is(err, dbr.ErrNotFound) {
		return RolloutState{}, ErrRolloutStateUninitialized
	}
	if err != nil {
		return RolloutState{}, fmt.Errorf("auth: load session rollout state: %w", err)
	}
	if err := validateRolloutState(state); err != nil {
		return RolloutState{}, err
	}
	return state, nil
}

// Initialize creates or monotonically adopts a takeover seed under a row lock.
// Concurrent starters serialize here; the strictest seed eventually wins.
func (s *RolloutControlStore) Initialize(ctx context.Context, seed RolloutSeed) (RolloutState, error) {
	var lastErr error
	for attempt := 0; attempt < rolloutInitializeRetries; attempt++ {
		state, err := s.initializeOnce(ctx, seed)
		if err == nil {
			return state, nil
		}
		if !isMySQLRetryableTakeover(err) {
			return RolloutState{}, err
		}
		lastErr = err
		if attempt == rolloutInitializeRetries-1 {
			break
		}
		delay := 10 * time.Millisecond << attempt
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return RolloutState{}, ctx.Err()
		case <-timer.C:
		}
	}
	return RolloutState{}, fmt.Errorf(
		"auth: initialize session rollout after %d attempts: %w",
		rolloutInitializeRetries,
		lastErr,
	)
}

func (s *RolloutControlStore) initializeOnce(ctx context.Context, seed RolloutSeed) (RolloutState, error) {
	if !seed.Floor.valid() {
		return RolloutState{}, fmt.Errorf("auth: initialize rollout with invalid floor %q", seed.Floor)
	}
	if seed.MaxPerUID <= 0 || seed.MaxPerUID > sessionMaxPerUIDLimit {
		return RolloutState{}, errors.New("auth: initialize rollout requires bounded max_per_uid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RolloutState{}, fmt.Errorf("auth: begin rollout initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, found, err := loadRolloutStateForUpdate(ctx, tx)
	if err != nil {
		return RolloutState{}, err
	}
	if !found {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO "+rolloutStateTable+" (singleton_id, floor, max_per_uid, version, paused) VALUES (1, ?, ?, 1, 0)",
			string(seed.Floor), seed.MaxPerUID,
		); err != nil {
			return RolloutState{}, fmt.Errorf("auth: insert session rollout state: %w", err)
		}
		if err := insertRolloutAdvance(ctx, tx, RolloutAdvanceRecord{
			From: "", To: seed.Floor, Actor: firstNonEmpty(seed.Actor, "startup"), RedisID: seed.RedisID,
		}, firstNonEmpty(seed.Source, "bootstrap")); err != nil {
			return RolloutState{}, err
		}
		current = RolloutState{Floor: seed.Floor, MaxPerUID: seed.MaxPerUID, Version: 1}
	} else if seed.Floor.rank() > current.Floor.rank() {
		result, err := tx.ExecContext(ctx,
			"UPDATE "+rolloutStateTable+" SET floor = ?, max_per_uid = ?, version = version + 1, updated_at = CURRENT_TIMESTAMP WHERE singleton_id = 1 AND version = ? AND floor = ?",
			string(seed.Floor), seed.MaxPerUID, current.Version, string(current.Floor),
		)
		if err != nil {
			return RolloutState{}, fmt.Errorf("auth: adopt legacy session rollout state: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return RolloutState{}, fmt.Errorf("auth: inspect rollout adoption: %w", err)
		}
		if changed != 1 {
			return RolloutState{}, ErrRolloutControlChanged
		}
		if err := insertRolloutAdvance(ctx, tx, RolloutAdvanceRecord{
			From: current.Floor, To: seed.Floor, Actor: firstNonEmpty(seed.Actor, "startup"), RedisID: seed.RedisID,
		}, firstNonEmpty(seed.Source, "legacy-takeover")); err != nil {
			return RolloutState{}, err
		}
		current.Floor = seed.Floor
		current.MaxPerUID = seed.MaxPerUID
		current.Version++
	}
	if err := tx.Commit(); err != nil {
		return RolloutState{}, fmt.Errorf("auth: commit rollout initialization: %w", err)
	}
	return current, nil
}

func isMySQLRetryableTakeover(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	switch mysqlErr.Number {
	case 1062, 1205, 1213: // duplicate singleton, lock wait timeout, deadlock victim
		return true
	default:
		return false
	}
}

// Advance atomically records the evidence and performs a monotonic, one-phase
// CAS. A loser rolls the evidence row back with the state update.
func (s *RolloutControlStore) Advance(ctx context.Context, current RolloutState, record RolloutAdvanceRecord) (RolloutState, error) {
	if record.From != current.Floor || record.To.rank() != current.Floor.rank()+1 {
		return RolloutState{}, ErrRolloutFloorNotNext
	}
	maxPerUID := current.MaxPerUID
	if maxPerUID <= 0 || maxPerUID > sessionMaxPerUIDLimit {
		return RolloutState{}, ErrSessionCapUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RolloutState{}, fmt.Errorf("auth: begin rollout advance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertRolloutAdvance(ctx, tx, record, "advance"); err != nil {
		return RolloutState{}, err
	}
	result, err := tx.ExecContext(ctx,
		"UPDATE "+rolloutStateTable+" SET floor = ?, max_per_uid = ?, version = version + 1, updated_at = CURRENT_TIMESTAMP WHERE singleton_id = 1 AND version = ? AND floor = ? AND paused = 0",
		string(record.To), maxPerUID, current.Version, string(current.Floor),
	)
	if err != nil {
		return RolloutState{}, fmt.Errorf("auth: advance session rollout state: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return RolloutState{}, fmt.Errorf("auth: inspect session rollout advance: %w", err)
	}
	if changed != 1 {
		return RolloutState{}, ErrRolloutControlChanged
	}
	if err := tx.Commit(); err != nil {
		return RolloutState{}, fmt.Errorf("auth: commit session rollout advance: %w", err)
	}
	current.Floor = record.To
	current.Version++
	return current, nil
}

func (s *RolloutControlStore) SetPaused(ctx context.Context, paused bool) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE "+rolloutStateTable+" SET paused = ?, version = version + 1, updated_at = CURRENT_TIMESTAMP WHERE singleton_id = 1",
		paused,
	)
	if err != nil {
		return fmt.Errorf("auth: set session rollout pause: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: inspect session rollout pause: %w", err)
	}
	if changed != 1 {
		return ErrRolloutStateUninitialized
	}
	return nil
}

func (s *RolloutControlStore) LastAdvance(ctx context.Context) (*RolloutAdvanceRecord, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT from_floor, to_floor, actor, evidence_json, redis_run_id, writer_fingerprint, transition_kind, UNIX_TIMESTAMP(created_at) * 1000 FROM "+rolloutAdvanceTable+" ORDER BY id DESC LIMIT 1",
	)
	var record RolloutAdvanceRecord
	var evidence []byte
	if err := row.Scan(
		&record.From,
		&record.To,
		&record.Actor,
		&evidence,
		&record.RedisID,
		&record.WriterFingerprint,
		&record.Kind,
		&record.AtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("auth: load last rollout advance: %w", err)
	}
	if len(evidence) > 0 && string(evidence) != "null" {
		var observation SessionObservation
		if err := json.Unmarshal(evidence, &observation); err != nil {
			return nil, fmt.Errorf("auth: decode rollout advance evidence: %w", err)
		}
		record.Observation = &observation
	}
	return &record, nil
}

func loadRolloutStateForUpdate(ctx context.Context, tx *dbr.Tx) (RolloutState, bool, error) {
	var state RolloutState
	err := tx.QueryRowContext(ctx,
		"SELECT floor, max_per_uid, version, paused, updated_at FROM "+rolloutStateTable+" WHERE singleton_id = 1 FOR UPDATE",
	).Scan(&state.Floor, &state.MaxPerUID, &state.Version, &state.Paused, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RolloutState{}, false, nil
	}
	if err != nil {
		return RolloutState{}, false, fmt.Errorf("auth: lock session rollout state: %w", err)
	}
	if err := validateRolloutState(state); err != nil {
		return RolloutState{}, false, err
	}
	return state, true, nil
}

func insertRolloutAdvance(ctx context.Context, tx *dbr.Tx, record RolloutAdvanceRecord, kind string) error {
	evidence, err := json.Marshal(record.Observation)
	if err != nil {
		return fmt.Errorf("auth: encode rollout advance evidence: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO "+rolloutAdvanceTable+" (from_floor, to_floor, actor, evidence_json, redis_run_id, writer_fingerprint, transition_kind) VALUES (?, ?, ?, ?, ?, ?, ?)",
		string(record.From), string(record.To), firstNonEmpty(record.Actor, "unknown"), evidence,
		record.RedisID, record.WriterFingerprint, kind,
	)
	if err != nil {
		return fmt.Errorf("auth: insert rollout advance evidence: %w", err)
	}
	return nil
}

func validateRolloutState(state RolloutState) error {
	if !state.Floor.valid() {
		return fmt.Errorf("auth: invalid persisted rollout floor %q", state.Floor)
	}
	if state.MaxPerUID <= 0 || state.MaxPerUID > sessionMaxPerUIDLimit {
		return fmt.Errorf("auth: invalid persisted rollout max_per_uid %d", state.MaxPerUID)
	}
	if state.Version <= 0 {
		return fmt.Errorf("auth: invalid persisted rollout version %d", state.Version)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
