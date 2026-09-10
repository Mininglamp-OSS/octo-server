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
// This test is what makes copying safe. A domain that mistypes a column type,
// forgets the singleton CHECK, or ships without a seeded legacy row fails here
// rather than during a cutover.
//
// Note what is asserted from where. The DDL — columns, types, defaults,
// collation, primary key, CHECK — is read from the LIVE migrated schema via
// information_schema, because that is what the migration actually produced. The
// seeded row is asserted from the MIGRATION SOURCE, because the live row is both
// unavailable (testutil.NewTestServer clears every table on entry and the
// ledger-gated migration does not re-seed) and the wrong subject (an activated
// database holds an active mode legitimately, so "the current row is inactive"
// is not an invariant — "the migration seeds inactive" is).
//
// Deviations that already shipped are recorded in knownTemplateDeviations with
// the reason they are not being fixed — the list is the documentation, and a
// new domain cannot join it by accident.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	root := repoRootForSchemaTest(t)
	domains := cutoverDomainList()
	require.NotEmpty(t, domains)
	for _, d := range domains {
		t.Run(d.name, func(t *testing.T) {
			require.NotEmpty(t, d.stateTable, "domain %s registered no state table", d.name)
			assertCutoverTableShape(t, db, schema, d.stateTable)
			assertSingletonCheckConstraint(t, db, schema, d.stateTable)
			assertMigrationSeedsInertRow(t, root, d.stateTable)

			// Re-run with the table emptied. This is the regression for the
			// defect this check shipped with: it asserted the seeded row from
			// the live table, which testutil.NewTestServer has already deleted
			// by the time any assertion runs (it clears every table except
			// gorp_migrations on ENTRY, and the ledger-gated migration does not
			// re-seed). It failed on entry, not on a second run, so removing
			// the harness's own cleanup would not have helped.
			//
			// A subtest rather than a separate top-level test: the assertions
			// are the same three, and a duplicate test would pay for a second
			// full module.Setup to pin nothing extra.
			t.Run("without the seeded row", func(t *testing.T) {
				_, err := db.UpdateBySql("DELETE FROM `" + d.stateTable + "`").Exec()
				require.NoError(t, err)
				assertCutoverTableShape(t, db, schema, d.stateTable)
				assertSingletonCheckConstraint(t, db, schema, d.stateTable)
				assertMigrationSeedsInertRow(t, root, d.stateTable)
			})
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

// assertMigrationSeedsInertRow checks that the migration which creates the
// table seeds it inactive. Deploying the schema must never activate anything:
// the flip is an operator action, separate from the deploy.
//
// The subject is the migration, NOT the live row, for two independent reasons.
// The live row is deleted before any assertion can see it (see
// TestCutoverSchemaConformanceDoesNotDependOnSeededRows). And it would be the
// wrong subject even if it survived: an activated production database holds
// mode=1 legitimately, so "the current row is inactive" is not an invariant,
// while "the migration seeds inactive" is.
//
// Only the `-- +migrate Up` section is considered. botevent's Down deliberately
// contains an INSERT of its own — the singleton-CHECK trip that refuses to drop
// an activated authority — and reading that as a seed would be nonsense.
func assertMigrationSeedsInertRow(t *testing.T, root, table string) {
	t.Helper()
	path, up := findCreatingMigration(t, root, table)

	// Strip line comments BEFORE flattening. Without this a seed that was
	// commented out — during a debugging pass, say — still satisfies the
	// assertion, and the test would report an inert seed for a deployed table
	// that has no singleton at all.
	//
	// Then normalize whitespace, so the assertion does not depend on how the
	// DDL is wrapped across lines.
	flat := strings.Join(strings.Fields(stripSQLLineComments(up)), " ")
	insert := regexp.MustCompile(
		"INSERT INTO `" + regexp.QuoteMeta(table) + "` " +
			"\\( *`singleton_id` *, *`mode` *, *`epoch` *, *`cutover_floor` *\\) " +
			"VALUES *\\( *([0-9]+) *, *([0-9]+) *, *([0-9]+) *, *([0-9]+) *\\)",
	).FindStringSubmatch(flat)
	require.NotNil(t, insert,
		"%s: no seed INSERT for %s with the template's column list "+
			"(`singleton_id`,`mode`,`epoch`,`cutover_floor`) found in the Up section.\n"+
			"Every cutover domain must seed its singleton so a deploy is inert; see "+
			"docs/cutover-framework.md.", path, table)

	require.Equal(t, strconv.Itoa(cutover.SingletonID), insert[1],
		"%s: seed must use singleton_id=%d", path, cutover.SingletonID)
	require.Equal(t, strconv.Itoa(cutover.ModeInactive), insert[2],
		"%s: seed must use mode=%d — a migration that seeds an active mode performs "+
			"the cutover at deploy time", path, cutover.ModeInactive)
	require.Equal(t, "0", insert[3], "%s: seed must use epoch=0; Flip owns the generation counter", path)
	require.Equal(t, "0", insert[4],
		"%s: seed must use cutover_floor=0; the floor is set by an activation, not a migration", path)
}

// findCreatingMigration returns the migration file that creates the table, and
// its `-- +migrate Up` section.
func findCreatingMigration(t *testing.T, root, table string) (string, string) {
	t.Helper()
	create := regexp.MustCompile("CREATE TABLE (IF NOT EXISTS )?`" + regexp.QuoteMeta(table) + "`")

	var foundPath, foundUp string
	for _, file := range migrationCorpus(t, root) {
		if !create.MatchString(file.content) {
			continue
		}
		require.Empty(t, foundPath,
			"two migrations create %s (%s and %s); the conformance check cannot tell which seeds it",
			table, foundPath, file.rel)
		foundPath, foundUp = file.rel, migrationUpSection(file.content)
	}
	require.NotEmpty(t, foundPath,
		"no migration under modules/*/sql/ creates %s — a registered cutover domain must ship one", table)
	return foundPath, foundUp
}

type migrationSource struct {
	rel     string
	content string
}

var (
	migrationCorpusOnce  sync.Once
	migrationCorpusFiles []migrationSource
	migrationCorpusErr   error
	// migrationCorpusWalks exists so TestMigrationCorpusIsWalkedOnce can assert
	// the memoization rather than trust it.
	migrationCorpusWalks atomic.Int64
)

// migrationCorpus reads every modules/**/*.sql file, once per test binary.
//
// The alternative — walking on each lookup — reads all 27 modules' migrations
// four times over to answer four questions about two files. Memoizing on
// nothing but the once is safe because root comes from repoRootForSchemaTest,
// which resolves the one module root the test binary runs in, and migrations on
// disk do not change mid-run.
func migrationCorpus(t *testing.T, root string) []migrationSource {
	t.Helper()
	migrationCorpusOnce.Do(func() {
		migrationCorpusWalks.Add(1)
		migrationCorpusErr = filepath.WalkDir(filepath.Join(root, "modules"), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
				return walkErr
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			migrationCorpusFiles = append(migrationCorpusFiles, migrationSource{rel: rel, content: string(content)})
			return nil
		})
	})
	require.NoError(t, migrationCorpusErr, "reading the migration tree under %s", root)
	return migrationCorpusFiles
}

// TestMigrationCorpusIsWalkedOnce pins the memoization.
//
// The conformance test calls findCreatingMigration once per domain and again
// inside each "without the seeded row" subtest, so an un-memoized lookup walks
// modules/ — every .sql file under 27 modules, read whole — four times to
// answer four questions about two files. The count is process-global and the
// corpus is immutable within a run, so this assertion holds no matter which
// other test loaded it first.
func TestMigrationCorpusIsWalkedOnce(t *testing.T) {
	root := repoRootForSchemaTest(t)
	for _, d := range cutoverDomainList() {
		findCreatingMigration(t, root, d.stateTable)
		findCreatingMigration(t, root, d.stateTable)
	}
	require.EqualValues(t, 1, migrationCorpusWalks.Load(),
		"the migration tree must be read once per test binary, not once per lookup")
}

// stripSQLLineComments removes `-- …` to end of line. Enough for the migration
// files in this repo, which use line comments only; it deliberately does not
// try to parse string literals, because a `--` inside one would make the seed
// assertion stricter, never looser.
func stripSQLLineComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// migrationUpSection returns everything between `-- +migrate Up` and
// `-- +migrate Down` (or the end of the file when there is no Down).
func migrationUpSection(content string) string {
	const upMarker, downMarker = "-- +migrate Up", "-- +migrate Down"
	if i := strings.Index(content, upMarker); i >= 0 {
		content = content[i+len(upMarker):]
	}
	if i := strings.Index(content, downMarker); i >= 0 {
		content = content[:i]
	}
	return content
}

// repoRootForSchemaTest locates the module root; the root package's tests run
// there, and go.mod is the sanity check that the walk below is not pointed at
// some other tree.
func repoRootForSchemaTest(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "expected go.mod at the module root %q", root)
	return root
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

// TestSchemaConformanceProbeCoversWhatTheHarnessNeeds keeps the probe honest.
//
// The probe decides between "skip" and "run testutil.NewTestServer", and
// NewTestServer does not fail politely: modules/common's Route panics without
// a master key. A panic takes the whole test binary down, so an incomplete
// probe costs not this test but the ~50 pure CLI, interrupt, redaction and
// dispatcher tests in this package that need no infrastructure at all — the
// ones whose entire value is being runnable on a laptop.
func TestSchemaConformanceProbeCoversWhatTheHarnessNeeds(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "")
	err := cutoverSchemaDepsAvailable()
	require.Error(t, err, "the probe must reject a missing master key rather than let NewTestServer panic")
	require.Contains(t, err.Error(), "OCTO_MASTER_KEY")
}

// cutoverSchemaDepsAvailable reports whether testutil.NewTestServer can run.
//
// It probes what NewTestServer actually needs, not just MySQL: module.Setup
// panics without OCTO_MASTER_KEY and several modules resolve Redis during
// setup. Cheapest check first, so the returned error names the thing that is
// actually missing even when more than one is.
func cutoverSchemaDepsAvailable() error {
	if key := os.Getenv("OCTO_MASTER_KEY"); key == "" {
		return errors.New("OCTO_MASTER_KEY is not set — testutil.NewTestServer panics " +
			"in modules/common without it (see ci.yml, which sets a fixed CI-only value)")
	}
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.DB.MySQLAddr = addr
	if redisAddr := os.Getenv("OCTO_TEST_REDIS_ADDR"); redisAddr != "" {
		cfg.DB.RedisAddr = redisAddr
	}
	probe := config.NewContext(cfg)
	if _, err := probe.DB().Exec("SELECT 1"); err != nil {
		return fmt.Errorf("no usable MySQL at %s: %w", redactedMySQLEndpoint(addr), err)
	}
	if _, err := probe.GetRedisConn().Ping(); err != nil {
		return fmt.Errorf("no usable Redis at %s: %w", cfg.DB.RedisAddr, err)
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
