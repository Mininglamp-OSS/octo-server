package db

import (
	"context"
	"database/sql"
	"regexp"
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

	t.Run("mixed — both old and new present, no rewrite for that pair", func(t *testing.T) {
		db, mock := openMock(t)
		defer db.Close()

		// Pick a second mapping entry so we can demonstrate the partial-rewrite
		// case: pair A has both old+new (skip), pair B has only old (rewrite).
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
				AddRow(newID). // pair A already migrated
				AddRow(oldB))  // pair B still legacy

		mock.ExpectBegin()
		stmt := mock.ExpectPrepare(regexp.QuoteMeta(
			"UPDATE gorp_migrations SET id = ? WHERE id = ?"))
		// Only pair B should be rewritten; pair A is skipped because newID
		// is already present.
		stmt.ExpectExec().
			WithArgs(newB, oldB).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := RewriteLegacyMigrationIDs(context.Background(), db); err != nil {
			t.Fatalf("expected nil for mixed state, got %v", err)
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
