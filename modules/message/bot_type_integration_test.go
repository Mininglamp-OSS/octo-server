package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestConversationAndSidebarShareCanonicalBotTypes(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	_, err := ctx.DB().Exec(`CREATE TABLE IF NOT EXISTS app_bot (
		id VARCHAR(40) PRIMARY KEY,uid VARCHAR(40) UNIQUE NOT NULL,display_name VARCHAR(100) NOT NULL,
		scope VARCHAR(20) NOT NULL DEFAULT 'platform',space_id VARCHAR(40) DEFAULT NULL,
		status TINYINT NOT NULL DEFAULT 0,token VARCHAR(100) UNIQUE NOT NULL,created_by VARCHAR(40) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	require.NoError(t, err)

	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "creator_uid", "status", "kind").
		Values("wire_user_bot", "wire_owner", 1, "user").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "creator_uid", "status", "kind").
		Values("wire_ownerless", "", 1, "user").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("robot").Columns(
		"robot_id", "creator_uid", "status", "auto_approve", "kind", "management_scope",
		"management_space_id", "created_by", "publication_state", "lifecycle_pending",
	).Values("wire_avatar", "", 1, 1, "avatar", "platform", "", "admin", "published", 0).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("robot").Columns(
		"robot_id", "creator_uid", "status", "auto_approve", "kind", "management_scope",
		"management_space_id", "created_by", "publication_state", "lifecycle_pending",
	).Values("wire_unpublished_avatar", "", 0, 1, "avatar", "platform", "", "admin", "unpublished", 0).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("app_bot").Columns(
		"id", "uid", "display_name", "scope", "status", "token", "created_by",
	).Values("wire_app_id", "wire_app_bot", "App", "platform", 1, "app_wire_token", "admin").Exec()
	require.NoError(t, err)

	uids := []string{"wire_user_bot", "wire_ownerless", "wire_avatar", "wire_unpublished_avatar", "wire_app_bot", "human"}
	types, err := queryActiveBotTypes(ctx, uids)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"wire_user_bot": "user_bot",
		"wire_avatar":   "avatar",
		"wire_app_bot":  "app_bot",
	}, types)

	items := make([]*SidebarItem, 0, len(uids))
	for _, uid := range uids {
		items = append(items, &SidebarItem{TargetType: 1, TargetID: uid})
	}
	require.NoError(t, NewSidebar(ctx).enrichSidebarBotTypes(items))
	for _, item := range items {
		require.Equal(t, types[item.TargetID], item.BotType)
	}
}
