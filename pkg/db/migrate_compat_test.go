package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestRewriteLegacyMigrationIDs covers the five paths the upgrade shim has
// to handle. Each case primes a sqlmock conversation that matches what
// RewriteLegacyMigrationIDs actually runs against MySQL, then asserts the
// function's return value and that no expected query was left unfulfilled.
//
// The cases together exercise the contract that motivated the shim: a
// fresh install must be a clean no-op (so module.Setup can create the
// table afterwards), already-rewritten databases must not double-rewrite,
// and mixed states must only touch the still-legacy rows.
func TestRewriteLegacyMigrationIDs(t *testing.T) {
	// Pick one mapping entry to drive the row scenarios. Using a known pair
	// from the embedded mapping avoids hard-coding fixtures that drift away
	// from the real file.
	mapping, err := loadMigrationIDMapping()
	if err != nil {
		t.Fatalf("loadMigrationIDMapping: %v", err)
	}
	if len(mapping) == 0 {
		t.Fatal("embedded mapping is empty — pkg/db/migration_id_mapping.json missing or malformed")
	}
	var oldID, newID string
	for k, v := range mapping {
		oldID, newID = k, v
		break
	}

	t.Run("table absent — fresh install no-op", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		// information_schema lookup returns zero rows → ErrNoRows path.
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}))

		// Nothing else should run.
		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for absent table, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("empty table — no rewrites", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).
				AddRow("gorp_migrations"))
		// Existing IDs query returns nothing.
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}))

		// loadExisting returns empty set → no candidates → no transaction.
		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for empty table, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("legacy rows rewritten", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(oldID))

		// Begin → prepare → exec(new, old) → commit.
		mock.ExpectBegin()
		stmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"UPDATE gorp_migrations SET id = ? WHERE id = ?"))
		stmt.ExpectExec().
			WithArgs(newID, oldID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for legacy rewrite, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("already-new rows unchanged", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		// Only the new ID is present — old absent → skip; nothing to rewrite.
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(newID))

		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for already-new state, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("mixed — pair A has both old+new (delete old), pair B has only old (rename)", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		// Pick a second mapping entry so we can demonstrate the two-action
		// case: pair A has both old+new in the table (we must DELETE the
		// stale old row so sql-migrate doesn't see it as "unknown"), pair B
		// has only old (canonical UPDATE rename).
		var oldB, newB string
		for k, v := range mapping {
			if k == oldID {
				continue
			}
			oldB, newB = k, v
			break
		}
		if oldB == "" {
			t.Skip("need at least two mapping entries to exercise the mixed case")
		}

		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).
				AddRow(oldID).
				AddRow(newID). // pair A: both present → delete oldID
				AddRow(oldB))  // pair B: only old → rename to newB

		mock.ExpectBegin()
		// Rename path: oldB → newB
		renameStmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"UPDATE gorp_migrations SET id = ? WHERE id = ?"))
		renameStmt.ExpectExec().
			WithArgs(newB, oldB).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// Delete path: oldID (pair A's already-superseded legacy row)
		delStmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"DELETE FROM gorp_migrations WHERE id = ?"))
		delStmt.ExpectExec().
			WithArgs(oldID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for mixed state, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("both old and new present alone — delete old, no rename", func(t *testing.T) {
		// Concurrency safety check: if a peer replica raced ahead and
		// wrote the new ID, our shim must still clean up the stale old
		// row rather than skip it (which would leave a "ghost" ID that
		// sql-migrate flags as unknown).
		db, mock := openMock(t)
		defer db.Close()

		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(oldID).AddRow(newID))

		mock.ExpectBegin()
		delStmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"DELETE FROM gorp_migrations WHERE id = ?"))
		delStmt.ExpectExec().WithArgs(oldID).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})
}

func openMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	// QueryMatcherEqual would over-constrain on whitespace differences
	// between the implementation and these expectations; regexp matching
	// with QuoteMeta gives us substring-anchored matches that are robust
	// to formatting tweaks.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return db, mock
}

func mustExpectationsMet(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Sanity check the embedded mapping is well-formed JSON with old→new pairs.
// Catches a class of mistake where the JSON file shipped with the binary
// silently becomes empty (e.g. tool regression). A migration-shim bug that
// would otherwise only manifest on a real upgrade attempt is caught here.
func TestLoadMigrationIDMapping(t *testing.T) {
	m, err := loadMigrationIDMapping()
	if err != nil {
		t.Fatalf("loadMigrationIDMapping: %v", err)
	}
	if len(m) < 100 {
		// Round 1 generated 124 pairs; allow some slack but flag if the
		// file dropped below the 100-entry floor.
		t.Errorf("mapping has %d entries, expected ≥100 — embedded JSON may have regressed", len(m))
	}
	for old, new := range m {
		if old == "" || new == "" {
			t.Errorf("mapping pair has empty key or value: %q → %q", old, new)
		}
		if old == new {
			t.Errorf("mapping pair is a self-loop (would no-op forever): %q", old)
		}
		if !strings.HasSuffix(old, ".sql") || !strings.HasSuffix(new, ".sql") {
			t.Errorf("mapping pair doesn't look like SQL filenames: %q → %q", old, new)
		}
	}
}

// TestReconcileThreadSchemaRecords covers the four states the thread schema
// reconciliation has to handle:
//   - no thread tables present (fresh install) → no-op
//   - all three thread tables present, no thread-* rows in gorp → INSERT all 6
//   - all three thread tables present, rows already in gorp → no-op
//   - partial state (only some thread tables present) → no-op, don't mask drift
func TestReconcileThreadSchemaRecords(t *testing.T) {
	probeQuery := regexp.QuoteMeta(
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN")

	t.Run("no thread tables — fresh install no-op", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()
		mock.ExpectQuery(probeQuery).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}))
		if err := ReconcileThreadSchemaRecords(context.Background(), db); err != nil {
			t.Fatalf("expected nil for fresh install, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("partial state — refuses to act", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()
		// Only `thread` exists; thread_member / thread_setting missing.
		mock.ExpectQuery(probeQuery).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("thread"))
		if err := ReconcileThreadSchemaRecords(context.Background(), db); err != nil {
			t.Fatalf("expected nil for partial state, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("all tables present, gorp empty of thread-* — INSERT all 6", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()
		mock.ExpectQuery(probeQuery).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).
				AddRow("thread").AddRow("thread_member").AddRow("thread_setting"))
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'gorp_migrations'")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("some_other_migration.sql"))
		mock.ExpectBegin()
		stmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"INSERT IGNORE INTO gorp_migrations (id, applied_at) VALUES (?, NOW())"))
		for _, id := range threadModuleSnapshotMigrationIDs {
			stmt.ExpectExec().WithArgs(id).WillReturnResult(sqlmock.NewResult(0, 1))
		}
		mock.ExpectCommit()
		if err := ReconcileThreadSchemaRecords(context.Background(), db); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("partial state — any thread-* present means sql-migrate owns it, no-op", func(t *testing.T) {
		// Regression test for the silent-corruption bug: if even one
		// thread-* row is already in gorp_migrations, sql-migrate is
		// actively tracking the module. Any *missing* thread-* row is a
		// genuine unapplied migration (e.g. a future ADD INDEX) and the
		// shim must NOT pre-seed it — doing so would mark a real DDL as
		// applied without ever running it.
		db, mock := openMock(t)
		defer db.Close()
		mock.ExpectQuery(probeQuery).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).
				AddRow("thread").AddRow("thread_member").AddRow("thread_setting"))
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'gorp_migrations'")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		// Only the first five thread-* rows are recorded; the sixth
		// (a hypothetical ADD INDEX in a later release) is genuinely
		// pending and must be left for sql-migrate to apply.
		rows := sqlmock.NewRows([]string{"id"})
		for _, id := range threadModuleSnapshotMigrationIDs[:5] {
			rows.AddRow(id)
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).WillReturnRows(rows)
		// No transaction — the shim must hand control back to sql-migrate.
		if err := ReconcileThreadSchemaRecords(context.Background(), db); err != nil {
			t.Fatalf("expected nil for partial state, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})

	t.Run("all tables present, gorp already has all thread-* — no-op", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()
		mock.ExpectQuery(probeQuery).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).
				AddRow("thread").AddRow("thread_member").AddRow("thread_setting"))
		mock.ExpectQuery(regexp.QuoteMeta(
			"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'gorp_migrations'")).
			WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME"}).AddRow("gorp_migrations"))
		rows := sqlmock.NewRows([]string{"id"})
		for _, id := range threadModuleSnapshotMigrationIDs {
			rows.AddRow(id)
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM gorp_migrations")).WillReturnRows(rows)
		// No transaction expected because nothing needs inserting.
		if err := ReconcileThreadSchemaRecords(context.Background(), db); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		mustExpectationsMet(t, mock)
	})
}

// TestThreadModuleMigrationIDsMatchDisk catches drift between the
// hard-coded ID lists used by the shim and the actual files in
// modules/thread/sql/. The contract is:
//
//   - Every .sql file on disk MUST appear in exactly one of
//     threadModuleSnapshotMigrationIDs or threadModulePostSnapshotMigrationIDs.
//   - Snapshot-baseline migrations get pre-seeded by ReconcileThreadSchemaRecords;
//     post-snapshot migrations stay pending so sql-migrate.Exec applies them.
//
// A file missing from both lists would leave its ID un-pre-seeded for snapshot
// installs (potential Error 1050 on the next sql-migrate.Exec). A file in both
// lists is also flagged because the pre-seed semantics conflict.
//
// Surface the mismatch in CI rather than in production.
func TestThreadModuleMigrationIDsMatchDisk(t *testing.T) {
	dir := filepath.Join("..", "..", "modules", "thread", "sql")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var onDisk []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		onDisk = append(onDisk, e.Name())
	}
	sort.Strings(onDisk)

	combined := append([]string(nil), threadModuleSnapshotMigrationIDs...)
	combined = append(combined, threadModulePostSnapshotMigrationIDs...)
	sort.Strings(combined)

	if !equalStrSlices(onDisk, combined) {
		t.Errorf("thread migration ID lists drifted from modules/thread/sql/:\n  on disk: %v\n  snapshot+post: %v", onDisk, combined)
	}

	// Detect duplicates across the two lists — pre-seed semantics would
	// conflict (snapshot says "already applied", post-snapshot says "still
	// pending"); only one can be right per ID.
	seen := map[string]string{}
	for _, id := range threadModuleSnapshotMigrationIDs {
		seen[id] = "snapshot"
	}
	for _, id := range threadModulePostSnapshotMigrationIDs {
		if where, dup := seen[id]; dup {
			t.Errorf("migration %q listed in both %s and post-snapshot lists; pick one", id, where)
		}
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
