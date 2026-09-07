package project

// PR #841 review round 5 (yujiawei P2-1, endorsed by Jerry-Xin). The two shipped collation guards
// assert that the legacy joining columns ARE utf8mb4_general_ci — against a database CI creates
// with `COLLATE utf8mb4_general_ci`. They therefore pass by inheritance and are structurally
// incapable of failing for the production shape they are named after.
//
// This test builds the drifted shape on purpose and runs the REAL query methods against it, so the
// failure mode the migration header documents becomes CI evidence. It also doubles as the
// acceptance proof for the conversion step recorded in the task brief: the same statements are
// expected to fail before the CONVERT and succeed after it.
//
// Design notes worth stating, because both alternatives are worse:
//
//   - An ISOLATED database, not the shared `test` one. The tables that have to be re-collated are
//     `space` / `space_member` / `user`, which every other case in this package uses; converting
//     them in place would corrupt whichever test runs next, and CI runs -shuffle=on.
//   - `CREATE TABLE ... LIKE`, not hand-written DDL. The structure is copied from the schema the
//     migrations actually produced — including the generated `active_name` column and every index
//     — so this test cannot drift away from the real shape the way a hand-written fixture would.
//     modules/thread's equivalent hand-writes its two small tables; six tables with a generated
//     column and a composite unique key is past where that stays honest.

import (
	"os"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const collationProbeDB = "octo_project_collation_probe"

// collationProbeTables lists what the module's cross-Space queries touch, with the collation each
// table has in production.
var collationProbeTables = []struct {
	name   string
	legacy bool // true = created without COLLATE, so it inherited the server default
}{
	{"octo_project", false},
	{"octo_project_member", false},
	{"space_member_removal_cleanup", false}, // migration-created, explicitly general_ci
	{"space", true},
	{"space_member", true},
	{"user", true},
}

// newCollationProbe builds an isolated database whose legacy tables are utf8mb4_0900_ai_ci and
// whose module tables are utf8mb4_general_ci — the measured production shape — and returns a
// *Project bound to it plus a converge() that performs the brief's conversion.
func newCollationProbe(t *testing.T) (*Project, func()) {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = testCtx.GetConfig().DB.MySQLAddr
	}
	// Point the DSN at the probe database, keeping credentials and options.
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	adminDSN := addr[:slash+1] + opts
	probeDSN := addr[:slash+1] + collationProbeDB + opts

	admin, err := dbr.Open("mysql", adminDSN, nil)
	require.NoError(t, err)
	defer admin.Close()
	adminSess := admin.NewSession(nil)

	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+collationProbeDB+"`")
	// Created general_ci, exactly like CI does — so the drift below comes from the TABLES, which
	// is how it arises in production (mysqldump omitting COLLATE, restored onto a MySQL 8 server).
	exec(adminSess, "CREATE DATABASE `"+collationProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", probeDSN, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + collationProbeDB + "`").Exec()
		_ = conn.Close()
	})
	sess := conn.NewSession(nil)

	src := currentSchemaName(t)
	for _, tbl := range collationProbeTables {
		exec(sess, "CREATE TABLE `"+tbl.name+"` LIKE `"+src+"`.`"+tbl.name+"`")
		if tbl.legacy {
			exec(sess, "ALTER TABLE `"+tbl.name+
				"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}

	p := &Project{db: &DB{session: sess}}
	p.cfg = loadConfig()
	converge := func() {
		for _, tbl := range collationProbeTables {
			if tbl.legacy {
				exec(sess, "ALTER TABLE `"+tbl.name+
					"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
			}
		}
	}
	return p, converge
}

func currentSchemaName(t *testing.T) string {
	t.Helper()
	var name string
	require.NoError(t, testCtx.DB().SelectBySql("SELECT DATABASE()").LoadOne(&name))
	require.NotEmpty(t, name)
	return name
}

// TestCollationDriftBreaksTheCrossSpaceQueriesAndTheConversionFixesThem is the CI evidence the
// migration header's failure description previously had only in prose.
func TestCollationDriftBreaksTheCrossSpaceQueriesAndTheConversionFixesThem(t *testing.T) {
	setup(t) // the shared schema is the source CREATE TABLE ... LIKE copies from
	p, converge := newCollationProbe(t)

	// Every statement the module issues that compares a pinned column against a legacy one.
	crossSpace := map[string]func() error{
		"queryI1ViolationPage": func() error {
			_, err := p.queryI1ViolationPage("", "", 10)
			return err
		},
		"queryAbandonedLeakPage": func() error {
			_, err := p.queryAbandonedLeakPage("", "", 10)
			return err
		},
		"queryInspectedProjectPage (orphan)": func() error {
			_, err := p.queryInspectedProjectPage(0, 10)
			return err
		},
		"listMembers (roster, LEFT JOIN user)": func() error {
			_, err := p.db.listMembers("any", 0, 10)
			return err
		},
	}
	// And the ones that touch only this module's own tables.
	pinnedOnly := map[string]func() error{
		"queryOwnerlessProjectPage": func() error {
			_, err := p.queryOwnerlessProjectPage(0, 10)
			return err
		},
	}

	// --- drifted: the cross-Space statements must fail, and fail with 1267 specifically ---
	for name, run := range crossSpace {
		err := run()
		require.Error(t, err, "%s must fail on a collation-drifted database", name)
		assert.True(t, isCollationMixErr(err),
			"%s must fail with MySQL 1267 (illegal mix of collations), not something else: %v",
			name, err)
	}
	// Empty tables do not save it — collation is resolved when the statement is prepared, not per
	// row. This is the assertion that makes "the flag bounds the blast radius" checkable: with no
	// projects at all, the scans still fail.
	for name, run := range pinnedOnly {
		assert.NoError(t, run(),
			"%s touches only pinned tables, so it must keep working while the legacy tables are "+
				"drifted — this is why the reconcile gate is scoped to the cross-Space scans", name)
	}

	// --- converged: the brief's conversion must fix every one of them ---
	converge()
	for name, run := range crossSpace {
		assert.NoError(t, run(),
			"%s must work once the legacy tables are converted to utf8mb4_general_ci — this is "+
				"the acceptance evidence for the conversion step in the task brief", name)
	}
	for name, run := range pinnedOnly {
		assert.NoError(t, run(), "%s must still work after the conversion", name)
	}
}

// isCollationMixErr reports whether err is MySQL 1267.
func isCollationMixErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "1267")
}
