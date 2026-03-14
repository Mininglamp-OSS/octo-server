package space

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestParseSpaceChannelID(t *testing.T) {
	tests := []struct {
		name           string
		channelID      string
		wantSpaceID    string
		wantUID        string
		wantIsSpace    bool
	}{
		{
			name:        "standard space channel",
			channelID:   "sspace123_user456",
			wantSpaceID: "space123",
			wantUID:     "user456",
			wantIsSpace: true,
		},
		{
			name:        "space id with underscore",
			channelID:   "sspace_with_under_user789",
			wantSpaceID: "space_with_under",
			wantUID:     "user789",
			wantIsSpace: true,
		},
		{
			name:        "normal user channel",
			channelID:   "normaluser123",
			wantSpaceID: "",
			wantUID:     "normaluser123",
			wantIsSpace: false,
		},
		{
			name:        "invalid space channel - no underscore",
			channelID:   "snounderscore",
			wantSpaceID: "",
			wantUID:     "snounderscore",
			wantIsSpace: false,
		},
		{
			name:        "empty channel",
			channelID:   "",
			wantSpaceID: "",
			wantUID:     "",
			wantIsSpace: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spaceID, uid, isSpace := parseSpaceChannelID(tt.channelID)
			assert.Equal(t, tt.wantSpaceID, spaceID)
			assert.Equal(t, tt.wantUID, uid)
			assert.Equal(t, tt.wantIsSpace, isSpace)
		})
	}
}

func TestCheckSpacePermission_BotToBot(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create two bots
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values("bot1", "token1", 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values("bot2", "token2", 1).Exec()
	assert.NoError(t, err)

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "sspace1_bot1",
		ToChannelID: "sspace1_bot2",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Allow)
	assert.Equal(t, "bot_to_bot", resp.Reason)
}

func TestCheckSpacePermission_HumanToHuman_SameSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create space and add both users
	spaceID := "testspace1"
	user1 := "user1"
	user2 := "user2"

	err = NewDB(ctx).insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID,
		Name:    "Test Space",
		Creator: user1,
		Status:  1,
	})
	assert.NoError(t, err)

	err = NewDB(ctx).insertMemberNoTx(&MemberModel{
		SpaceId: spaceID,
		UID:     user1,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	err = NewDB(ctx).insertMemberNoTx(&MemberModel{
		SpaceId: spaceID,
		UID:     user2,
		Role:    0,
		Status:  1,
	})
	assert.NoError(t, err)

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "s" + spaceID + "_" + user1,
		ToChannelID: "s" + spaceID + "_" + user2,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.True(t, resp.Allow)
}

func TestCheckSpacePermission_HumanToHuman_SameSpace_NotBothMembers(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create space and add only one user
	spaceID := "testspace2"
	user1 := "user1"
	user2 := "user2"

	err = NewDB(ctx).insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID,
		Name:    "Test Space",
		Creator: user1,
		Status:  1,
	})
	assert.NoError(t, err)

	err = NewDB(ctx).insertMemberNoTx(&MemberModel{
		SpaceId: spaceID,
		UID:     user1,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)
	// user2 is NOT a member

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "s" + spaceID + "_" + user1,
		ToChannelID: "s" + spaceID + "_" + user2,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Allow)
	assert.Equal(t, "not_both_members", resp.Reason)
}

func TestCheckSpacePermission_HumanToHuman_DifferentSpace_Friend(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create two users with friend relationship
	user1 := "user1"
	user2 := "user2"

	_, err = ctx.DB().InsertInto("friend").
		Columns("uid", "to_uid", "is_deleted", "is_alone").
		Values(user1, user2, 0, 0).Exec()
	assert.NoError(t, err)

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "sspace1_" + user1,
		ToChannelID: "sspace2_" + user2,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.True(t, resp.Allow)
}

func TestCheckSpacePermission_HumanToHuman_DifferentSpace_NotFriend(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	user1 := "user1"
	user2 := "user2"
	// No friend relationship

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "sspace1_" + user1,
		ToChannelID: "sspace2_" + user2,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Allow)
	assert.Equal(t, "not_friend", resp.Reason)
}

func TestCheckSpacePermission_HumanToBot_DifferentSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create a bot
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values("bot1", "token1", 1).Exec()
	assert.NoError(t, err)

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "sspace1_user1",
		ToChannelID: "sspace2_bot1",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Allow)
	assert.Equal(t, "cross_space_bot", resp.Reason)
}

func TestCheckSpacePermission_HumanToBot_SameSpace_Friend(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	user1 := "user1"
	botUID := "bot1"
	spaceID := "testspace1"

	// Create a bot
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values(botUID, "token1", 1).Exec()
	assert.NoError(t, err)

	// Create friend relationship
	_, err = ctx.DB().InsertInto("friend").
		Columns("uid", "to_uid", "is_deleted", "is_alone").
		Values(user1, botUID, 0, 0).Exec()
	assert.NoError(t, err)

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "s" + spaceID + "_" + user1,
		ToChannelID: "s" + spaceID + "_" + botUID,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.True(t, resp.Allow)
}

func TestCheckSpacePermission_HumanToBot_SameSpace_NotFriend(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	user1 := "user1"
	botUID := "bot1"
	spaceID := "testspace1"

	// Create a bot
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values(botUID, "token1", 1).Exec()
	assert.NoError(t, err)

	// No friend relationship

	svc := NewPermissionService(ctx)
	resp, err := svc.CheckSpacePermission(&CheckSpacePermissionReq{
		FromUID:     "s" + spaceID + "_" + user1,
		ToChannelID: "s" + spaceID + "_" + botUID,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.Allow)
	assert.Equal(t, "not_friend", resp.Reason)
}

func TestAddAndRemoveSpaceMemberFromCache(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	spaceID := "testspace_cache"
	uid := "testuser_cache"

	// Add member to cache
	err := AddSpaceMemberToCache(ctx, spaceID, uid)
	assert.NoError(t, err)

	// Verify member is in cache
	cacheKey := CacheKeySpaceMembers + spaceID
	isMember, err := ctx.GetRedisConn().Sismember(cacheKey, uid)
	assert.NoError(t, err)
	assert.Equal(t, 1, isMember)

	// Remove member from cache
	err = RemoveSpaceMemberFromCache(ctx, spaceID, uid)
	assert.NoError(t, err)

	// Verify member is removed
	isMember, err = ctx.GetRedisConn().Sismember(cacheKey, uid)
	assert.NoError(t, err)
	assert.Equal(t, 0, isMember)
}

func TestAddAndRemoveBotFromCache(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	botUID := "testbot_cache"

	// Add bot to cache
	err := AddBotToCache(ctx, botUID)
	assert.NoError(t, err)

	// Verify bot is in cache
	isMember, err := ctx.GetRedisConn().Sismember(CacheKeyBotsAll, botUID)
	assert.NoError(t, err)
	assert.Equal(t, 1, isMember)

	// Remove bot from cache
	err = RemoveBotFromCache(ctx, botUID)
	assert.NoError(t, err)

	// Verify bot is removed
	isMember, err = ctx.GetRedisConn().Sismember(CacheKeyBotsAll, botUID)
	assert.NoError(t, err)
	assert.Equal(t, 0, isMember)
}
