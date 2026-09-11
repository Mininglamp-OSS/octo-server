package db

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// migrationIDMapping is the old-filename → new-filename map produced by
// tools/migrate-rename. Embedding the file keeps the binary self-contained:
// after a release, a freshly-deployed octo-server still knows how to upgrade
// a database whose gorp_migrations table predates the timestamp-prefix
// rename.
//
//go:embed migration_id_mapping.json
var migrationIDMappingJSON []byte

type migrationIDMapping struct {
	Mapping map[string]string `json:"mapping"`
}

// RewriteLegacyMigrationIDs maps any legacy entries in `gorp_migrations.id`
// to their new timestamp-prefixed equivalents.
//
// Why this exists: sql-migrate (rubenv/sql-migrate@v1.5.2 migrate.go:135-146)
// falls back to lexicographic `m.Id < other.Id` when filenames don't start
// with digits, so the historical `<module>-<YYYYMMDD>-<NN>.sql` scheme
// ordered migrations by module name first — which caused cross-module
// dependencies like `botfather-20260417-01.sql` (ALTERs `robot`) to run
// before `robot-20210926-01.sql` (CREATEs the table). The fix is to rename
// every file to a 14-digit timestamp prefix; the cost is that any
// already-applied database has the old IDs in `gorp_migrations` and would
// otherwise hit sql-migrate's "unknown migration in database" safety check.
//
// This function is idempotent: it only rewrites rows whose old ID is present
// AND whose new ID is absent, and it leaves no trace on a fresh install
// (the table is empty, so the loop is a no-op).
//
// Call this once at startup, before any call to migrate.Exec / module.Setup.
func RewriteLegacyMigrationIDs(ctx context.Context, db *sql.DB) error {
	if err := ensureGorpMigrationsTable(ctx, db); err != nil {
		// gorp_migrations doesn't exist yet — fresh install. sql-migrate
		// will create the table during the upcoming migrate.Exec call,
		// and there are no legacy IDs to rewrite, so this is a clean
		// no-op rather than a startup failure.
		if errors.Is(err, errTableAbsent) {
			return nil
		}
		return fmt.Errorf("check gorp_migrations existence: %w", err)
	}

	mapping, err := loadMigrationIDMapping()
	if err != nil {
		return fmt.Errorf("load embedded mapping: %w", err)
	}
	if len(mapping) == 0 {
		return nil
	}

	existing, err := loadExistingMigrationIDs(ctx, db)
	if err != nil {
		return fmt.Errorf("read gorp_migrations: %w", err)
	}

	// Classify each mapping pair into the right action:
	//
	//   - only old in table  → UPDATE old → new (the canonical rename)
	//   - only new in table  → no-op (already migrated)
	//   - both in table      → DELETE old (a concurrent replica or a
	//                          previous partial run already wrote new;
	//                          leaving old behind would make sql-migrate
	//                          panic with "unknown migration in database"
	//                          when it sees an ID with no corresponding
	//                          embedded file)
	//   - neither in table   → no-op
	//
	// Splitting the "both" case out of the original "skip" branch is what
	// makes the shim safe under partial rollouts — without it, an aborted
	// or racing rewrite leaves dangling legacy IDs that the next plan
	// stage chokes on.
	var renames [][2]string
	var deletes []string
	for old, new := range mapping {
		if !existing[old] {
			continue
		}
		if existing[new] {
			deletes = append(deletes, old)
			continue
		}
		renames = append(renames, [2]string{old, new})
	}
	if len(renames) == 0 && len(deletes) == 0 {
		return nil
	}

	// Sort both lists by the old ID so every replica acquires row locks
	// in the same order, eliminating a deadlock window during multi-pod
	// rolling deploys. Map iteration order in Go is randomised, so without
	// this two pods could pick opposite orders on the same input.
	sort.Slice(renames, func(i, j int) bool { return renames[i][0] < renames[j][0] })
	sort.Strings(deletes)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(renames) > 0 {
		stmt, err := tx.PrepareContext(ctx, "UPDATE gorp_migrations SET id = ? WHERE id = ?")
		if err != nil {
			return fmt.Errorf("prepare update: %w", err)
		}
		for _, pair := range renames {
			if _, err := stmt.ExecContext(ctx, pair[1], pair[0]); err != nil {
				stmt.Close()
				return fmt.Errorf("rewrite %s → %s: %w", pair[0], pair[1], err)
			}
		}
		stmt.Close()
	}
	if len(deletes) > 0 {
		stmt, err := tx.PrepareContext(ctx, "DELETE FROM gorp_migrations WHERE id = ?")
		if err != nil {
			return fmt.Errorf("prepare delete: %w", err)
		}
		for _, old := range deletes {
			if _, err := stmt.ExecContext(ctx, old); err != nil {
				stmt.Close()
				return fmt.Errorf("delete dangling legacy id %s: %w", old, err)
			}
		}
		stmt.Close()
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// threadModuleSnapshotMigrationIDs lists the thread-module migration files
// whose effects are **already present** in the init-db.sql snapshot. These
// get pre-seeded into gorp_migrations on snapshot installs so sql-migrate
// treats them as already-applied and skips the CREATE TABLE / ADD COLUMN
// statements (which would otherwise hit Error 1050 on existing tables).
//
// !! When you move an ID into this list, the init-db.sql snapshot MUST
// already include the corresponding DDL. TestThreadModuleMigrationIDsMatchDisk
// can only check ID coverage, not whether the SQL actually landed in the
// snapshot — wrongly promoting an entry here silently skips a real DDL on
// snapshot installs. Always update the snapshot in the same change.
var threadModuleSnapshotMigrationIDs = []string{
	"20260402000001_thread_legacy01.sql",
	"20260402000002_thread_legacy02.sql",
	"20260410000003_thread_legacy01.sql",
	"20260413000001_thread_legacy01.sql",
	"20260422000001_thread_legacy01.sql",
	"20260511000001_thread_legacy01.sql",
}

// threadModulePostSnapshotMigrationIDs lists thread-module migrations added
// **after** the init-db.sql snapshot baseline. They are **not** pre-seeded
// in ReconcileThreadSchemaRecords — sql-migrate will detect them as pending
// and apply them on snapshot installs.
//
// Background (Jerry-Xin PR #123 round-4 warning): adding an ADD-INDEX-only
// migration to the snapshot list would let ReconcileThreadSchemaRecords
// mark it as already applied without ever creating the index, producing
// invisible schema drift. So new post-snapshot migrations go here instead;
// TestThreadModuleMigrationIDsMatchDisk asserts the union covers every file
// on disk so drift in either direction surfaces in CI.
var threadModulePostSnapshotMigrationIDs = []string{
	"20260522000002_thread_group_status_created_index.sql",
	"20260711000002_thread_unarchive_pending_mention_backfill.sql",
}

// ReconcileThreadSchemaRecords pre-seeds gorp_migrations with the thread
// module's migration IDs when the thread tables are already present from a
// prior snapshot-built install but the migration rows are missing. Without
// this, the next sql-migrate.Exec would try to apply CREATE TABLE `thread`
// (no IF NOT EXISTS) and panic with Error 1050.
//
// Why a separate step from RewriteLegacyMigrationIDs: the legacy-ID rewrite
// is a 1:1 in-place rename, while this is a "the schema is here, please
// record that" reconciliation. Splitting them keeps each function's
// invariants narrow.
//
// Idempotent on three axes:
//   - fresh install (no thread tables): no-op.
//   - already-reconciled install (rows present): no-op.
//   - already-applied install (sql-migrate ran the migrations itself): no-op.
//
// Call after RewriteLegacyMigrationIDs and before module.Setup.
func ReconcileThreadSchemaRecords(ctx context.Context, db *sql.DB) error {
	// All three tables have to be present before we treat the schema as
	// "already built". A partial state (e.g. only `thread` present) is a
	// corrupted DB we'd rather fail loudly on than mask.
	required := []string{"thread", "thread_member", "thread_setting"}
	rows, err := db.QueryContext(ctx,
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?, ?, ?)",
		required[0], required[1], required[2])
	if err != nil {
		return fmt.Errorf("probe thread tables: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scan thread table name: %w", err)
		}
		have[name] = true
	}
	rows.Close()
	if len(have) == 0 {
		// Fresh install path — sql-migrate will create the tables itself.
		return nil
	}
	if len(have) < len(required) {
		// Partial state: do nothing rather than mask a schema corruption.
		// sql-migrate will surface the underlying issue on first ALTER.
		return nil
	}

	// gorp_migrations may not exist yet on a brand-new database; if so
	// there's nothing to reconcile (sql-migrate will create the table and
	// apply the migrations cleanly).
	if err := ensureGorpMigrationsTable(ctx, db); err != nil {
		if errors.Is(err, errTableAbsent) {
			return nil
		}
		return fmt.Errorf("check gorp_migrations existence: %w", err)
	}

	existing, err := loadExistingMigrationIDs(ctx, db)
	if err != nil {
		return fmt.Errorf("read gorp_migrations: %w", err)
	}
	// Historically this loop used an all-or-nothing "anyPresent" gate: if
	// *any* thread-* ID was already recorded, the whole reconciliation was
	// skipped, on the theory that sql-migrate was already tracking the
	// module and any gap must be a genuine pending migration (e.g. a new
	// ADD INDEX).
	//
	// That gate has a hole (see #831): MySQL DDL auto-commits per
	// statement and is NOT covered by sql-migrate's surrounding
	// transaction, so a mid-migration failure — or two pods racing the
	// same migration during a single-replica RollingUpdate window — can
	// leave a migration's columns/tables physically applied while its
	// gorp_migrations row never got written. That produces a *partial*
	// thread-* ledger: some snapshot IDs present, some missing, even
	// though every ID's underlying schema effect already landed via
	// sql-migrate (not a snapshot). The old gate saw anyPresent=true and
	// bailed, leaving the missing ID pending; sql-migrate then replayed
	// its ADD COLUMN against a table that already had the column,
	// panicking with MySQL Error 1060 on every restart.
	//
	// Fix: judge each missing ID independently by checking whether its
	// actual DDL effect is already present in the schema (INFORMATION_SCHEMA
	// guard, same pattern as #252). Only pre-seed IDs whose effect we can
	// positively confirm; leave any ID we can't confirm as genuinely
	// pending so sql-migrate still applies it. This keeps both original
	// invariants (snapshot installs still get seeded; genuinely-new
	// post-snapshot migrations still run) while also covering the
	// partial-ledger case the anyPresent gate could not.
	var missing []string
	for _, id := range threadModuleSnapshotMigrationIDs {
		if !existing[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	appliedEffects, err := probeThreadSnapshotEffects(ctx, db)
	if err != nil {
		return fmt.Errorf("probe thread migration effects: %w", err)
	}

	var toRecord []string
	for _, id := range missing {
		if appliedEffects[id] {
			toRecord = append(toRecord, id)
		}
	}
	if len(toRecord) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// applied_at is the column gorp_migrations uses; the timestamp marks
	// "reconciled by shim" so the source of these rows is auditable.
	//
	// INSERT IGNORE rather than plain INSERT: two replicas can race past
	// the loadExistingMigrationIDs check above (both see "no thread-*"
	// at the same instant), both reach this INSERT, and the second one
	// would otherwise hit a duplicate primary key on commit. Treating
	// the dup as "the peer already did the work" is exactly the desired
	// semantics — by the time we get the IGNORE'd error, the row is
	// already there and sql-migrate's plan stage will be satisfied.
	stmt, err := tx.PrepareContext(ctx,
		"INSERT IGNORE INTO gorp_migrations (id, applied_at) VALUES (?, NOW())")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	for _, id := range toRecord {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("record %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// probeThreadSnapshotEffects reports, per snapshot-baseline migration ID,
// whether that migration's DDL effect is already physically present in the
// schema. This lets ReconcileThreadSchemaRecords judge each missing
// gorp_migrations row independently instead of relying on an all-or-nothing
// "any thread-* row present" gate (see #831): a partially-recorded ledger
// (some IDs present, some missing) no longer blocks seeding the ones whose
// columns/tables/index we can positively confirm already exist.
//
// Every check here must mirror the actual DDL in the corresponding
// modules/thread/sql/*.sql file. If a listed migration's SQL changes, this
// map must be updated in the same change, or a real pending migration could
// be wrongly marked as applied.
func probeThreadSnapshotEffects(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	cols, err := loadTableColumns(ctx, db, "thread")
	if err != nil {
		return nil, fmt.Errorf("load thread columns: %w", err)
	}
	tables, err := loadExistingTables(ctx, db, []string{"thread", "thread_member", "thread_setting"})
	if err != nil {
		return nil, fmt.Errorf("load thread tables: %w", err)
	}

	effects := map[string]bool{
		// CREATE TABLE `thread` (no IF NOT EXISTS).
		"20260402000001_thread_legacy01.sql": tables["thread"],
		// CREATE TABLE IF NOT EXISTS `thread_member`.
		"20260402000002_thread_legacy02.sql": tables["thread_member"],
		// Four ADD COLUMN statements on `thread`; only "applied" once all four
		// columns are present, matching the migration's own INFORMATION_SCHEMA
		// guards.
		"20260410000003_thread_legacy01.sql": cols["message_count"] && cols["last_message_at"] &&
			cols["last_message_content"] && cols["last_message_sender_uid"],
		// ADD COLUMN thread_md / thread_md_version / thread_md_updated_at /
		// thread_md_updated_by on `thread`.
		"20260413000001_thread_legacy01.sql": cols["thread_md"] && cols["thread_md_version"] &&
			cols["thread_md_updated_at"] && cols["thread_md_updated_by"],
		// CREATE TABLE `thread_setting` (no IF NOT EXISTS).
		"20260422000001_thread_legacy01.sql": tables["thread_setting"],
	}

	hasIdx, err := hasIndex(ctx, db, "thread", "idx_status_last_msg_id")
	if err != nil {
		return nil, fmt.Errorf("check thread index: %w", err)
	}
	// ADD INDEX idx_status_last_msg_id on `thread`.
	effects["20260511000001_thread_legacy01.sql"] = hasIdx

	return effects, nil
}

func loadTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func loadExistingTables(ctx context.Context, db *sql.DB, tables []string) (map[string]bool, error) {
	if len(tables) == 0 {
		return map[string]bool{}, nil
	}
	placeholders := make([]string, len(tables))
	args := make([]any, len(tables))
	for i, t := range tables {
		placeholders[i] = "?"
		args[i] = t
	}
	query := fmt.Sprintf(
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (%s)",
		strings.Join(placeholders, ", "))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func hasIndex(ctx context.Context, db *sql.DB, table, index string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?",
		table, index).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func loadMigrationIDMapping() (map[string]string, error) {
	var parsed migrationIDMapping
	if err := json.Unmarshal(migrationIDMappingJSON, &parsed); err != nil {
		return nil, err
	}
	return parsed.Mapping, nil
}

func ensureGorpMigrationsTable(ctx context.Context, db *sql.DB) error {
	// On a clean install gorp_migrations doesn't exist yet — sql-migrate will
	// create it during the first migrate.Exec call. In that case we have
	// nothing to rewrite and must not error.
	var name string
	err := db.QueryRowContext(ctx,
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'gorp_migrations'",
	).Scan(&name)
	if err == sql.ErrNoRows {
		return errTableAbsent
	}
	return err
}

var errTableAbsent = fmt.Errorf("gorp_migrations table absent (fresh install)")

func loadExistingMigrationIDs(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT id FROM gorp_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
