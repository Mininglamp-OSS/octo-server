// Package bot_api · PR#109 R3 regression coverage for the persona_prompt
// migration backfill.
//
// Why this file exists
// --------------------
// 20260521000001_obo_v2_persona_prompt.sql adds the persona_prompt TEXT
// column. MySQL < 8.0.13 forbids `DEFAULT ''` on TEXT, so the column
// lands NULL-able and pre-v2 rows materialize with persona_prompt =
// NULL. Multiple read paths in obo_db.go scan that column into a
// non-pointer string field on oboGrantModel, and dbr/database/sql
// rejects NULL → string with "converting NULL to string is
// unsupported". PR#109 R3 closes the gap by appending a backfill
// UPDATE that promotes every NULL row to '' as part of the same
// migration step.
//
// This file's two tests pin both halves of that contract:
//
//   1. The backfill UPDATE is and stays present in the migration
//      file. (TestPersonaPromptMigrationContainsBackfill)
//   2. Running the migration on a sqlite DB whose obo_grants rows
//      were inserted with NULL persona_prompt promotes every NULL
//      to '' so the production read paths see the safe value.
//      (TestPersonaPromptBackfillNullRowsToEmptyString)
package bot_api

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestPersonaPromptMigrationContainsBackfill is a literal-content guard.
// The reviewer can delete the ALTER and the UPDATE will fail loudly;
// what we want to prevent is a future cleanup that drops the UPDATE
// while keeping the ALTER, which would silently re-introduce NULL rows.
func TestPersonaPromptMigrationContainsBackfill(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("sql", "20260521000001_obo_v2_persona_prompt.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "ADD COLUMN persona_prompt TEXT") {
		t.Fatalf("expected ALTER ADD COLUMN persona_prompt TEXT in migration, got:\n%s", body)
	}
	// The exact text below is what the prod migration runner sees. If
	// someone reformats it, also update the assertion (the rename is the
	// signal that PR#109 R3's contract is being modified).
	wantBackfill := "UPDATE obo_grants SET persona_prompt = '' WHERE persona_prompt IS NULL"
	if !strings.Contains(body, wantBackfill) {
		t.Fatalf("expected backfill statement %q in migration body, got:\n%s",
			wantBackfill, body)
	}
}

// TestPersonaPromptBackfillNullRowsToEmptyString runs the backfill UPDATE
// on a sqlite obo_grants table that holds three NULL persona_prompt
// rows seeded via raw SQL. After the UPDATE every row must read back
// as '' (not NULL), which is exactly what the prod migration relies
// on so the downstream `SELECT *` paths in obo_db.go can safely scan
// the column into oboGrantModel.PersonaPrompt (a non-pointer string).
//
// We deliberately bypass the real MySQL schema — this test owns the
// backfill behavior, not the full obo_grants DDL. A minimal sqlite
// CREATE TABLE with the same column shape is enough to exercise the
// UPDATE's predicate and its empty-string write.
func TestPersonaPromptBackfillNullRowsToEmptyString(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Minimal schema: only the columns the backfill predicate touches.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE obo_grants (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			grantor_uid     TEXT NOT NULL,
			grantee_bot_uid TEXT NOT NULL,
			persona_prompt  TEXT
		);
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Seed three pre-v2 rows whose persona_prompt is NULL — the exact
	// state a production server would be in immediately after the
	// ALTER but before the backfill runs.
	for i := 1; i <= 3; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO obo_grants (grantor_uid, grantee_bot_uid, persona_prompt) VALUES (?, ?, NULL)`,
			"alice", "bot_"+string(rune('a'+i-1)),
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// Sanity check: confirm we really have NULL rows before the backfill.
	// If sqlite (or a future schema tweak) coerces our NULL inserts into
	// '' on its own, the rest of this test is meaningless — fail loud.
	var nullsBefore int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM obo_grants WHERE persona_prompt IS NULL`,
	).Scan(&nullsBefore); err != nil {
		t.Fatalf("count nulls before: %v", err)
	}
	if nullsBefore != 3 {
		t.Fatalf("expected 3 NULL rows before backfill, got %d", nullsBefore)
	}

	// Run the exact backfill statement the migration ships.
	if _, err := db.ExecContext(ctx,
		`UPDATE obo_grants SET persona_prompt = '' WHERE persona_prompt IS NULL`,
	); err != nil {
		t.Fatalf("backfill UPDATE: %v", err)
	}

	// No NULLs may survive.
	var nullsAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM obo_grants WHERE persona_prompt IS NULL`,
	).Scan(&nullsAfter); err != nil {
		t.Fatalf("count nulls after: %v", err)
	}
	if nullsAfter != 0 {
		t.Fatalf("expected 0 NULL rows after backfill, got %d", nullsAfter)
	}

	// Every row must read back as the empty string — the exact value
	// obo_db.go read paths expect to land in oboGrantModel.PersonaPrompt.
	// We scan into a non-pointer string here to mirror the production
	// scan target: this is what proves the backfill closes the
	// "converting NULL to string is unsupported" failure mode.
	rows, err := db.QueryContext(ctx,
		`SELECT id, grantor_uid, grantee_bot_uid, persona_prompt FROM obo_grants ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("select rows after backfill: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var (
			id        int64
			grantor   string
			grantee   string
			prompt    string // non-pointer, matches oboGrantModel.PersonaPrompt
		)
		if err := rows.Scan(&id, &grantor, &grantee, &prompt); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if prompt != "" {
			t.Fatalf("row %d: persona_prompt = %q, want \"\"", id, prompt)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if seen != 3 {
		t.Fatalf("expected to scan 3 rows, scanned %d", seen)
	}

	// Idempotency: re-running the backfill must be a no-op (it is the
	// only behavior the migration runner can give us, because gorp
	// records `applied` per file and never replays a file mid-flight,
	// but a defensive operator may run it by hand on suspect data).
	res, err := db.ExecContext(ctx,
		`UPDATE obo_grants SET persona_prompt = '' WHERE persona_prompt IS NULL`,
	)
	if err != nil {
		t.Fatalf("re-run backfill: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if affected != 0 {
		t.Fatalf("expected 0 rows affected on second backfill run, got %d", affected)
	}
}
