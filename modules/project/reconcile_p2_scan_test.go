package project

// Behavioural coverage for the I4 missing all-member-group-pointer scan.
//
// The scan reports active Projects whose stored group pointer is empty,
// disbanded, or attached to another Project. A source-shape guard is not a
// substitute for checking those production states.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i4MissingCount(t *testing.T, p *Project) int {
	t.Helper()
	resetCursorsForTest()
	p.scanMissingAllMemberGroups()
	return int(readGauge(t, allMemberGroupMissing))
}

// ---------- scan A ----------

// TestI4ScanAReportsTheThreeUnusableStates covers all three states the gauge
// deliberately folds into one, plus the healthy case.
func TestI4ScanAReportsTheThreeUnusableStates(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_i4a")
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	model, err := p.createProject(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4a",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	require.Equal(t, "grp_i4a", allMemberGroupNoOf(t, model.ProjectID))
	require.Zero(t, i4MissingCount(t, p), "a healthy project must not be reported")

	// State 2: the group is disbanded under it.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = ? WHERE group_no = ?", groupStatusDisband, "grp_i4a").Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4MissingCount(t, p), "a pointer at a disbanded group is unusable")

	// State 3: the group is alive but detached to Space-direct while the Project
	// still points at it. Group-side operations do not rewrite octo_project, so a
	// stale pointer remains a missing-group state for the Project scan.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = 1, project_id = '' WHERE group_no = ?", "grp_i4a").Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4MissingCount(t, p), "a pointer at a group of another project is unusable")

	// State 1: never provisioned.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = '' WHERE project_id = ?", model.ProjectID).Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4MissingCount(t, p), "the empty sentinel is unusable")

	// And a disbanded PROJECT is out of scope rather than a violation.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET status = ? WHERE project_id = ?", StatusDisbanded, model.ProjectID).Exec()
	require.NoError(t, err)
	assert.Zero(t, i4MissingCount(t, p),
		"I4 is about ACTIVE projects; a disbanded one has no group to be missing")
}
