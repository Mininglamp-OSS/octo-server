package project

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The legacy native-membership group list's execution plan is asserted rather
// than reasoned about.
//
// PR #861's review rounds asked for this twice and I deferred it twice, on the
// grounds that `group` is empty in CI so the assertion would be weak. That reason
// does not hold and the review said so: EXPLAIN resolves the chosen index from
// schema metadata, not from row counts, so an empty table still proves which index
// MySQL picked. What made it cheap in the end is #855's plan rig (explainRows /
// accessTypeOf, all_member_group_plan_test.go), landed for a different predicate.
//
// The property is worth a guard for the same reason that predicate's was: the
// statement is a user-facing chat-room projection, and the failure mode is silent.
// A later edit that drops `g.space_id` — the LEADING column of group_space_project,
// which db_group.go's comment calls the Space isolation boundary rather than
// decoration — degrades the request from an index range to a scan of a core IM
// table with nothing red anywhere.
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

func TestTheBatchProjectGroupListReachesItsRowsByAnIndex(t *testing.T) {
	setup(t)
	p := New(testCtx)
	rows := explainRows(t, p.db.session, sqlListProjectGroupRelationsByProjectIDs,
		"plan_probe_uid", "plan_probe_space",
		[]string{"plan_probe_project_a", "plan_probe_project_b"}, groupStatusDisband)

	var groupRows int
	for _, row := range rows {
		if derefOr(row.Table, "") != "g" {
			continue
		}
		groupRows++
		assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
			"the batched Sidebar Project relation list must not scan table %q", derefOr(row.Table, ""))
		assert.NotEmpty(t, derefOr(row.Key, ""),
			"the batched Sidebar Project relation list must use an index for table %q", derefOr(row.Table, ""))
	}
	require.Equal(t, 1, groupRows, "EXPLAIN must include the related group table")
	assert.Equal(t, "group_space_project", indexChosenFor(t, rows, "g"),
		"the relation list must use the Space+Project group index")

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
		"plan_probe_space", StatusNormal, MemberStatusActive, "plan_probe_uid")

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
