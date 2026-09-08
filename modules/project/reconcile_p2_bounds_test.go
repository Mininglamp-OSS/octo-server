package project

// The cost guards, applied to the file the P2 scans actually live in.
//
// Same gap, same fix as reconcile_p1_bounds_test.go: TestReconcileQueriesAreBounded
// reads reconcile.go and TestP1ReconcileQueriesAreBounded reads reconcile_p1.go, so
// the two scans added by P2 — in reconcile_p2.go — were covered by neither. And the
// omission was not theoretical here either: queryMissingAllMemberGroupPage shipped
// with `g.id IS NULL` in its WHERE, which is precisely what the flag-over-base-page
// rule forbids and for precisely the reason it forbids it. That predicate matches
// NOTHING in a healthy database, so LIMIT bounded rows returned rather than rows
// examined and the statement walked the whole of octo_project every tick — costing
// most exactly when there was nothing to report.
//
// Re-applied to this file rather than widened in place, for the reason P1 gives: the
// exemptions P2 needs are declared here, next to their arguments, instead of being
// loosened into a rule shared with P0 and P1.

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reconcileP2File = "reconcile_p2.go"

func p2Src(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(reconcileP2File)
	require.NoError(t, err)
	return string(raw)
}

// TestReconcileP2QueriesAreBounded is TestReconcileQueriesAreBounded's rule applied
// to reconcile_p2.go: every SELECT carries LIMIT ?, and every scan bounds its page
// loop.
//
// No paging exemption list, unlike P1's: both P2 scans read populations that are
// expected to be NON-empty in normal operation (every active project is a base row
// of scan A, every active member of scan B), so neither has the argument
// scanRemovingStalls has.
func TestReconcileP2QueriesAreBounded(t *testing.T) {
	src := readStripped(t, reconcileP2File)

	selects := 0
	for _, stmt := range splitOnSelectBySql(src) {
		selects++
		if !containsAny(stmt, "LIMIT ?") {
			t.Errorf("%s has a SELECT without LIMIT ?: %s", reconcileP2File, stmt)
		}
	}
	require.Equal(t, 2, selects,
		"expected exactly the two P2 scan statements in %s, found %d — if you added one, "+
			"it is covered by this guard now; if one vanished, that is the regression this "+
			"floor exists to catch", reconcileP2File, selects)

	lineSrc := readLinesWithoutComments(t, reconcileP2File)
	scans := scanFuncNames(lineSrc)
	require.Equal(t, 2, len(scans),
		"expected exactly the two P2 scans, found %d: %v — the enumeration stopped matching, "+
			"which would make this check vacuous", len(scans), scans)

	for _, scan := range scans {
		body := scanFuncBody(lineSrc, scan)
		assert.Contains(t, body, "page < reconcileMaxPages",
			"%s does not bound its page loop with reconcileMaxPages, so one tick can walk "+
				"the whole table", scan)
	}
}

// whereExemptionsP2 declares, per predicate, why it is allowed to stay in the WHERE
// clause despite the flag-over-base-page rule.
//
// One entry, and it is the same shape as P1's project_id exemption: `p.status` is not
// a violation being filtered away, it SELECTS THE BASE POPULATION. I4 is a statement
// about active projects; a disbanded project is out of scope, not a row that happens
// to be compliant. Moving it into the flag would make every disbanded project a base
// row and cost the walk the rule exists to prevent.
//
// This used to add "and it leads idx_octo_project_all_member_group". Measured, that
// index was never chosen by either scan under either collation shape, and it has
// since been dropped from the migration (PR #855s fifth review, Q1). The exemption
// does not depend on it: a base selector earns its place in the WHERE by what it
// selects, not by which index serves it.
//
// The test for whether a predicate belongs here: does removing it change WHICH ROWS
// THE INVARIANT IS ABOUT, or only which of them are reported? The first is a base
// selector and stays; the second is a violation test and goes in the flag.
var whereExemptionsP2 = map[string]string{
	"p.status = ?": "selects the base population (I4 is about active projects); in the " +
		"flag it would make every disbanded project a base row",
}

// innerJoinOnExemptionsP2 does the same for INNER JOIN ON clauses, which bound base
// rows exactly as WHERE does.
//
// LEFT JOINs are deliberately not inspected, for P1's reason: a LEFT JOIN's ON clause
// cannot eliminate a base row, only decide whether the right side comes back NULL.
// That is why `g.status <> ?` in scan A and `gm.status = 1` in scan B are fine where
// they are — they are part of the violation test, evaluated per examined row.
var innerJoinOnExemptionsP2 = map[string]string{
	"pm.status = ?": "moving it into the flag makes every departed member row a base row — " +
		"the whole membership history of every project walked each tick, which is worse " +
		"than what the rule protects against; it also rides the octo_project_member " +
		"PRIMARY KEY lookup the join key already performs",
}

// TestP2PagedQueriesUseTheFlagOverBasePageShape is the cost guard's rule, applied to
// reconcile_p2.go.
func TestP2PagedQueriesUseTheFlagOverBasePageShape(t *testing.T) {
	src := p2Src(t)

	var fns []string
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "func (p *Project) query") || !strings.Contains(line, "Page(") {
			continue
		}
		paren := strings.Index(line, "Page(")
		require.Positive(t, paren, "unexpected signature shape: %s", line)
		fns = append(fns, line[:paren+len("Page")])
	}
	require.Equal(t, 2, len(fns),
		"expected exactly two paged P2 queries, found %d: %v — if you added one, cover it "+
			"here; if one vanished, that is the regression this floor exists to catch",
		len(fns), fns)

	for _, fn := range fns {
		i := strings.Index(src, fn)
		require.NotEqual(t, -1, i, "function not found: %s", fn)
		body := fnBody(t, src, i)

		assert.Contains(t, body, "AS violating",
			"%s must evaluate its predicates as a per-row SELECT flag over the LIMIT-bounded "+
				"base rows; a WHERE-clause predicate bounds rows returned, not rows examined", fn)

		where := whereClause(t, body)
		require.NotEmpty(t, where, "%s must have a WHERE clause", fn)
		stripped := where
		for exempt := range whereExemptionsP2 {
			stripped = strings.ReplaceAll(stripped, exempt, "")
		}
		for _, banned := range whereMustNotFilterOn {
			assert.NotContains(t, stripped, banned,
				"%s must not filter on %q in its WHERE clause — put it in the violating flag, "+
					"or add it to whereExemptionsP2 with the reason: %s", fn, banned, where)
		}

		for _, on := range innerJoinOnClauses(body) {
			onStripped := on
			for exempt := range innerJoinOnExemptionsP2 {
				onStripped = strings.ReplaceAll(onStripped, exempt, "")
			}
			for _, banned := range whereMustNotFilterOn {
				assert.NotContains(t, onStripped, banned,
					"%s filters on %q in an INNER JOIN's ON clause, which bounds base rows "+
						"exactly as WHERE does — put it in the violating flag, or add it to "+
						"innerJoinOnExemptionsP2 with the reason: %s", fn, banned, on)
			}
		}
	}
}
