package project

// Behavioural tests for the two I4 scans.
//
// Until PR #855's review these existed only as source guards — the bounds rules
// and the collation drift probe — so nothing had ever run either scan and looked
// at what it reported. A scan is a claim about production state; a guard on its
// SQL shape is not a test of that claim.
//
// The negative halves matter as much as the positive ones. A reconcile gauge that
// fires during normal operation trains whoever watches it to ignore it, which is
// worse than no gauge: the alert exists and is dead.

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i4MissingCount(t *testing.T, p *Project) int {
	t.Helper()
	resetCursorsForTest()
	p.scanMissingAllMemberGroups()
	return int(readGauge(t, allMemberGroupMissing))
}

func i4GapCount(t *testing.T, p *Project) int {
	t.Helper()
	resetCursorsForTest()
	p.scanAllMemberGroupGaps()
	return int(readGauge(t, allMemberGroupMemberGaps))
}

// backdateSeat pushes a project seat's updated_at outside the admit grace window,
// so scan B stops exempting it. Without this every case below would measure the
// grace period rather than the invariant.
func backdateSeat(t *testing.T, projectID, uid string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET updated_at = ? WHERE project_id = ? AND uid = ?",
		time.Now().UTC().Add(-time.Hour), projectID, uid,
	).Exec()
	require.NoError(t, err)
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

	// State 3: the group is alive again but detached to Space-direct, which is what
	// P1's cascade does when a group's creator leaves the project. The pointer is
	// deliberately NOT cleared by that path — modules/group must not write
	// octo_project — so this is a state normal operation reaches.
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

// ---------- scan B ----------

// TestI4ScanBReportsAMemberMissingFromTheGroup is the positive case, seeded by
// deleting the group row directly: no endpoint produces this state, which is
// exactly why the scan exists.
func TestI4ScanBReportsAMemberMissingFromTheGroup(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	// A real group row, so scan A is satisfied and only scan B can speak.
	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")

	require.Equal(t, 1, i4GapCount(t, p),
		"the owner holds a seat and is not in the group: that is the ⊇ half of I4")

	seedGroupMemberRow(t, groupNo, "u_owner")
	require.Zero(t, i4GapCount(t, p), "and once they are in it, nothing is reported")
}

// TestI4ScanBExemptsAFreshSeat covers the grace window, which is the exemption
// normal operation produces on every single add: the admitter runs AFTER the seat
// transaction commits (D12), so the seat legitimately exists without the group row
// for as long as that takes.
func TestI4ScanBExemptsAFreshSeat(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-grace",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)

	// The seat was just written and the group row does not exist — the in-flight
	// state. Not a violation.
	require.Zero(t, i4GapCount(t, p),
		"a seat inside the admit grace window is the DESIGN, not a violation; reporting it "+
			"would fire on every add and train the reader to ignore the gauge")

	backdateSeat(t, model.ProjectID, "u_owner")
	require.Equal(t, 1, i4GapCount(t, p), "past the window the same state IS a gap")
}

// TestI4ScanBExemptsAClosingSeat covers the removing = 1 exemption: the seat is on
// its way out and the cascade owns its group rows.
func TestI4ScanBExemptsAClosingSeat(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-removing",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")
	require.Equal(t, 1, i4GapCount(t, p), "precondition: this seat IS a gap while it is open")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 1 WHERE project_id = ? AND uid = ?",
		model.ProjectID, "u_owner").Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")
	assert.Zero(t, i4GapCount(t, p), "a closing seat's group rows belong to the cascade")
}

// TestI4ScanBStaysQuietWhenTheSpaceIsBanned is the evidence for a DECLARED
// DEVIATION from the brief, which asks scan B to exempt projects in a banned
// Space (as P1's I2 scan does) and does not.
//
// P1 needs that exemption because it is the ⊆ direction: CheckMembershipForCleanup
// deliberately leaves a banned Space's seats alone, so the group rows are EXPECTED
// to remain and would otherwise be reported. Scan B is the ⊇ direction, and the
// state it would suppress cannot arise — the cascade leaves BOTH sides alone, and
// every path that could strip a group row while leaving the seat is refused by D7.
// Adding the exemption would be a `space` lookup per examined row that never fires.
//
// That argument was written before it was checked, which is not evidence. This is:
// the same project, the same members, the Space banned, and the gauge at zero —
// and the second half shows the scan has not simply gone silent.
func TestI4ScanBStaysQuietWhenTheSpaceIsBanned(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-banned",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	seedGroupMemberRow(t, groupNo, "u_owner")
	backdateSeat(t, model.ProjectID, "u_owner")
	require.Zero(t, i4GapCount(t, p), "precondition: healthy before the ban")

	// Ban the Space. Seats stay, group rows stay — that is the whole point of the
	// banned-Space treatment, and it is why the ⊇ direction has nothing to suppress.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `space` SET status = 2 WHERE space_id = ?", spaceA).Exec()
	require.NoError(t, err)
	assert.Zero(t, i4GapCount(t, p),
		"a banned Space produces no I4-B gap, so the exemption the brief asks for would be "+
			"a per-row `space` lookup that can never fire")

	// And the scan is not merely mute: break the invariant under the same ban.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted = 1 WHERE group_no = ? AND uid = ?",
		groupNo, "u_owner").Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4GapCount(t, p),
		"a REAL gap inside a banned Space is still worth reporting — an admitter failure "+
			"does not stop being one because the Space was banned afterwards")
}
