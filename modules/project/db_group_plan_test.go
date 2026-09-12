package project

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The batched relation-only group list's execution plan is asserted against
// representative rows rather than an empty test table.
//
// The query serves the unified Sidebar and must keep its group-space predicate
// indexable while applying the per-Project window bound in SQL. EXPLAIN resolves
// the chosen index from schema metadata; the fixture below supplies both matching
// rows and unrelated padding so MySQL cannot answer from an empty-table shortcut.
//
// The property is worth a guard because the failure mode is silent: a later edit
// that drops `g.space_id` or the setting lookup can turn a bounded relation
// projection into a scan of a core IM table without changing the response shape.

func TestTheBatchProjectGroupListReachesItsRowsByAnIndex(t *testing.T) {
	setup(t)
	p := New(testCtx)
	seedPlanProbeRows(t, p.db.session, "plan_probe_project_a", "plan_probe_space", "plan_probe_group_a")
	seedPlanProbeRows(t, p.db.session, "plan_probe_project_b", "plan_probe_space", "plan_probe_group_b")
	for i := range 200 {
		_, err := p.db.session.InsertBySql(
			"INSERT INTO `octo_project_group_user_setting` "+
				"(space_id, project_id, group_no, uid, pinned, pinned_at, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, 1, NOW(3), NOW(3), NOW(3))",
			fmt.Sprintf("plan_setting_space_%03d", i),
			fmt.Sprintf("plan_setting_project_%03d", i),
			fmt.Sprintf("plan_setting_group_%03d", i),
			"plan_probe_uid",
		).Exec()
		require.NoError(t, err)
	}
	var analyzed []struct {
		Table string `db:"Table"`
	}
	_, err := p.db.session.SelectBySql("ANALYZE TABLE `octo_project_group_user_setting`").Load(&analyzed)
	require.NoError(t, err)
	rows := explainRows(t, p.db.session, sqlListProjectGroupRelationsByProjectIDs,
		"plan_probe_uid", "plan_probe_space",
		[]string{"plan_probe_project_a", "plan_probe_project_b"}, groupStatusDisband,
		aiteam.GroupPurpose, projectDefaultPageLimit)

	require.Contains(t, sqlListProjectGroupRelationsByProjectIDs,
		"ROW_NUMBER() OVER (PARTITION BY g.project_id",
		"the batched Sidebar Project relation list must bound each Project in SQL")
	require.Contains(t, sqlListProjectGroupRelationsByProjectIDs, "project_row_num <= ?",
		"the per-Project bound must be applied before rows reach Go")

	var groupRows, settingRows int
	for _, row := range rows {
		table := derefOr(row.Table, "")
		switch table {
		case "g":
			groupRows++
			assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
				"the batched Sidebar Project relation list must not scan table %q", table)
			assert.NotEmpty(t, derefOr(row.Key, ""),
				"the batched Sidebar Project relation list must use an index for table %q", table)
		case "s":
			settingRows++
			assert.NotEqual(t, "ALL", derefOr(row.Type, ""),
				"the batched Sidebar Project relation list must not scan table %q", table)
			assert.NotEmpty(t, derefOr(row.Key, ""),
				"the batched Sidebar Project relation list must use an index for table %q", table)
		}
	}
	require.Equal(t, 1, groupRows, "EXPLAIN must include the related group table")
	require.Equal(t, 1, settingRows, "EXPLAIN must include the pin-setting table")
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
