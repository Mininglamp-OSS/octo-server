package space

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestMemberQueriesCollationMismatch is a regression test for issue #482.
//
// queryMembers / searchMembers / querySpaceBots JOIN space_member.uid against
// user, user_verification and robot. user_verification was force-converted to
// utf8mb4_general_ci by the OSS compat-repair migration (20260512), while
// space_member.uid (and user.uid / robot.robot_id) inherit the DB-default
// collation. On a MySQL-8 utf8mb4_0900_ai_ci-default deployment those ON
// comparisons mix collations and raise Error 1267 → the member list 500s.
// CI provisions general_ci, so the bytes match and the failure is invisible.
//
// To reproduce the failure mode on ANY DB default (including CI's general_ci)
// we force space_member.uid to a *distinct* collation (utf8mb4_bin), which
// recreates the exact asymmetry against the general_ci JOIN targets. The
// explicit COLLATE utf8mb4_general_ci pins added on every sm.uid JOIN must make
// each query succeed regardless. Without the pins these queries return
// Error 1267 (this test fails).
func TestMemberQueriesCollationMismatch(t *testing.T) {
	_, _, err := setup(t)
	assert.NoError(t, err)

	// Capture and restore the original space_member.uid collation so sibling
	// tests in this package are unaffected (CleanAllTables clears data, not schema).
	var origCollation string
	_, err = testCtx.DB().SelectBySql(
		"SELECT COLLATION_NAME FROM information_schema.COLUMNS " +
			"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='space_member' AND COLUMN_NAME='uid'",
	).Load(&origCollation)
	assert.NoError(t, err)
	if origCollation == "" {
		t.Skip("could not read space_member.uid collation; skipping collation regression")
	}
	modifyUIDCollation := func(coll string) {
		_, e := testCtx.DB().UpdateBySql(
			"ALTER TABLE `space_member` MODIFY `uid` VARCHAR(40) " +
				"CHARACTER SET utf8mb4 COLLATE " + coll + " NOT NULL DEFAULT ''",
		).Exec()
		assert.NoError(t, e)
	}
	// utf8mb4_0900_ai_ci is the MySQL-8 server default that triggers the real
	// bug (and CI runs mysql:8.0), so it faithfully recreates the asymmetry
	// against the general_ci JOIN targets. It genuinely conflicts with
	// general_ci (Error 1267) — unlike utf8mb4_bin, which MySQL freely coerces.
	modifyUIDCollation("utf8mb4_0900_ai_ci")
	defer modifyUIDCollation(origCollation)

	const spaceId = "sp-collation-482"
	seedMemberSearchSpace(t, spaceId, testutil.UID)

	// Human member with a verification row (exercises the user + user_verification JOINs).
	const humanUID = "u-collation-human"
	seedFallbackUser(t, humanUID, "Alice")
	seedFallbackVerification(t, humanUID, "Alice Real")
	seedMemberSearchMember(t, spaceId, humanUID, 0, 1)

	// Robot member (exercises the robot JOIN — the path PR #489's migration regressed).
	const botUID = "u-collation-bot"
	_, err = testCtx.DB().InsertInto("robot").
		Columns("robot_id", "token", "status", "creator_uid").
		Values(botUID, "test-token", 1, testutil.UID).Exec()
	assert.NoError(t, err)
	seedMemberSearchMember(t, spaceId, botUID, 0, 1)

	// queryMembers: the endpoint from #482. Must not raise Error 1267.
	members, err := testSpaceDB.queryMembers(spaceId, testutil.UID, 1, 100)
	assert.NoError(t, err, "queryMembers must survive cross-collation sm.uid JOINs (#482)")
	if _, ok := findMemberDetail(members, humanUID); !ok {
		t.Errorf("expected human member %q in queryMembers result", humanUID)
	}

	// searchMembers + countSearchMembers: same sm.uid JOIN pattern.
	list, err := testSpaceDB.searchMembers(spaceId, "", 1, 100)
	assert.NoError(t, err, "searchMembers must survive cross-collation sm.uid JOINs (#482)")
	assert.NotEmpty(t, list)
	_, err = testSpaceDB.countSearchMembers(spaceId, "")
	assert.NoError(t, err, "countSearchMembers must survive cross-collation sm.uid JOINs (#482)")

	// querySpaceBots: INNER JOIN robot on sm.uid.
	bots, err := testSpaceDB.querySpaceBots(spaceId)
	assert.NoError(t, err, "querySpaceBots must survive cross-collation sm.uid JOINs (#482)")
	assert.NotEmpty(t, bots)
}
