package project

import (
	"fmt"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// explainRow is the subset of EXPLAIN this test reads. Pointers because MySQL
// returns NULL for `key` when no index was chosen — which is the answer being
// asserted against, so it must not silently scan as "".
type explainRow struct {
	Table *string `db:"table"`
	Type  *string `db:"type"`
	Key   *string `db:"key"`
}

func explainRows(t *testing.T, sess *dbr.Session, stmt string, args ...interface{}) []explainRow {
	t.Helper()
	var rows []explainRow
	_, err := sess.SelectBySql("EXPLAIN "+stmt, args...).Load(&rows)
	require.NoError(t, err, "EXPLAIN %s", stmt)
	require.NotEmpty(t, rows, "EXPLAIN returned no plan for %s", stmt)
	return rows
}

// accessTypeOf returns the access type EXPLAIN reports for one table alias.
func accessTypeOf(t *testing.T, sess *dbr.Session, alias, stmt string, args ...interface{}) string {
	t.Helper()
	for _, row := range explainRows(t, sess, stmt, args...) {
		if derefOr(row.Table, "") == alias {
			return derefOr(row.Type, "")
		}
	}
	t.Fatalf("EXPLAIN produced no row for alias %q in %s", alias, stmt)
	return ""
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// seedPlanProbeRows gives the optimizer something to plan against.
//
// An EMPTY table makes this test vacuous rather than failing: EXPLAIN answers
// "no matching row in const table" with a NULL table and NULL type, so every
// "type is not ALL" assertion would pass without looking at an index. The matching
// row is what makes the plan real, and the padding rows are what make a full scan
// a distinguishable alternative to using the index.
func seedPlanProbeRows(t *testing.T, sess *dbr.Session, projectID, spaceID, groupNo string) {
	t.Helper()
	paddingPrefix := projectID
	if len(paddingPrefix) > 8 {
		paddingPrefix = paddingPrefix[len(paddingPrefix)-8:]
	}
	_, err := sess.InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, "+
			"all_member_group_no, created_at, updated_at) "+
			"VALUES (?, ?, 'plan probe', 'u_owner', 1, ?, NOW(3), NOW(3))",
		projectID, spaceID, groupNo).Exec()
	require.NoError(t, err)

	_, err = sess.InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
			"VALUES (?, 'plan probe', 'u_owner', 1, ?, ?)",
		groupNo, spaceID, projectID).Exec()
	require.NoError(t, err)

	for i := range 200 {
		_, err = sess.InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
				"VALUES (?, 'padding', 'u_owner', 1, ?, '')",
			fmt.Sprintf("plan_pad_%s_%03d", paddingPrefix, i), spaceID).Exec()
		require.NoError(t, err)
	}
	// ANALYZE so the optimizer plans against the rows just written rather than
	// against an empty table's stale statistics.
	var analyzed []struct {
		Table string `db:"Table"`
	}
	_, err = sess.SelectBySql("ANALYZE TABLE `group`").Load(&analyzed)
	require.NoError(t, err)
}
