package db

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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

	// Build the rewrite set: rows where old is present and new is absent.
	// Skipping rows where both are present (or where neither is present)
	// keeps the operation idempotent across restarts and concurrent rollouts.
	var rewrites [][2]string
	for old, new := range mapping {
		if !existing[old] {
			continue
		}
		if existing[new] {
			continue
		}
		rewrites = append(rewrites, [2]string{old, new})
	}
	if len(rewrites) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, "UPDATE gorp_migrations SET id = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, pair := range rewrites {
		if _, err := stmt.ExecContext(ctx, pair[1], pair[0]); err != nil {
			return fmt.Errorf("rewrite %s → %s: %w", pair[0], pair[1], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
