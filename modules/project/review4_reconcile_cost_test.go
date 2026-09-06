package project

// yujiawei Q4: the reconcile page queries bound ROWS RETURNED, not ROWS EXAMINED. The
// NOT EXISTS predicates live in the WHERE clause, so in the healthy case (zero violations)
// MySQL walks the primary key from the cursor to the END of the table evaluating three
// subqueries per row — LIMIT never triggers, reconcileMaxPages never helps, and the short
// page resets the cursor so the next tick repeats the full walk. Cost is
// O(active memberships) x subqueries per interval per pod.
//
// The fix under test: page the BASE rows (LIMIT over the primary key), evaluate the
// predicates as a per-row FLAG in the SELECT list, and let Go filter. LIMIT then bounds
// work, the cursor advances over inspected rows, and a short page means the table end.

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcilePageQueriesExamineBoundedRows asserts the shape the cost bound depends on:
// each paged reconcile query evaluates its predicates as a per-row flag ("AS violating")
// over a LIMIT-bounded base row set, and carries NO predicate in its WHERE clause beyond
// the base-row keyset. In the healthy case (zero violations) the old shape walked the
// primary key to the END of the table — LIMIT bounded rows returned, never rows examined.
// With the flag shape, LIMIT bounds work: the base page is at most ReconcileLimit rows, the
// cursor advances over INSPECTED rows, and a short page really is the table end.
func TestReconcilePageQueriesExamineBoundedRows(t *testing.T) {
	// Enumerated from the source, not hand-listed: round 3 added a fifth scan, and a guard
	// whose coverage has to be extended by hand is how the read-view guard came to cover one
	// call site out of six.
	fns := pagedReconcileQueries(t)
	require.GreaterOrEqual(t, len(fns), 4,
		"expected at least four paged reconcile queries; found %d — the enumeration probably "+
			"stopped matching, which would make this guard vacuous", len(fns))
	for _, fn := range fns {
		i := strings.Index(src(t), fn)
		require.NotEqual(t, -1, i, "function not found: %s", fn)
		body := fnBody(t, src(t), i)
		assert.Contains(t, body, "AS violating",
			"%s must evaluate its predicates as a per-row SELECT flag over the LIMIT-bounded "+
				"base rows; a WHERE-clause predicate bounds rows returned, not rows examined, "+
				"so a healthy table would be walked end to end every tick", fn)

		// The WHERE clause must carry NOTHING but the keyset cursor. round 2 found `status`
		// still in there: rows are never deleted in either table (removal and disband flip a
		// status column), and no index leads with status, so as closed rows accumulate that one
		// predicate put LIMIT back to bounding rows RETURNED — the property this whole shape
		// exists to provide. It belongs in the flag with the others.
		where := whereClause(t, body)
		require.NotEmpty(t, where, "%s must have a WHERE clause", fn)
		assert.NotContains(t, where, "status",
			"%s must not filter on status in its WHERE clause — put it in the violating flag: %s",
			fn, where)
	}
}

// whereClause returns the text of the query's WHERE clause up to ORDER BY, with the Go string
// concatenation glue removed so a clause split across source lines reads as one.
func whereClause(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "\"WHERE ")
	if i < 0 {
		return ""
	}
	rest := body[i:]
	end := strings.Index(rest, "ORDER BY")
	if end < 0 {
		end = len(rest)
	}
	clause := rest[:end]
	for _, glue := range []string{"\" + \"", "\"+\"", "\" +\"", "\"+ \""} {
		clause = strings.ReplaceAll(clause, glue, "")
	}
	return strings.ReplaceAll(clause, "\n", " ")
}

// pagedReconcileQueries returns the signature of every paged reconcile page query, found by
// naming convention (query...Page) so a newly added scan is covered without editing this test.
func pagedReconcileQueries(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(src(t), "\n") {
		if !strings.HasPrefix(line, "func (p *Project) query") || !strings.Contains(line, "Page(") {
			continue
		}
		// Keep the signature up to the parameter list: that is what Index(src, fn) matches on.
		paren := strings.Index(line, "Page(")
		require.Positive(t, paren, "unexpected signature shape: %s", line)
		out = append(out, line[:paren+len("Page")])
	}
	return out
}

// src / fnBody are small helpers over the file text.
func src(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("reconcile.go")
	require.NoError(t, err)
	return string(raw)
}

func fnBody(t *testing.T, s string, from int) string {
	t.Helper()
	end := strings.Index(s[from:], "\n}\n")
	require.NotEqual(t, -1, end)
	return s[from : from+end]
}
