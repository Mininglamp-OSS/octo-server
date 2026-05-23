package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
)

// TestSpaceID_BuildFollowItems_GroupAndThreadFilled 验证 buildFollowItems 把
// groupSpaceMap 里的值回填到 SidebarItem.SpaceID（GH octo-server#153）。
func TestSpaceID_BuildFollowItems_GroupAndThreadFilled(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1000},
		{ChannelID: "g1____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 1100},
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 1200},
	}
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: &cat, CategorySort: 0, CategoryGroupSort: 0},
	}
	threadExtMap := map[string]*convext.Model{
		"g1____th1": {TargetID: "g1____th1", FollowSort: 5},
	}
	followedDMs := map[string]*convext.Model{
		"u1": {TargetID: "u1", FollowSort: 7},
	}
	groupSpaceMap := map[string]string{
		"g1": "spaceA",
	}

	items := buildFollowItems(convs, categorySetting, nil, followedDMs, threadExtMap, nil, nil, groupSpaceMap, nil, "")

	bySpace := map[string]string{}
	byTarget := map[string]*SidebarItem{}
	for _, it := range items {
		byTarget[it.TargetID] = it
		bySpace[it.TargetID] = it.SpaceID
	}
	assert.Equal(t, "spaceA", bySpace["g1"], "group SpaceID 来自 groupSpaceMap")
	assert.Equal(t, "spaceA", bySpace["g1____th1"], "thread 继承父群 spaceA")
	assert.Equal(t, "", bySpace["u1"], "DM SpaceID 永远留空")
}

// TestSpaceID_BuildFollowItems_NilGroupSpaceMap 验证 nil map 时不 panic 且字段
// 留空 —— group service 调用失败的 fail-open 路径（CollectGroupSpaceMap 返回
// ok=false，handler 退化到空 map）。
func TestSpaceID_BuildFollowItems_NilGroupSpaceMap(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1000},
	}
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: &cat},
	}

	assert.NotPanics(t, func() {
		items := buildFollowItems(convs, categorySetting, nil, nil, nil, nil, nil, nil, nil, "")
		assert.Len(t, items, 1)
		assert.Equal(t, "", items[0].SpaceID)
	})
}

// TestSpaceID_BuildRecentItems_GroupAndThreadFilled 验证 buildRecentItems 同
// 口径回填 SpaceID。
func TestSpaceID_BuildRecentItems_GroupAndThreadFilled(t *testing.T) {
	now := nowRecent()
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: now},
		{ChannelID: "g1____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: now},
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: now},
		// 父群 g_missing 没出现在 groupSpaceMap：thread 应该 SpaceID="" 但仍出现在 recent。
		{ChannelID: "g_missing____th2", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: now},
	}
	groupSpaceMap := map[string]string{
		"g1": "spaceB",
	}

	items := buildRecentItems(convs, nil, groupSpaceMap, nil, "")

	bySpace := map[string]string{}
	for _, it := range items {
		bySpace[it.TargetID] = it.SpaceID
	}
	assert.Equal(t, "spaceB", bySpace["g1"])
	assert.Equal(t, "spaceB", bySpace["g1____th1"], "thread 取父群 SpaceID")
	assert.Equal(t, "", bySpace["u1"], "DM SpaceID 留空")
	assert.Equal(t, "", bySpace["g_missing____th2"], "groupSpaceMap 缺父群：留空，不猜")
}

// TestSpaceID_MergeThreadEntries_FillsParentSpaceID 验证 DB-only thread（IM
// 没返回但 user_conversation_ext 有的）补齐时也回填父群 SpaceID。
func TestSpaceID_MergeThreadEntries_FillsParentSpaceID(t *testing.T) {
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: &cat},
	}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____alive", FollowSort: 1},
	}
	groupSpaceMap := map[string]string{"g1": "spaceC"}

	result := mergeThreadEntries(nil, threadExtRows, aliveThread("g1____alive", nil), categorySetting, nil, groupSpaceMap, nil, "")

	if assert.Len(t, result, 1) {
		assert.Equal(t, "g1____alive", result[0].TargetID)
		assert.Equal(t, "spaceC", result[0].SpaceID, "DB-only thread 应继承父群 SpaceID")
	}
}
