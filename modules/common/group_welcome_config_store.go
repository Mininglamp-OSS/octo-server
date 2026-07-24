package common

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gocraft/dbr/v2"
)

// Per-group new-member welcome config (task group-welcome-message).
//
// This is the group counterpart to the per-Space welcome config
// (SpaceWelcomeConfigStore). A group owner/manager manages one row per group via
// modules/group's /v1/groups/:group_no/welcome CRUD; modules/notify reads the
// effective config on the delivery path. The store is a stateless read-through
// (PK-indexed), so an admin write is visible on the next read across replicas
// with no snapshot TTL.
//
// Unlike the Space feature there is NO platform-global fallback: a group's
// effective config is simply its own row iff present AND enabled, else off.
//
// Time discipline mirrors the delivery ledger: created_at/updated_at are
// application-computed UTC values passed as bound parameters (never NOW());
// active_from is stored as the RFC3339 UTC string so ParsedActiveFrom /
// ValidateGroupWelcomeCombination apply on the string form.

const (
	// GroupWelcomeMessageMaxRunes bounds the welcome body in Unicode code points,
	// enforced on the write path (protects the VARCHAR(2000) column).
	GroupWelcomeMessageMaxRunes = 2000
	// GroupWelcomeActiveFromMaxLen bounds the active_from string to its storage
	// column width (VARCHAR(40)). Enforced on the write path independently of
	// enabled/parse: a disabled config skips the RFC3339 parse, and time.Parse
	// otherwise accepts arbitrarily long fractional seconds, so without this guard
	// an over-column value could reach the upsert and fail as a driver 500 (strict
	// sql_mode) or truncate silently (permissive).
	GroupWelcomeActiveFromMaxLen = 40
)

const groupWelcomeConfigTable = "octo_group_welcome_config"

const groupWelcomeConfigCols = "group_no, enabled, active_from, message, updated_by, created_at, updated_at"

// GroupWelcomeConfig is one per-group welcome config row / the effective config
// consumed by the delivery path.
type GroupWelcomeConfig struct {
	GroupNo       string
	Enabled       bool
	ActiveFromRaw string // RFC3339 UTC string; "" when unset
	Message       string
	UpdatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ParsedActiveFrom parses ActiveFromRaw as RFC3339 and returns it in UTC. ok is
// false when the value is empty or unparseable — callers treat that as an
// invalid combination (fail closed) when the feature is enabled.
func (c GroupWelcomeConfig) ParsedActiveFrom() (time.Time, bool) {
	if c.ActiveFromRaw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.ActiveFromRaw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// ValidateGroupWelcomeCombination validates the tuple as a coherent combination.
//
//   - When disabled it always passes (a partial/empty config is fine while off).
//   - When enabled it requires a non-empty group_no, a parseable RFC3339
//     active_from, and a message non-empty (after trim) within
//     GroupWelcomeMessageMaxRunes.
//
// Returns the first offending field name ("" when valid). There is no group
// existence check here — the CRUD write path has already confirmed the path
// group is active via the authz gate.
func ValidateGroupWelcomeCombination(cfg GroupWelcomeConfig) (field string) {
	if !cfg.Enabled {
		return ""
	}
	if strings.TrimSpace(cfg.GroupNo) == "" {
		return "group_no"
	}
	if _, ok := cfg.ParsedActiveFrom(); !ok {
		return "active_from"
	}
	if strings.TrimSpace(cfg.Message) == "" || utf8.RuneCountInString(cfg.Message) > GroupWelcomeMessageMaxRunes {
		return "message"
	}
	return ""
}

// groupWelcomeConfigRow is the DB projection (active_from is a plain string
// column: "" means unset, avoiding a nullable-scan dependency).
type groupWelcomeConfigRow struct {
	GroupNo    string    `db:"group_no"`
	Enabled    int       `db:"enabled"`
	ActiveFrom string    `db:"active_from"`
	Message    string    `db:"message"`
	UpdatedBy  string    `db:"updated_by"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

func (r groupWelcomeConfigRow) toConfig() GroupWelcomeConfig {
	return GroupWelcomeConfig{
		GroupNo:       r.GroupNo,
		Enabled:       r.Enabled == 1,
		ActiveFromRaw: r.ActiveFrom,
		Message:       r.Message,
		UpdatedBy:     r.UpdatedBy,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// GroupWelcomeConfigStore is the DB access layer for per-group welcome config.
type GroupWelcomeConfigStore struct {
	db *dbr.Session
}

// NewGroupWelcomeConfigStore builds a store over the given session.
func NewGroupWelcomeConfigStore(db *dbr.Session) *GroupWelcomeConfigStore {
	return &GroupWelcomeConfigStore{db: db}
}

// Get returns the per-group config row; found=false when absent (not an error).
func (s *GroupWelcomeConfigStore) Get(ctx context.Context, groupNo string) (*GroupWelcomeConfig, bool, error) {
	if groupNo == "" {
		return nil, false, nil
	}
	var row groupWelcomeConfigRow
	err := s.db.SelectBySql(
		"SELECT "+groupWelcomeConfigCols+" FROM "+groupWelcomeConfigTable+" WHERE group_no=?",
		groupNo,
	).LoadOneContext(ctx, &row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get group welcome config: %w", err)
	}
	cfg := row.toConfig()
	return &cfg, true, nil
}

// Exists reports whether a per-group row exists for groupNo (any enabled value).
func (s *GroupWelcomeConfigStore) Exists(ctx context.Context, groupNo string) (bool, error) {
	if groupNo == "" {
		return false, nil
	}
	var n int
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM "+groupWelcomeConfigTable+" WHERE group_no=?",
		groupNo,
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("exists group welcome config: %w", err)
	}
	return n > 0, nil
}

// Upsert inserts or updates the per-group config in a single statement.
//
// NOTE: this is a simple idempotent write for single-writer / test contexts.
// Production admin CRUD MUST use UpsertMerged, which serializes concurrent PUTs
// (insert-then-lock + deadlock retry) to prevent lost partial updates when two
// managers edit the same group at once.
func (s *GroupWelcomeConfigStore) Upsert(ctx context.Context, cfg GroupWelcomeConfig, now time.Time) error {
	enabled := 0
	if cfg.Enabled {
		enabled = 1
	}
	_, err := s.db.InsertBySql(
		"INSERT INTO "+groupWelcomeConfigTable+" (group_no, enabled, active_from, message, updated_by, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE enabled=VALUES(enabled), active_from=VALUES(active_from), "+
			"message=VALUES(message), updated_by=VALUES(updated_by), updated_at=VALUES(updated_at)",
		cfg.GroupNo, enabled, cfg.ActiveFromRaw, cfg.Message, cfg.UpdatedBy, now, now,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("upsert group welcome config: %w", err)
	}
	return nil
}

// UpsertMerged performs a read-modify-write of the per-group config inside a
// transaction, serialized so two managers PUTting the same group concurrently
// never lose each other's fields — whether editing an existing config OR both
// creating it for the first time. It uses the same insert-then-lock strategy as
// the Space store: an idempotent INSERT ... ON DUPLICATE KEY UPDATE ensures the
// row exists, then SELECT ... FOR UPDATE locks it before merge runs. Doing the
// INSERT before any gap-locking read avoids the first-create gap-lock deadlock;
// the single UNIQUE key makes concurrent same-key inserts a linear wait. A rare
// transient deadlock/lock-timeout is retried (see isRetryableTxErr, shared with
// the Space store).
//
// merge receives the current row (found=false when this PUT just created the
// default row) and returns the config to persist; a non-nil merge error aborts
// the write (tx rolls back) and is returned verbatim (never retried). On success
// the persisted row, re-read inside the tx, is returned.
func (s *GroupWelcomeConfigStore) UpsertMerged(
	ctx context.Context,
	groupNo string,
	now time.Time,
	merge func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error),
) (*GroupWelcomeConfig, error) {
	if groupNo == "" {
		return nil, errors.New("group welcome upsert: empty group no")
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		cfg, err := s.upsertMergedOnce(ctx, groupNo, now, merge)
		if err == nil {
			return cfg, nil
		}
		if !isRetryableTxErr(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("upsert group welcome config: retries exhausted: %w", lastErr)
}

func (s *GroupWelcomeConfigStore) upsertMergedOnce(
	ctx context.Context,
	groupNo string,
	now time.Time,
	merge func(cur *GroupWelcomeConfig, found bool) (GroupWelcomeConfig, error),
) (*GroupWelcomeConfig, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin group welcome upsert tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 1) Ensure the row exists (idempotent no-op if present; created_at untouched).
	res, err := tx.InsertBySql(
		"INSERT INTO "+groupWelcomeConfigTable+" (group_no, enabled, active_from, message, updated_by, created_at, updated_at) "+
			"VALUES (?, 0, '', '', '', ?, ?) ON DUPLICATE KEY UPDATE group_no=group_no",
		groupNo, now, now,
	).ExecContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure group welcome row: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("ensure group welcome row affected: %w", err)
	}
	created := affected == 1

	// 2) Lock the now-existing row and read the current state under the lock.
	var row groupWelcomeConfigRow
	if err := tx.SelectBySql(
		"SELECT "+groupWelcomeConfigCols+" FROM "+groupWelcomeConfigTable+" WHERE group_no=? FOR UPDATE",
		groupNo,
	).LoadOneContext(ctx, &row); err != nil {
		return nil, fmt.Errorf("lock group welcome config: %w", err)
	}

	// 3) Merge. found=false when we just created the default row.
	var cur *GroupWelcomeConfig
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
		"UPDATE "+groupWelcomeConfigTable+" SET enabled=?, active_from=?, message=?, updated_by=?, updated_at=? WHERE group_no=?",
		enabled, next.ActiveFromRaw, next.Message, next.UpdatedBy, now, groupNo,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("update group welcome config: %w", err)
	}

	// 5) Re-read inside the tx for the persisted view, then commit.
	var out groupWelcomeConfigRow
	if err := tx.SelectBySql(
		"SELECT "+groupWelcomeConfigCols+" FROM "+groupWelcomeConfigTable+" WHERE group_no=?",
		groupNo,
	).LoadOneContext(ctx, &out); err != nil {
		return nil, fmt.Errorf("reload group welcome config: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit group welcome upsert tx: %w", err)
	}
	cfg := out.toConfig()
	return &cfg, nil
}

// Delete hard-deletes the per-group config row. deleted=false when none existed
// (idempotent). After a delete the group reverts to off (no fallback).
func (s *GroupWelcomeConfigStore) Delete(ctx context.Context, groupNo string) (bool, error) {
	if groupNo == "" {
		return false, nil
	}
	res, err := s.db.DeleteFrom(groupWelcomeConfigTable).
		Where("group_no=?", groupNo).ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("delete group welcome config: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListEnabled returns all enabled per-group configs, ordered by group_no, for
// the reconciler / worker to iterate the set of groups with an active welcome.
func (s *GroupWelcomeConfigStore) ListEnabled(ctx context.Context) ([]GroupWelcomeConfig, error) {
	var rows []groupWelcomeConfigRow
	_, err := s.db.SelectBySql(
		"SELECT "+groupWelcomeConfigCols+" FROM "+groupWelcomeConfigTable+" WHERE enabled=1 ORDER BY group_no",
	).LoadContext(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("list enabled group welcome configs: %w", err)
	}
	out := make([]GroupWelcomeConfig, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toConfig())
	}
	return out, nil
}
