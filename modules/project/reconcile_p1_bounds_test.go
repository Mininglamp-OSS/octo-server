package project

// The cost guards, applied to the file the P1 scans actually live in.
//
// PR #844's review found the gap: TestReconcileQueriesAreBounded reads
// reconcile.go, and the cost guard's src() reads reconcile.go, so the three
// scans added by P1 — in reconcile_p1.go — were covered by neither. The
// omission was not theoretical: queryI2Page carried `g.status <> 2` in its WHERE
// clause, which is exactly what the cost guard forbids and for exactly the
// reason it forbids it.
//
// This file re-applies both rules to reconcile_p1.go rather than widening the
// existing guards in place, so the P0 guards keep failing on P0's terms and the
// two exemptions P1 needs are declared here, next to their reasons, instead of
// being loosened into a shared rule.

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
	require.GreaterOrEqual(t, len(scans), 3,
		"expected at least the three P1 scans, found %d: %v — the enumeration stopped "+
			"matching, which would make this check vacuous", len(scans), scans)

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

// whereMustNotFilterOn lists, per paged query, the predicates that are allowed
// to stay in the WHERE clause despite the flag-over-base-page rule.
//
// One entry, and it is the reason the rule cannot simply be copied: moving the
// project_id emptiness test into the violating flag would make every Space-direct
// group's members base rows — a walk of the whole group_member table every tick,
// which is the opposite of what the rule is for. It is also the selective
// predicate: what it excludes is most of the product.
//
// Everything else belongs in the flag. `g.status <> 2` was in the WHERE until
// PR #844's review; `group`.status leads no index, so as disbanded groups
// accumulate it put LIMIT back to bounding rows RETURNED rather than examined.
var whereMustNotFilterOn = []string{"status"}

// innerJoinOnExemptions declares, per predicate, why it is allowed to sit in an
// INNER JOIN's ON clause.
//
// The WHERE check above was, on its own, a claim the guard did not check: for
// cost purposes an INNER JOIN's ON clause filters base rows exactly as WHERE
// does, so moving a predicate from one to the other satisfies the rule while
// changing nothing. Raised by PR #846's review as P2-3.
//
// LEFT JOINs are a different matter and are deliberately not inspected: a LEFT
// JOIN's ON clause cannot eliminate a base row, only decide whether the right
// side is NULL. That is why `jobp.status = 0` / `jobs.status = 0` are fine where
// they are, and why the rule has to distinguish the two join kinds rather than
// banning a needle everywhere it appears.
// goStringGlue matches the `" +` … `"` (optionally with a // comment line in
// between) that separates adjacent fragments of one SQL string in the source.
var goStringGlue = regexp.MustCompile(`"\s*\+\s*(?://[^\n]*\n\s*)*"`)

var innerJoinOnExemptions = map[string]string{
	"gm.is_deleted = 0": "moving it into the flag makes every soft-deleted member row a " +
		"base row — the whole tombstone history walked each tick, which is worse than " +
		"what the rule is protecting against",
	"gm.status = 1": "rides the same group_member.group_no index lookup as the join key " +
		"itself, so it costs nothing where it is; in the flag it would widen the base " +
		"page by every blacklisted member",
}

// TestP1PagedQueriesUseTheFlagOverBasePageShape is the cost guard's rule,
// applied to reconcile_p1.go.
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
	require.Equal(t, 2, len(fns),
		"expected exactly two paged P1 queries, found %d: %v — if you added one, cover it "+
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
		for _, banned := range whereMustNotFilterOn {
			assert.NotContains(t, where, banned,
				"%s must not filter on %q in its WHERE clause — put it in the violating "+
					"flag: %s", fn, banned, where)
		}

		for _, on := range innerJoinOnClauses(body) {
			stripped := on
			for exempt := range innerJoinOnExemptions {
				stripped = strings.ReplaceAll(stripped, exempt, "")
			}
			for _, banned := range whereMustNotFilterOn {
				assert.NotContains(t, stripped, banned,
					"%s filters on %q in an INNER JOIN's ON clause, which bounds base rows "+
						"exactly as WHERE does — put it in the violating flag, or add it to "+
						"innerJoinOnExemptions with the reason: %s", fn, banned, on)
			}
		}
	}
}

// innerJoinOnClauses returns the ON clause of each INNER JOIN in the body, with
// the Go concatenation glue removed so a clause split across source lines reads
// as one.
//
// A clause ends at the next JOIN, WHERE, ORDER BY or the end of the statement.
// LEFT JOINs are skipped for the reason innerJoinOnExemptions gives.
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
