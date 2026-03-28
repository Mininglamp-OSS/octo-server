package message

import (
	"testing"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

func TestFilterConversationsBySpace_DirectMatch(t *testing.T) {
	// 会话 SpaceID 直接匹配 filterSpaceID → 保留
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: "spaceA"},
		{ChannelID: "g2", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: "spaceB"},
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "spaceA"},
	}

	// 所有会话都有 SpaceID，不触发 bareGroupNos / bareDMUIDs 逻辑
	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, false, false)
	assert.Len(t, result, 2)
	assert.Equal(t, "g1", result[0].ChannelID)
	assert.Equal(t, "u1", result[1].ChannelID)
}

func TestFilterConversationsBySpace_SystemBotsVisible(t *testing.T) {
	// 系统 Bot 应在所有 Space 可见（非默认 Space 中的裸 DM）
	convs := []*SyncUserConversationResp{
		{ChannelID: "botfather", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
		{ChannelID: "u_10000", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
		{ChannelID: "fileHelper", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
		{ChannelID: "custom_bot", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
	}

	// filterSpaceID != defaultSpaceID，所以走"非默认 Space 中的 DM"分支
	// botSet=nil → custom_bot 不被识别为 Bot，当作普通 DM 保留
	// 传入 botSet 标记 custom_bot 为 Bot，且不在此 Space → 不显示
	botSet := map[string]bool{"custom_bot": true}
	botInSpace := map[string]bool{}
	result := filterConversationsCore(convs, "spaceB", "spaceA", nil, botSet, botInSpace, false, false)
	// 系统 Bot 可见，custom_bot（Bot 不在此 Space）不可见
	assert.Len(t, result, 3)
	ids := []string{result[0].ChannelID, result[1].ChannelID, result[2].ChannelID}
	assert.Contains(t, ids, "botfather")
	assert.Contains(t, ids, "u_10000")
	assert.Contains(t, ids, "fileHelper")
}

func TestFilterConversationsBySpace_DefaultSpaceBareConvs(t *testing.T) {
	// 裸 UID 旧会话只在默认 Space 显示
	convs := []*SyncUserConversationResp{
		{ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
		{ChannelID: "user2", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
	}

	// filterSpaceID == defaultSpaceID → 旧会话保留
	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, false, false)
	assert.Len(t, result, 2)

	// filterSpaceID != defaultSpaceID → 无 Recents 匹配 → 不显示
	result = filterConversationsCore(convs, "spaceB", "spaceA", nil, nil, nil, false, false)
	assert.Len(t, result, 0)
}

func TestFilterConversationsBySpace_NonDefaultSpaceDMVisible(t *testing.T) {
	// 非默认 Space 中，普通 DM 需有 Recents 中 space_id 匹配才显示
	convs := []*SyncUserConversationResp{
		{
			ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "",
			Recents: []*MsgSyncResp{{Payload: map[string]interface{}{"space_id": "spaceB", "content": "hi"}}},
		},
		{
			ChannelID: "user2", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "",
			Recents: []*MsgSyncResp{{Payload: map[string]interface{}{"space_id": "spaceA", "content": "old"}}},
		},
		{ChannelID: "custom_bot", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
		{ChannelID: "bot_in_space", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
	}

	botSet := map[string]bool{"custom_bot": true, "bot_in_space": true}
	botInSpace := map[string]bool{"bot_in_space": true}

	// filterSpaceID=spaceB != defaultSpaceID=spaceA
	result := filterConversationsCore(convs, "spaceB", "spaceA", nil, botSet, botInSpace, false, false)

	// user1（Recents 有 spaceB 消息）保留；user2（Recents 只有 spaceA）过滤；
	// bot_in_space（Bot 在此 Space）保留；custom_bot（Bot 不在此 Space）不保留
	assert.Len(t, result, 2)
	ids := make([]string, len(result))
	for i, r := range result {
		ids[i] = r.ChannelID
	}
	assert.Contains(t, ids, "user1")
	assert.Contains(t, ids, "bot_in_space")
	assert.NotContains(t, ids, "user2")
	assert.NotContains(t, ids, "custom_bot")
}

func TestFilterConversationsBySpace_NewSpaceCleanSlate(t *testing.T) {
	// 全新 Space：所有 DM 的 Recents 都没有该 Space 的消息 → 全部过滤
	convs := []*SyncUserConversationResp{
		{
			ChannelID: "user1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "",
			Recents: []*MsgSyncResp{{Payload: map[string]interface{}{"space_id": "spaceA", "content": "hi"}}},
		},
		{
			ChannelID: "user2", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "",
			Recents: []*MsgSyncResp{{Payload: map[string]interface{}{"content": "no space"}}},
		},
		{
			ChannelID: "user3", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "",
			// 空 Recents
		},
	}

	result := filterConversationsCore(convs, "spaceNew", "spaceA", nil, map[string]bool{}, map[string]bool{}, false, false)

	// 新 Space 没有任何 DM 有匹配消息 → clean slate
	assert.Len(t, result, 0)
}

func TestPersonConvHasSpaceMessages(t *testing.T) {
	// 有匹配的 space_id
	conv1 := &SyncUserConversationResp{
		Recents: []*MsgSyncResp{
			{Payload: map[string]interface{}{"content": "hello"}},
			{Payload: map[string]interface{}{"content": "world", "space_id": "spaceX"}},
		},
	}
	assert.True(t, personConvHasSpaceMessages(conv1, "spaceX"))
	assert.False(t, personConvHasSpaceMessages(conv1, "spaceY"))

	// 空 Recents
	conv2 := &SyncUserConversationResp{}
	assert.False(t, personConvHasSpaceMessages(conv2, "spaceX"))

	// nil conv
	assert.False(t, personConvHasSpaceMessages(nil, "spaceX"))

	// payload 为 nil
	conv3 := &SyncUserConversationResp{
		Recents: []*MsgSyncResp{{Payload: nil}},
	}
	assert.False(t, personConvHasSpaceMessages(conv3, "spaceX"))
}

func TestFilterConversationsBySpace_GroupSpaceMap(t *testing.T) {
	// 群聊通过 groupSpaceMap 匹配
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: ""},
		{ChannelID: "g2", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"g1": "spaceA", "g2": "spaceB"}

	result := filterConversationsCore(convs, "spaceA", "spaceA", groupMap, nil, nil, false, false)
	assert.Len(t, result, 1)
	assert.Equal(t, "g1", result[0].ChannelID)
}

func TestFilterConversationsBySpace_SkipGroupFilter(t *testing.T) {
	// skipGroupFilter=true 时保留所有裸群聊
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: ""},
		{ChannelID: "g2", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: ""},
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: "spaceA"},
	}

	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, true, false)
	// g1, g2 保留（skipGroupFilter），u1 保留（直接匹配）
	assert.Len(t, result, 3)
}

func TestGetBotUIDs_SkipsSystemBots(t *testing.T) {
	// 系统 Bot 不应被 GetBotUIDs 查询（它们在调用前被排除）
	uids := []string{"botfather", "u_10000", "fileHelper"}
	for _, uid := range uids {
		assert.True(t, spacepkg.SystemBots[uid], "should be system bot: %s", uid)
	}
}

func TestGetGroupSpaceMap_Empty(t *testing.T) {
	result, err := spacepkg.GetGroupSpaceMap(nil, func(nos []string) ([]spacepkg.GroupSpaceInfo, error) {
		t.Fatal("should not be called for empty input")
		return nil, nil
	})
	assert.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetGroupSpaceMap_Maps(t *testing.T) {
	result, err := spacepkg.GetGroupSpaceMap([]string{"g1", "g2"}, func(nos []string) ([]spacepkg.GroupSpaceInfo, error) {
		return []spacepkg.GroupSpaceInfo{
			{GroupNo: "g1", SpaceID: "spaceA"},
			{GroupNo: "g2", SpaceID: "spaceB"},
		}, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "spaceA", result["g1"])
	assert.Equal(t, "spaceB", result["g2"])
}

func TestCheckBotsInSpace_EmptyInputs(t *testing.T) {
	// Empty spaceID → empty result without DB call
	result, err := spacepkg.CheckBotsInSpace(nil, "", map[string]bool{"bot1": true})
	assert.NoError(t, err)
	assert.Empty(t, result)

	// Empty botUIDs → empty result without DB call
	result, err = spacepkg.CheckBotsInSpace(nil, "spaceA", map[string]bool{})
	assert.NoError(t, err)
	assert.Empty(t, result)
}
