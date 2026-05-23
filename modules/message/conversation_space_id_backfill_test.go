package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

// TestSpaceID_FillConversationSpaceIDs_GroupBackfill 验证群聊会话从
// rawGroupSpaceMap 拿到 resolved space_id 回填到 SyncUserConversationResp.SpaceID。
//
// 背景 (GH octo-server#153)：newSyncUserConversationResp 用 ParseChannelID 推
// SpaceID，群聊 channel_id 是裸 group_no 拿不到，必须用 group 表权威值回填，
// 客户端才能正确判定 WebSocket 群消息的 Space 归属。
//
// Round-3 (GH octo-server#154 Round-2 Finding 1)：rawGroupSpaceMap 必须来自
// GetGroups（不经 SetEffectiveSpaceID 改写），而不是 GetGroupDetails 的结果。
func TestSpaceID_FillConversationSpaceIDs_GroupBackfill(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g_internal", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "g_external", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "g_legacy", ChannelType: common.ChannelTypeGroup.Uint8()},
		// PERSON 频道：保持原 SpaceID 不动。
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), SpaceID: ""},
	}
	rawGroupSpaceMap := map[string]string{
		"g_internal": "spaceA",
		"g_external": "spaceB", // 群表权威值，未经 effective rewrite
		"g_legacy":   "",        // 旧群无 space_id
	}
	externalGroupMap := map[string]string{
		"g_external": "spaceA", // 当前 user 从 spaceA 加入了 g_external
	}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, "")

	assert.Equal(t, "spaceA", resps[0].SpaceID, "internal group: SpaceID 来自 group 表")
	assert.Equal(t, "", resps[0].MySourceSpaceID, "non-external group: MySourceSpaceID 保持空")

	assert.Equal(t, "spaceB", resps[1].SpaceID, "external group: 群本身 SpaceID 还是 group 表")
	assert.Equal(t, "spaceA", resps[1].MySourceSpaceID, "external group: MySourceSpaceID 来自 externalGroupMap")

	assert.Equal(t, "", resps[2].SpaceID, "legacy group with empty space_id: 保持空")

	assert.Equal(t, "", resps[3].SpaceID, "PERSON: 不写 conversation-level SpaceID")
	assert.Equal(t, "", resps[3].MySourceSpaceID, "PERSON: 不填 MySourceSpaceID")
}

// TestSpaceID_FillConversationSpaceIDs_ThreadInheritsParent 验证子区(thread)会话
// 的 SpaceID 来自父群（与 FilterRawConversationsBySpace thread 分支同口径）。
func TestSpaceID_FillConversationSpaceIDs_ThreadInheritsParent(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g_parent____thread1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		// 父群在 externalGroupMap，子区也应继承 my_source_space_id。
		{ChannelID: "g_ext____thread2", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		// 父群不在 rawGroupSpaceMap（未活跃，本批没返回）：不补 SpaceID，保留空字符串。
		{ChannelID: "g_missing____thread3", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		// channel_id 形式错误：跳过。
		{ChannelID: "invalid-no-separator", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	}
	rawGroupSpaceMap := map[string]string{
		"g_parent": "spaceX",
		"g_ext":    "spaceY",
	}
	externalGroupMap := map[string]string{
		"g_ext": "spaceX",
	}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, "")

	assert.Equal(t, "spaceX", resps[0].SpaceID, "thread 继承父群 spaceX")
	assert.Equal(t, "", resps[0].MySourceSpaceID)

	assert.Equal(t, "spaceY", resps[1].SpaceID, "external 父群的 thread 仍取父群 SpaceID")
	assert.Equal(t, "spaceX", resps[1].MySourceSpaceID, "thread 继承父群的 MySourceSpaceID")

	assert.Equal(t, "", resps[2].SpaceID, "父群不在 rawGroupSpaceMap：保持空，不猜")

	assert.Equal(t, "", resps[3].SpaceID, "malformed thread channel id：跳过")
}

// TestSpaceID_FillConversationSpaceIDs_PreservesExistingSpaceID 验证已有非空
// SpaceID 不被覆盖 —— ParseChannelID 已经能从前缀解出 SpaceID 的情况
// （Space-prefixed channel）下，我们不应被空 rawGroupSpaceMap 项覆盖回空。
func TestSpaceID_FillConversationSpaceIDs_PreservesExistingSpaceID(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: "spaceA"},
	}
	// rawGroupSpaceMap 给一个不同的值（不该发生，但回填逻辑必须以已 enriched 的 SpaceID 为准）。
	rawGroupSpaceMap := map[string]string{
		"g1": "spaceB",
	}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, nil, "")

	assert.Equal(t, "spaceA", resps[0].SpaceID, "不覆盖已 enriched 的 SpaceID")
}

// TestSpaceID_FillConversationSpaceIDs_HandlesNilEntries 边界：nil entry / nil
// maps 不应 panic。
func TestSpaceID_FillConversationSpaceIDs_HandlesNilEntries(t *testing.T) {
	resps := []*SyncUserConversationResp{
		nil,
		{ChannelID: "g1", ChannelType: common.ChannelTypeGroup.Uint8()},
	}
	assert.NotPanics(t, func() {
		fillConversationSpaceIDs(resps, nil, nil, "")
	})
	assert.Equal(t, "", resps[1].SpaceID)
}
