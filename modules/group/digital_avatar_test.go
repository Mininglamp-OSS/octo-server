package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/require"
)

func TestDigitalAvatarCanOnlyJoinAITeamGroups(t *testing.T) {
	svc, userDB, ctx := setupServiceTestWithCtx(t)
	creatorA, creatorB, groupOwner := "avatar_inviter_a", "avatar_inviter_b", "avatar_group_owner"
	avatarID := "avatar_group_" + util.GenerUUID()[:8]
	insertTestUsers(t, userDB, creatorA, creatorB, groupOwner)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,1,0)",
		avatarID, "Digital employee", avatarID, util.GenerUUID()[:8]).Exec()
	require.NoError(t, err)

	spaceA, spaceB := "avatar_group_space_a", "avatar_group_space_b"
	seedSpaceWithMembers(t, ctx, spaceA, groupOwner, creatorA, avatarID)
	seedSpaceWithMembers(t, ctx, spaceB, creatorB)
	_, err = ctx.DB().InsertBySql(`INSERT INTO robot
		(robot_id,status,creator_uid,kind,management_scope,management_space_id,
		 created_by,publication_state,lifecycle_pending)
		VALUES (?,1,'','avatar','space',?,'manager','published',0)`, avatarID, spaceA).Exec()
	require.NoError(t, err)

	created, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: groupOwner, Members: []string{creatorA}, Name: "avatar ordinary group", SpaceID: spaceA,
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.GroupNo)
	_, err = svc.AddGroupMembers(&AddGroupMembersServiceReq{
		GroupNo: created.GroupNo, Members: []string{avatarID}, OperatorUID: creatorA,
	})
	require.ErrorIs(t, err, ErrAvatarOrdinaryGroupDenied)

	_, err = svc.CreateGroup(&CreateGroupServiceReq{
		Creator: creatorB, Members: []string{avatarID}, Name: "cross-space", SpaceID: spaceB,
	})
	require.ErrorIs(t, err, ErrAvatarOrdinaryGroupDenied)

	// AI Team owns its group rows and enters through the narrow bridge instead
	// of any public group-admission route.
	aiGroupNo := "avatar_ai_" + util.GenerUUID()[:8]
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	_, err = tx.InsertBySql("INSERT INTO `group` (group_no,name,creator,status,version,space_id,purpose) VALUES (?,?,?,1,1,?,?)",
		aiGroupNo, "Avatar AI", groupOwner, spaceA, aiteampkg.GroupPurpose).Exec()
	require.NoError(t, err)
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS ai_team_agent (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		space_id VARCHAR(40) NOT NULL,user_uid VARCHAR(40) NOT NULL,bot_id VARCHAR(40) NOT NULL,
		group_no VARCHAR(40) NULL,is_added TINYINT NOT NULL DEFAULT 1,container_state TINYINT NOT NULL DEFAULT 0,
		UNIQUE KEY uk_ai_team_agent_owner (space_id,user_uid,bot_id), UNIQUE KEY uk_ai_team_agent_group (group_no))`)
	require.NoError(t, err)
	_, err = tx.InsertBySql(`INSERT INTO ai_team_agent (space_id,user_uid,bot_id,group_no,is_added)
		VALUES (?,?,?,?,1)`, spaceA, groupOwner, avatarID, aiGroupNo).Exec()
	require.NoError(t, err)
	_, err = AdmitAITeamContainerMembersTx(ctx, tx, aiGroupNo, spaceA, groupOwner, avatarID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var avatarMember int
	require.NoError(t, ctx.DB().SelectBySql(`SELECT COUNT(*) FROM group_member
		WHERE group_no=? AND uid=? AND status=1 AND is_deleted=0`, aiGroupNo, avatarID).LoadOne(&avatarMember))
	require.Equal(t, 1, avatarMember)
}
