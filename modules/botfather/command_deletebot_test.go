package botfather

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestDeleteBotCleansUpGroupMembers verifies that deleting a bot
// removes it from all group_member records.
func TestDeleteBotCleansUpGroupMembers(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	_ = s

	botUID := "test_bot_001"
	creatorUID := testutil.UID
	groupNo := "test_group_001"

	// Setup: create user, bot, group, and group membership
	userDB := user.NewDB(ctx)
	err := userDB.Insert(&user.Model{UID: creatorUID, Name: "creator"})
	assert.NoError(t, err)
	err = userDB.Insert(&user.Model{UID: botUID, Name: "TestBot"})
	assert.NoError(t, err)

	groupDB := group.NewDB(ctx)
	err = groupDB.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "TestGroup",
		Creator: creatorUID,
		Status:  1,
		Version: 1,
	})
	assert.NoError(t, err)

	// Add bot as group member
	memberVersion, _ := ctx.GenSeq(common.GroupMemberSeqKey)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     botUID,
		Role:    group.MemberRoleCommon,
		Version: memberVersion,
	})
	assert.NoError(t, err)

	// Verify bot is in group
	exists, err := groupDB.ExistMember(botUID, groupNo)
	assert.NoError(t, err)
	assert.True(t, exists)

	// Execute: simulate delete cleanup (the DB parts only)
	groups, err := group.NewService(ctx).GetGroupsWithMemberUID(botUID)
	assert.NoError(t, err)
	assert.Len(t, groups, 1)

	for _, g := range groups {
		version, _ := ctx.GenSeq(common.GroupMemberSeqKey)
		_, err = ctx.DB().Update("group_member").
			Set("is_deleted", 1).
			Set("version", version).
			Where("group_no=? and uid=? and is_deleted=0", g.GroupNo, botUID).
			Exec()
		assert.NoError(t, err)
	}

	// Verify: bot is no longer in group
	exists, err = groupDB.ExistMember(botUID, groupNo)
	assert.NoError(t, err)
	assert.False(t, exists)
}

// TestDeleteBotCleansUpFriends verifies that deleting a bot
// removes it from all friend records (both directions).
func TestDeleteBotCleansUpFriends(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	_ = s

	botUID := "test_bot_002"
	userUID := "test_user_002"

	// Setup: create users
	userDB := user.NewDB(ctx)
	err := userDB.Insert(&user.Model{UID: userUID, Name: "TestUser"})
	assert.NoError(t, err)
	err = userDB.Insert(&user.Model{UID: botUID, Name: "TestBot2"})
	assert.NoError(t, err)

	// Setup: create friend relationship (both directions)
	friendVersion, _ := ctx.GenSeq(common.FriendSeqKey)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO friend(uid, to_uid, version, is_deleted) VALUES(?, ?, ?, 0)",
		userUID, botUID, friendVersion,
	).Exec()
	assert.NoError(t, err)

	friendVersion2, _ := ctx.GenSeq(common.FriendSeqKey)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO friend(uid, to_uid, version, is_deleted) VALUES(?, ?, ?, 0)",
		botUID, userUID, friendVersion2,
	).Exec()
	assert.NoError(t, err)

	// Verify friend records exist
	var count int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM friend WHERE (uid=? OR to_uid=?) AND is_deleted=0",
		botUID, botUID,
	).LoadOne(&count)
	assert.NoError(t, err)
	assert.Equal(t, 2, count)

	// Execute: delete friend records with version
	deleteVersion, _ := ctx.GenSeq(common.FriendSeqKey)
	_, err = ctx.DB().Update("friend").
		Set("is_deleted", 1).
		Set("version", deleteVersion).
		Where("(uid=? or to_uid=?) and is_deleted=0", botUID, botUID).
		Exec()
	assert.NoError(t, err)

	// Verify: no active friend records
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM friend WHERE (uid=? OR to_uid=?) AND is_deleted=0",
		botUID, botUID,
	).LoadOne(&count)
	assert.NoError(t, err)
	assert.Equal(t, 0, count)

	// Verify: records still exist but marked as deleted with version
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM friend WHERE (uid=? OR to_uid=?) AND is_deleted=1",
		botUID, botUID,
	).LoadOne(&count)
	assert.NoError(t, err)
	assert.Equal(t, 2, count)
}

// TestDeleteBotReleasesUsername verifies that deleteRobot clears the username
// field so that existRobotByUsername no longer considers it occupied.
func TestDeleteBotReleasesUsername(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	db := newBotfatherDB(ctx)

	robotID := "test_bot_username_release"
	username := "reuse_me_bot"

	// Insert a robot with a username directly via SQL
	_, err := ctx.DB().InsertInto("robot").Columns(
		"robot_id", "username", "status", "version",
	).Values(robotID, username, 1, 1).Exec()
	assert.NoError(t, err)

	// Confirm username is occupied
	exists, err := db.existRobotByUsername(username)
	assert.NoError(t, err)
	assert.True(t, exists, "username should be occupied before delete")

	// Delete the robot
	err = db.deleteRobot(robotID)
	assert.NoError(t, err)

	// Confirm username is now free
	exists, err = db.existRobotByUsername(username)
	assert.NoError(t, err)
	assert.False(t, exists, "username should be free after delete")

	// Verify a new bot can reuse the same username (real-world scenario)
	newRobotID := "test_bot_username_reuse_new"
	_, err = ctx.DB().InsertInto("robot").Columns(
		"robot_id", "username", "status", "version",
	).Values(newRobotID, username, 1, 1).Exec()
	assert.NoError(t, err, "should be able to create new bot with same username after delete")

	exists, err = db.existRobotByUsername(username)
	assert.NoError(t, err)
	assert.True(t, exists, "new bot should now occupy the username")
}
