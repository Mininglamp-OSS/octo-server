package botfather

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestAvatarIsExcludedFromUserBotPersistencePaths(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const avatarID = "avatar_botfather_boundary"
	_, err := ctx.DB().InsertInto("robot").Columns(
		"robot_id", "creator_uid", "description", "bot_token", "status", "auto_approve", "kind",
		"management_scope", "management_space_id", "created_by", "publication_state", "lifecycle_pending",
	).Values(avatarID, "", "original", "bf_avatar_original", 1, 1, "avatar",
		"platform", "", "admin", "published", 0).Exec()
	require.NoError(t, err)

	db := newBotfatherDB(ctx)
	row, err := db.queryRobotByRobotID(avatarID)
	require.NoError(t, err)
	require.Nil(t, row, "BotFather must not resolve an Avatar as a personal User Bot")

	require.NoError(t, db.updateRobotDescription(avatarID, "bypassed"))
	require.NoError(t, db.updateRobotBotToken(avatarID, "bf_avatar_bypassed"))
	require.NoError(t, db.deleteRobot(avatarID))
	bound, err := db.bindRobotCAS(avatarID, "", "attacker")
	require.NoError(t, err)
	require.Zero(t, bound)

	var stored struct {
		Description string `db:"description"`
		BotToken    string `db:"bot_token"`
		Status      int    `db:"status"`
		Bound       string `db:"bound_agent_ref"`
	}
	require.NoError(t, ctx.DB().Select("description", "bot_token", "status", "bound_agent_ref").
		From("robot").Where("robot_id=?", avatarID).LoadOne(&stored))
	require.Equal(t, "original", stored.Description)
	require.Equal(t, "bf_avatar_original", stored.BotToken)
	require.Equal(t, 1, stored.Status)
	require.Empty(t, stored.Bound)
}
