package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpaceID_Round3_Finding1_GroupTableAuthoritativeSpaceID 验证：
//
// fillConversationSpaceIDs 必须使用群表的权威 space_id 回填
// SyncUserConversationResp.SpaceID，不能使用 GetGroupDetails 返回的、
// 已被 SetEffectiveSpaceIDFromMap 改写的 effective 值。
//
// 场景 (GH octo-server#154 Round-2 Finding 1)：
//   - 群 g_external 的群表 space_id = "spaceB"。
//   - 用户作为外部成员从 spaceA 加入 g_external（externalGroupMap[g_external]=spaceA）。
//   - GetGroupDetails 在内部会调用 SetEffectiveSpaceIDFromMap，把
//     GroupResp.SpaceID 从 "spaceB" 改写成 "spaceA"。
//   - 如果 fillConversationSpaceIDs 直接读 GroupDetails 的结果，
//     SyncUserConversationResp.SpaceID 就会变成 "spaceA"，与
//     MySourceSpaceID 一致——客户端无从分辨"群本身在 spaceB"和"我从 spaceA 加入"。
//
// Round-3 修复后的契约：handler 必须额外用 GetGroups(groupNos) 拿原始 space_id
// 构建 rawGroupSpaceMap 传给 fillConversationSpaceIDs；这里直接验证传入
// rawGroupSpaceMap（== 群表权威值）后，SyncUserConversationResp.SpaceID 是
// "spaceB" 不是 "spaceA"。
func TestSpaceID_Round3_Finding1_GroupTableAuthoritativeSpaceID(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g_external", ChannelType: common.ChannelTypeGroup.Uint8()},
		{ChannelID: "g_external____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	}
	// rawGroupSpaceMap = GetGroups 的结果（未经 effective rewrite）。
	rawGroupSpaceMap := map[string]string{
		"g_external": "spaceB",
	}
	// externalGroupMap = 当前 user 是外部成员（从 spaceA 加入 g_external）。
	externalGroupMap := map[string]string{
		"g_external": "spaceA",
	}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, "")

	// SpaceID 必须是 spaceB（群表权威值），不是 spaceA（effective rewrite 值）。
	assert.Equal(t, "spaceB", resps[0].SpaceID,
		"GROUP: SyncUserConversationResp.SpaceID 必须是群表权威值 spaceB，"+
			"不能是 SetEffectiveSpaceIDFromMap 改写后的 spaceA")
	assert.Equal(t, "spaceA", resps[0].MySourceSpaceID,
		"MySourceSpaceID 来自 externalGroupMap，是 spaceA")
	assert.NotEqual(t, resps[0].SpaceID, resps[0].MySourceSpaceID,
		"外部群场景下 SpaceID 与 MySourceSpaceID 必须不同——"+
			"客户端要据此区分'群在哪个 Space'与'我从哪个 Space 看到这个群'")

	assert.Equal(t, "spaceB", resps[1].SpaceID,
		"COMMUNITY_TOPIC: 父群 SpaceID 同样是权威值 spaceB")
	assert.Equal(t, "spaceA", resps[1].MySourceSpaceID,
		"thread 继承父群的 MySourceSpaceID")
}

// TestSpaceID_Round3_Finding1_GetGroupsNotEffectiveRewrite 通过 group.InfoResp
// 类型契约验证：GetGroups 返回的 SpaceID 字段直接来自群表行，没有 effective
// rewrite 路径。
//
// 这是上面单测在 stub 层的延伸：确认 stubGroupService.GetGroups 用 InfoResp
// 而非 GroupResp，意味着永远不会触发 SetEffectiveSpaceIDFromMap。
func TestSpaceID_Round3_Finding1_GetGroupsNotEffectiveRewrite(t *testing.T) {
	stub := &stubGroupService{
		spaces: map[string]string{
			"g_external": "spaceB", // 群表权威值
		},
	}
	infos, err := stub.GetGroups([]string{"g_external"})
	require.NoError(t, err)
	require.Len(t, infos, 1)

	// InfoResp 没有 IsExternalGroup 字段被 SetEffectiveSpaceID 利用的语义；
	// SpaceID 永远是群表原值。
	assert.Equal(t, "spaceB", infos[0].SpaceID,
		"GetGroups 返回 *InfoResp.SpaceID 永远是群表权威值")

	// 以此类型构建 rawGroupSpaceMap 是 Round-3 handler 修复的核心：
	rawGroupSpaceMap := map[string]string{}
	for _, g := range infos {
		rawGroupSpaceMap[g.GroupNo] = g.SpaceID
	}
	assert.Equal(t, "spaceB", rawGroupSpaceMap["g_external"])
}

// TestSpaceID_Round3_Finding2_FillConversation_EmptySourceFallback 验证：
//
// 当 externalGroupMap[groupNo] 存在但值为空串（旧外部成员行
// source_space_id=""）时，fillConversationSpaceIDs 必须把
// MySourceSpaceID 兜底为 defaultSpaceID（用户最早加入的 Space），
// 与 decideConvKeepInSpace（space_filter.go:171/234）同口径。
//
// 修复前：fillConversationSpaceIDs 直接 r.MySourceSpaceID = src，
// 客户端拿到空串 + omitempty → 字段缺失，无法判断这个外部群在哪个 Space 下可见。
func TestSpaceID_Round3_Finding2_FillConversation_EmptySourceFallback(t *testing.T) {
	resps := []*SyncUserConversationResp{
		// GROUP：source_space_id="" 兜底到 defaultSpaceID。
		{ChannelID: "g_legacy_ext", ChannelType: common.ChannelTypeGroup.Uint8()},
		// COMMUNITY_TOPIC：父群 source_space_id="" 同样兜底。
		{ChannelID: "g_legacy_ext____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
		// GROUP：source_space_id 非空，原值不变。
		{ChannelID: "g_new_ext", ChannelType: common.ChannelTypeGroup.Uint8()},
	}
	rawGroupSpaceMap := map[string]string{
		"g_legacy_ext": "spaceB",
		"g_new_ext":    "spaceB",
	}
	externalGroupMap := map[string]string{
		"g_legacy_ext": "",       // 旧外部成员行
		"g_new_ext":    "spaceA", // 正常外部成员行
	}
	defaultSpaceID := "spaceDefault" // 用户最早加入的 Space

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, defaultSpaceID)

	assert.Equal(t, "spaceDefault", resps[0].MySourceSpaceID,
		"GROUP: source_space_id='' 必须兜底到 defaultSpaceID")
	assert.Equal(t, "spaceB", resps[0].SpaceID,
		"SpaceID 不受 source_space_id 兜底影响")

	assert.Equal(t, "spaceDefault", resps[1].MySourceSpaceID,
		"COMMUNITY_TOPIC: 父群 source_space_id='' 同样兜底到 defaultSpaceID")
	assert.Equal(t, "spaceB", resps[1].SpaceID,
		"thread SpaceID 继承父群权威值")

	assert.Equal(t, "spaceA", resps[2].MySourceSpaceID,
		"非空 source_space_id 不被覆盖")
}

// TestSpaceID_Round3_Finding2_FillConversation_NoFallbackForNonExternal 验证：
//
// externalGroupMap 没记录的群（用户不是外部成员）不应被兜底到 defaultSpaceID。
// 兜底语义只针对"已登记为外部成员但 source_space_id 留空"的旧行。
func TestSpaceID_Round3_Finding2_FillConversation_NoFallbackForNonExternal(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g_internal", ChannelType: common.ChannelTypeGroup.Uint8()},
	}
	rawGroupSpaceMap := map[string]string{
		"g_internal": "spaceA",
	}
	externalGroupMap := map[string]string{} // 用户不是任何群的外部成员
	defaultSpaceID := "spaceDefault"

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, defaultSpaceID)

	assert.Equal(t, "", resps[0].MySourceSpaceID,
		"非外部成员：MySourceSpaceID 留空，不被兜底覆盖")
	assert.Equal(t, "spaceA", resps[0].SpaceID)
}

// TestSpaceID_Round3_Finding2_Sidebar_EmptySourceFallback 验证 sidebar
// builders（buildFollowItems / buildRecentItems / mergeThreadEntries）
// 同样对 source_space_id="" 兜底到 defaultSpaceID。
func TestSpaceID_Round3_Finding2_Sidebar_EmptySourceFallback(t *testing.T) {
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g_legacy_ext": {GroupNo: "g_legacy_ext", CategoryID: &cat},
	}
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g_legacy_ext", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: nowRecent()},
		{ChannelID: "g_legacy_ext____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: nowRecent()},
	}
	threadExtMap := map[string]*convext.Model{
		"g_legacy_ext____th1": {TargetID: "g_legacy_ext____th1", FollowSort: 0},
	}
	groupSpaceMap := map[string]string{"g_legacy_ext": "spaceB"}
	externalGroupMap := map[string]string{"g_legacy_ext": ""} // 旧外部成员行
	defaultSpaceID := "spaceDefault"

	t.Run("buildFollowItems", func(t *testing.T) {
		items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap, nil, nil,
			groupSpaceMap, externalGroupMap, defaultSpaceID)
		byID := map[string]*SidebarItem{}
		for _, it := range items {
			byID[it.TargetID] = it
		}
		require.Contains(t, byID, "g_legacy_ext")
		assert.Equal(t, "spaceDefault", byID["g_legacy_ext"].MySourceSpaceID,
			"follow tab GROUP: source='' 兜底到 defaultSpaceID")
		require.Contains(t, byID, "g_legacy_ext____th1")
		assert.Equal(t, "spaceDefault", byID["g_legacy_ext____th1"].MySourceSpaceID,
			"follow tab THREAD: 父群 source='' 兜底到 defaultSpaceID")
	})

	t.Run("buildRecentItems", func(t *testing.T) {
		items := buildRecentItems(convs, nil, groupSpaceMap, externalGroupMap, defaultSpaceID)
		byID := map[string]*SidebarItem{}
		for _, it := range items {
			byID[it.TargetID] = it
		}
		require.Contains(t, byID, "g_legacy_ext")
		assert.Equal(t, "spaceDefault", byID["g_legacy_ext"].MySourceSpaceID,
			"recent tab GROUP: source='' 兜底到 defaultSpaceID")
		require.Contains(t, byID, "g_legacy_ext____th1")
		assert.Equal(t, "spaceDefault", byID["g_legacy_ext____th1"].MySourceSpaceID,
			"recent tab THREAD: source='' 兜底到 defaultSpaceID")
	})

	t.Run("mergeThreadEntries", func(t *testing.T) {
		extRows := []*convext.Model{
			{TargetID: "g_legacy_ext____alive", FollowSort: 1},
		}
		result := mergeThreadEntries(nil, extRows,
			aliveThread("g_legacy_ext____alive", nil),
			categorySetting, nil, groupSpaceMap, externalGroupMap, defaultSpaceID)
		require.Len(t, result, 1)
		assert.Equal(t, "spaceDefault", result[0].MySourceSpaceID,
			"DB-only thread: 父群 source='' 兜底到 defaultSpaceID")
	})
}

// TestSpaceID_Round3_Finding2_DefaultSpaceIDEmpty_StaysEmpty 验证：
//
// defaultSpaceID 也为空（极端场景：用户没有任何 space_member 行）时，
// MySourceSpaceID 退化为空串——保持 omitempty 行为，与历史一致，
// 不会写入"垃圾值"。
func TestSpaceID_Round3_Finding2_DefaultSpaceIDEmpty_StaysEmpty(t *testing.T) {
	resps := []*SyncUserConversationResp{
		{ChannelID: "g_legacy_ext", ChannelType: common.ChannelTypeGroup.Uint8()},
	}
	rawGroupSpaceMap := map[string]string{"g_legacy_ext": "spaceB"}
	externalGroupMap := map[string]string{"g_legacy_ext": ""}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, externalGroupMap, "")

	assert.Equal(t, "", resps[0].MySourceSpaceID,
		"defaultSpaceID 也空时不写垃圾值，omitempty 让客户端拿不到字段")
}

// 编译期 sanity：确认 resolveMySourceSpaceID helper 行为。
// 单元测试覆盖三条分支：非空直返、空+default、空+空default。
func TestSpaceID_Round3_ResolveMySourceSpaceID(t *testing.T) {
	assert.Equal(t, "spaceA", resolveMySourceSpaceID("spaceA", "spaceDefault"),
		"non-empty source: 直接返回")
	assert.Equal(t, "spaceDefault", resolveMySourceSpaceID("", "spaceDefault"),
		"empty source: 兜底到 defaultSpaceID")
	assert.Equal(t, "", resolveMySourceSpaceID("", ""),
		"empty source + empty default: 退化为空串")
}

// TestSpaceID_Round3_SidebarMySourceSpaceID 覆盖 sidebar helper 三个分支。
func TestSpaceID_Round3_SidebarMySourceSpaceID(t *testing.T) {
	t.Run("not external member", func(t *testing.T) {
		got := sidebarMySourceSpaceID(map[string]string{}, "g1", "spaceDefault")
		assert.Equal(t, "", got, "非外部成员：返回空，不写 defaultSpaceID")
	})
	t.Run("nil map", func(t *testing.T) {
		got := sidebarMySourceSpaceID(nil, "g1", "spaceDefault")
		assert.Equal(t, "", got)
	})
	t.Run("non-empty source", func(t *testing.T) {
		got := sidebarMySourceSpaceID(map[string]string{"g1": "spaceA"}, "g1", "spaceDefault")
		assert.Equal(t, "spaceA", got, "非空 source: 直接返回")
	})
	t.Run("empty source falls back", func(t *testing.T) {
		got := sidebarMySourceSpaceID(map[string]string{"g1": ""}, "g1", "spaceDefault")
		assert.Equal(t, "spaceDefault", got, "空 source: 兜底 defaultSpaceID")
	})
	t.Run("empty source and empty default", func(t *testing.T) {
		got := sidebarMySourceSpaceID(map[string]string{"g1": ""}, "g1", "")
		assert.Equal(t, "", got, "都空：退化空串")
	})
}

// 防止 unused import: group。这里通过引用 InfoResp 类型让 import 有意义。
var _ = (*group.InfoResp)(nil)
