package robot

// D14 on the THIRD bot-deletion path.
//
// PR #855 routed the chat-command deletion and then, one round later, the REST
// deletion through the Space removal outbox. Both times the reasoning was a census —
// "which entry points can delete a bot" — and both times the census was run as "who
// WRITES space_member". That question structurally cannot find this door, whose whole
// defect is that it deletes a bot WITHOUT writing space_member. The tenth review ran
// the right question and found DELETE /v1/manager/robots/:robot_id.
//
// What it left behind is worse than the other two doors' bare UPDATE, because the
// shape is different: the Space seat stays ACTIVE, so the project seat stays active
// too — while RemoveUserFromGroupsForLifecycleCleanup has already taken the bot out of
// every group, the all-member group included. That is exactly I4 scan B's violating
// state (an active project member missing from their all-member group), it is exempt
// from none of the five exemptions, and scan B is report-only by design. D13 cannot
// reclaim the seat (it requires an active robot row, and this deletion just disabled
// it) and an operator cannot repair it by re-adding, because addOneMemberOnce refuses
// a disabled robot row. A reachable admin action, a permanent violation, no repair.
//
// The set-level guard is bot_deletion_census_test.go in modules/space; this file is
// the endpoint-level half for this door, the same pair the other two doors have.

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seatCountForRobot(t *testing.T, m *Manager, uid string, status int) int {
	t.Helper()
	var n int
	require.NoError(t, m.ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE uid = ? AND status = ?", uid, status,
	).LoadOne(&n))
	return n
}

func cleanupJobsForRobot(t *testing.T, m *Manager, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, m.ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member_removal_cleanup WHERE uid = ?", uid,
	).LoadOne(&n))
	return n
}

// seatManagedBotInSpaces gives a bot a live robot/user row and a seat in each Space.
func seatManagedBotInSpaces(t *testing.T, m *Manager, botUID string, spaceIDs ...string) {
	t.Helper()
	_, err := m.ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, robot, status) VALUES (?, ?, ?, 1, 1)",
		botUID, "managed bot", botUID).Exec()
	require.NoError(t, err)
	_, err = m.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		botUID, testutil.UID).Exec()
	require.NoError(t, err)
	for _, spaceID := range spaceIDs {
		_, err = m.ctx.DB().InsertBySql(
			"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
			spaceID, spaceID, testutil.UID).Exec()
		require.NoError(t, err)
		_, err = m.ctx.DB().InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
			spaceID, botUID).Exec()
		require.NoError(t, err)
	}
}

// TestManagerBotDeleteClosesSeatsThroughTheOutbox is this door's endpoint-level pin:
// every Space seat closed AND a cleanup job left behind for each.
//
// The job is the load-bearing half. Closing the seat alone is what the OTHER two doors'
// bare UPDATE already did; the job is what drives the project-seat cascade and the
// group detach, and its absence is the state no scan in this repository repairs.
func TestManagerBotDeleteClosesSeatsThroughTheOutbox(t *testing.T) {
	route, manager := setupRobotManagerDeleteTest(t)

	botUID := "mgr-bot-" + util.GenerUUID()[:8]
	// Two Spaces, so the assertion is about "every seat" rather than "a seat".
	seatManagedBotInSpaces(t, manager, botUID, "sp_mgr_a", "sp_mgr_b")
	require.Equal(t, 2, seatCountForRobot(t, manager, botUID, 1), "precondition: the bot is seated")

	w := deleteManagedRobot(t, route, botUID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Zero(t, seatCountForRobot(t, manager, botUID, 1), "every Space seat must be closed")
	assert.Equal(t, 2, cleanupJobsForRobot(t, manager, botUID),
		"and each closed seat must leave a cleanup job — that job is what drives P0's "+
			"project-seat cascade and P1's group detach. Deleting the bot without one leaves "+
			"an ACTIVE project seat on an account that no longer exists, which is I4 scan B's "+
			"violating state, and scan B is report-only")

	robot, err := manager.db.queryRobotWithRobtID(botUID)
	require.NoError(t, err)
	require.NotNil(t, robot)
	assert.Zero(t, robot.Status, "the deletion itself still completes")
}

// TestManagerBotDeleteAttributesTheRemovalToTheAdmin pins the operator on the ticket.
//
// The chat-command door shipped with both arguments set to the bot's own uid, so the
// audit trail read "this bot removed itself from every project" — an actor that does
// not exist. That was fixed there; repeating it here would re-introduce it on the one
// door where the actor is a super-admin doing something to somebody else's bot.
func TestManagerBotDeleteAttributesTheRemovalToTheAdmin(t *testing.T) {
	route, manager := setupRobotManagerDeleteTest(t)

	botUID := "mgr-attr-" + util.GenerUUID()[:8]
	seatManagedBotInSpaces(t, manager, botUID, "sp_mgr_attr")

	w := deleteManagedRobot(t, route, botUID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var operator string
	require.NoError(t, manager.ctx.DB().SelectBySql(
		"SELECT operator_uid FROM space_member_removal_cleanup WHERE uid = ?", botUID,
	).LoadOne(&operator))
	assert.Equal(t, testutil.UID, operator,
		"the operator on the ticket must be the acting super-admin, not the bot: this value "+
			"flows into the cascade's audit and log attribution, and the bot is not the actor")

	var reason string
	require.NoError(t, manager.ctx.DB().SelectBySql(
		"SELECT reason FROM space_member_removal_cleanup WHERE uid = ?", botUID,
	).LoadOne(&reason))
	assert.Equal(t, spacemod.MemberRemoveReasonBotDeleted, reason,
		"bot_deleted, not force_removed: the group cascade reads the reason to decide whether "+
			"to post 'X was removed by Y', and for an account that ceased to exist that "+
			"sentence is false in every group it was in")
}

// TestManagerBotDeleteAbortsWhenSeatsCannotBeClosed pins the abort rule.
//
// Continuing past a failed seat close is what the fourth review took off the other two
// doors: the bot's row would go to status=0 while its seats stayed open, and from then
// on nothing can select that bot to try again. Aborting keeps robot.status = 1, so the
// admin can simply retry.
func TestManagerBotDeleteAbortsWhenSeatsCannotBeClosed(t *testing.T) {
	route, manager := setupRobotManagerDeleteTest(t)

	botUID := "mgr-abort-" + util.GenerUUID()[:8]
	seatManagedBotInSpaces(t, manager, botUID, "sp_mgr_abort")

	manager.closeSeatsFn = func(_ *config.Context, _, _, _ string) ([]string, error) {
		return nil, assert.AnError
	}

	w := deleteManagedRobot(t, route, botUID)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	robot, err := manager.db.queryRobotWithRobtID(botUID)
	require.NoError(t, err)
	require.NotNil(t, robot)
	assert.Equal(t, 1, robot.Status,
		"the robot row must stay selectable so the admin can retry — once it is status=0 "+
			"nothing can find this bot to close its seats")
}
