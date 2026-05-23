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

// TestSpaceID_Round2_Finding1_ThreadParentPrefetched 验证 syncUserConversation
// 在构建 groupNos 预取列表时，把 COMMUNITY_TOPIC 频道解析出的父群 groupNo 也合入
// 集合（GH octo-server#153 Round-2 Critical 1）。
//
// 修复前：sync 批次只含 thread "g1____th1" 而父群 g1 没出现在 IM 返回里时，
// groupMap 只会缓存 "g1____th1" 的 GetGroupDetails 结果（实际拿不到任何东西），
// fillConversationSpaceIDs 走 thread 分支查 groupMap["g1"] 是 miss → space_id
// 被回填为空。
//
// 这里用 fillConversationSpaceIDs 的契约来验证：只要 groupMap 包含父群 g1，
// space_id 就被正确回填。Round-2 修复确保 syncUserConversation 在调
// GetGroupDetails 之前就把 g1 加入 groupNos，从而让 groupMap 真的包含 g1。
func TestSpaceID_Round2_Finding1_ThreadParentPrefetched(t *testing.T) {
	// 模拟 Round-2 修复后的状态：thread 的父群已被预取并写入 rawGroupSpaceMap，
	// 即便 IM 批次没把父群作为独立 conversation 返回。
	resps := []*SyncUserConversationResp{
		{ChannelID: "g1____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8()},
	}
	rawGroupSpaceMap := map[string]string{
		"g1": "spaceA",
	}

	fillConversationSpaceIDs(resps, rawGroupSpaceMap, nil, "")

	assert.Equal(t, "spaceA", resps[0].SpaceID,
		"thread 的 SpaceID 应继承父群——前提是父群已被预取到 rawGroupSpaceMap")
}

// TestSpaceID_Round2_Finding2_DBOnlyThreadParentInGroupSpaceMap 验证 sidebar
// follow tab 下，DB-only thread（IM 没返回）的父群虽不在 IM conversations 里，
// 仍能通过 CollectGroupSpaceMap 的 extraGroupNos 入参参与 group service 查询，
// 最终 groupSpaceMap 里包含该父群（GH octo-server#153 Round-2 Critical 2）。
//
// 这里用 CollectGroupSpaceMap 直接验证契约：传入空 conversations + 父群作为
// extra → fakeService 仍然被调到 → 返回 map 包含父群。
func TestSpaceID_Round2_Finding2_DBOnlyThreadParentInGroupSpaceMap(t *testing.T) {
	// IM 返回里没有任何 conversation —— 所有 thread 都是 DB-only。
	conversations := []*config.SyncUserConversationResp{}
	extraParentGroupNos := []string{"g_db_parent"}

	stub := &stubGroupService{
		spaces: map[string]string{"g_db_parent": "spaceX"},
	}

	m, ok := CollectGroupSpaceMap(conversations, extraParentGroupNos, stub)

	require.True(t, ok, "fake group service 不应失败")
	assert.Equal(t, "spaceX", m["g_db_parent"],
		"DB-only thread 父群必须出现在 groupSpaceMap，否则 mergeThreadEntries 拿不到 space_id")
	assert.Equal(t, []string{"g_db_parent"}, stub.lastCall,
		"父群必须真的传给 group service 查询")
}

// TestSpaceID_Round2_Finding2_DBOnlyThreadInheritsParentSpace 端到端验证：
// CollectGroupSpaceMap → groupSpaceMap → mergeThreadEntries → SidebarItem.SpaceID。
func TestSpaceID_Round2_Finding2_DBOnlyThreadInheritsParentSpace(t *testing.T) {
	conversations := []*config.SyncUserConversationResp{}
	extRows := []*convext.Model{
		{TargetID: "g_db_parent____th9", FollowSort: 1},
	}
	extra := uniqueThreadParentGroupNos(extRows)
	stub := &stubGroupService{
		spaces: map[string]string{"g_db_parent": "spaceX"},
	}

	groupSpaceMap, ok := CollectGroupSpaceMap(conversations, extra, stub)
	require.True(t, ok)

	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g_db_parent": {GroupNo: "g_db_parent", CategoryID: &cat},
	}

	items := mergeThreadEntries(nil, extRows, aliveThread("g_db_parent____th9", nil),
		categorySetting, nil, groupSpaceMap, nil, "")

	require.Len(t, items, 1)
	assert.Equal(t, "spaceX", items[0].SpaceID,
		"DB-only thread 的 space_id 必须从父群继承，即便父群本批不在 IM 返回里")
}

// TestSpaceID_Round2_Finding3_BuildFollowItems_ExternalGroupMySource 验证
// follow tab 外部群 SidebarItem 的 my_source_space_id 字段被正确填充
// （GH octo-server#153 Round-2 P1）。
func TestSpaceID_Round2_Finding3_BuildFollowItems_ExternalGroupMySource(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g_internal", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1000},
		{ChannelID: "g_external", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1100},
		{ChannelID: "g_external____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 1200},
	}
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g_internal": {GroupNo: "g_internal", CategoryID: &cat},
		"g_external": {GroupNo: "g_external", CategoryID: &cat},
	}
	threadExtMap := map[string]*convext.Model{
		"g_external____th1": {TargetID: "g_external____th1", FollowSort: 5},
	}
	groupSpaceMap := map[string]string{
		"g_internal": "spaceA",
		"g_external": "spaceB",
	}
	externalGroupMap := map[string]string{
		// 当前用户从 spaceA 加入了 g_external（外部群成员）。
		"g_external": "spaceA",
	}

	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap, nil, nil, groupSpaceMap, externalGroupMap, "")

	byID := map[string]*SidebarItem{}
	for _, it := range items {
		byID[it.TargetID] = it
	}

	require.Contains(t, byID, "g_internal")
	assert.Equal(t, "spaceA", byID["g_internal"].SpaceID, "internal group: SpaceID 来自 group 表")
	assert.Equal(t, "", byID["g_internal"].MySourceSpaceID, "non-external group: my_source_space_id 留空")

	require.Contains(t, byID, "g_external")
	assert.Equal(t, "spaceB", byID["g_external"].SpaceID, "external group: SpaceID 仍是群本身的 spaceB")
	assert.Equal(t, "spaceA", byID["g_external"].MySourceSpaceID,
		"external group: my_source_space_id = 用户加入时的 source space")

	require.Contains(t, byID, "g_external____th1")
	assert.Equal(t, "spaceB", byID["g_external____th1"].SpaceID, "thread 继承父群 spaceB")
	assert.Equal(t, "spaceA", byID["g_external____th1"].MySourceSpaceID,
		"thread 同样继承父群的 my_source_space_id")
}

// TestSpaceID_Round2_Finding3_BuildRecentItems_ExternalGroupMySource recent tab
// 同样要回填 my_source_space_id。
func TestSpaceID_Round2_Finding3_BuildRecentItems_ExternalGroupMySource(t *testing.T) {
	now := nowRecent()
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "g_external", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: now},
		{ChannelID: "g_external____th1", ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: now},
		{ChannelID: "u1", ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: now},
	}
	groupSpaceMap := map[string]string{"g_external": "spaceB"}
	externalGroupMap := map[string]string{"g_external": "spaceA"}

	items := buildRecentItems(convs, nil, groupSpaceMap, externalGroupMap, "")

	byID := map[string]*SidebarItem{}
	for _, it := range items {
		byID[it.TargetID] = it
	}

	require.Contains(t, byID, "g_external")
	assert.Equal(t, "spaceA", byID["g_external"].MySourceSpaceID, "external group recent tab")
	require.Contains(t, byID, "g_external____th1")
	assert.Equal(t, "spaceA", byID["g_external____th1"].MySourceSpaceID, "thread 继承 my_source_space_id")
	require.Contains(t, byID, "u1")
	assert.Equal(t, "", byID["u1"].MySourceSpaceID, "PERSON: my_source_space_id 留空")
}

// TestSpaceID_Round2_Finding3_MergeThreadEntries_ExternalGroupMySource DB-only
// thread 也要继承父群的 my_source_space_id。
func TestSpaceID_Round2_Finding3_MergeThreadEntries_ExternalGroupMySource(t *testing.T) {
	cat := "cat-A"
	categorySetting := map[string]*GroupCategorySetting{
		"g_external": {GroupNo: "g_external", CategoryID: &cat},
	}
	threadExtRows := []*convext.Model{
		{TargetID: "g_external____alive", FollowSort: 1},
	}
	groupSpaceMap := map[string]string{"g_external": "spaceB"}
	externalGroupMap := map[string]string{"g_external": "spaceA"}

	result := mergeThreadEntries(nil, threadExtRows,
		aliveThread("g_external____alive", nil),
		categorySetting, nil, groupSpaceMap, externalGroupMap, "")

	require.Len(t, result, 1)
	assert.Equal(t, "spaceB", result[0].SpaceID)
	assert.Equal(t, "spaceA", result[0].MySourceSpaceID,
		"DB-only thread 必须从父群继承 my_source_space_id")
}

// stubGroupService 是 group.IService 的最小实现，仅支持 GetGroups。
// 通过嵌入 group.IService 来满足整个接口签名，未实现的方法被调用时会触发
// nil pointer panic — 对单元测试足够。
type stubGroupService struct {
	group.IService
	spaces   map[string]string
	lastCall []string
}

func (s *stubGroupService) GetGroups(groupNos []string) ([]*group.InfoResp, error) {
	s.lastCall = append([]string(nil), groupNos...)
	out := make([]*group.InfoResp, 0, len(groupNos))
	for _, no := range groupNos {
		sid, ok := s.spaces[no]
		if !ok {
			continue
		}
		out = append(out, &group.InfoResp{GroupNo: no, SpaceID: sid})
	}
	return out, nil
}
