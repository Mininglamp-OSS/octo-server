package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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
	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, nil, false, false)
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
	result := filterConversationsCore(convs, "spaceB", "spaceA", nil, nil, botSet, botInSpace, false, false)
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
	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, nil, false, false)
	assert.Len(t, result, 2)

	// filterSpaceID != defaultSpaceID → 无 Recents 匹配 → 不显示
	result = filterConversationsCore(convs, "spaceB", "spaceA", nil, nil, nil, nil, false, false)
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
	result := filterConversationsCore(convs, "spaceB", "spaceA", nil, nil, botSet, botInSpace, false, false)

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

	result := filterConversationsCore(convs, "spaceNew", "spaceA", nil, nil, map[string]bool{}, map[string]bool{}, false, false)

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

	result := filterConversationsCore(convs, "spaceA", "spaceA", groupMap, nil, nil, nil, false, false)
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

	result := filterConversationsCore(convs, "spaceA", "spaceA", nil, nil, nil, nil, true, false)
	// g1, g2 保留（skipGroupFilter），u1 保留（直接匹配）
	assert.Len(t, result, 3)
}

func TestFilterConversationsBySpace_ExternalGroupVisibleInSourceSpace(t *testing.T) {
	// 外部群：用户作为 Space B 的外部成员加入 Space A 的群，
	// 在 Space B 会话列表中应可见，在其他 Space 不应可见。
	convs := []*SyncUserConversationResp{
		{ChannelID: "gExt", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: "spaceA"},
	}
	externalMap := map[string]string{"gExt": "spaceB"}

	// 在 source Space（B）下可见
	result := filterConversationsCore(convs, "spaceB", "spaceB", nil, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)
	assert.Equal(t, "gExt", result[0].ChannelID)

	// 在群的原生 Space（A）下也可见
	result = filterConversationsCore(convs, "spaceA", "spaceB", nil, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)

	// 在无关 Space（C）下不可见
	result = filterConversationsCore(convs, "spaceC", "spaceB", nil, externalMap, nil, nil, false, false)
	assert.Len(t, result, 0)
}

func TestFilterConversationsBySpace_ExternalGroupFallbackToDefault(t *testing.T) {
	// 外部成员已离开 source Space 时，source_space_id 为空，
	// 应 fallback 到默认 Space 显示。
	convs := []*SyncUserConversationResp{
		{ChannelID: "gExt", ChannelType: common.ChannelTypeGroup.Uint8(), SpaceID: "spaceA"},
	}
	externalMap := map[string]string{"gExt": ""} // source Space 记录已失效

	// defaultSpaceID=spaceB，fallback 后在 spaceB 可见
	result := filterConversationsCore(convs, "spaceB", "spaceB", nil, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)
}

func TestFilterConversationsBySpace_ThreadChannelInParentGroupSpace(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"g1": "spaceA"}

	result := filterConversationsCore(convs, "spaceA", "spaceDefault", groupMap, nil, nil, nil, false, false)
	assert.Len(t, result, 1)

	result = filterConversationsCore(convs, "spaceB", "spaceDefault", groupMap, nil, nil, nil, false, false)
	assert.Len(t, result, 0)
}

func TestFilterConversationsBySpace_ThreadChannelNoLeakInDefaultSpace(t *testing.T) {
	// 父群在 spaceA，用户在默认 Space → 子区不能借兜底分支漏出来
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"g1": "spaceA"}

	result := filterConversationsCore(convs, "spaceDefault", "spaceDefault", groupMap, nil, nil, nil, false, false)
	assert.Len(t, result, 0)
}

func TestFilterConversationsBySpace_ThreadChannelExternalGroup(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "gExt____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"gExt": "spaceA"}
	externalMap := map[string]string{"gExt": "spaceB"}

	result := filterConversationsCore(convs, "spaceB", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)

	result = filterConversationsCore(convs, "spaceA", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)

	result = filterConversationsCore(convs, "spaceC", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 0)
}

func TestFilterConversationsBySpace_ThreadChannelExternalOnly(t *testing.T) {
	// 隔离外部群路径：父群 home Space 与 filterSpaceID 不重合，
	// 子区只能通过外部群 source Space 才能匹配到 filterSpaceID。
	convs := []*SyncUserConversationResp{
		{ChannelID: "gExt____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"gExt": "spaceHome"}
	externalMap := map[string]string{"gExt": "spaceB"}

	// filter=spaceB 只能通过 external 路径命中
	result := filterConversationsCore(convs, "spaceB", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)

	// filter=spaceHome 只能通过直接匹配命中（不走 external）
	result = filterConversationsCore(convs, "spaceHome", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)
}

func TestFilterConversationsBySpace_ThreadChannelExternalFallback(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "gExt____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"gExt": "spaceA"}
	externalMap := map[string]string{"gExt": ""}

	result := filterConversationsCore(convs, "spaceDefault", "spaceDefault", groupMap, externalMap, nil, nil, false, false)
	assert.Len(t, result, 1)
}

func TestFilterConversationsBySpace_ThreadChannelLegacyParent(t *testing.T) {
	// 父群无 space_id（旧群） → 子区跟旧群一样所有 Space 可见
	convs := []*SyncUserConversationResp{
		{ChannelID: "gLegacy____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	result := filterConversationsCore(convs, "spaceA", "spaceDefault", map[string]string{}, nil, nil, nil, false, false)
	assert.Len(t, result, 1)
	result = filterConversationsCore(convs, "spaceB", "spaceDefault", map[string]string{}, nil, nil, nil, false, false)
	assert.Len(t, result, 1)
}

func TestFilterConversationsBySpace_ThreadChannelInvalidID(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "bad-channel-id", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
		{ChannelID: "g1____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	groupMap := map[string]string{"g1": "spaceA"}

	result := filterConversationsCore(convs, "spaceA", "spaceDefault", groupMap, nil, nil, nil, false, false)
	assert.Len(t, result, 1)
	assert.Equal(t, "g1____123456789012345", result[0].ChannelID)
}

func TestFilterConversationsBySpace_ThreadChannelSkipGroupFilter(t *testing.T) {
	convs := []*SyncUserConversationResp{
		{ChannelID: "g1____123456789012345", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
		{ChannelID: "g2____987654321098765", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), SpaceID: ""},
	}
	result := filterConversationsCore(convs, "spaceA", "spaceDefault", nil, nil, nil, nil, true, false)
	assert.Len(t, result, 2)
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

// ---- YUJ-216 / GH#1280: SystemBot sync bypass ----

func TestEnsureSystemBotsPresent_InjectsMissingBots(t *testing.T) {
	// 空会话列表 → 所有系统 Bot 都作为占位注入
	result := EnsureSystemBotsPresent(nil)
	assert.Len(t, result, len(spacepkg.SystemBotList()))

	// 每个系统 Bot 都应以 Person 占位形式存在
	ids := make(map[string]*SyncUserConversationResp, len(result))
	for _, conv := range result {
		ids[conv.ChannelID] = conv
	}
	for _, uid := range spacepkg.SystemBotList() {
		conv, ok := ids[uid]
		assert.True(t, ok, "system bot %s should be injected", uid)
		assert.Equal(t, common.ChannelTypePerson.Uint8(), conv.ChannelType)
		assert.Equal(t, "", conv.SpaceID)
		assert.NotNil(t, conv.Recents, "Recents should be [] not nil for client compat")
		assert.Empty(t, conv.Recents)
		// 占位 entry 不应伪造 version/unread，避免客户端把占位 ack 回去
		assert.Equal(t, int64(0), conv.Version)
		assert.Equal(t, 0, conv.Unread)
	}
}

func TestEnsureSystemBotsPresent_PreservesExistingEntries(t *testing.T) {
	// 已存在的 botfather 真实会话不应被占位覆盖
	real := &SyncUserConversationResp{
		ChannelID:   "botfather",
		ChannelType: common.ChannelTypePerson.Uint8(),
		SpaceID:     "",
		Version:     12345,
		Unread:      7,
		Recents: []*MsgSyncResp{
			{Payload: map[string]interface{}{"content": "hello"}},
		},
	}
	in := []*SyncUserConversationResp{real}

	result := EnsureSystemBotsPresent(in)

	// 找回 botfather → 必须是原对象，字段未改
	var got *SyncUserConversationResp
	for _, conv := range result {
		if conv.ChannelID == "botfather" {
			got = conv
			break
		}
	}
	assert.NotNil(t, got)
	assert.Same(t, real, got, "existing botfather entry must be preserved in-place")
	assert.Equal(t, int64(12345), got.Version)
	assert.Equal(t, 7, got.Unread)
	assert.Len(t, got.Recents, 1)

	// u_10000 / fileHelper 仍被补齐
	ids := map[string]bool{}
	for _, conv := range result {
		ids[conv.ChannelID] = true
	}
	assert.True(t, ids["u_10000"])
	assert.True(t, ids["fileHelper"])
}

func TestEnsureSystemBotsPresent_IgnoresSameUIDOnNonPersonChannel(t *testing.T) {
	// 极端情况：同名 channel 不是 Person（理论上不会发生）不能错认为已存在
	// 否则会漏掉占位注入。
	weird := &SyncUserConversationResp{
		ChannelID:   "botfather",
		ChannelType: common.ChannelTypeGroup.Uint8(), // 非 Person
	}
	result := EnsureSystemBotsPresent([]*SyncUserConversationResp{weird})

	var personCount int
	for _, conv := range result {
		if conv.ChannelID == "botfather" && conv.ChannelType == common.ChannelTypePerson.Uint8() {
			personCount++
		}
	}
	assert.Equal(t, 1, personCount, "Person placeholder should still be injected")
}

func TestEnsureSystemBotsPresent_HandlesNilEntries(t *testing.T) {
	// nil 会话不应 panic，且不阻止后续 Bot 注入
	result := EnsureSystemBotsPresent([]*SyncUserConversationResp{nil})
	ids := map[string]bool{}
	for _, conv := range result {
		if conv == nil {
			continue
		}
		ids[conv.ChannelID] = true
	}
	for _, uid := range spacepkg.SystemBotList() {
		assert.True(t, ids[uid], "system bot %s should still be injected when list contains nil", uid)
	}
}

func TestEnsureSystemBotsPresent_AllBotsAlreadyPresent(t *testing.T) {
	// 响应中已有全部系统 Bot → 不改变长度、不新增占位
	in := make([]*SyncUserConversationResp, 0)
	for _, uid := range spacepkg.SystemBotList() {
		in = append(in, &SyncUserConversationResp{
			ChannelID:   uid,
			ChannelType: common.ChannelTypePerson.Uint8(),
			Recents:     []*MsgSyncResp{{Payload: map[string]interface{}{"content": "x"}}},
		})
	}
	result := EnsureSystemBotsPresent(in)
	assert.Len(t, result, len(in))
}

func TestSystemBotList_DeterministicOrder(t *testing.T) {
	// 连续调用返回同顺序（方便响应序列化稳定 & 测试幂等）
	a := spacepkg.SystemBotList()
	b := spacepkg.SystemBotList()
	assert.Equal(t, a, b)
	// 至少包含 botfather
	assert.Contains(t, a, "botfather")
}

func TestIsSystemBot(t *testing.T) {
	assert.True(t, spacepkg.IsSystemBot("botfather"))
	assert.True(t, spacepkg.IsSystemBot("u_10000"))
	assert.True(t, spacepkg.IsSystemBot("fileHelper"))
	assert.False(t, spacepkg.IsSystemBot("random_user"))
	assert.False(t, spacepkg.IsSystemBot(""))
}

// TestSyncPipeline_SystemBotsAlwaysReturned 模拟 POST /v1/conversation/sync
// 完整数据流：IM 核心返回的会话 → FilterConversationsBySpace → EnsureSystemBotsPresent。
// 对任意 X-Space-ID（含默认 Space、非默认 Space、全新 Space），最终响应都必须
// 包含每一个系统 Bot 的 entry。这是 YUJ-216 / GH#1280 的验收门槛。
func TestSyncPipeline_SystemBotsAlwaysReturned(t *testing.T) {
	// 模拟 IM 核心返回的原始会话：
	//   - 一条普通 DM（属于 spaceA 的消息）
	//   - 一个普通群（spaceA）
	//   - botfather 本次没有新消息 → 增量 sync 中缺席
	base := []*SyncUserConversationResp{
		{
			ChannelID:   "peer1",
			ChannelType: common.ChannelTypePerson.Uint8(),
			SpaceID:     "",
			Recents: []*MsgSyncResp{
				{Payload: map[string]interface{}{"space_id": "spaceA", "content": "hi"}},
			},
		},
		{
			ChannelID:   "g1",
			ChannelType: common.ChannelTypeGroup.Uint8(),
			SpaceID:     "",
		},
	}
	groupMap := map[string]string{"g1": "spaceA"}

	run := func(filterSpaceID, defaultSpaceID string) []*SyncUserConversationResp {
		// 深拷贝一份，避免用例间干扰
		in := make([]*SyncUserConversationResp, len(base))
		for i, c := range base {
			cp := *c
			in[i] = &cp
		}
		filtered := filterConversationsCore(in, filterSpaceID, defaultSpaceID, groupMap, nil, nil, nil, false, false)
		return EnsureSystemBotsPresent(filtered)
	}

	cases := []struct {
		name           string
		filterSpaceID  string
		defaultSpaceID string
	}{
		{"default space", "spaceA", "spaceA"},
		{"non-default space with history", "spaceA", "spaceB"},
		{"brand-new space with no history", "spaceNew", "spaceA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := run(tc.filterSpaceID, tc.defaultSpaceID)
			ids := map[string]bool{}
			for _, conv := range result {
				ids[conv.ChannelID] = true
			}
			for _, uid := range spacepkg.SystemBotList() {
				assert.Truef(t, ids[uid],
					"X-Space-ID=%s 必须包含系统 Bot %s 的 entry", tc.filterSpaceID, uid)
			}
		})
	}
}

// ---------------- YUJ-219-A / GH#1283 · filterPersonMessagesBySpace ----------------
//
// 单测覆盖四种场景（analysis-report.md §4.1）：
//   1) SystemBot 跨 Space 历史消息 → 丢弃；当前 Space 消息 → 保留
//   2) 普通 DM 老消息（无 space_id）→ 保留（向前兼容）
//   3) 普通 DM 跨 Space 消息 → 丢弃
//   4) 空 spaceID / 空列表 / nil 消息的 no-op 行为

func newMsgWithSpaceID(messageID int64, spaceID string) *MsgSyncResp {
	payload := map[string]interface{}{
		"content": "hi",
	}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	return &MsgSyncResp{
		MessageID: messageID,
		Payload:   payload,
	}
}

func TestFilterPersonMessagesBySpace_SystemBot(t *testing.T) {
	// channelID = "botfather"（SystemBot）
	// 当前 Space = spaceA，按规则：
	//   - spaceA 消息保留
	//   - spaceB 消息丢弃
	//   - 无 space_id 消息丢弃（SystemBot 老消息默认隐藏，对齐 Android filterSystemBotMessages）
	msgs := []*MsgSyncResp{
		newMsgWithSpaceID(1, "spaceA"),
		newMsgWithSpaceID(2, "spaceB"),
		newMsgWithSpaceID(3, ""), // 老 SystemBot 消息
		newMsgWithSpaceID(4, "spaceA"),
	}

	got := filterPersonMessagesBySpace(msgs, "botfather", "spaceA")
	assert.Len(t, got, 2)
	assert.Equal(t, int64(1), got[0].MessageID)
	assert.Equal(t, int64(4), got[1].MessageID)
}

func TestFilterPersonMessagesBySpace_OrdinaryDMLegacyCompat(t *testing.T) {
	// channelID = "peer_uid"（非 SystemBot）
	// 当前 Space = spaceA：
	//   - spaceA 消息保留
	//   - spaceB 消息丢弃
	//   - 无 space_id 消息保留（普通 DM 向前兼容，对齐 filterConversationsCore 普通 DM 口径）
	msgs := []*MsgSyncResp{
		newMsgWithSpaceID(10, "spaceA"),
		newMsgWithSpaceID(11, ""), // 老 DM 消息，无 space_id
		newMsgWithSpaceID(12, "spaceB"),
	}

	got := filterPersonMessagesBySpace(msgs, "peer_uid", "spaceA")
	assert.Len(t, got, 2)
	ids := []int64{got[0].MessageID, got[1].MessageID}
	assert.Contains(t, ids, int64(10))
	assert.Contains(t, ids, int64(11))
	assert.NotContains(t, ids, int64(12))
}

func TestFilterPersonMessagesBySpace_CrossSpaceDropped(t *testing.T) {
	// 所有消息都属于 spaceB，当前 Space = spaceA → 全部丢弃。
	msgs := []*MsgSyncResp{
		newMsgWithSpaceID(20, "spaceB"),
		newMsgWithSpaceID(21, "spaceB"),
	}
	got := filterPersonMessagesBySpace(msgs, "peer_uid", "spaceA")
	assert.Len(t, got, 0)
}

func TestFilterPersonMessagesBySpace_AllInSpaceKept(t *testing.T) {
	// 所有消息都属于 spaceA → 全部保留。
	msgs := []*MsgSyncResp{
		newMsgWithSpaceID(30, "spaceA"),
		newMsgWithSpaceID(31, "spaceA"),
		newMsgWithSpaceID(32, "spaceA"),
	}
	got := filterPersonMessagesBySpace(msgs, "peer_uid", "spaceA")
	assert.Len(t, got, 3)
}

func TestFilterPersonMessagesBySpace_EmptySpaceIDNoOp(t *testing.T) {
	// spaceID 空 → 不过滤，原样返回。保证老客户端未发送 X-Space-ID header
	// 时行为不变（向前兼容）。
	msgs := []*MsgSyncResp{
		newMsgWithSpaceID(40, "spaceA"),
		newMsgWithSpaceID(41, ""),
		newMsgWithSpaceID(42, "spaceB"),
	}
	got := filterPersonMessagesBySpace(msgs, "botfather", "")
	assert.Equal(t, msgs, got)
}

func TestFilterPersonMessagesBySpace_EmptySliceReturnsSame(t *testing.T) {
	got := filterPersonMessagesBySpace(nil, "botfather", "spaceA")
	assert.Nil(t, got)

	empty := []*MsgSyncResp{}
	got = filterPersonMessagesBySpace(empty, "botfather", "spaceA")
	assert.Equal(t, empty, got)
}

func TestFilterPersonMessagesBySpace_NilMessagesSkipped(t *testing.T) {
	// 切片里混入 nil 条目时不应 panic，且 nil 被跳过。
	msgs := []*MsgSyncResp{
		nil,
		newMsgWithSpaceID(50, "spaceA"),
		nil,
		newMsgWithSpaceID(51, "spaceB"),
	}
	got := filterPersonMessagesBySpace(msgs, "peer_uid", "spaceA")
	assert.Len(t, got, 1)
	assert.Equal(t, int64(50), got[0].MessageID)
}

func TestFilterPersonMessagesBySpace_PayloadSpaceIDWrongType(t *testing.T) {
	// payload.space_id 不是字符串（例如异常数据）→ 视为空值，按"无 space_id"
	// 分支处理（普通 DM 保留 / SystemBot 丢弃）。
	msgA := &MsgSyncResp{MessageID: 60, Payload: map[string]interface{}{"space_id": 123}}

	// 普通 DM：保留
	got := filterPersonMessagesBySpace([]*MsgSyncResp{msgA}, "peer_uid", "spaceA")
	assert.Len(t, got, 1)

	// SystemBot：丢弃
	got = filterPersonMessagesBySpace([]*MsgSyncResp{msgA}, "fileHelper", "spaceA")
	assert.Len(t, got, 0)
}

func TestFilterPersonMessagesBySpace_SystemBotListCoverage(t *testing.T) {
	// 保证三个已知系统 Bot 都走 SystemBot 分支（老消息被丢弃）。
	msgs := []*MsgSyncResp{newMsgWithSpaceID(70, "")}
	for _, bot := range spacepkg.SystemBotList() {
		got := filterPersonMessagesBySpace(msgs, bot, "spaceA")
		assert.Emptyf(t, got, "SystemBot %s 的无 space_id 消息应被丢弃", bot)
	}
}

func TestExtractPayloadSpaceID(t *testing.T) {
	assert.Equal(t, "", extractPayloadSpaceID(nil))
	assert.Equal(t, "", extractPayloadSpaceID(map[string]interface{}{}))
	assert.Equal(t, "", extractPayloadSpaceID(map[string]interface{}{"foo": "bar"}))
	assert.Equal(t, "", extractPayloadSpaceID(map[string]interface{}{"space_id": 123}))
	assert.Equal(t, "spaceA", extractPayloadSpaceID(map[string]interface{}{"space_id": "spaceA"}))
}
