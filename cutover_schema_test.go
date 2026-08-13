package main

// Schema conformance for every registered cutover domain.
//
// The three cutover mechanisms deliberately keep separate authority tables
// (docs/cutover-framework.md explains why merging them was rejected: per-module
// migrations, a hot FOR SHARE row that should not share a page with an
// unrelated domain's flip, and a per-table Down guard that cannot survive
// consolidation). The cost of that decision is that every new domain COPIES the
// DDL template, and copied schema drifts exactly the way copied code does.
//
// This test is what makes copying safe. It reads the real migrated schema out
// of information_schema and checks it against the template, so a domain that
// mistypes a column type, forgets the singleton CHECK, or ships without a
// seeded legacy row fails here rather than during a cutover.
//
// Deviations that already shipped are recorded in knownTemplateDeviations with
// the reason they are not being fixed — the list is the documentation, and a
// new domain cannot join it by accident.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// templateColumn is one column the framework template requires, with the exact
// information_schema spelling of its type.
type templateColumn struct {
	name       string
	columnType string
	nullable   string
	// defaultValue is the expected COLUMN_DEFAULT; "" means the template does
	// not pin one (singleton_id has no default — the seed supplies it).
	defaultValue string
	// why explains what breaks if the column is wrong, for the failure message.
	why string
}

func cutoverTemplateColumns() []templateColumn {
	return []templateColumn{
		{
			name: "singleton_id", columnType: "tinyint unsigned", nullable: "NO",
			why: "cutover.SingletonID is the fixed key every read and the CAS bind to",
		},
		{
			name: "mode", columnType: "tinyint", nullable: "NO", defaultValue: "0",
			why: "the default is what makes deploying the schema inert — a non-zero default activates a domain on migration",
		},
		{
			name: "epoch", columnType: "bigint unsigned", nullable: "NO", defaultValue: "0",
			why: "Flip writes epoch+1; a signed or nullable column changes what a generation means",
		},
		{
			name: "cutover_floor", columnType: "bigint", nullable: "NO", defaultValue: "0",
			why: "the floor is a signed watermark compared against observed maxima",
		},
		{
			name: "updated_at", columnType: "timestamp", nullable: "NO",
			why: "operators need to know when the authority last moved",
		},
	}
}

// knownTemplateDeviations records where an already-shipped table diverges from
// the template, keyed by table then column, with the reason it stays that way.
//
// A deviation listed here is reported, not failed. Anything NOT listed fails —
// so a new domain inherits the full template and cannot quietly skip part of it.
var knownTemplateDeviations = map[string]map[string]string{
	msgextraseq.StateTable: {
		"updated_at": "#627 shipped before updated_at was part of the template. Adding it " +
			"now means an ALTER on a table that every message_extra write reads FOR SHARE, " +
			"for a purely observational column — not worth the migration. New domains must " +
			"include it.",
	},
}

// TestCutoverDomainTablesMatchTheFrameworkTemplate checks the real migrated
// schema of every registered domain.
func TestCutoverDomainTablesMatchTheFrameworkTemplate(t *testing.T) {
	db := cutoverSchemaTestDB(t)
	schema := currentSchemaName(t, db)

	domains := cutoverDomainList()
	require.NotEmpty(t, domains)
	for _, d := range domains {
		t.Run(d.name, func(t *testing.T) {
			require.NotEmpty(t, d.stateTable, "domain %s registered no state table", d.name)
			assertCutoverTableShape(t, db, schema, d.stateTable)
			assertSingletonCheckConstraint(t, db, schema, d.stateTable)
			assertSeededInertRow(t, db, d.stateTable)
		})
	}
}

func assertCutoverTableShape(t *testing.T, db *dbr.Session, schema, table string) {
	t.Helper()
	type columnRow struct {
		Name      string         `db:"COLUMN_NAME"`
		Type      string         `db:"COLUMN_TYPE"`
		Nullable  string         `db:"IS_NULLABLE"`
		Default   dbr.NullString `db:"COLUMN_DEFAULT"`
		Collation dbr.NullString `db:"COLLATION_NAME"`
	}
	var rows []columnRow
	_, err := db.SelectBySql(
		"SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, COLLATION_NAME "+
			"FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=?",
		schema, table,
	).Load(&rows)
	require.NoError(t, err, "read columns of %s", table)
	require.NotEmpty(t, rows, "table %s does not exist in schema %s — did the domain's migration run?", table, schema)

	byName := make(map[string]columnRow, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}

	for _, want := range cutoverTemplateColumns() {
		got, present := byName[want.name]
		if !present {
			if reason, known := knownTemplateDeviations[table][want.name]; known {
				t.Logf("known deviation: %s has no %s column — %s", table, want.name, reason)
				continue
			}
			t.Errorf("%s is missing the template column %q (%s).\n"+
				"Copy the DDL from docs/cutover-framework.md, or record a deliberate "+
				"deviation in knownTemplateDeviations with the reason.", table, want.name, want.why)
			continue
		}
		if got.Type != want.columnType {
			t.Errorf("%s.%s type is %q, template requires %q — %s",
				table, want.name, got.Type, want.columnType, want.why)
		}
		if got.Nullable != want.nullable {
			t.Errorf("%s.%s IS_NULLABLE is %q, template requires %q",
				table, want.name, got.Nullable, want.nullable)
		}
		if want.defaultValue != "" && got.Default.String != want.defaultValue {
			t.Errorf("%s.%s default is %q, template requires %q — %s",
				table, want.name, got.Default.String, want.defaultValue, want.why)
		}
	}

	// Collation is pinned by the template because octo-server's MySQL server
	// default is utf8mb4_0900_ai_ci, which mixes badly with the rest of the
	// schema in joins (the same reason CI forces it when creating `test`).
	var collation string
	_, err = db.SelectBySql(
		"SELECT TABLE_COLLATION FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?",
		schema, table,
	).Load(&collation)
	require.NoError(t, err)
	require.Equal(t, "utf8mb4_general_ci", collation, "%s collation must match the template", table)

	// The primary key must be the singleton column alone: Flip and every read
	// bind singleton_id=? and rely on hitting a unique index, which is what
	// makes the FOR UPDATE a record lock rather than a gap lock.
	var pkColumns []string
	_, err = db.SelectBySql(
		"SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE "+
			"WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY' ORDER BY ORDINAL_POSITION",
		schema, table,
	).Load(&pkColumns)
	require.NoError(t, err)
	require.Equal(t, []string{"singleton_id"}, pkColumns, "%s primary key must be (singleton_id)", table)
}

// assertSingletonCheckConstraint pins the constraint that makes a second row
// impossible. It is also the hook botevent's migration uses to refuse a Down
// after activation, so losing it silently weakens two things at once.
func assertSingletonCheckConstraint(t *testing.T, db *dbr.Session, schema, table string) {
	t.Helper()
	var clauses []string
	_, err := db.SelectBySql(
		"SELECT cc.CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS cc "+
			"JOIN information_schema.TABLE_CONSTRAINTS tc "+
			"  ON tc.CONSTRAINT_SCHEMA=cc.CONSTRAINT_SCHEMA AND tc.CONSTRAINT_NAME=cc.CONSTRAINT_NAME "+
			"WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME=?",
		schema, table,
	).Load(&clauses)
	require.NoError(t, err, "read CHECK constraints of %s", table)

	for _, clause := range clauses {
		normalized := strings.ReplaceAll(strings.ReplaceAll(clause, "`", ""), " ", "")
		if strings.Contains(normalized, "singleton_id=1") {
			return
		}
	}
	t.Errorf("%s has no CHECK constraint pinning singleton_id = 1 (found %v).\n"+
		"Without it a second row can exist, and the migration Down guard that refuses "+
		"to drop an activated authority has nothing to trip on.", table, clauses)
}

// assertSeededInertRow checks the migration left exactly one row, in the
// inactive mode. Deploying the schema must never activate anything: the flip is
// an operator action, separate from the deploy.
func assertSeededInertRow(t *testing.T, db *dbr.Session, table string) {
	t.Helper()
	var count int
	_, err := db.SelectBySql("SELECT COUNT(*) FROM `" + table + "`").Load(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "%s must hold exactly one seeded row", table)

	var row struct {
		SingletonID int   `db:"singleton_id"`
		Mode        int   `db:"mode"`
		Epoch       int64 `db:"epoch"`
		Floor       int64 `db:"cutover_floor"`
	}
	_, err = db.SelectBySql(
		"SELECT `singleton_id`, `mode`, `epoch`, `cutover_floor` FROM `"+table+"` WHERE `singleton_id`=?",
		cutover.SingletonID,
	).Load(&row)
	require.NoError(t, err)
	require.Equal(t, cutover.SingletonID, row.SingletonID)
	require.Equal(t, cutover.ModeInactive, row.Mode,
		"%s seeds mode=%d: a fresh migration must leave the domain on its legacy path, "+
			"or deploying the schema performs the cutover", table, row.Mode)
	require.Zero(t, row.Epoch, "%s must seed epoch=0; Flip owns the generation counter", table)
	require.Zero(t, row.Floor, "%s must seed cutover_floor=0; the floor is set by an activation, not a migration", table)
}

// cutoverSchemaTestDB brings up the real migrated schema.
//
// Unlike the domain packages' own tests — which deliberately use
// NewTestContext and CREATE TABLE IF NOT EXISTS to dodge migration-ledger
// fragility — this test's whole subject IS the migration output, so it must run
// module.Setup. The root package pulls in every module through internal/, so
// both domains' migrations are present.
func cutoverSchemaTestDB(t *testing.T) *dbr.Session {
	t.Helper()
	if err := cutoverSchemaDepsAvailable(); err != nil {
		// Same posture as pkg/botevent's TestMain: skipping in CI would be
		// indistinguishable from passing on the check that guards the DDL.
		if os.Getenv("CI") != "" {
			t.Fatalf("%v\n\nThis test reads the migrated schema of every cutover domain; "+
				"skipping it in CI would look exactly like passing. Set OCTO_TEST_MYSQL_ADDR "+
				"if MySQL is not on its default address.", err)
		}
		t.Skipf("%v — skipping schema conformance locally (set CI=true to make this a failure)", err)
	}
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { testutil.CleanAllTables(ctx) })
	return ctx.DB()
}

func cutoverSchemaDepsAvailable() error {
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.DB.MySQLAddr = addr
	probe := config.NewContext(cfg)
	if _, err := probe.DB().Exec("SELECT 1"); err != nil {
		return fmt.Errorf("no usable MySQL at %s: %w", addr, err)
	}
	return nil
}

// currentSchemaName resolves the schema the test connection is pointed at, so
// the information_schema queries above cannot accidentally inspect a
// same-named table in another database.
func currentSchemaName(t *testing.T, db *dbr.Session) string {
	t.Helper()
	var schema string
	_, err := db.SelectBySql("SELECT DATABASE()").Load(&schema)
	require.NoError(t, err)
	require.NotEmpty(t, schema, "connection has no default schema")
	return schema
}
