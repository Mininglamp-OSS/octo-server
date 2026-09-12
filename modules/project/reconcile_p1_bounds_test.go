package project

// The cost guards, applied to the file the P1 scans actually live in.
//
// The query-bound guard used to miss the P1 scans because TestReconcileQueriesAreBounded
// reads reconcile.go while these scans live in reconcile_p1.go. This file applies the same
// bounded-query rules directly to the P1 implementation, so a new scan or query cannot
// silently escape the cost contract.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reconcileP1File = "reconcile_p1.go"

// scansExemptFromPaging are scans that deliberately do not page.
//
// scanRemovingStalls is the only one, and the brief records it as a declared
// deviation rather than an oversight: the population it reads is meant to be
// EMPTY, the (removing, updated_at) index takes it straight to the stalled rows,
// and LIMIT caps what comes back. A cursor over a set that is supposed to have
// no members would be machinery with nothing to do — and if it is ever large
// enough to need paging, the alert it exists to raise has already fired.
var scansExemptFromPaging = map[string]string{
	"scanRemovingStalls": "single bounded read over a population that is meant to be empty",
}

func p1Src(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(reconcileP1File)
	require.NoError(t, err)
	return string(raw)
}

// TestP1ReconcileQueriesAreBounded is TestReconcileQueriesAreBounded's rule,
// applied to reconcile_p1.go: every SELECT carries LIMIT ?, and every scan that
// pages bounds its page loop.
func TestP1ReconcileQueriesAreBounded(t *testing.T) {
	src := readStripped(t, reconcileP1File)

	selects := 0
	for _, stmt := range splitOnSelectBySql(src) {
		selects++
		if !containsAny(stmt, "LIMIT ?") {
			t.Errorf("%s has a SELECT without LIMIT ?: %s", reconcileP1File, stmt)
		}
	}
	require.NotZero(t, selects,
		"no SelectBySql found in %s; this guard would pass vacuously", reconcileP1File)

	lineSrc := readLinesWithoutComments(t, reconcileP1File)
	scans := scanFuncNames(lineSrc)
	require.Equal(t, 2, len(scans),
		"expected the two P1 scans (I3 and removal stalls), found %d: %v — update this "+
			"guard when the P1 scan inventory changes", len(scans), scans)

	for _, scan := range scans {
		if why, exempt := scansExemptFromPaging[scan]; exempt {
			body := scanFuncBody(lineSrc, scan)
			assert.NotContains(t, body, "page < reconcileMaxPages",
				"%s is on the no-paging exemption list (%s) but pages anyway — remove the "+
					"exemption rather than leaving it as a lie", scan, why)
			continue
		}
		body := scanFuncBody(lineSrc, scan)
		assert.Contains(t, body, "page < reconcileMaxPages",
			"%s does not bound its page loop with reconcileMaxPages, so one tick can walk "+
				"the whole table", scan)
	}
}

// whereMustNotFilterOn lists predicates that must not narrow a paged query's
// base rows in the WHERE clause. Such predicates belong in the per-row flag.
var whereMustNotFilterOn = []string{"status"}

// An INNER JOIN's ON clause filters base rows just like WHERE does. Keep the
// check separate from the WHERE check so a predicate cannot evade the cost
// contract by moving between equivalent clauses.
var goStringGlue = regexp.MustCompile(`"\s*\+\s*(?://[^\n]*\n\s*)*"`)

// TestP1PagedQueriesUseTheFlagOverBasePageShape applies the cost guard to
// reconcile_p1.go.
func TestP1PagedQueriesUseTheFlagOverBasePageShape(t *testing.T) {
	src := p1Src(t)

	var fns []string
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "func (p *Project) query") || !strings.Contains(line, "Page(") {
			continue
		}
		paren := strings.Index(line, "Page(")
		require.Positive(t, paren, "unexpected signature shape: %s", line)
		fns = append(fns, line[:paren+len("Page")])
	}
	require.Equal(t, 1, len(fns),
		"expected exactly one paged P1 query, found %d: %v — update this guard if the "+
			"P1 query inventory changes", len(fns), fns)

	for _, fn := range fns {
		i := strings.Index(src, fn)
		require.NotEqual(t, -1, i, "function not found: %s", fn)
		body := fnBody(t, src, i)

		assert.Contains(t, body, "AS violating",
			"%s must evaluate its predicates as a per-row SELECT flag over the LIMIT-bounded "+
				"base rows; a WHERE-clause predicate bounds rows returned, not rows examined", fn)

		where := whereClause(t, body)
		require.NotEmpty(t, where, "%s must have a WHERE clause", fn)
		for _, banned := range whereMustNotFilterOn {
			assert.NotContains(t, where, banned,
				"%s must not filter on %q in its WHERE clause — put it in the violating "+
					"flag: %s", fn, banned, where)
		}

		for _, on := range innerJoinOnClauses(body) {
			for _, banned := range whereMustNotFilterOn {
				assert.NotContains(t, on, banned,
					"%s filters on %q in an INNER JOIN's ON clause, which bounds base rows "+
						"exactly as WHERE does — move it into the violating flag", fn, banned)
			}
		}
	}
}

// innerJoinOnClauses returns the ON clause of each INNER JOIN in the body, with
// the Go concatenation glue removed so a clause split across source lines reads
// as one.
//
// A clause ends at the next JOIN, WHERE, ORDER BY or the end of the statement.
// LEFT JOINs are skipped because their ON clauses do not eliminate base rows.
func innerJoinOnClauses(body string) []string {
	// Collapse the Go string concatenation glue (and the comments that sit
	// between the fragments) so a clause split across source lines reads as one —
	// otherwise an exemption that happens to straddle a line break would not
	// match and the guard would report it as a violation.
	flat := goStringGlue.ReplaceAllString(body, "")
	flat = strings.ReplaceAll(flat, "\n", " ")

	var out []string
	rest := flat
	for {
		i := strings.Index(rest, "INNER JOIN ")
		if i < 0 {
			return out
		}
		rest = rest[i+len("INNER JOIN "):]
		on := strings.Index(rest, " ON ")
		if on < 0 {
			return out
		}
		clause := rest[on:]
		end := len(clause)
		for _, stop := range []string{"INNER JOIN ", "LEFT JOIN ", "RIGHT JOIN ", "WHERE ", "ORDER BY "} {
			if j := strings.Index(clause, stop); j >= 0 && j < end {
				end = j
			}
		}
		out = append(out, clause[:end])
	}
}
