package botfather

// D14 — a failed Space-seat close must ABORT the bot deletion.
//
// PR #855's fifth review demonstrated by mutation that this rule had nothing
// behind it: reverting the abort to log-and-continue left the entire
// modules/botfather suite green. That is the exact defect the fourth round
// blocked on, one edit away from returning with no test going red.
//
// What the rule protects: if the seat close fails, the bot keeps an active
// space_member row, therefore an active project seat, therefore keeps satisfying
// the I2 admission gate and stays a reading member of that project's groups. No
// scan in this repository looks for "an active Space seat whose robot row is
// disabled", so that state is invisible. And once deleteRobot has set
// robot.status = 0 the owner can no longer select the bot, so the retry the
// failure message promises is unreachable. Aborting keeps the row selectable.

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func robotStatusOf(t *testing.T, ctx *config.Context, robotID string) int {
	t.Helper()
	var got []int
	_, err := ctx.DB().SelectBySql("SELECT status FROM robot WHERE robot_id=?", robotID).Load(&got)
	require.NoError(t, err)
	require.Len(t, got, 1)
	return got[0]
}

// seedDeletableBot puts the handler in the state onDeleteConfirm expects: a live
// robot row owned by the caller, and the state machine pointing at it.
func seedDeletableBot(t *testing.T, ctx *config.Context, h *commandHandler, ownerUID, botID string) {
	t.Helper()
	userDB := user.NewDB(ctx)
	require.NoError(t, userDB.Insert(&user.Model{UID: ownerUID, Name: "owner", ShortNo: ownerUID}))
	require.NoError(t, userDB.Insert(&user.Model{UID: botID, Name: "bot", ShortNo: botID, Robot: 1}))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, token, status, creator_uid, agent_hosting) "+
			"VALUES (?, ?, 1, ?, 'octo_hosted')",
		botID, "tok-"+botID, ownerUID,
	).Exec()
	require.NoError(t, err)
	require.NoError(t, h.sm.SetField(ownerUID, h.spaceID(ownerUID), FieldBotID, botID))
}

// TestDeleteBotAbortsWhenTheSeatCloseFails is the mutation-visible half: revert
// the abort and this goes red.
func TestDeleteBotAbortsWhenTheSeatCloseFails(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const owner, botID = "seatclose_owner", "seatclose_bot"
	h := newCommandHandler(ctx)
	seedDeletableBot(t, ctx, h, owner, botID)

	var called bool
	h.closeSeatsFn = func(_ *config.Context, uid, operatorUID, reason string) ([]string, error) {
		called = true
		assert.Equal(t, botID, uid, "the bot is the account being closed")
		assert.Equal(t, owner, operatorUID,
			"the OWNER who issued the delete is the operator, not the bot itself")
		// A partial failure: one Space closed, another did not.
		return []string{"space_ok"}, errors.New("space_bad: close failed")
	}

	h.onDeleteConfirm(owner, "Yes, delete it")

	require.True(t, called, "precondition: the deletion must reach the seat close")
	assert.Equal(t, 1, robotStatusOf(t, ctx, botID),
		"the robot row must still be ACTIVE. deleteRobot sets status = 0, and the bot "+
			"selection query requires status = 1, so continuing past a failed seat close "+
			"makes the retry the failure message promises unreachable — while the bot keeps "+
			"an active Space seat, an active project seat, and its membership of that "+
			"project's groups, in a state no scan in this repository looks for")
}

// TestDeleteBotProceedsWhenTheSeatCloseSucceeds is the control. Without it, a
// guard that aborted unconditionally would pass the case above.
func TestDeleteBotProceedsWhenTheSeatCloseSucceeds(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const owner, botID = "seatok_owner", "seatok_bot"
	h := newCommandHandler(ctx)
	seedDeletableBot(t, ctx, h, owner, botID)

	h.closeSeatsFn = func(_ *config.Context, _, _, _ string) ([]string, error) {
		return []string{"space_ok"}, nil
	}

	h.onDeleteConfirm(owner, "Yes, delete it")

	assert.Equal(t, 0, robotStatusOf(t, ctx, botID),
		"a successful seat close must let the deletion complete")
}
