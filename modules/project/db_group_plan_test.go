package project

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The group list's execution plan, asserted rather than reasoned about.
//
// PR #861's review rounds asked for this twice and I deferred it twice, on the
// grounds that `group` is empty in CI so the assertion would be weak. That reason
// does not hold and the review said so: EXPLAIN resolves the chosen index from
// schema metadata, not from row counts, so an empty table still proves which index
// MySQL picked. What made it cheap in the end is #855's plan rig (explainRows /
// accessTypeOf, all_member_group_plan_test.go), landed for a different predicate.
//
// The property is worth a guard for the same reason that predicate's was: the
// statement is a user-facing paginated GET, and the failure mode is silent. A later
// edit that drops `g.space_id` — the LEADING column of group_space_project, which
// db_group.go's comment calls the Space isolation boundary rather than decoration —
// degrades the request from an index range to a scan of a core IM table with
// nothing red anywhere.
func TestTheProjectGroupListReachesItsRowsByAnIndex(t *testing.T) {
	setup(t)
	p := New(testCtx)
	sess := p.db.session

	const (
		probeSpace   = "plan_probe_space"
		probeProject = "plan_probe_project"
		probeUID     = "plan_probe_uid"
	)

	// The production constant, not a copy of it: a copy passes forever once the two
	// drift, which is what makes a stale plan assertion worse than none.
	rows := explainRows(t, sess, sqlListMyProjectGroups,
		probeSpace, probeProject, groupStatusDisband, probeUID, 1, 10, 0)

	require.NotEmpty(t, rows, "EXPLAIN returned no rows; the assertions below would be vacuous")
	for _, row := range rows {
		table := derefOr(row.Table, "")
		assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
			"the project group list must reach %q by an index: a full scan here is a scan "+
				"of a core IM table on every render of the project 群聊 tab", table)
		assert.NotEmpty(t, derefOr(row.Key, ""),
			"the project group list must have chosen an index for %q", table)
	}

	// Naming the index, not just "some index". group_space_project is
	// (space_id, project_id), and the whole reason space_id is in the predicate
	// rather than inferred from project_id is that it is the leading column —
	// without it the index cannot serve the query at all.
	assert.Equal(t, "group_space_project", indexChosenFor(t, rows, "g"),
		"dropping g.space_id from the predicate would still return the right rows and "+
			"would silently lose this index; that is what this assertion exists to catch")
}

// TestThePinQuotaCountReachesItsRowsByAnIndex covers the other statement this PR
// added on a write path.
//
// It runs once per pin, so a scan here is a scan of a table that grows with users
// times pinned projects, on an ordinary user action. The migration justifies
// idx_octo_project_user_setting_uid_pinned by exactly this query — the unique key
// leads with project_id and cannot serve a lookup by uid — so the justification is
// asserted rather than left in a comment.
func TestThePinQuotaCountReachesItsRowsByAnIndex(t *testing.T) {
	setup(t)
	p := New(testCtx)
	sess := p.db.session

	rows := explainRows(t, sess, sqlCountPinnedInSpace,
		MemberStatusActive, "plan_probe_uid", "plan_probe_space", StatusNormal,
		DiscoverabilitySpaceListed)

	require.NotEmpty(t, rows, "EXPLAIN returned no rows; the assertions below would be vacuous")
	for _, row := range rows {
		table := derefOr(row.Table, "")
		assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
			"the pin quota count must reach %q by an index: it runs on every pin", table)
	}
	assert.Equal(t, "idx_octo_project_user_setting_uid_pinned", indexChosenFor(t, rows, "s"),
		"this is the index the migration adds and justifies by this query; the unique "+
			"key leads with project_id and cannot serve a lookup by uid")
}

// indexChosenFor returns the index MySQL picked for one alias, or "" if the alias
// is absent from the plan.
func indexChosenFor(t *testing.T, rows []explainRow, alias string) string {
	t.Helper()
	for _, row := range rows {
		if derefOr(row.Table, "") == alias {
			return derefOr(row.Key, "")
		}
	}
	return ""
}

// TestTheProjectStatementsAreWhatProductionRuns pins the other half of the plan
// guard: EXPLAINing a constant proves nothing if the function stopped running it.
//
// Same shape as #855's TestTheSplitPredicatesAreWhatProductionRuns, and the same
// hazard — a body that inlined its SQL again would regress production with every
// plan assertion still green, and the drift suite could not see it either because
// the inlined shape still executes.
func TestTheProjectStatementsAreWhatProductionRuns(t *testing.T) {
	for _, want := range []struct {
		path      string
		signature string
		constant  string
	}{
		{"db_group.go", "func (d *DB) listMyProjectGroups(", "sqlListMyProjectGroups"},
		{"db_user_setting.go", "func (d *DB) countPinnedInSpaceTx(", "sqlCountPinnedInSpace"},
		{"db.go", "func (d *DB) listVisibleInSpace(", "sqlListVisibleInSpace"},
	} {
		fn := functionBodyOf(t, want.path, want.signature)
		require.Contains(t, fn, want.constant,
			"%s must RUN %s: the plan guard EXPLAINs that constant, so a body that "+
				"stopped executing it would leave the guard green while production "+
				"regressed", want.signature, want.constant)
	}
}

// TestTheProjectListReachesItsRowsByAnIndex covers the third statement PR-5
// touches — the one it did not add, but did make more expensive.
//
// PR-5's ORDER BY is not servable by an index (the sort keys live in a per-caller
// table), so this statement now materialises and sorts every project visible to
// the caller in the Space before LIMIT applies. Measured with EXPLAIN ANALYZE on a
// seeded database of 4000 projects and 5000 seats, 2000 of them visible in the
// probed Space:
//
//	before PR-5   0.28ms   Backward index scan, 23 rows touched, no sort at all
//	after PR-5   13.5ms    Using temporary; Using filesort, 1667 rows sorted,
//	                       1667 dependent-subquery loops for seat_count
//	with seat_count moved out of the statement (this PR)   7.9ms
//
// Two things follow. The sort is accepted: the alternative is two statements and
// two read views for one page, trading a bounded latency cost for a pagination
// correctness risk. Losing the Space access path on top of it is not — that path
// is what keeps the sort input proportional to one Space rather than to the whole
// table, and an edit that dropped p.space_id or wrapped it would still return
// exactly the right rows.
//
// The assertion is "a Space-leading index", not one index by name, and that is a
// finding rather than a hedge: on the empty CI table the optimizer picks
// uk_octo_project_space_active_name, and on the seeded database above it picks
// idx_octo_project_space_status. Both lead with space_id, which is the property
// worth pinning; naming either one would make this test fail on the other
// database and teach the next reader the wrong lesson.
//
// The escalation condition, stated so it is a decision rather than a discovery: if
// a Space's visible-project count reaches the thousands, the sort input has to
// stop being every visible project — pins are capped at six per Space, so the
// shape that gets there is one bounded read of the caller's pinned ids plus the
// existing p.id DESC page, concatenated with the offset arithmetic done in Go.
//
// And it has a detector, because a condition nobody can observe arrives as user
// latency instead of as a signal: project_space_project_count, sampled on the
// sparse metrics tick (metrics.go). It measures projects per Space rather than the
// sort input itself — the sort input is the subset one caller can see, which needs
// a per-caller query — and bounds it from above, which is the conservative
// direction. Reviewer yujiawei asked for it on PR #861.
func TestTheProjectListReachesItsRowsByAnIndex(t *testing.T) {
	setup(t)
	p := New(testCtx)

	rows := explainRows(t, p.db.session, sqlListVisibleInSpace,
		roleNonMember, "plan_probe_uid", "plan_probe_uid", "plan_probe_space",
		StatusNormal, DiscoverabilitySpaceListed, 20, 0)

	require.NotEmpty(t, rows, "EXPLAIN returned no rows; the assertions below would be vacuous")
	spaceLeading := map[string]bool{
		"idx_octo_project_space_status":     true,
		"uk_octo_project_space_active_name": true,
		"idx_octo_project_space_creator":    true,
	}
	key := indexChosenFor(t, rows, "p")
	assert.True(t, spaceLeading[key],
		"the project list must reach octo_project through an index that LEADS with "+
			"space_id; it chose %q. Without one, the sort PR-5 introduced runs over "+
			"the whole table instead of over one Space", key)
	for _, row := range rows {
		if derefOr(row.Table, "") == "p" {
			assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
				"and must not get there by a full scan")
		}
	}
}
