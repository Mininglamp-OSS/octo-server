package project

import (
	"fmt"
	"os"
	"strings"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// The D7 guard's execution plan, asserted against a deliberately drifted database.
//
// # Why a plan assertion and not just "the statement runs"
//
// TestP2StatementsSurviveCollationDrift already proves these statements EXECUTE
// under the production collation shape. That is the 1267 question, and it is not
// the question that cost this PR its fifth round: the statement ran fine and
// planned as a full scan of `group` on every group exit, because an explicit
// COLLATE has coercibility 0 — the comparison lands in that collation and the
// legacy column has to be converted per row, so its UNIQUE index cannot serve it.
//
// CI creates its database general_ci on both sides, so no ordinary test can see
// that: every plan there is const. This one builds the drifted shape on purpose
// and reads the plan, which is the only way the suite can hold the property that
// matters — the guard on the product's hottest group paths must reach its row by
// an index.
//
// # The control is the point
//
// A "type is not ALL" assertion is worthless unless the fixture can produce an
// ALL. So this first EXPLAINs the JOINED form that shipped and was removed, and
// requires that it IS a full scan under drift and is NOT one after the
// conversion. That single pair of assertions is the finding itself, held in CI:
// it says why the join was replaced, and it says why "just move the COLLATE to
// the legacy side" is right today and wrong after the conversion.
func TestTheD7PredicateReachesItsRowByAnIndexUnderCollationDrift(t *testing.T) {
	setup(t)
	p, converge := newP1CollationProbe(t)
	sess := p.db.session

	const (
		probeProject = "p2_plan_project"
		probeGroup   = "p2_plan_group"
		probeSpace   = "p2_plan_space"
	)
	seedPlanProbeRows(t, sess, probeProject, probeSpace, probeGroup)

	// The joined form, verbatim as it shipped before the fifth review.
	joined := "SELECT p.all_member_group_no FROM `octo_project` p " +
		"JOIN `group` g ON g.group_no = p.all_member_group_no COLLATE utf8mb4_general_ci " +
		"AND g.status <> ? AND g.project_id = p.project_id COLLATE utf8mb4_general_ci " +
		"WHERE p.project_id = ? AND p.status = 1"

	require.Equal(t, "ALL", accessTypeOf(t, sess, "g", joined, groupStatusDisband, probeProject),
		"CONTROL: under the production collation shape the joined form must full-scan "+
			"`group`. If this stops being true the fixture has stopped reproducing "+
			"production, and every other assertion in this test is vacuous")

	// The shipped split reads: two single-table statements, no cross-schema
	// comparison, no COLLATE anywhere in them.
	shipped := splitPredicateStatements(probeProject, probeGroup)
	for name, s := range shipped {
		for _, row := range explainRows(t, sess, s.sql, s.args...) {
			require.NotEqual(t, "ALL", derefOr(row.Type, ""),
				"%s must reach its row by an index under the production collation shape — "+
					"a full scan here is a scan of a core table on every group exit, "+
					"disband, member removal, transfer and blacklist (table %q)",
				name, derefOr(row.Table, ""))
			require.NotEmpty(t, derefOr(row.Key, ""),
				"%s must have chosen an index (table %q)", name, derefOr(row.Table, ""))
		}
	}

	converge()

	require.NotEqual(t, "ALL", accessTypeOf(t, sess, "g", joined, groupStatusDisband, probeProject),
		"CONTROL, other direction: once the legacy tables are converted the joined form "+
			"plans fine again. That is why 'move the COLLATE to the legacy side' is not "+
			"the same fix — it would be correct today and silently wrong here")

	for name, s := range shipped {
		for _, row := range explainRows(t, sess, s.sql, s.args...) {
			require.NotEqual(t, "ALL", derefOr(row.Type, ""),
				"%s must STILL use an index after the conversion: a statement that needs "+
					"no collation opinion is supposed to survive it untouched", name)
		}
	}
}

type planStatement struct {
	sql  string
	args []interface{}
}

// splitPredicateStatements is every statement the two split predicates run, taken
// from the source of truth rather than copied — a copy would pin the plan of a
// statement nobody executes.
func splitPredicateStatements(projectID, groupNo string) map[string]planStatement {
	pkgStmts := projectpkg.AllMemberGroupPredicateStatementsForTest()
	return map[string]planStatement{
		"pkg/project.IsAllMemberGroup pointer read": {
			sql: pkgStmts[0], args: []interface{}{projectID},
		},
		"pkg/project.IsAllMemberGroup group read": {
			sql: pkgStmts[1], args: []interface{}{groupNo, projectpkg.GroupStatusDisband, projectID},
		},
		"queryAllMemberGroupNo pointer read": {
			sql: sqlProjectAllMemberGroupPointer, args: []interface{}{projectID, StatusNormal},
		},
		"queryAllMemberGroupNo group read": {
			sql: sqlProjectAllMemberGroupRow, args: []interface{}{groupNo, groupStatusDisband, projectID},
		},
		// The repair write. It was the last statement on a write path still carrying
		// the joined-with-COLLATE shape; a project with no active owner pays it on
		// every members/add and buys nothing. PR #855s sixth review, P2-1.
		"clearStaleAllMemberGroupPointer write": {
			sql:  sqlProjectClearStaleAllMemberGroup,
			args: []interface{}{projectID, StatusNormal, groupNo},
		},
	}
}

// TestTheSplitPredicatesAreWhatProductionRuns closes the gap the plan guard has on
// its own: it EXPLAINs constants, and a constant stays referenced by this test even
// if the production function stops using it.
//
// So somebody could re-inline a joined query into IsAllMemberGroup, leave the
// constant in place, and the plan guard would stay green while production
// regressed to the shape the fifth review blocked on. PR #855s sixth review, P2-4.
func TestTheSplitPredicatesAreWhatProductionRuns(t *testing.T) {
	for path, wanted := range map[string][]string{
		"../../pkg/project/all_member_group.go": {
			"sqlAllMemberGroupPointer", "sqlAllMemberGroupRow",
		},
		"db_all_member_group.go": {
			"sqlProjectAllMemberGroupPointer",
			"sqlProjectAllMemberGroupRow",
			"sqlProjectClearStaleAllMemberGroup",
		},
	} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		src := string(raw)
		for _, name := range wanted {
			// Twice: the declaration and at least one use. A constant declared and
			// never executed is exactly the state this guard exists to catch.
			require.GreaterOrEqual(t, strings.Count(src, name), 2,
				"%s must both declare and USE %s — the plan guard EXPLAINs that constant, "+
					"so a production function that stopped running it would regress silently "+
					"with every plan assertion still green", path, name)
		}
	}
}

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

	for i := 0; i < 200; i++ {
		_, err = sess.InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
				"VALUES (?, 'padding', 'u_owner', 1, ?, '')",
			fmt.Sprintf("plan_pad_%03d", i), spaceID).Exec()
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
