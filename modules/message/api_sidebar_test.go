package message

// =============================================================================
// Sidebar API — unit tests (RED → GREEN)
//
// These tests exercise the pure-logic functions extracted from Sidebar.Sync:
//   - buildFollowItemsFromIM   (follow-tab: IM conversations → SidebarItem slice)
//   - buildRecentItemsFromIM   (recent-tab: IM conversations → SidebarItem slice)
//   - mergeThreadEntries       (append thread entries not in IM result)
//   - sortFollowItems / sortRecentItems
//   - validateSidebarRequest
//
// Integration-level HTTP tests are kept thin: only two cases (happy + bad-req)
// to avoid needing a running IM core.
// =============================================================================

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeIMConv(channelID string, channelType uint8, ts int64) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   ts,
		Unread:      0,
	}
}

// now3DaysAgo returns a unix timestamp 3+ days in the past (stale)
func now3DaysAgo() int64 { return time.Now().Add(-73 * time.Hour).Unix() }

// nowRecent returns a unix timestamp well within the 3-day window
func nowRecent() int64 { return time.Now().Add(-1 * time.Hour).Unix() }

// ---------------------------------------------------------------------------
// validateSidebarRequest
// ---------------------------------------------------------------------------

func TestValidateSidebarRequest_MissingTab(t *testing.T) {
	req := &sidebarSyncReq{Tab: "", DeviceUUID: "dev-1"}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tab")
}

func TestValidateSidebarRequest_InvalidTab(t *testing.T) {
	req := &sidebarSyncReq{Tab: "unknown", DeviceUUID: "dev-1"}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tab")
}

func TestValidateSidebarRequest_MissingDeviceUUID(t *testing.T) {
	req := &sidebarSyncReq{Tab: "follow", DeviceUUID: ""}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_uuid")
}

func TestValidateSidebarRequest_Valid_Follow(t *testing.T) {
	req := &sidebarSyncReq{Tab: "follow", DeviceUUID: "dev-1"}
	assert.NoError(t, validateSidebarRequest(req))
}

func TestValidateSidebarRequest_Valid_Recent(t *testing.T) {
	req := &sidebarSyncReq{Tab: "recent", DeviceUUID: "dev-1"}
	assert.NoError(t, validateSidebarRequest(req))
}

func TestValidateSidebarRequest_NegativeVersion(t *testing.T) {
	req := &sidebarSyncReq{Tab: "recent", DeviceUUID: "dev-1", Version: -1}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestValidateSidebarRequest_NegativeMsgCount(t *testing.T) {
	req := &sidebarSyncReq{Tab: "recent", DeviceUUID: "dev-1", MsgCount: -1}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "msg_count")
}

func TestValidateSidebarRequest_MsgCountTooLarge(t *testing.T) {
	req := &sidebarSyncReq{Tab: "recent", DeviceUUID: "dev-1", MsgCount: maxMsgCount + 1}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "msg_count")
}

func TestValidateSidebarRequest_DeviceUUIDTooLong(t *testing.T) {
	req := &sidebarSyncReq{Tab: "recent", DeviceUUID: strings.Repeat("a", maxDeviceUUIDLen+1)}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_uuid")
}

func TestValidateSidebarRequest_LastMsgSeqsTooLong(t *testing.T) {
	req := &sidebarSyncReq{
		Tab:         "recent",
		DeviceUUID:  "dev-1",
		LastMsgSeqs: strings.Repeat("a", maxLastMsgSeqsLen+1),
	}
	err := validateSidebarRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last_msg_seqs")
}

// ---------------------------------------------------------------------------
// buildFollowItems — follow tab filtering
// ---------------------------------------------------------------------------

func TestBuildFollowItems_GroupFollowed(t *testing.T) {
	// group g1 has a category and is NOT unfollowed → should appear
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	unfollowedGroups := map[string]struct{}{} // empty: not unfollowed

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil, nil, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "g1", items[0].TargetID)
	assert.Equal(t, "cat1", *items[0].CategoryID)
	assert.Equal(t, 1, items[0].CategorySort)
}

func TestBuildFollowItems_GroupUnfollowed_Excluded(t *testing.T) {
	// group g1 is blacklisted (group_unfollowed=1) → excluded from follow tab
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	unfollowedGroups := map[string]struct{}{"g1": {}}

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil, nil, nil)
	assert.Len(t, items, 0)
}

func TestBuildFollowItems_GroupWithoutCategory_Excluded(t *testing.T) {
	// group g1 has no category → not in follow set
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{} // no entry
	unfollowedGroups := map[string]struct{}{}

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil, nil, nil)
	assert.Len(t, items, 0)
}

func TestBuildFollowItems_DMFollowed(t *testing.T) {
	// DM peer1 is followed_dm=1 → should appear
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 5},
	}

	items := buildFollowItems(convs, nil, nil, followedDMs, nil, nil, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "peer1", items[0].TargetID)
	assert.Equal(t, 5, items[0].FollowSort)
}

func TestBuildFollowItems_DMNotFollowed_Excluded(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer2", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	followedDMs := map[string]*convext.Model{} // no entry for peer2

	items := buildFollowItems(convs, nil, nil, followedDMs, nil, nil, nil)
	assert.Len(t, items, 0)
}

func TestBuildFollowItems_ThreadAsIMEntry_IncludedWhenParentFollowed(t *testing.T) {
	// Thread "g1____th1" from IM; parent g1 is in follow set; thread has ext row → include
	threadChannelID := "g1____th1"
	convs := []*config.SyncUserConversationResp{
		makeIMConv(threadChannelID, common.ChannelTypeCommunityTopic.Uint8(), nowRecent()),
	}
	// parent group in follow set (has category, not unfollowed)
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	threadExtMap := map[string]*convext.Model{
		threadChannelID: {TargetID: threadChannelID, FollowSort: 2},
	}

	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap, nil, nil)
	require.Len(t, items, 1)
	assert.Equal(t, threadChannelID, items[0].TargetID)
	assert.Equal(t, int(common.ChannelTypeCommunityTopic), items[0].TargetType)
}

func TestBuildFollowItems_ThreadWithoutExtRow_Excluded(t *testing.T) {
	// Thread from IM; parent g1 is followed; but NO ext row for this thread → excluded
	threadChannelID := "g1____th2"
	convs := []*config.SyncUserConversationResp{
		makeIMConv(threadChannelID, common.ChannelTypeCommunityTopic.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	threadExtMap := map[string]*convext.Model{} // no ext for this thread

	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap, nil, nil)
	assert.Len(t, items, 0)
}

// ---------------------------------------------------------------------------
// buildRecentItems — recent tab filtering
// ---------------------------------------------------------------------------

func TestBuildRecentItems_GroupWithinWindow_Included(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	items := buildRecentItems(convs, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "g1", items[0].TargetID)
}

func TestBuildRecentItems_GroupOutsideWindow_Excluded(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), now3DaysAgo()),
	}
	items := buildRecentItems(convs, nil)
	assert.Len(t, items, 0)
}

func TestBuildRecentItems_DMAlwaysIncluded(t *testing.T) {
	// DMs are not subject to the 3-day window
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), now3DaysAgo()),
	}
	items := buildRecentItems(convs, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "peer1", items[0].TargetID)
}

func TestBuildRecentItems_ThreadWithinWindow_Included(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1____th1", common.ChannelTypeCommunityTopic.Uint8(), nowRecent()),
	}
	items := buildRecentItems(convs, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "g1____th1", items[0].TargetID)
}

func TestBuildRecentItems_ThreadOutsideWindow_Excluded(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1____th1", common.ChannelTypeCommunityTopic.Uint8(), now3DaysAgo()),
	}
	items := buildRecentItems(convs, nil)
	assert.Len(t, items, 0)
}

func TestBuildRecentItems_PinnedFirst(t *testing.T) {
	pinnedSet := map[string]struct{}{
		channelKey("g2", common.ChannelTypeGroup.Uint8()): {},
	}
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
		makeIMConv("g2", common.ChannelTypeGroup.Uint8(), nowRecent()-10),
	}
	items := buildRecentItems(convs, pinnedSet)
	require.Len(t, items, 2)

	// buildRecentItems sets IsPinned flag; sorting is done separately
	var pinnedItem *SidebarItem
	for _, it := range items {
		if it.TargetID == "g2" {
			pinnedItem = it
			break
		}
	}
	require.NotNil(t, pinnedItem)
	assert.True(t, pinnedItem.IsPinned)

	// After sort, pinned item is first
	sortRecentItems(items)
	assert.Equal(t, "g2", items[0].TargetID)
}

// ---------------------------------------------------------------------------
// mergeThreadEntries — append standalone thread entries not in IM result
// ---------------------------------------------------------------------------

// followedG1 is the standard "g1 is followed" set used in mergeThreadEntries tests.
func followedG1() (map[string]*GroupCategorySetting, map[string]struct{}) {
	cat := "cat-1"
	return map[string]*GroupCategorySetting{
			"g1": {GroupNo: "g1", CategoryID: &cat, CategorySort: 1, CategoryGroupSort: 1},
		},
		map[string]struct{}{}
}

// aliveThread builds a lastMsgAtMap entry for a thread channel ID.
// Helper for tests written after the round-3 follow-up which require ext rows
// to be "alive" (present in the active-thread map) to be emitted.
func aliveThread(channelID string, lastMsgAt *time.Time) map[string]*time.Time {
	return map[string]*time.Time{channelID: lastMsgAt}
}

func TestMergeThreadEntries_AddsNewEntry(t *testing.T) {
	existing := []*SidebarItem{
		{TargetID: "g1____th1", TargetType: int(common.ChannelTypeCommunityTopic)},
	}
	// th2 has ext row but is NOT in IM result
	th2ChannelID := "g1____th2"
	threadExtRows := []*convext.Model{
		{TargetID: "g1____th1", FollowSort: 1},
		{TargetID: th2ChannelID, FollowSort: 2},
	}
	t30 := time.Now().Add(-30 * time.Minute)
	lastMsgAtMap := map[string]*time.Time{
		"g1____th1":  &t30, // both must be alive even though th1 is dedup'd via existing
		th2ChannelID: &t30,
	}

	cat, unfollowed := followedG1()
	result := mergeThreadEntries(existing, threadExtRows, lastMsgAtMap, cat, unfollowed)
	require.Len(t, result, 2)
	ids := []string{result[0].TargetID, result[1].TargetID}
	assert.Contains(t, ids, th2ChannelID)
}

func TestMergeThreadEntries_NoDuplicateIfAlreadyPresent(t *testing.T) {
	existing := []*SidebarItem{
		{TargetID: "g1____th1", TargetType: int(common.ChannelTypeCommunityTopic)},
	}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____th1", FollowSort: 1}, // already present
	}
	cat, unfollowed := followedG1()
	// nil map is fine here: presentIDs short-circuits before the alive check.
	result := mergeThreadEntries(existing, threadExtRows, nil, cat, unfollowed)
	assert.Len(t, result, 1) // no duplicate
}

func TestMergeThreadEntries_EmptyExt(t *testing.T) {
	existing := []*SidebarItem{}
	cat, unfollowed := followedG1()
	result := mergeThreadEntries(existing, nil, nil, cat, unfollowed)
	assert.Len(t, result, 0)
}

// PR review follow-up: ext 行存在但目标 thread 已删除 / 不存在（不在 lastMsgAtMap）
// 必须跳过，不能把 timestamp=0 的幽灵 thread emit 给客户端。
func TestMergeThreadEntries_SkipWhenThreadDeleted(t *testing.T) {
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____ghost", FollowSort: 1},
	}
	cat, unfollowed := followedG1()
	// lastMsgAtMap 为空（thread 已被删除，cleanup 还没清理 ext 行）
	result := mergeThreadEntries(existing, threadExtRows, map[string]*time.Time{}, cat, unfollowed)
	assert.Len(t, result, 0,
		"thread 已删除时 ext 行必须被 skip，避免 ghost 条目出现在 follow tab")
}

// PR review follow-up: 父群相同但 short_id 跨群冲突的旧 bug 已通过复合键关闭，
// 这里同时验证 alive thread emit + alive timestamp 正确读取。
func TestMergeThreadEntries_AliveThreadEmitsTimestamp(t *testing.T) {
	now := time.Now().Add(-10 * time.Minute)
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____alive", FollowSort: 3},
	}
	cat, unfollowed := followedG1()
	result := mergeThreadEntries(existing, threadExtRows, aliveThread("g1____alive", &now), cat, unfollowed)
	require.Len(t, result, 1)
	assert.Equal(t, now.Unix(), result[0].Timestamp,
		"alive thread 的 timestamp 必须从 lastMsgAtMap 正确读取")
	assert.Equal(t, "g1", result[0].ParentChannelID)
	assert.Equal(t, 3, result[0].FollowSort)
}

// PR review follow-up: 活跃 thread 但 last_message_at NULL（新建未发消息）→ ts=0 但仍 emit。
func TestMergeThreadEntries_AliveThreadNilLastMsgAt(t *testing.T) {
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____fresh", FollowSort: 1},
	}
	cat, unfollowed := followedG1()
	// 键存在但值为 nil = thread 活跃但还没消息
	result := mergeThreadEntries(existing, threadExtRows, aliveThread("g1____fresh", nil), cat, unfollowed)
	require.Len(t, result, 1, "活跃 thread 即便 last_message_at=NULL 也必须 emit")
	assert.Equal(t, int64(0), result[0].Timestamp)
}

// PR review (Round 3) Blocking #4: DB-only thread entries must respect the same
// parent-follow predicate that buildFollowItems applies to IM-returned threads.
// If the parent group is unfollowed, the standalone thread must NOT surface.
func TestMergeThreadEntries_SkipWhenParentUnfollowed(t *testing.T) {
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "g1____th-orphan", FollowSort: 1},
	}
	cat, _ := followedG1()
	unfollowed := map[string]struct{}{"g1": {}}

	result := mergeThreadEntries(existing, threadExtRows, aliveThread("g1____th-orphan", nil), cat, unfollowed)
	assert.Len(t, result, 0,
		"thread whose parent group is unfollowed must NOT be merged into follow tab")
}

// PR review (Round 3) Blocking #4 — companion: parent group with no category
// (i.e. not in the follow set) → thread must be filtered.
func TestMergeThreadEntries_SkipWhenParentHasNoCategory(t *testing.T) {
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "g-noCat____th-orphan", FollowSort: 1},
	}
	cat, unfollowed := followedG1() // only g1 is in the follow set, g-noCat is not

	result := mergeThreadEntries(existing, threadExtRows, aliveThread("g-noCat____th-orphan", nil), cat, unfollowed)
	assert.Len(t, result, 0,
		"thread whose parent group lacks a category (not in follow set) must NOT be merged")
}

// PR review (Round 3) Blocking #4 — malformed thread channel ID (no separator)
// must be skipped silently rather than appended with an empty parent.
func TestMergeThreadEntries_SkipMalformedChannelID(t *testing.T) {
	existing := []*SidebarItem{}
	threadExtRows := []*convext.Model{
		{TargetID: "no-separator-here", FollowSort: 1},
	}
	cat, unfollowed := followedG1()

	result := mergeThreadEntries(existing, threadExtRows, nil, cat, unfollowed)
	assert.Len(t, result, 0,
		"malformed thread channel id (no separator) must be skipped")
}

// ---------------------------------------------------------------------------
// sortFollowItems
// ---------------------------------------------------------------------------

func TestSortFollowItems_CategorySort_Then_FollowSort(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "g3", CategorySort: 2, FollowSort: 1},
		{TargetID: "g1", CategorySort: 1, FollowSort: 2},
		{TargetID: "g2", CategorySort: 1, FollowSort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "g2", items[0].TargetID)
	assert.Equal(t, "g1", items[1].TargetID)
	assert.Equal(t, "g3", items[2].TargetID)
}

func TestSortFollowItems_PinnedBeforeFollowSort(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "g1", CategorySort: 1, FollowSort: 1, IsPinned: false},
		{TargetID: "g2", CategorySort: 1, FollowSort: 2, IsPinned: true},
	}
	sortFollowItems(items)
	// pinned takes precedence within same category
	assert.Equal(t, "g2", items[0].TargetID)
}

func TestSortFollowItems_NoCategoryNilID_ZeroSort(t *testing.T) {
	// items without CategoryID (nil) should sort by CategorySort=0 (zero)
	items := []*SidebarItem{
		{TargetID: "dm1", CategorySort: 0, FollowSort: 3},
		{TargetID: "dm2", CategorySort: 0, FollowSort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "dm2", items[0].TargetID)
}

// PR #21 review (lml2468 blocker #3) regression：CategorySort（来自
// group_category.sort）是 primary key；同一 category 内由 intraCategorySort
// （来自 group_setting.category_sort）二级 sort 决定群之间顺序。
func TestSortFollowItems_CategoryGroupSort_PrimaryOverIntra(t *testing.T) {
	items := []*SidebarItem{
		// 同 CategoryGroupSort（CategorySort=1）下 intraCategorySort 排序
		{TargetID: "g1", CategorySort: 1, intraCategorySort: 2, FollowSort: 1},
		{TargetID: "g2", CategorySort: 1, intraCategorySort: 1, FollowSort: 1},
		// 另一类 (CategoryGroupSort=2) —— 整个 cluster 在后
		{TargetID: "g3", CategorySort: 2, intraCategorySort: 0, FollowSort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "g2", items[0].TargetID, "intra-category sort breaks ties within same CategorySort")
	assert.Equal(t, "g1", items[1].TargetID)
	assert.Equal(t, "g3", items[2].TargetID, "different CategorySort dominates intra order")
}

// PR #21 Round-6 P0-2 (Jerry-Xin / yujiawei) regression：uniqueThreadParentGroupNos
// 必须去重 + 跳过无法解析的 thread channel ID。
func TestUniqueThreadParentGroupNos(t *testing.T) {
	type ext = convext.Model
	tests := []struct {
		name string
		rows []*ext
		want []string
	}{
		{name: "empty", rows: nil, want: []string{}},
		{
			name: "dedup same parent",
			rows: []*ext{
				{TargetID: "gA____thr1"},
				{TargetID: "gA____thr2"},
			},
			want: []string{"gA"},
		},
		{
			name: "multiple distinct",
			rows: []*ext{
				{TargetID: "gA____thr1"},
				{TargetID: "gB____thr2"},
				{TargetID: "gA____thr3"},
			},
			want: []string{"gA", "gB"},
		},
		{
			name: "malformed skipped",
			rows: []*ext{
				{TargetID: "no-separator"},
				{TargetID: "gA____thr1"},
			},
			want: []string{"gA"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueThreadParentGroupNos(tc.rows)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// PR #21 Round-4 review I6 (lml2468 #3) regression：appendThreadParentGroupNos
// 必须把 thread ext 的父群 groupNo 合入查询集合，且去重不重复添加 IM 已经返回的群。
func TestAppendThreadParentGroupNos(t *testing.T) {
	type ext = convext.Model
	tests := []struct {
		name    string
		initial []string
		rows    []*ext
		want    []string
	}{
		{
			name:    "empty rows: no-op",
			initial: []string{"g1"},
			rows:    nil,
			want:    []string{"g1"},
		},
		{
			name:    "parent NOT in initial: append",
			initial: []string{"g1"},
			rows:    []*ext{{TargetID: "g2____thr-x"}},
			want:    []string{"g1", "g2"},
		},
		{
			name:    "parent IN initial: skip (dedup)",
			initial: []string{"g1"},
			rows:    []*ext{{TargetID: "g1____thr-x"}},
			want:    []string{"g1"},
		},
		{
			name:    "multiple threads, dedup parent",
			initial: []string{},
			rows: []*ext{
				{TargetID: "g3____thr-a"},
				{TargetID: "g3____thr-b"},
				{TargetID: "g4____thr-c"},
			},
			want: []string{"g3", "g4"},
		},
		{
			name:    "malformed thread id: skipped",
			initial: []string{},
			rows: []*ext{
				{TargetID: "no-separator"},
				{TargetID: "g5____thr-x"},
			},
			want: []string{"g5"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appendThreadParentGroupNos(tc.initial, tc.rows)
			assert.Equal(t, tc.want, got)
		})
	}
}

// PR #21 Round-6 P1-2 (yujiawei) + 原型 image-v1.png 确认：thread 必须跟父群在
// 同一分类块内排序。 SidebarItem.CategorySort（来自父群 group_category.sort）
// 与 intraCategorySort（来自父群 group_setting.category_sort）必须从父群 copy 过来，
// 否则所有 thread 都默认 0 → 与 DM 混挤在 follow tab 顶部，违反 UX 期望。
func TestBuildFollowItems_ThreadInheritsParentCategorySort(t *testing.T) {
	parentNo := "g-parent"
	threadID := parentNo + "____thr-1"
	convs := []*config.SyncUserConversationResp{
		makeIMConv(parentNo, common.ChannelTypeGroup.Uint8(), 1_700_000_100),
		makeIMConv(threadID, common.ChannelTypeCommunityTopic.Uint8(), 1_700_000_200),
	}
	categorySetting := map[string]*GroupCategorySetting{
		parentNo: {
			GroupNo:           parentNo,
			CategoryID:        strPtr("cat-work"),
			CategorySort:      5,  // group_setting.category_sort
			CategoryGroupSort: 42, // group_category.sort
		},
	}
	threadExtMap := map[string]*convext.Model{
		threadID: {TargetID: threadID, FollowSort: 1},
	}
	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap, nil, nil)
	require.Len(t, items, 2)

	var groupItem, threadItem *SidebarItem
	for _, it := range items {
		switch it.TargetID {
		case parentNo:
			groupItem = it
		case threadID:
			threadItem = it
		}
	}
	require.NotNil(t, groupItem)
	require.NotNil(t, threadItem)
	assert.Equal(t, groupItem.CategorySort, threadItem.CategorySort,
		"thread.CategorySort 必须等于父群（同一类间排序桶）")
	assert.Equal(t, groupItem.intraCategorySort, threadItem.intraCategorySort,
		"thread.intraCategorySort 必须等于父群（与父群紧贴，同类内位置）")
	assert.Equal(t, "cat-work", *threadItem.CategoryID)
}

// 端到端 sort 回归：父群在分类 sort=42 内、CategorySort=5；DM 在 0 桶；
// thread 必须排在父群附近、不会浮到顶部。
func TestSortFollowItems_ThreadDoesNotFloatToTop(t *testing.T) {
	items := []*SidebarItem{
		// DM，CategorySort=0，理应在顶部
		{TargetID: "dm-1", TargetType: 1, CategorySort: 0, FollowSort: 1},
		// thread，继承父群的 (42, 5)
		{TargetID: "g-parent____thr-1", TargetType: 5,
			CategorySort: 42, intraCategorySort: 5, FollowSort: 1, ParentChannelID: "g-parent"},
		// 父群本身，(42, 5)
		{TargetID: "g-parent", TargetType: 2, CategorySort: 42, intraCategorySort: 5, FollowSort: 0},
	}
	sortFollowItems(items)
	// 期望顺序: DM (顶部) → 父群 → thread（同分类块）
	assert.Equal(t, "dm-1", items[0].TargetID, "DM 在最顶（CategorySort=0）")
	// 父群与 thread 顺序由 FollowSort 二级 key 决定（父群 0 在前）。
	assert.Equal(t, "g-parent", items[1].TargetID, "父群必须紧挨 thread")
	assert.Equal(t, "g-parent____thr-1", items[2].TargetID, "thread 跟在父群后")
}

// TestBuildFollowItems_CategoryGroupSort_Propagates 验证 buildFollowItems
// 把 GroupCategorySetting.CategoryGroupSort 映射到 SidebarItem.CategorySort
// （swagger 暴露字段），并保留 group_setting.category_sort 作为内部二级 key。
func TestBuildFollowItems_CategoryGroupSort_Propagates(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {
			GroupNo:           "g1",
			CategoryID:        strPtr("cat1"),
			CategorySort:      7,  // group_setting.category_sort —— 内部二级 key
			CategoryGroupSort: 42, // group_category.sort       —— 客户端可见 category_sort
		},
	}
	items := buildFollowItems(convs, categorySetting, nil, nil, nil, nil, nil)
	require.Len(t, items, 1)
	assert.Equal(t, 42, items[0].CategorySort, "客户端可见的 category_sort 必须取自 group_category.sort")
	assert.Equal(t, 7, items[0].intraCategorySort, "intraCategorySort 必须取自 group_setting.category_sort")
}

// ---------------------------------------------------------------------------
// sortRecentItems
// ---------------------------------------------------------------------------

func TestSortRecentItems_PinnedFirst_ThenTimestampDesc(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "a", Timestamp: 100, IsPinned: false},
		{TargetID: "b", Timestamp: 200, IsPinned: false},
		{TargetID: "c", Timestamp: 50, IsPinned: true},
	}
	sortRecentItems(items)
	assert.Equal(t, "c", items[0].TargetID) // pinned first
	assert.Equal(t, "b", items[1].TargetID) // newer
	assert.Equal(t, "a", items[2].TargetID)
}

func TestSortRecentItems_MultiplePinned_ByTimestampDesc(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "p1", Timestamp: 100, IsPinned: true},
		{TargetID: "p2", Timestamp: 300, IsPinned: true},
	}
	sortRecentItems(items)
	assert.Equal(t, "p2", items[0].TargetID)
}

// ---------------------------------------------------------------------------
// edge cases
// ---------------------------------------------------------------------------

func TestBuildFollowItems_EmptyConversations(t *testing.T) {
	items := buildFollowItems(nil, nil, nil, nil, nil, nil, nil)
	assert.Len(t, items, 0)
}

func TestBuildRecentItems_EmptyConversations(t *testing.T) {
	items := buildRecentItems(nil, nil)
	assert.Len(t, items, 0)
}

func TestSortFollowItems_Empty(t *testing.T) {
	sortFollowItems(nil) // must not panic
}

func TestSortRecentItems_Empty(t *testing.T) {
	sortRecentItems(nil) // must not panic
}

func TestBuildFollowItems_MixedTypes(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),                 // followed group
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), nowRecent()),             // followed DM
		makeIMConv("peer2", common.ChannelTypePerson.Uint8(), nowRecent()),             // un-followed DM
		makeIMConv("g2", common.ChannelTypeGroup.Uint8(), nowRecent()),                 // group without category
		makeIMConv("g1____th1", common.ChannelTypeCommunityTopic.Uint8(), nowRecent()), // followed thread
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 1},
	}
	threadExtMap := map[string]*convext.Model{
		"g1____th1": {TargetID: "g1____th1", FollowSort: 1},
	}

	items := buildFollowItems(convs, categorySetting, nil, followedDMs, threadExtMap, nil, nil)
	// g1 + peer1 + g1____th1 = 3; peer2 (not followed) and g2 (no category) excluded
	assert.Len(t, items, 3)
	ids := make(map[string]bool)
	for _, it := range items {
		ids[it.TargetID] = true
	}
	assert.True(t, ids["g1"])
	assert.True(t, ids["peer1"])
	assert.True(t, ids["g1____th1"])
	assert.False(t, ids["peer2"])
	assert.False(t, ids["g2"])
}

// ---------------------------------------------------------------------------
// Sorted integration — sort stability check (no flakiness with same fields)
// ---------------------------------------------------------------------------

func TestSortFollowItems_StableOnEqualKeys(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "a", CategorySort: 1, FollowSort: 1, IsPinned: false},
		{TargetID: "b", CategorySort: 1, FollowSort: 1, IsPinned: false},
	}
	sortFollowItems(items)
	// both have identical keys; order is implementation-defined but must not panic
	assert.Len(t, items, 2)
	// verify both are present
	ids := []string{items[0].TargetID, items[1].TargetID}
	sort.Strings(ids)
	assert.Equal(t, []string{"a", "b"}, ids)
}

// ---------------------------------------------------------------------------
// extractGroupNos
// ---------------------------------------------------------------------------

func TestExtractGroupNos_OnlyGroups(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), 100),
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), 100),
		makeIMConv("g1____th1", common.ChannelTypeCommunityTopic.Uint8(), 100),
		makeIMConv("g2", common.ChannelTypeGroup.Uint8(), 100),
	}
	noss := extractGroupNos(convs)
	assert.Equal(t, []string{"g1", "g2"}, noss)
}

func TestExtractGroupNos_Empty(t *testing.T) {
	assert.Len(t, extractGroupNos(nil), 0)
}

// ---------------------------------------------------------------------------
// buildFollowItems — DM with DMCategoryID set
// ---------------------------------------------------------------------------

func TestBuildFollowItems_DMFollowed_WithDMCategory(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	// PR #21 Round-6: dm_category_id 类型从 BIGINT 改为 VARCHAR(32) UUID。
	catID := "cat-uuid-abc"
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 3, DMCategoryID: &catID},
	}
	items := buildFollowItems(convs, nil, nil, followedDMs, nil, nil, nil)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].CategoryID)
	assert.Equal(t, catID, *items[0].CategoryID, "DMCategoryID 直接透传 UUID，不再 fmt.Sprintf 转字符串")
}

// ---------------------------------------------------------------------------
// Issue #41 regression: follow tab cross-type drag-sort must survive reload.
// ---------------------------------------------------------------------------

// 群分支必须读取 user_conversation_ext.follow_sort（旧实现恒置为 0，导致 sidebar
// 拖拽群条目后下次 sync 返回的群仍按默认顺序）。
func TestBuildFollowItems_GroupHonorsFollowSort(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	groupExts := map[string]*convext.Model{
		"g1": {TargetID: "g1", FollowSort: 7},
	}
	items := buildFollowItems(convs, categorySetting, nil, nil, nil, groupExts, nil)
	require.Len(t, items, 1)
	assert.Equal(t, 7, items[0].FollowSort, "group FollowSort 必须取自 user_conversation_ext.follow_sort")
}

// 群分支没有 ext 行时回落 0（已关注但用户未拖拽过）。
func TestBuildFollowItems_GroupMissingExtFallsBackToZero(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g-noext", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{
		"g-noext": {GroupNo: "g-noext", CategoryID: strPtr("cat1"), CategorySort: 1, CategoryGroupSort: 1},
	}
	items := buildFollowItems(convs, categorySetting, nil, nil, nil, map[string]*convext.Model{}, nil)
	require.Len(t, items, 1)
	assert.Equal(t, 0, items[0].FollowSort)
}

// DM 分支必须从 dmCategorySorts 加载 group_category.sort 写入 CategorySort。
// 旧实现只 copy DMCategoryID，导致带 category 的 DM 仍以 CategorySort=0 排序，
// 与同 category 群不在同一桶内。
func TestBuildFollowItems_DMLoadsCategorySort(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	catID := "cat-dm"
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 4, DMCategoryID: &catID},
	}
	dmCategorySorts := map[string]int{catID: 42}
	items := buildFollowItems(convs, nil, nil, followedDMs, nil, nil, dmCategorySorts)
	require.Len(t, items, 1)
	assert.Equal(t, 42, items[0].CategorySort, "DM 的 CategorySort 必须取自 group_category.sort")
	require.NotNil(t, items[0].CategoryID)
	assert.Equal(t, catID, *items[0].CategoryID)
}

// DM 无 category 时 CategorySort=0、CategoryID=nil。
func TestBuildFollowItems_DMNoCategory_NoCategorySort(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer1", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 4},
	}
	items := buildFollowItems(convs, nil, nil, followedDMs, nil, nil, nil)
	require.Len(t, items, 1)
	assert.Equal(t, 0, items[0].CategorySort)
	assert.Nil(t, items[0].CategoryID)
}

// 新的 sort 顺序：FollowSort 必须凌驾于 intraCategorySort，让 sidebar 拖拽真正生效。
func TestSortFollowItems_FollowSortBeforeIntraCategory(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "a", CategorySort: 1, intraCategorySort: 1, FollowSort: 2},
		{TargetID: "b", CategorySort: 1, intraCategorySort: 2, FollowSort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "b", items[0].TargetID, "FollowSort 优先级必须高于 intraCategorySort")
	assert.Equal(t, "a", items[1].TargetID)
}

// Issue #41 主场景：DM + 群同 CategorySort=0，FollowSort 决定相对顺序，
// 两次相反拖拽请求必须产生两种不同结果（旧实现群恒在 DM 之前）。
func TestSortFollowItems_CrossTypeDrag_DMBeforeGroup(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "dm-1", TargetType: 1, CategorySort: 0, FollowSort: 1},
		{TargetID: "g-1", TargetType: 2, CategorySort: 0, FollowSort: 2},
	}
	sortFollowItems(items)
	assert.Equal(t, "dm-1", items[0].TargetID, "FollowSort=1 的 DM 必须排在 FollowSort=2 的群前面")
	assert.Equal(t, "g-1", items[1].TargetID)
}

func TestSortFollowItems_CrossTypeDrag_GroupBeforeDM(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "dm-1", TargetType: 1, CategorySort: 0, FollowSort: 2},
		{TargetID: "g-1", TargetType: 2, CategorySort: 0, FollowSort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "g-1", items[0].TargetID, "FollowSort=1 的群必须排在 FollowSort=2 的 DM 前面")
	assert.Equal(t, "dm-1", items[1].TargetID)
}

// intraCategorySort 在 FollowSort 后作为回退（用户没拖过 sidebar 但用过 category 管理 UI）。
func TestSortFollowItems_IntraCategoryAsFallbackWhenFollowSortEqual(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "g-a", CategorySort: 1, FollowSort: 0, intraCategorySort: 2},
		{TargetID: "g-b", CategorySort: 1, FollowSort: 0, intraCategorySort: 1},
	}
	sortFollowItems(items)
	assert.Equal(t, "g-b", items[0].TargetID, "FollowSort 同为 0 时回退到 category-management UI 的顺序")
}

// PR #42 review (yujiawei) regression：IsPinned 从 T3 提升到 T2 后，pin 必须
// 在同 category 内胜过 FollowSort —— 即便被 pin 的 item 的 FollowSort 远大于
// 未 pin 的 item。"pin overrides everything within a category" 是排序契约的
// 显式语义，需要专门验证以防回归。
func TestSortFollowItems_PinnedBeatsFollowSort(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "unpinned", CategorySort: 1, IsPinned: false, FollowSort: 1},
		{TargetID: "pinned", CategorySort: 1, IsPinned: true, FollowSort: 99},
	}
	sortFollowItems(items)
	assert.Equal(t, "pinned", items[0].TargetID,
		"pin 必须凌驾于 FollowSort：FollowSort=99 的 pinned 项必须排在 FollowSort=1 的 unpinned 项前")
	assert.Equal(t, "unpinned", items[1].TargetID)
}

// pin 与 intraCategorySort 的交互：pin 同样必须凌驾于 intraCategorySort（T2 > T4）。
func TestSortFollowItems_PinnedBeatsIntraCategorySort(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "unpinned-low-intra", CategorySort: 1, IsPinned: false, intraCategorySort: 0},
		{TargetID: "pinned-high-intra", CategorySort: 1, IsPinned: true, intraCategorySort: 99},
	}
	sortFollowItems(items)
	assert.Equal(t, "pinned-high-intra", items[0].TargetID)
	assert.Equal(t, "unpinned-low-intra", items[1].TargetID)
}

// 所有排序键完全相同时按 TargetID ASC 决定，保证响应顺序确定。
func TestSortFollowItems_TieBreakOnTargetID(t *testing.T) {
	items := []*SidebarItem{
		{TargetID: "zzz", CategorySort: 0, FollowSort: 0, intraCategorySort: 0},
		{TargetID: "aaa", CategorySort: 0, FollowSort: 0, intraCategorySort: 0},
		{TargetID: "mmm", CategorySort: 0, FollowSort: 0, intraCategorySort: 0},
	}
	sortFollowItems(items)
	assert.Equal(t, "aaa", items[0].TargetID)
	assert.Equal(t, "mmm", items[1].TargetID)
	assert.Equal(t, "zzz", items[2].TargetID)
}

// ---------------------------------------------------------------------------
// parseThreadChannelIDSidebar
// ---------------------------------------------------------------------------

func TestParseThreadChannelIDSidebar_Valid(t *testing.T) {
	groupNo, shortID, err := parseThreadChannelIDSidebar("myGroup____th123")
	require.NoError(t, err)
	assert.Equal(t, "myGroup", groupNo)
	assert.Equal(t, "th123", shortID)
}

func TestParseThreadChannelIDSidebar_Invalid(t *testing.T) {
	cases := []string{"", "nothreadsep", "____", "abc____"}
	for _, c := range cases {
		_, _, err := parseThreadChannelIDSidebar(c)
		assert.Error(t, err, "expected error for %q", c)
	}
}

// ---------------------------------------------------------------------------
// helpers used only in tests
// ---------------------------------------------------------------------------

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }
