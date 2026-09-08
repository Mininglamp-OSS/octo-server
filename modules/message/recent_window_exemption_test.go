package message

// =============================================================================
// 不活跃窗口的豁免规则（task inactive-hiding-user-control / P2）
//
// 窗口是「这个会话安静了，从我眼前淡出」的每人视图投影。四类东西不得被它淡出：
// 真实未读、人工未读、置顶、系统 Bot。前三者是「把窗口交给用户自己设」（Batch 2）的安全前提 ——
// 一个能吞掉未读的窗口不能交到用户手里；第四类保证系统 Bot 在每个 Space 可达。
//
// 纯逻辑测试：keepDespiteRecentWindow 与两个端点的过滤函数，无需 DB / IM。
// =============================================================================

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// keepDespiteRecentWindow —— 谓词本身
// ---------------------------------------------------------------------------

func TestKeepDespiteRecentWindow_Unread(t *testing.T) {
	assert.True(t, keepDespiteRecentWindow("g1", common.ChannelTypeGroup.Uint8(), 1, false),
		"有未读必须豁免：隐藏会话等于连未读消息一起藏走，用户无从察觉")
	assert.False(t, keepDespiteRecentWindow("g1", common.ChannelTypeGroup.Uint8(), 0, false),
		"无未读且未置顶 → 正常参与窗口判定")
}

func TestKeepDespiteRecentWindow_Pinned(t *testing.T) {
	assert.True(t, keepDespiteRecentWindow("g1", common.ChannelTypeGroup.Uint8(), 0, true),
		"置顶是关于位置的显式声明，时间窗口不得推翻它")
}

func TestKeepDespiteRecentWindow_SystemBotDM(t *testing.T) {
	bots := spacepkg.SystemBotList()
	if len(bots) == 0 {
		t.Skip("该部署未配置系统 Bot")
	}
	assert.True(t, keepDespiteRecentWindow(bots[0], common.ChannelTypePerson.Uint8(), 0, false),
		"系统 Bot 占位 Timestamp/Unread 均为 0，person 窗口一旦开启就会被丢；必须豁免")
	assert.False(t, keepDespiteRecentWindow(bots[0], common.ChannelTypeGroup.Uint8(), 0, false),
		"豁免只对 Person 频道成立，不得因 channelID 巧合放行群聊")
}

func TestKeepDespiteRecentWindow_OrdinaryDMNotExempt(t *testing.T) {
	assert.False(t, keepDespiteRecentWindow("some-ordinary-uid", common.ChannelTypePerson.Uint8(), 0, false),
		"普通 DM 不豁免——否则 person 窗口配置将完全失效")
}

// ---------------------------------------------------------------------------
// /v1/conversation/sync —— filterRecentConversations
// ---------------------------------------------------------------------------

func makeSyncRespUnread(channelID string, channelType uint8, ts int64, unread int) *SyncUserConversationResp {
	r := makeSyncResp(channelID, channelType, ts)
	r.Unread = unread
	return r
}

func TestFilterRecentConversations_StaleButUnreadKept(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncRespUnread("g-stale-unread", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 5),
		makeSyncRespUnread("g-stale-read", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs(), nil)
	assert.Equal(t, []string{"g-stale-unread"}, channelIDs(got),
		"超窗但有未读必须保留，超窗且已读才隐藏")
}

func TestFilterRecentConversations_StaleButPinnedKept(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncResp("g-stale-pinned", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
		makeSyncResp("g-stale-plain", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
	}
	pinned := map[string]struct{}{
		channelKey("g-stale-pinned", common.ChannelTypeGroup.Uint8()): {},
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs(), pinned)
	assert.Equal(t, []string{"g-stale-pinned"}, channelIDs(got))
}

func TestFilterRecentConversations_StaleButManualUnreadKept(t *testing.T) {
	manual := makeSyncResp("g-stale-manual", common.ChannelTypeGroup.Uint8(), now3DaysAgo())
	manual.Extra = &conversationExtraResp{ManualUnread: true}
	plain := makeSyncResp("g-stale-plain", common.ChannelTypeGroup.Uint8(), now3DaysAgo())

	got := filterRecentConversations([]*SyncUserConversationResp{manual, plain}, defaultRecentCutoffs(), nil)

	assert.Equal(t, []string{"g-stale-manual"}, channelIDs(got))
}

// 置顶集合为空（例如置顶查询失败降级）时不得 panic，且未读豁免仍然生效 ——
// 两个端点的降级方向必须一致。
func TestFilterRecentConversations_NilPinnedSetStillHonoursUnread(t *testing.T) {
	resps := []*SyncUserConversationResp{
		makeSyncRespUnread("g-stale-unread", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 1),
	}
	got := filterRecentConversations(resps, defaultRecentCutoffs(), nil)
	assert.Equal(t, []string{"g-stale-unread"}, channelIDs(got))
}

func TestFilterRecentConversations_SystemBotSurvivesPersonWindow(t *testing.T) {
	bots := spacepkg.SystemBotList()
	if len(bots) == 0 {
		t.Skip("该部署未配置系统 Bot")
	}
	cutoffs := defaultRecentCutoffs()
	cutoffs.person = daysCutoff(time.Now(), 1) // 显式打开 DM 窗口

	resps := []*SyncUserConversationResp{
		// 占位条目：Timestamp 0 / Unread 0，正是 EnsureSystemBotsPresent 产出的形状
		makeSyncResp(bots[0], common.ChannelTypePerson.Uint8(), 0),
		makeSyncResp("quiet-human", common.ChannelTypePerson.Uint8(), 0),
	}
	got := filterRecentConversations(resps, cutoffs, nil)
	assert.Equal(t, []string{bots[0]}, channelIDs(got),
		"系统 Bot 占位必须穿透 person 窗口，普通安静 DM 则应被过滤")
}

// ---------------------------------------------------------------------------
// /v1/sidebar/sync —— buildRecentItems 走同一谓词
// ---------------------------------------------------------------------------

func makeRawConv(channelID string, channelType uint8, ts int64, unread int) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   ts,
		Unread:      unread,
	}
}

func TestBuildRecentItems_PinnedSurvivesWindow(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeRawConv("g-stale-pinned", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
		makeRawConv("g-stale-plain", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	pinned := map[string]struct{}{
		channelKey("g-stale-pinned", common.ChannelTypeGroup.Uint8()): {},
	}
	items := buildRecentItems(convs, defaultRecentCutoffs(), pinned, nil, nil, nil, "")

	require.Len(t, items, 1, "置顶条目必须存活，未置顶的陈旧群应被过滤")
	assert.Equal(t, "g-stale-pinned", items[0].TargetID)
	assert.True(t, items[0].IsPinned,
		"回归防护：置顶判定必须发生在窗口判定之前，否则条目在被标记 IsPinned 前就已丢弃")
}

func TestBuildRecentItems_UnreadSurvivesWindow(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeRawConv("g-stale-unread", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 3),
		makeRawConv("g-stale-read", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	items := buildRecentItems(convs, defaultRecentCutoffs(), nil, nil, nil, nil, "")

	require.Len(t, items, 1)
	assert.Equal(t, "g-stale-unread", items[0].TargetID)
	assert.Equal(t, 3, items[0].Unread, "未读数必须原样透出，不能因豁免路径丢字段")
}

func TestRecentWindowBuildItems_ManualUnreadSurvivesWindow(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeRawConv("g-stale-manual", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
		makeRawConv("g-stale-plain", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	manualUnreadSet := map[string]struct{}{
		channelKey("g-stale-manual", common.ChannelTypeGroup.Uint8()): {},
	}

	items := buildRecentItems(convs, defaultRecentCutoffs(), nil, manualUnreadSet, nil, nil, "")

	require.Len(t, items, 1)
	assert.Equal(t, "g-stale-manual", items[0].TargetID)
	assert.True(t, items[0].ManualUnread)
}

// 两个端点在同一输入上必须给出同一可见集合 —— 本 task 的核心不变量。
func TestRecentWindow_BothEndpointsAgree(t *testing.T) {
	cutoffs := defaultRecentCutoffs()
	pinnedID := "g-pinned"
	pinned := map[string]struct{}{
		channelKey(pinnedID, common.ChannelTypeGroup.Uint8()): {},
	}

	rawConvs := []*config.SyncUserConversationResp{
		makeRawConv("g-fresh", common.ChannelTypeGroup.Uint8(), nowRecent(), 0),
		makeRawConv("g-stale", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
		makeRawConv("g-stale-unread", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 2),
		makeRawConv(pinnedID, common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	respConvs := []*SyncUserConversationResp{
		makeSyncRespUnread("g-fresh", common.ChannelTypeGroup.Uint8(), nowRecent(), 0),
		makeSyncRespUnread("g-stale", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
		makeSyncRespUnread("g-stale-unread", common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 2),
		makeSyncRespUnread(pinnedID, common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0),
	}
	manualID := "g-stale-manual"
	rawConvs = append(rawConvs, makeRawConv(manualID, common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0))
	manualResp := makeSyncRespUnread(manualID, common.ChannelTypeGroup.Uint8(), now3DaysAgo(), 0)
	manualResp.Extra = &conversationExtraResp{ManualUnread: true}
	respConvs = append(respConvs, manualResp)
	manualUnreadSet := map[string]struct{}{
		channelKey(manualID, common.ChannelTypeGroup.Uint8()): {},
	}

	items := buildRecentItems(rawConvs, cutoffs, pinned, manualUnreadSet, nil, nil, "")
	sidebarIDs := make([]string, 0, len(items))
	for _, it := range items {
		sidebarIDs = append(sidebarIDs, it.TargetID)
	}

	convIDs := channelIDs(filterRecentConversations(respConvs, cutoffs, pinned))

	assert.Equal(t, sidebarIDs, convIDs,
		"/v1/sidebar/sync 与 /v1/conversation/sync 的窗口可见集合必须逐条一致")
}
