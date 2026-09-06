package project

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

// The project → group cascade, end to end.
//
// These run in modules/project's test binary, which blank-imports
// octo-server/internal through the external test package and therefore has
// modules/group registered — including its reverse-registered detach and
// disband steps. So the steps under test are the REAL ones, not stand-ins; a
// cascade that silently stopped being registered fails here rather than in
// production.

func groupMemberActive(t *testing.T, groupNo, uid string) bool {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, uid).LoadOne(&n))
	return n > 0
}

func groupProjectID(t *testing.T, groupNo string) string {
	t.Helper()
	var pid string
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT project_id FROM `group` WHERE group_no=?", groupNo).LoadOne(&pid))
	return pid
}

func memberEpoch(t *testing.T, projectID string) int64 {
	t.Helper()
	var e int64
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT member_epoch FROM `octo_project` WHERE project_id=?", projectID).LoadOne(&e))
	return e
}

// seedProjectGroupWithMembers creates a project group and puts uids in it,
// writing group_member directly. Direct writes are correct for a fixture here:
// the admission funnel lives in modules/group and is exercised by that package's
// own tests; what this file tests is what the cascade does to rows that exist.
func seedProjectGroupWithMembers(t *testing.T, groupNo, spaceID, projectID, creator string, members ...string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, space_id, project_id) "+
			"VALUES (?, 'g', ?, 1, 1, ?, ?)", groupNo, creator, spaceID, projectID).Exec()
	require.NoError(t, err)
	for _, uid := range members {
		role := 0
		if uid == creator {
			role = 1 // MemberRoleCreator in modules/group
		}
		_, err := testCtx.DB().InsertBySql(
			"INSERT INTO group_member (group_no, uid, role, `version`, is_deleted, status, vercode, robot, invite_uid) "+
				"VALUES (?, ?, ?, 1, 0, 1, ?, 0, '')",
			groupNo, uid, role, util.GenerUUID()).Exec()
		require.NoError(t, err)
	}
}

// TestCascadeRemovesTheMemberFromEveryProjectGroup is the headline behaviour:
// closing a project seat takes the member out of that project's groups, and
// only those.
func TestCascadeRemovesTheMemberFromEveryProjectGroup(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "leaver")
	seedSpaceMember(t, spaceA, "leaver", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "cascade")
	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "leaver")
	require.NoError(t, err)

	g1, g2 := util.GenerUUID(), util.GenerUUID()
	seedProjectGroupWithMembers(t, g1, spaceA, created.ProjectID, "owner1", "owner1", "leaver")
	seedProjectGroupWithMembers(t, g2, spaceA, created.ProjectID, "owner1", "owner1", "leaver")
	// A Space-direct group the member is also in. The cascade must NOT touch it:
	// leaving a project says nothing about a group that does not belong to it.
	gOther := util.GenerUUID()
	seedProjectGroupWithMembers(t, gOther, spaceA, "", "owner1", "owner1", "leaver")

	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "leaver")
	require.NoError(t, err)

	// Before the worker runs: still in the groups, and still a member of record
	// with removing = 1. That intermediate state is D4's whole point.
	require.True(t, groupMemberActive(t, g1, "leaver"))
	seat, err := p.db.queryMember(created.ProjectID, "leaver")
	require.NoError(t, err)
	require.Equal(t, MemberStatusActive, seat.Status, "status stays active until the detach finishes")
	require.Equal(t, 1, seat.Removing, "the seat is marked closing immediately")

	drainRemovalCascade(t, p)

	require.False(t, groupMemberActive(t, g1, "leaver"), "detached from the first project group")
	require.False(t, groupMemberActive(t, g2, "leaver"), "detached from the second project group")
	require.True(t, groupMemberActive(t, gOther, "leaver"),
		"a Space-direct group must be untouched: leaving a project says nothing about it")

	seat, err = p.db.queryMember(created.ProjectID, "leaver")
	require.NoError(t, err)
	require.Equal(t, MemberStatusRemoved, seat.Status)
	require.Equal(t, 0, seat.Removing)
}

// TestCascadeIsIdempotentAndDoesNotDoubleBumpTheEpoch.
//
// member_epoch moves when `removing` is set — that is when the membership
// changed from every consumer's point of view — and must NOT move again when
// the worker finishes. P0 established the "+1 per membership change" rule and
// nearly broke it; the two-phase removal gives it two chances to break.
func TestCascadeIsIdempotentAndDoesNotDoubleBumpTheEpoch(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "m1")
	seedSpaceMember(t, spaceA, "m1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "epoch")
	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "m1")
	require.NoError(t, err)

	g := util.GenerUUID()
	seedProjectGroupWithMembers(t, g, spaceA, created.ProjectID, "owner1", "owner1", "m1")

	before := memberEpoch(t, created.ProjectID)
	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "m1")
	require.NoError(t, err)
	afterBegin := memberEpoch(t, created.ProjectID)
	require.Equal(t, before+1, afterBegin, "setting removing=1 bumps the epoch by exactly one")

	drainRemovalCascade(t, p)
	require.Equal(t, afterBegin, memberEpoch(t, created.ProjectID),
		"closing the seat must NOT bump again: one membership change, one epoch step")

	// A second removal of an already-removed member changes nothing at all.
	changed, err := p.removeMember(created.ProjectID, spaceA, "owner1", "m1")
	require.Error(t, err, "removing a non-member is refused")
	require.False(t, changed)
	require.Equal(t, afterBegin, memberEpoch(t, created.ProjectID))
}

// TestReAdmissionCancelsAnInFlightCascade covers D4's most consequential
// choice: re-adding during the window CANCELS rather than being rejected.
//
// Rejecting is the obvious alternative and it is worse in a specific way — the
// cascade can legitimately run long, and a rejection would make an unrelated
// admin action fail for as long as it does, with no self-service remedy.
func TestReAdmissionCancelsAnInFlightCascade(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "boomerang")
	seedSpaceMember(t, spaceA, "boomerang", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "cancel")
	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "boomerang")
	require.NoError(t, err)

	g := util.GenerUUID()
	seedProjectGroupWithMembers(t, g, spaceA, created.ProjectID, "owner1", "owner1", "boomerang")

	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "boomerang")
	require.NoError(t, err)
	pending, err := p.db.countPendingRemovalJobs()
	require.NoError(t, err)
	require.Equal(t, 1, pending)

	// Re-add before the worker gets there.
	_, err = p.addOneMember(created.ProjectID, spaceA, "owner1", "boomerang")
	require.NoError(t, err)

	seat, err := p.db.queryMember(created.ProjectID, "boomerang")
	require.NoError(t, err)
	require.Equal(t, MemberStatusActive, seat.Status)
	require.Equal(t, 0, seat.Removing, "re-admission clears the closing flag")

	pending, err = p.db.countPendingRemovalJobs()
	require.NoError(t, err)
	require.Zero(t, pending, "the outstanding job must be retired, not left for the worker to discover")

	// And the worker, run anyway, must leave the member alone.
	p.runRemovalCascade()
	require.True(t, groupMemberActive(t, g, "boomerang"),
		"a cancelled cascade must not tear down a membership that is legitimate again")
	seat, err = p.db.queryMember(created.ProjectID, "boomerang")
	require.NoError(t, err)
	require.Equal(t, MemberStatusActive, seat.Status)
}

// TestCascadeHandsOverAGroupOwnedByTheDepartingMember.
//
// RemoveGroupMembers silently skips role = creator, so without a handover the
// creator's group_member row would outlive their project seat forever — an I2
// violation the reconcile scan reports and nothing repairs.
//
// The successor must ALSO be an active member of the project. Handing the group
// to someone outside it would create the very violation the cascade exists to
// prevent.
func TestCascadeHandsOverAGroupOwnedByTheDepartingMember(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	for _, uid := range []string{"gcreator", "successor"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	created := createProjectVia(t, srv, spaceA, token, "handover")
	for _, uid := range []string{"gcreator", "successor"} {
		_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", uid)
		require.NoError(t, err)
	}

	g := util.GenerUUID()
	seedProjectGroupWithMembers(t, g, spaceA, created.ProjectID, "gcreator", "gcreator", "successor")

	_, err := p.removeMember(created.ProjectID, spaceA, "owner1", "gcreator")
	require.NoError(t, err)
	drainRemovalCascade(t, p)

	require.False(t, groupMemberActive(t, g, "gcreator"),
		"after the handover the departing creator is removable, and removed")
	require.True(t, groupMemberActive(t, g, "successor"))

	var role int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=?", g, "successor").LoadOne(&role))
	require.Equal(t, 1, role, "the successor must now be the group's creator")
	require.Equal(t, created.ProjectID, groupProjectID(t, g),
		"a group with a successor stays in the project")
}

// TestCascadeDetachesAGroupWithNoEligibleSuccessor.
//
// When the departing creator is the only project member left in the group there
// is nobody to hand it to. The group reverts to Space-direct with everyone
// still in it — deliberately the same rule as project disband, so the two
// situations are one rule rather than two. It is NOT disbanded: group disband
// only flips group.status and leaves every group_member row, so it would
// destroy access without cleaning anything up.
func TestCascadeDetachesAGroupWithNoEligibleSuccessor(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "solo")
	seedSpaceMember(t, spaceA, "solo", 0, 1)
	seedUser(t, "outsider")
	seedSpaceMember(t, spaceA, "outsider", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "nosucc")
	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "solo")
	require.NoError(t, err)

	// "outsider" is in the group but NOT in the project, so it is not an
	// eligible successor — promoting them would be the I2 violation.
	g := util.GenerUUID()
	seedProjectGroupWithMembers(t, g, spaceA, created.ProjectID, "solo", "solo", "outsider")

	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "solo")
	require.NoError(t, err)
	drainRemovalCascade(t, p)

	require.Equal(t, "", groupProjectID(t, g),
		"with no eligible successor the group reverts to Space-direct")
	require.True(t, groupMemberActive(t, g, "solo"), "members are left in place, including the creator")
	require.True(t, groupMemberActive(t, g, "outsider"))
}

// TestDisbandRevertsGroupsAndLeavesMembersAlone is the product decision: a
// disbanded project hands its groups back to the Space intact.
func TestDisbandRevertsGroupsAndLeavesMembersAlone(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "m1")
	seedSpaceMember(t, spaceA, "m1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "disband-groups")
	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "m1")
	require.NoError(t, err)

	g1, g2 := util.GenerUUID(), util.GenerUUID()
	seedProjectGroupWithMembers(t, g1, spaceA, created.ProjectID, "owner1", "owner1", "m1")
	seedProjectGroupWithMembers(t, g2, spaceA, created.ProjectID, "owner1", "owner1")

	_, err = p.disbandProject(created.ProjectID, "owner1", spaceA)
	require.NoError(t, err)

	require.Equal(t, "", groupProjectID(t, g1), "the project's groups revert to Space-direct")
	require.Equal(t, "", groupProjectID(t, g2))
	require.True(t, groupMemberActive(t, g1, "owner1"), "group_member is untouched")
	require.True(t, groupMemberActive(t, g1, "m1"))

	// Idempotent: running the step again changes nothing.
	p.runDisbandSteps(ProjectDisband{ProjectID: created.ProjectID, SpaceID: spaceA})
	require.Equal(t, "", groupProjectID(t, g1))
	require.True(t, groupMemberActive(t, g1, "m1"))
}
