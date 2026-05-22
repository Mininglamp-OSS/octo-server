// Package bot_api · GH#122 — sqlmock-backed regression tests for the
// nullable `persona_prompt` column added by migration
// 20260521000001_obo_v2_persona_prompt.sql. These tests live next to the
// fake-backed unit tests because the bug only surfaces at the SQL boundary:
// the in-memory fake stores `PersonaPrompt string` directly so it cannot
// reproduce the "scan NULL into string" panic that motivated the fix.
//
// Two contracts pinned here:
//   - insertGrant carries `persona_prompt` in the INSERT column list (so a
//     row written today has an explicit empty string, not NULL).
//   - The four read paths that decode into oboGrantModel (findGrantByID,
//     findActiveGrantByGrantorBot, findGrantByGrantorBot,
//     findActiveGrantsForChannel, findGlobalGrantsWithoutScope) all wrap
//     `persona_prompt` in COALESCE so legacy rows whose column is NULL
//     load cleanly into the non-pointer struct field.
package bot_api

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

// newSqlmockBotAPIDB returns a *botAPIDB whose dbr session is backed by
// sqlmock. The returned closer must be deferred. ctx stays nil — none of
// the SQL paths exercised here touch the cache helpers (which short-circuit
// on nil ctx anyway).
func newSqlmockBotAPIDB(t *testing.T) (*botAPIDB, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)
	return &botAPIDB{session: session, ctx: nil}, mock, func() { _ = rawDB.Close() }
}

// grantRow is the column shape the production SELECT paths decode into
// oboGrantModel. Helper so each test does not repeat the slice literal.
func grantRowCols() []string {
	return []string{"id", "grantor_uid", "grantee_bot_uid", "mode",
		"global_enabled", "active", "created_at", "updated_at",
		"revoked_at", "persona_prompt"}
}

// fakeTime supplies a non-nil time.Time so the dbr scanner does not
// complain about NULL → *time.Time on created_at / updated_at. The actual
// value is irrelevant; only the persona_prompt column shape is under test.
var fakeTime = time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)

// TestInsertGrant_WritesPersonaPromptColumn pins that the INSERT statement
// now lists `persona_prompt` (so brand-new rows can never be NULL on read).
// The `mode` arg is left empty to also exercise the "default to auto"
// branch that was already there pre-fix.
func TestInsertGrant_WritesPersonaPromptColumn(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	// dbr's MySQL dialect inlines parameters before they hit the driver, so
	// match on the column list (regex must escape the parens + backticks).
	mock.ExpectExec(
		"INSERT INTO `obo_grants` \\(`grantor_uid`,`grantee_bot_uid`,`mode`,`persona_prompt`,",
	).WillReturnResult(sqlmock.NewResult(42, 1))

	id, err := d.insertGrant("grantor_x", "bot_x", "", "")
	require.NoError(t, err)
	require.Equal(t, int64(42), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFindGrantByID_NullPersonaPromptDoesNotPanic regresses the GH#122
// trigger: a row whose persona_prompt column is NULL (legacy rows from
// before the migration backfilled '' / rows written by code paths that
// did not include the column) used to fail loading into the struct because
// `PersonaPrompt string` cannot scan NULL. The fix wraps the column in
// `COALESCE(persona_prompt, '')` so the driver hands the scanner an empty
// string instead. We assert both that the production SQL contains the
// COALESCE rewrite and that the load succeeds with PersonaPrompt == "".
func TestFindGrantByID_NullPersonaPromptDoesNotPanic(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	// COALESCE collapses NULL → "" at the driver boundary; we hand sqlmock
	// the post-coalesce value ("") and assert via the SQL regex below that
	// the production query asked for the rewrite. A bare `persona_prompt`
	// scan would still blow up on a NULL column in real MySQL.
	rows := sqlmock.NewRows(grantRowCols()).
		AddRow(int64(1), "user_g", "bot_b", "auto", 1, 1,
			fakeTime, fakeTime, sql.NullTime{}, "")

	mock.ExpectQuery("COALESCE\\(persona_prompt, ''\\) AS persona_prompt").
		WillReturnRows(rows)

	g, err := d.findGrantByID(1)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, "", g.PersonaPrompt, "NULL column must scan as empty string after COALESCE")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFindActiveGrantByGrantorBot_UsesCoalesceOnPersonaPrompt — same
// guarantee for the hot-path checkOBO read.
func TestFindActiveGrantByGrantorBot_UsesCoalesceOnPersonaPrompt(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	rows := sqlmock.NewRows(grantRowCols()).
		AddRow(int64(7), "g", "b", "auto", 1, 1,
			fakeTime, fakeTime, sql.NullTime{}, "")

	mock.ExpectQuery("COALESCE\\(persona_prompt, ''\\) AS persona_prompt").
		WillReturnRows(rows)

	g, err := d.findActiveGrantByGrantorBot("g", "b")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, "", g.PersonaPrompt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFindActiveGrantsForChannel_UsesCoalesceOnPersonaPrompt — same
// guarantee for the fan-out feeder. The SELECT here aliases the table as
// `g`, so the COALESCE wraps `g.persona_prompt`.
func TestFindActiveGrantsForChannel_UsesCoalesceOnPersonaPrompt(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	rows := sqlmock.NewRows(grantRowCols()).
		AddRow(int64(9), "g", "b", "auto", 1, 1,
			fakeTime, fakeTime, sql.NullTime{}, "")

	mock.ExpectQuery("COALESCE\\(g.persona_prompt, ''\\) AS persona_prompt").
		WillReturnRows(rows)

	grants, err := d.findActiveGrantsForChannel("ch_1", 2)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "", grants[0].PersonaPrompt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFindGlobalGrantsWithoutScope_UsesCoalesceOnPersonaPrompt — same
// guarantee for the implicit-scope feeder.
func TestFindGlobalGrantsWithoutScope_UsesCoalesceOnPersonaPrompt(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	rows := sqlmock.NewRows(grantRowCols()).
		AddRow(int64(11), "g", "b", "auto", 1, 1,
			fakeTime, fakeTime, sql.NullTime{}, "")

	mock.ExpectQuery("COALESCE\\(g.persona_prompt, ''\\) AS persona_prompt").
		WillReturnRows(rows)

	grants, err := d.findGlobalGrantsWithoutScope("ch_1", "ch_1", 2)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, "", grants[0].PersonaPrompt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFindGrantByGrantorBot_UsesCoalesceOnPersonaPrompt — the reactivation
// path read uses the same SELECT shape as findGrantByID; pin it here too.
func TestFindGrantByGrantorBot_UsesCoalesceOnPersonaPrompt(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	rows := sqlmock.NewRows(grantRowCols()).
		AddRow(int64(13), "g", "b", "auto", 0, 0,
			fakeTime, fakeTime, sql.NullTime{}, "")

	mock.ExpectQuery("COALESCE\\(persona_prompt, ''\\) AS persona_prompt").
		WillReturnRows(rows)

	g, err := d.findGrantByGrantorBot("g", "b")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Equal(t, "", g.PersonaPrompt)
	require.NoError(t, mock.ExpectationsWereMet())
}
