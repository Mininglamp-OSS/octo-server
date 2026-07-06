package space

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collationOf returns the COLLATION_NAME of a single column in the test schema.
func collationOf(t *testing.T, table, column string) string {
	t.Helper()
	var name string
	_, err := testCtx.DB().SelectBySql(
		"SELECT COLLATION_NAME FROM information_schema.columns "+
			"WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?",
		table, column,
	).Load(&name)
	require.NoError(t, err)
	return name
}

func mustExec(t *testing.T, sql string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(sql).Exec()
	require.NoError(t, err, sql)
}

// alignUpStatements reads the shipped migration file and returns its "+migrate
// Up" statements. The repro test executes exactly what production runs, so the
// green half proves the migration on disk — not a hand-copied variant — resolves
// the 1267.
func alignUpStatements(t *testing.T) []string {
	t.Helper()
	raw, err := sqlFS.ReadFile("sql/20260707000001_align_member_join_collation.sql")
	require.NoError(t, err)
	body := string(raw)
	if i := strings.Index(body, "-- +migrate Down"); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "-- +migrate Up"))

	// Drop comment / blank lines first so a ';' inside a comment does not split
	// a statement, then split the remaining SQL on ';'.
	var kept []string
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}
		kept = append(kept, ln)
	}

	var stmts []string
	for _, chunk := range strings.Split(strings.Join(kept, "\n"), ";") {
		if stmt := strings.TrimSpace(chunk); stmt != "" {
			stmts = append(stmts, stmt)
		}
	}
	require.Len(t, stmts, 4, "migration should carry four ALTER statements")
	return stmts
}

// TestQueryMembersCollationMismatch1267 reproduces XIN-459 / XIN-476: when the
// GetSpaceMembers join keys drift onto different collations (as they do on a
// fresh MySQL 8 dev DB whose server default is utf8mb4_0900_ai_ci), queryMembers
// aborts with MySQL error 1267 and the endpoint returns 400. Applying the
// collation-alignment migration makes every join key share utf8mb4_general_ci,
// after which the same query succeeds and the name-fallback chain still works.
func TestQueryMembersCollationMismatch1267(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	// Force the local-style drift: put the migration-created join keys on the
	// MySQL 8 server default (utf8mb4_0900_ai_ci) while user_verification keeps
	// the utf8mb4_general_ci its DDL pins. Only the uv.user_id = sm.uid
	// comparison then mixes collations — exactly the reported failure shape.
	mustExec(t, "ALTER TABLE `space_member` MODIFY `uid` VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT ''")
	mustExec(t, "ALTER TABLE `user` MODIFY `uid` VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT ''")
	mustExec(t, "ALTER TABLE `robot` MODIFY `robot_id` VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL DEFAULT ''")
	mustExec(t, "ALTER TABLE `user_verification` MODIFY `user_id` VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL")
	require.Equal(t, "utf8mb4_0900_ai_ci", collationOf(t, "space_member", "uid"))
	require.Equal(t, "utf8mb4_general_ci", collationOf(t, "user_verification", "user_id"))

	const spaceId = "sp-collfix"
	seedMemberSearchSpace(t, spaceId, testutil.UID)
	const uid = "u-collfix"
	seedFallbackUser(t, uid, "")
	seedFallbackVerification(t, uid, "Zhang Wei")
	seedMemberSearchMember(t, spaceId, uid, 0, 1)

	// RED: mismatched collation on the uv join aborts the whole query with 1267.
	_, err = testSpaceDB.queryMembers(spaceId, testutil.UID, 1, 100)
	require.Error(t, err, "expected MySQL 1267 before collation alignment")
	assert.Contains(t, strings.ToLower(err.Error()), "illegal mix of collations", err.Error())

	// Apply the shipped migration verbatim.
	for _, stmt := range alignUpStatements(t) {
		mustExec(t, stmt)
	}

	// GREEN: every join key now shares one collation, so the query succeeds and
	// the user.name -> user_verification.real_name fallback is intact.
	members, err := testSpaceDB.queryMembers(spaceId, testutil.UID, 1, 100)
	require.NoError(t, err, "query must succeed after collation alignment")
	m, ok := findMemberDetail(members, uid)
	require.True(t, ok, "seeded member missing from result")
	assert.Equal(t, "Zhang Wei", m.DisplayName(), "real_name fallback must survive the fix")

	for _, c := range []struct{ table, column string }{
		{"space_member", "uid"},
		{"user", "uid"},
		{"robot", "robot_id"},
		{"user_verification", "user_id"},
	} {
		assert.Equal(t, "utf8mb4_general_ci", collationOf(t, c.table, c.column),
			"%s.%s should be aligned to utf8mb4_general_ci", c.table, c.column)
	}
}
