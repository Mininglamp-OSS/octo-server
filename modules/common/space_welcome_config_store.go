package common

import (
	"context"
	"errors"
	"fmt"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// Per-Space onboarding welcome config (task space-welcome-per-space-admin-crud).
//
// This is the per-Space counterpart to the platform-global onboarding.space_*
// keys in system_setting. A Space admin (Role>=1) manages one row per Space via
// modules/space's /v1/space/:space_id/welcome CRUD; modules/notify reads the
// effective config on the delivery path. The store is a stateless read-through
// (PK-indexed), so an admin write is visible on the next read across replicas
// with no snapshot TTL — unlike the global fallback, which retains the 60s
// SystemSettings snapshot window.
//
// Time discipline mirrors the delivery ledger: created_at/updated_at are
// application-computed UTC values passed as bound parameters (never NOW());
// active_from is stored as the RFC3339 UTC string so ParsedActiveFrom /
// ValidateSpaceWelcomeCombination apply unchanged.

// SpaceWelcomeMessageMaxRunes is the max welcome body length in Unicode code
// points, enforced on every write path (per-Space CRUD and the manager global
// config). Exported so modules/space can bound its column write without
// duplicating the constant.
const SpaceWelcomeMessageMaxRunes = spaceWelcomeMessageMaxRunes

// SpaceWelcomeActiveFromMaxLen bounds the active_from string to its storage
// column width (VARCHAR(40) in the config migration). Enforced on the write path
// independently of enabled/parse: a disabled config skips the RFC3339 parse
// entirely, and time.Parse otherwise accepts arbitrarily long fractional
// seconds, so without this guard an over-column value could reach the upsert and
// fail as a driver/store 500 (strict sql_mode) or truncate silently (permissive).
const SpaceWelcomeActiveFromMaxLen = 40

const spaceWelcomeConfigTable = "octo_space_welcome_config"

// SpaceWelcomeSpaceConfig is one per-Space welcome config row.
type SpaceWelcomeSpaceConfig struct {
	SpaceID       string
	Enabled       bool
	ActiveFromRaw string // RFC3339 UTC string; "" when unset
	Message       string
	UpdatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// spaceWelcomeConfigRow is the DB projection (active_from is a plain string
// column: "" means unset, avoiding a nullable-scan dependency).
type spaceWelcomeConfigRow struct {
	SpaceID    string    `db:"space_id"`
	Enabled    int       `db:"enabled"`
	ActiveFrom string    `db:"active_from"`
	Message    string    `db:"message"`
	UpdatedBy  string    `db:"updated_by"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

func (r spaceWelcomeConfigRow) toConfig() SpaceWelcomeSpaceConfig {
	return SpaceWelcomeSpaceConfig{
		SpaceID:       r.SpaceID,
		Enabled:       r.Enabled == 1,
		ActiveFromRaw: r.ActiveFrom,
		Message:       r.Message,
		UpdatedBy:     r.UpdatedBy,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// SpaceWelcomeConfigStore is the DB access layer for per-Space welcome config.
type SpaceWelcomeConfigStore struct {
	db *dbr.Session
}

// NewSpaceWelcomeConfigStore builds a store over the given session.
func NewSpaceWelcomeConfigStore(db *dbr.Session) *SpaceWelcomeConfigStore {
	return &SpaceWelcomeConfigStore{db: db}
}

const spaceWelcomeConfigCols = "space_id, enabled, active_from, message, updated_by, created_at, updated_at"

// Get returns the per-Space config row; found=false when absent (not an error).
func (s *SpaceWelcomeConfigStore) Get(ctx context.Context, spaceID string) (*SpaceWelcomeSpaceConfig, bool, error) {
	if spaceID == "" {
		return nil, false, nil
	}
	var row spaceWelcomeConfigRow
	err := s.db.SelectBySql(
		"SELECT "+spaceWelcomeConfigCols+" FROM "+spaceWelcomeConfigTable+" WHERE space_id=?",
		spaceID,
	).LoadOneContext(ctx, &row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get space welcome config: %w", err)
	}
	cfg := row.toConfig()
	return &cfg, true, nil
}

// Exists reports whether a per-Space row exists for spaceID (any enabled value).
// Used to decide whether the global fallback is overridden.
func (s *SpaceWelcomeConfigStore) Exists(ctx context.Context, spaceID string) (bool, error) {
	if spaceID == "" {
		return false, nil
	}
	var n int
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM "+spaceWelcomeConfigTable+" WHERE space_id=?",
		spaceID,
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("exists space welcome config: %w", err)
	}
	return n > 0, nil
}

// Upsert inserts or updates the per-Space config. An empty ActiveFromRaw is
// stored as the empty string ("" = unset). now is the application-computed UTC
// value written to created_at (on insert) and updated_at (both).
func (s *SpaceWelcomeConfigStore) Upsert(ctx context.Context, cfg SpaceWelcomeSpaceConfig, now time.Time) error {
	enabled := 0
	if cfg.Enabled {
		enabled = 1
	}
	_, err := s.db.InsertBySql(
		"INSERT INTO "+spaceWelcomeConfigTable+" (space_id, enabled, active_from, message, updated_by, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE enabled=VALUES(enabled), active_from=VALUES(active_from), "+
			"message=VALUES(message), updated_by=VALUES(updated_by), updated_at=VALUES(updated_at)",
		cfg.SpaceID, enabled, cfg.ActiveFromRaw, cfg.Message, cfg.UpdatedBy, now, now,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("upsert space welcome config: %w", err)
	}
	return nil
}

// spaceWelcomeUpsertMaxAttempts bounds the insert-then-lock retry on a transient
// InnoDB deadlock / lock-wait-timeout (see UpsertMerged).
const spaceWelcomeUpsertMaxAttempts = 3

// UpsertMerged performs a read-modify-write of the per-Space config inside a
// transaction, serialized so that two admins PUTting the same Space concurrently
// never lose each other's fields — whether they are editing an existing config
// OR both creating it for the first time.
//
// It uses an insert-then-lock strategy: first an idempotent INSERT ... ON
// DUPLICATE KEY UPDATE ensures the row exists (no-op if it already does), THEN a
// SELECT ... FOR UPDATE locks that now-guaranteed row before merge runs. This is
// the fix for concurrent FIRST creates: a plain SELECT ... FOR UPDATE cannot
// lock a row that does not exist yet, so two creators would both merge onto
// defaults and the later write would clobber the earlier's disjoint field (or
// gap-lock deadlock under REPEATABLE READ). Doing the INSERT first — without a
// preceding gap-locking SELECT — gives the lock a real row to serialize on and
// avoids the gap-lock deadlock (the config table has a single UNIQUE key, so
// concurrent same-key inserts are a linear wait, not a cycle). A rare transient
// deadlock/lock-timeout is retried up to spaceWelcomeUpsertMaxAttempts.
//
// merge receives the current row (found=false when this PUT just created the
// default row) and returns the config to persist. A non-nil error from merge
// aborts the write (tx rolls back) and is returned verbatim — callers use it to
// surface validation failures without writing, and it is never retried. On
// success the persisted row, re-read inside the tx, is returned. now is the
// app-computed UTC timestamp for created_at/updated_at.
func (s *SpaceWelcomeConfigStore) UpsertMerged(
	ctx context.Context,
	spaceID string,
	now time.Time,
	merge func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error),
) (*SpaceWelcomeSpaceConfig, error) {
	if spaceID == "" {
		return nil, errors.New("space welcome upsert: empty space id")
	}
	var lastErr error
	for attempt := 1; attempt <= spaceWelcomeUpsertMaxAttempts; attempt++ {
		cfg, err := s.upsertMergedOnce(ctx, spaceID, now, merge)
		if err == nil {
			return cfg, nil
		}
		// A merge (validation) error and any non-transient DB error are terminal
		// and surfaced verbatim so the caller's errors.Is checks still work.
		if !isRetryableTxErr(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("upsert space welcome config: retries exhausted: %w", lastErr)
}

// upsertMergedOnce is a single insert-then-lock attempt; see UpsertMerged.
func (s *SpaceWelcomeConfigStore) upsertMergedOnce(
	ctx context.Context,
	spaceID string,
	now time.Time,
	merge func(cur *SpaceWelcomeSpaceConfig, found bool) (SpaceWelcomeSpaceConfig, error),
) (*SpaceWelcomeSpaceConfig, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin space welcome upsert tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 1) Ensure the row exists (idempotent). The ON DUPLICATE clause is a no-op
	//    (space_id=space_id) so an existing row's fields — and its created_at —
	//    are untouched. affected==1 => inserted (this PUT is a first create);
	//    affected==0 => the row already existed.
	res, err := tx.InsertBySql(
		"INSERT INTO "+spaceWelcomeConfigTable+" (space_id, enabled, active_from, message, updated_by, created_at, updated_at) "+
			"VALUES (?, 0, '', '', '', ?, ?) ON DUPLICATE KEY UPDATE space_id=space_id",
		spaceID, now, now,
	).ExecContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure space welcome row: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("ensure space welcome row affected: %w", err)
	}
	created := affected == 1

	// 2) Lock the now-existing row and read the current state under the lock.
	var row spaceWelcomeConfigRow
	if err := tx.SelectBySql(
		"SELECT "+spaceWelcomeConfigCols+" FROM "+spaceWelcomeConfigTable+" WHERE space_id=? FOR UPDATE",
		spaceID,
	).LoadOneContext(ctx, &row); err != nil {
		return nil, fmt.Errorf("lock space welcome config: %w", err)
	}

	// 3) Merge. found=false when we just created the default row, so the caller
	//    starts from its own defaults (identical to the inserted default row).
	var cur *SpaceWelcomeSpaceConfig
	if !created {
		c := row.toConfig()
		cur = &c
	}
	next, err := merge(cur, !created)
	if err != nil {
		return nil, err // terminal (validation): surfaced verbatim, tx rolls back
	}

	// 4) Persist with a plain UPDATE (row guaranteed present; created_at preserved).
	enabled := 0
	if next.Enabled {
		enabled = 1
	}
	if _, err := tx.UpdateBySql(
		"UPDATE "+spaceWelcomeConfigTable+" SET enabled=?, active_from=?, message=?, updated_by=?, updated_at=? WHERE space_id=?",
		enabled, next.ActiveFromRaw, next.Message, next.UpdatedBy, now, spaceID,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("update space welcome config: %w", err)
	}

	// 5) Re-read inside the tx so the returned view reflects the persisted row.
	var out spaceWelcomeConfigRow
	if err := tx.SelectBySql(
		"SELECT "+spaceWelcomeConfigCols+" FROM "+spaceWelcomeConfigTable+" WHERE space_id=?",
		spaceID,
	).LoadOneContext(ctx, &out); err != nil {
		return nil, fmt.Errorf("reload space welcome config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit space welcome upsert tx: %w", err)
	}
	cfg := out.toConfig()
	return &cfg, nil
}

// isRetryableTxErr reports whether err is a transient InnoDB deadlock (1213) or
// lock-wait timeout (1205) that a fresh attempt may resolve.
func isRetryableTxErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	return false
}

// Delete hard-deletes the per-Space config row. deleted=false when none existed
// (idempotent). After a delete the Space reverts to the global fallback (or off).
func (s *SpaceWelcomeConfigStore) Delete(ctx context.Context, spaceID string) (bool, error) {
	if spaceID == "" {
		return false, nil
	}
	res, err := s.db.DeleteFrom(spaceWelcomeConfigTable).
		Where("space_id=?", spaceID).ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("delete space welcome config: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListEnabled returns all enabled per-Space configs, ordered by space_id, for
// the reconciler / worker to iterate the set of Spaces with an active welcome.
func (s *SpaceWelcomeConfigStore) ListEnabled(ctx context.Context) ([]SpaceWelcomeSpaceConfig, error) {
	var rows []spaceWelcomeConfigRow
	_, err := s.db.SelectBySql(
		"SELECT "+spaceWelcomeConfigCols+" FROM "+spaceWelcomeConfigTable+" WHERE enabled=1 ORDER BY space_id",
	).LoadContext(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("list enabled space welcome configs: %w", err)
	}
	out := make([]SpaceWelcomeSpaceConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toConfig())
	}
	return out, nil
}

// ResolveEffectiveSpaceWelcome computes the config in effect for spaceID:
//
//   - a per-Space row (if present) wins outright, EVEN when disabled — a present
//     row lets an admin opt their Space out of a global campaign;
//   - otherwise the platform-global config applies iff it is enabled and its
//     space_welcome_space_id names this Space;
//   - otherwise the feature is off for this Space.
//
// The result reuses the SpaceWelcomeConfig shape so callers keep applying
// ValidateSpaceWelcomeCombination / ParsedActiveFrom unchanged.
func ResolveEffectiveSpaceWelcome(row *SpaceWelcomeSpaceConfig, found bool, global SpaceWelcomeConfig, spaceID string) SpaceWelcomeConfig {
	if found && row != nil {
		return SpaceWelcomeConfig{
			Enabled:       row.Enabled,
			SpaceID:       spaceID,
			ActiveFromRaw: row.ActiveFromRaw,
			Message:       row.Message,
		}
	}
	if global.Enabled && global.SpaceID == spaceID {
		return global
	}
	return SpaceWelcomeConfig{SpaceID: spaceID}
}
