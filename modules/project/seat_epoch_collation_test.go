package project

// The half of the seat-identity rule that lives on THIS side of the registry.
//
// modules/space pins that a seat transition hands its tx step the spelling
// `space_member` stores (modules/space/seat_identity_collation_test.go). That is
// necessary and not sufficient: the step then has to match that spelling against
// `octo_project_member`, which every migration here pins to utf8mb4_general_ci while
// `space_member` is dump-imported at utf8mb4_0900_ai_ci in production. So this file
// asserts the other direction on a DELIBERATELY DRIFTED schema:
//
//   - given the canonical spelling, the bump enumerates the seat and moves the epoch;
//   - given a spelling that only space_member's collation considers equal, it does
//     NOT — which is the control proving the fixture reproduces the divergence and
//     therefore that the assertion above is not vacuous.
//
// The second case is deliberately an assertion about the DEFECT rather than about the
// fix. It is what makes the pair meaningful: without it a database with no drift at
// all would pass the first case, and that is exactly how CI reads as green here —
// ci/run-e2e-shard.sh creates its schema with an explicit COLLATE utf8mb4_general_ci,
// so every table agrees and this class is invisible in every automated lane.

import (
	"os"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const seatEpochProbeDB = "octo_project_seat_epoch_probe"

var seatEpochProbeTables = []struct {
	name   string
	legacy bool
}{
	{"octo_project", false},
	{"octo_project_member", false},
	{"space", true},
	{"space_member", true},
}

func newSeatEpochProbe(t *testing.T) *dbr.Session {
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
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+seatEpochProbeDB+"`")
	exec(adminSess, "CREATE DATABASE `"+seatEpochProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+seatEpochProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + seatEpochProbeDB + "`").Exec()
		_ = conn.Close()
		_ = admin.Close()
	})
	sess := conn.NewSession(nil)

	src := currentSchemaName(t)
	for _, tbl := range seatEpochProbeTables {
		exec(sess, "CREATE TABLE `"+tbl.name+"` LIKE `"+src+"`.`"+tbl.name+"`")
		if tbl.legacy {
			exec(sess, "ALTER TABLE `"+tbl.name+
				"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}
	return sess
}

// seatEpochDrifts mirrors modules/space's two families: one compatibility WIDTH form
// and one letterlike symbol. Both match under utf8mb4_0900_ai_ci and neither matches
// under utf8mb4_general_ci — measured on MySQL 8.0.33 against both production
// collations. Accents and ASCII case are NOT in this class (they match under both) and
// are therefore useless as pins here, which earlier rounds of this branch got wrong in
// both directions.
var seatEpochDrifts = []struct {
	name    string
	stored  string
	drifted string
}{
	// The drifted spellings are written as EXPLICIT \u escapes, never as literal
	// characters. These code points are invisible or near-invisible in every editor
	// and diff view, and a copy-paste that silently degrades U+212A to ASCII "K"
	// turns the case into ordinary case drift — which BOTH collations fold, so the
	// test still passes while testing nothing. That happened once while writing
	// this file and was caught only by the control assertion below; the escapes are
	// so it cannot happen silently again.
	{"fullwidth", "sedriftwide", "\uFF53edriftwide"},      // U+FF53 FULLWIDTH LATIN SMALL S
	{"letterlike", "sedriftkelvin", "sedrift\u212Aelvin"}, // U+212A KELVIN SIGN, for the k
}

func TestSpaceMemberEpochBumpUnderCollationDrift(t *testing.T) {
	setup(t) // the shared schema is what CREATE TABLE ... LIKE copies from
	sess := newSeatEpochProbe(t)
	db := &DB{session: sess}

	exec := func(stmt string, args ...interface{}) {
		_, err := sess.InsertBySql(stmt, args...).Exec()
		require.NoError(t, err, stmt)
	}
	epochOfProbe := func(t *testing.T, projectID string) int64 {
		t.Helper()
		var got []int64
		_, err := sess.SelectBySql(
			"SELECT member_epoch FROM `octo_project` WHERE project_id = ?", projectID).Load(&got)
		require.NoError(t, err)
		require.Len(t, got, 1)
		return got[0]
	}
	bump := func(t *testing.T, spaceID, uid string) {
		t.Helper()
		tx, err := sess.Begin()
		require.NoError(t, err)
		defer tx.RollbackUnlessCommitted()
		require.NoError(t, db.bumpMemberEpochForSpaceMemberTx(tx, spaceID, uid))
		require.NoError(t, tx.Commit())
	}

	for _, drift := range seatEpochDrifts {
		t.Run(drift.name, func(t *testing.T) {
			spaceID := "sesp-" + drift.name
			projectID := "sepj-" + drift.name

			exec("INSERT INTO `space` (space_id, name, creator, status, created_at, updated_at) "+
				"VALUES (?, ?, 'probeop', 1, NOW(), NOW())", spaceID, spaceID)
			exec("INSERT INTO `space_member` (space_id, uid, role, status, created_at, updated_at) "+
				"VALUES (?, ?, 0, 1, NOW(), NOW())", spaceID, drift.stored)
			exec("INSERT INTO `octo_project` (project_id, space_id, name, creator, status, "+
				"created_at, updated_at) VALUES (?, ?, 'epoch probe', 'probeop', 1, NOW(3), NOW(3))",
				projectID, spaceID)
			exec("INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, "+
				"removing, invite_uid, created_at, updated_at) "+
				"VALUES (?, ?, ?, 0, 1, 0, 'probeop', NOW(3), NOW(3))",
				projectID, drift.stored, spaceID)

			// Control: the seat this fixture holds really is unreachable from the
			// drifted spelling under octo_project_member's collation. Without this the
			// positive case below could pass on a database with no drift at all.
			before := epochOfProbe(t, projectID)
			bump(t, spaceID, drift.drifted)
			require.Equal(t, before, epochOfProbe(t, projectID),
				"CONTROL — the drifted spelling must enumerate NOTHING here. If the epoch "+
					"moved, this fixture is not reproducing production's collation split and "+
					"the assertion below proves nothing. This is also the defect itself: a "+
					"seat transition that hands the caller's spelling to this step closes or "+
					"reopens a Space seat and moves no epoch, so a peer's cached decision "+
					"keeps agreeing with a number that never changed.")

			// And the shipped path: the canonical spelling enumerates and bumps, on the
			// same drifted schema. This is also the 1267 assertion — the statement is
			// rooted at octo_project_member and joins no legacy table, so it must run.
			bump(t, spaceID, drift.stored)
			assert.Greater(t, epochOfProbe(t, projectID), before,
				"the spelling space_member stores must enumerate the project seat and move "+
					"member_epoch, on a schema carrying production's collation split. This is "+
					"what modules/space's ResolveSeatTx exists to guarantee gets here.")
		})
	}
}
