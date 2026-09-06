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
	for _, fn := range []string{
		"func (p *Project) queryI1ViolationPage",
		"func (p *Project) queryAbandonedLeakPage",
		"func (p *Project) queryInspectedProjectPage",
	} {
		i := strings.Index(src(t), fn)
		require.NotEqual(t, -1, i, "function not found: %s", fn)
		body := fnBody(t, src(t), i)
		assert.Contains(t, body, "AS violating",
			"%s must evaluate its predicates as a per-row SELECT flag over the LIMIT-bounded "+
				"base rows; a WHERE-clause predicate bounds rows returned, not rows examined, "+
				"so a healthy table would be walked end to end every tick", fn)
	}
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
