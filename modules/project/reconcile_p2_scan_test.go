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
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

func i4GapCount(t *testing.T, p *Project) int {
	t.Helper()
	resetCursorsForTest()
	p.scanAllMemberGroupGaps()
	return int(readGauge(t, allMemberGroupMemberGaps))
}

// backdateSeat pushes a project seat outside the admit grace window, so scan B
// stops exempting it.
func backdateSeat(t *testing.T, projectID, uid string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET updated_at = ? WHERE project_id = ? AND uid = ?",
		time.Now().UTC().Add(-time.Hour), projectID, uid,
	).Exec()
	require.NoError(t, err)
}

// TestI4ScanBReportsAMemberMissingFromTheGroup is the positive I4 superset case.
func TestI4ScanBReportsAMemberMissingFromTheGroup(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

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
		"the owner holds a seat and is not in the group")

	seedGroupMemberRow(t, groupNo, "u_owner")
	require.Zero(t, i4GapCount(t, p), "once the owner is in the group, nothing is reported")
}

// TestI4ScanBExemptsAFreshSeat covers the post-commit admitter grace window.
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

	require.Zero(t, i4GapCount(t, p),
		"a seat inside the admit grace window is in-flight work, not a violation")

	backdateSeat(t, model.ProjectID, "u_owner")
	require.Equal(t, 1, i4GapCount(t, p), "past the window the same state is a gap")
}

// TestI4ScanBExemptsAClosingSeat covers the removing=1 exemption.
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
	require.Equal(t, 1, i4GapCount(t, p), "precondition: this seat is a gap while open")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 1 WHERE project_id = ? AND uid = ?",
		model.ProjectID, "u_owner").Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")
	assert.Zero(t, i4GapCount(t, p), "a closing seat belongs to the removal cascade")
}

// TestI4ScanBExemptsABannedSpace covers a gap that is not actionable while its
// Space is banned.
func TestI4ScanBExemptsABannedSpace(t *testing.T) {
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

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted = 1 WHERE group_no = ? AND uid = ?",
		groupNo, "u_owner").Exec()
	require.NoError(t, err)
	require.Equal(t, 1, i4GapCount(t, p), "precondition: reported while Space is live")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `space` SET status = 2 WHERE space_id = ?", spaceA).Exec()
	require.NoError(t, err)
	assert.Zero(t, i4GapCount(t, p), "a banned Space makes the gap temporarily unactionable")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `space` SET status = 1 WHERE space_id = ?", spaceA).Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4GapCount(t, p), "unbanning restores the actionable gap")
}

// TestI4ScanBExemptsAnOwnerRetainedAfterSpaceRevocation keeps the retained
// Owner identity out of the active-access scan until Space membership returns.
func TestI4ScanBExemptsAnOwnerRetainedAfterSpaceRevocation(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-owner-revoked",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")
	require.Equal(t, 1, i4GapCount(t, p), "an active Space seat with no group row is a gap")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?",
		spaceA, "u_owner").Exec()
	require.NoError(t, err)
	member, err := p.db.queryMember(model.ProjectID, "u_owner")
	require.NoError(t, err)
	require.NotNil(t, member)
	require.Equal(t, RoleOwner, member.Role, "Space revocation retains the Owner identity")
	require.Equal(t, MemberStatusActive, member.Status)
	assert.Zero(t, i4GapCount(t, p), "a retained but revoked Owner has no active Space access")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 1 WHERE space_id = ? AND uid = ?",
		spaceA, "u_owner").Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4GapCount(t, p), "restoring Space membership makes the seat actionable again")
}

// TestI4ScanBDoesNotDoubleReportScanAsProblem keeps scan A authoritative when
// the pointed-at group is gone or detached.
func TestI4ScanBDoesNotDoubleReportScanAsProblem(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-double",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	backdateSeat(t, model.ProjectID, "u_owner")

	require.Equal(t, 1, i4GapCount(t, p), "precondition: group is usable and the seat is missing")
	require.Zero(t, i4MissingCount(t, p), "scan A is quiet while the group is usable")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = ? WHERE group_no = ?", groupStatusDisband, groupNo).Exec()
	require.NoError(t, err)

	assert.Equal(t, 1, i4MissingCount(t, p), "scan A owns the unusable-group problem")
	assert.Zero(t, i4GapCount(t, p), "scan B must not count members of an unusable group")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = 1, project_id = '' WHERE group_no = ?", groupNo).Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, i4MissingCount(t, p))
	assert.Zero(t, i4GapCount(t, p), "a detached group is equally scan A's business")
}

// TestI4ScanBResumesAcrossTicks verifies that a page cap carries both the
// composite cursor and running total until a complete rotation.
func TestI4ScanBResumesAcrossTicks(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1", "m2")
	groupNo := allMemberGroupNoOf(t, created.ProjectID)
	require.NotEmpty(t, groupNo)

	_, err := testCtx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted = 1 WHERE group_no = ?", groupNo,
	).Exec()
	require.NoError(t, err)
	for _, uid := range []string{"owner1", "m1", "m2"} {
		backdateSeat(t, created.ProjectID, uid)
	}

	oldLimit, oldPages := p.cfg.ReconcileLimit, reconcileMaxPagesForTest()
	p.cfg.ReconcileLimit = 1
	setReconcileMaxPagesForTest(1)
	t.Cleanup(func() {
		p.cfg.ReconcileLimit = oldLimit
		setReconcileMaxPagesForTest(oldPages)
		resetCursorsForTest()
	})

	allMemberGroupMemberGaps.Set(0)
	resetCursorsForTest()
	p.scanAllMemberGroupGaps()
	_, _, running := cursors.i4GapResume()
	require.Equal(t, 1, running)
	require.Zero(t, readGauge(t, allMemberGroupMemberGaps),
		"a partial rotation must not publish a partial gauge")

	p.scanAllMemberGroupGaps()
	p.scanAllMemberGroupGaps()
	p.scanAllMemberGroupGaps()
	assert.Equal(t, float64(3), readGauge(t, allMemberGroupMemberGaps),
		"the completed rotation must include all members across prior page-bound ticks")
	_, _, running = cursors.i4GapResume()
	assert.Zero(t, running, "a completed rotation resets its running total")
}

// TestI4ScanBExemptsSystemBots ensures the dedicated scan uses the shared
// system-bot whitelist and does not report those identities as missing seats.
func TestI4ScanBExemptsSystemBots(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "botfather")
	seedSpaceMember(t, spaceA, "botfather", 0, 1)

	groupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-system-bot",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, groupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	seedGroupMemberRow(t, groupNo, "u_owner")

	now := time.Now().UTC().Add(-time.Hour)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
		model.ProjectID, "botfather", spaceA, RoleCommon, MemberStatusActive,
		"u_owner", now, now, now,
	).Exec()
	require.NoError(t, err)

	assert.True(t, spacepkg.IsSystemBot("botfather"))
	assert.Zero(t, i4GapCount(t, p),
		"the shared system bot identity is not required in the dedicated group")
}

// TestI4ScanBDoesNotInspectOrdinaryProjectGroups makes the ordinary-group
// independence explicit: a difference there is not an I4-B gap.
func TestI4ScanBDoesNotInspectOrdinaryProjectGroups(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "ordinary-only")
	seedSpaceMember(t, spaceA, "ordinary-only", 0, 1)

	allGroupNo := util.GenerUUID()
	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "i4b-ordinary-independent",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	seedProjectGroup(t, allGroupNo, spaceA, model.ProjectID)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		allGroupNo, model.ProjectID).Exec()
	require.NoError(t, err)
	seedGroupMemberRow(t, allGroupNo, "u_owner")

	ordinaryGroupNo := util.GenerUUID()
	seedProjectGroup(t, ordinaryGroupNo, spaceA, model.ProjectID)
	seedGroupMemberRow(t, ordinaryGroupNo, "ordinary-only")

	assert.Zero(t, i4GapCount(t, p),
		"ordinary Project-linked group membership is independent of I4-B")
}
