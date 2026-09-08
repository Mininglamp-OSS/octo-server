package project

import (
	"os"
	"strings"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	userpkg "github.com/Mininglamp-OSS/octo-server/pkg/user"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Collation-drift coverage for the ACCOUNT-liveness gate.
//
// This is the trap the account gate was one line away from falling into, and CI is
// structurally incapable of catching it: ci/run-e2e-shard.sh creates its database
// with an explicit COLLATE utf8mb4_general_ci, so every table agrees there and any
// JOIN passes. In production `user` / `space` / `space_member` are dump-imported and
// sit at utf8mb4_0900_ai_ci while migration-created octo_* tables declare
// general_ci — so the obvious shape of this fix,
//
//	SELECT ... FROM octo_project_member pm INNER JOIN `user` u ON u.uid = pm.uid
//
// is MySQL error 1267 THERE while green in CI. On a fail-closed authorization
// endpoint that means the peer is denied everything, in the one lane nobody watches.
//
// So both halves of the shipped shape are asserted against a DRIFTED database:
// pkg/user.ActiveAccounts (single-table, used by the read path) and
// lockSpaceSeatsTx's JOIN (rooted at space_member, where all three sides drifted
// together and the join is therefore safe). And both are re-asserted after
// converging the collations, so an explicit COLLATE that only works against today's
// drift cannot pass either.

const accountLivenessProbeDB = "octo_project_account_liveness_probe"

// accountLivenessProbeTables is what the two statements under test touch.
var accountLivenessProbeTables = []struct {
	name   string
	legacy bool
}{
	{"octo_project", false},
	{"octo_project_member", false},
	{"space", true},
	{"space_member", true},
	{"user", true},
}

// newAccountLivenessProbe builds an isolated database whose legacy tables carry the
// production drift.
//
// An isolated database, not the shared one: `space` / `space_member` / `user` are
// shared fixtures, and CONVERTing them under -shuffle=on would wreck whatever case
// runs next. Structure comes from CREATE TABLE ... LIKE against the real schema, so
// this fixture cannot drift away from what the migrations actually produce.
func newAccountLivenessProbe(t *testing.T) (*dbr.Session, func()) {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = testCtx.GetConfig().DB.MySQLAddr
	}
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	defer admin.Close()
	adminSess := admin.NewSession(nil)

	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+accountLivenessProbeDB+"`")
	// Created general_ci exactly like CI does, so the drift comes from the TABLES —
	// which is how it arises in production.
	exec(adminSess, "CREATE DATABASE `"+accountLivenessProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+accountLivenessProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + accountLivenessProbeDB + "`").Exec()
		_ = conn.Close()
	})
	sess := conn.NewSession(nil)

	src := currentSchemaName(t)
	for _, tbl := range accountLivenessProbeTables {
		exec(sess, "CREATE TABLE `"+tbl.name+"` LIKE `"+src+"`.`"+tbl.name+"`")
		if tbl.legacy {
			exec(sess, "ALTER TABLE `"+tbl.name+
				"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}

	// Enough rows that the statements resolve against real data rather than an empty
	// table — 1267 is raised at statement RESOLUTION, so empty tables would "pass"
	// either way, but a row proves the predicate also answers correctly.
	exec(sess, "INSERT INTO `space` (space_id, name, creator, status) VALUES ('cs1','cs1','u0',1)")
	exec(sess, "INSERT INTO `user` (uid, name, short_no, status, is_destroy) "+
		"VALUES ('clive','clive','clive',1,0), ('cbanned','cbanned','cbanned',0,0)")
	exec(sess, "INSERT INTO `space_member` (space_id, uid, role, status) "+
		"VALUES ('cs1','clive',0,1), ('cs1','cbanned',0,1)")

	converge := func() {
		for _, tbl := range accountLivenessProbeTables {
			if tbl.legacy {
				exec(sess, "ALTER TABLE `"+tbl.name+
					"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
			}
		}
	}
	return sess, converge
}

// TestAccountLivenessSurvivesCollationDrift asserts the shipped shape works on a
// drifted database AND after the conversion, for both the read and the write half.
func TestAccountLivenessSurvivesCollationDrift(t *testing.T) {
	setup(t) // the shared schema is what CREATE TABLE ... LIKE copies from
	sess, converge := newAccountLivenessProbe(t)
	db := &DB{session: sess}

	// A control: prove this fixture really does reproduce the drift, by running the
	// shape that MUST fail on it. Without this the two assertions below could pass
	// on a database with no drift at all, which is exactly how CI reads as green.
	_, err := sess.SelectBySql(
		"SELECT pm.uid FROM `octo_project_member` pm "+
			"INNER JOIN `user` u ON u.uid = pm.uid AND u.status = 1 "+
			"WHERE pm.project_id = ?", "cp1",
	).ReturnStrings()
	require.Error(t, err,
		"the fixture must reproduce the production drift: a JOIN from an octo_* table onto "+
			"`user` has to fail here. If this passes, the probe database is not drifted and "+
			"everything below proves nothing")
	assert.Contains(t, err.Error(), "1267",
		"and it must fail with the collation error specifically, not something else: %v", err)

	checks := map[string]func() error{
		// The READ path's half: single-table, so no cross-collation comparison arises.
		"pkg/user.ActiveAccounts": func() error {
			live, e := userpkg.ActiveAccounts(sess, []string{"clive", "cbanned"})
			if e != nil {
				return e
			}
			assert.True(t, live["clive"], "the live account must be reported live")
			assert.False(t, live["cbanned"], "the banned account must not be")
			return nil
		},
		// The READ path END TO END. Asserting only the two halves separately left a
		// real gap: a mutation that replaced ActiveAccounts with a JOIN inside
		// ProjectMemberships was caught by the source guard below but NOT by this
		// test, because nothing here executed the composed function. That is the
		// mutation this entry exists for.
		"pkg/project.ProjectMemberships": func() error {
			exec := func(stmt string, args ...interface{}) {
				_, e := sess.UpdateBySql(stmt, args...).Exec()
				require.NoError(t, e, stmt)
			}
			exec("INSERT INTO `octo_project` " +
				"(project_id, space_id, name, creator, member_epoch, status, created_at, updated_at) " +
				"VALUES ('cp1','cs1','CP1','clive',1,1,NOW(3),NOW(3))")
			exec("INSERT INTO `octo_project_member` " +
				"(project_id, uid, space_id, role, status, created_at, updated_at, removing) " +
				"VALUES ('cp1','clive','cs1',0,1,NOW(3),NOW(3),0), " +
				"       ('cp1','cbanned','cs1',0,1,NOW(3),NOW(3),0)")

			_, roles, e := projectpkg.ProjectMemberships(sess, "cs1", "cp1",
				[]string{"clive", "cbanned"})
			if e != nil {
				return e
			}
			assert.Contains(t, roles, projectpkg.FoldID("clive"),
				"the live member must survive every conjunction")
			assert.NotContains(t, roles, projectpkg.FoldID("cbanned"),
				"and the banned one must be denied — on the drifted database too")
			// Left in place for the post-converge pass; the rows are idempotent
			// because the inserts above run once per closure invocation.
			exec("DELETE FROM `octo_project_member` WHERE project_id = 'cp1'")
			exec("DELETE FROM `octo_project` WHERE project_id = 'cp1'")
			return nil
		},
		// The WRITE path's half: JOIN rooted at space_member, where all three sides
		// drifted together.
		"lockSpaceSeatsTx": func() error {
			tx, e := sess.Begin()
			if e != nil {
				return e
			}
			defer tx.RollbackUnlessCommitted()
			held, e := db.lockSpaceSeatsTx(tx, "cs1", []string{"clive", "cbanned"})
			if e != nil {
				return e
			}
			assert.True(t, held["clive"], "the live member must hold a seat")
			assert.False(t, held["cbanned"],
				"the banned account must be refused by the user join")
			return tx.Commit()
		},
	}

	for name, run := range checks {
		assert.NoError(t, run(),
			"%s must work on a collation-drifted database. This is the production shape, and "+
				"CI cannot see it: ci/run-e2e-shard.sh creates its database general_ci, so every "+
				"table agrees there. A failure here means the statement compares an octo_* "+
				"column against a legacy one — make it a separate single-table read (see "+
				"pkg/user.ActiveAccounts) rather than adding an explicit COLLATE.", name)
	}

	converge()
	for name, run := range checks {
		assert.NoError(t, run(),
			"%s must still work once the legacy tables are converted: a statement that only "+
				"works against today's drift would break at the conversion window", name)
	}
}

// TestProjectMembershipsHasNoJoinOntoUser is the cheap structural half.
//
// The drift test above needs a live MySQL and an isolated database; this costs a
// string scan and fails the instant someone "simplifies" the separate liveness read
// into a join. It is scoped to the ONE function whose rows come from an octo_* table.
func TestProjectMembershipsHasNoJoinOntoUser(t *testing.T) {
	body := readPkgProjectFuncBody(t, "ProjectMemberships")
	upper := strings.ToUpper(body)
	require.Contains(t, upper, "OCTO_PROJECT_MEMBER",
		"the guard must actually be reading the right function — if this fails the scan is "+
			"vacuous")
	assert.NotContains(t, upper, "JOIN `USER`",
		"ProjectMemberships must NOT join `user`: its rows come from octo_project_member "+
			"(general_ci) while `user` is dump-imported (0900_ai_ci in production), so the join "+
			"is error 1267 there and green in CI. Account liveness goes through the separate "+
			"single-table read pkg/user.ActiveAccounts")
	assert.Contains(t, body, "user.ActiveAccounts(",
		"and the account half must still be conjoined — dropping it serves a globally banned "+
			"account to the peer as a project member with its real role")
}

// readPkgProjectFuncBody returns one function's source from pkg/project/membership.go,
// comments stripped so a doc comment mentioning the forbidden shape cannot fail the
// scan (that regression already happened once in this package's guards).
func readPkgProjectFuncBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../pkg/project/membership.go")
	require.NoError(t, err, "pkg/project/membership.go must be readable from here; if the "+
		"layout changed, re-point this guard rather than deleting it")
	src := string(raw)
	start := strings.Index(src, "func "+name+"(")
	require.Positive(t, start, "%s not found in pkg/project/membership.go", name)
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	var kept strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		kept.WriteString(line)
		kept.WriteByte('\n')
	}
	return kept.String()
}
