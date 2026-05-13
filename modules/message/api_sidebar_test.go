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
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1},
	}
	unfollowedGroups := map[string]struct{}{} // empty: not unfollowed

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil)
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
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1},
	}
	unfollowedGroups := map[string]struct{}{"g1": {}}

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil)
	assert.Len(t, items, 0)
}

func TestBuildFollowItems_GroupWithoutCategory_Excluded(t *testing.T) {
	// group g1 has no category → not in follow set
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g1", common.ChannelTypeGroup.Uint8(), nowRecent()),
	}
	categorySetting := map[string]*GroupCategorySetting{} // no entry
	unfollowedGroups := map[string]struct{}{}

	items := buildFollowItems(convs, categorySetting, unfollowedGroups, nil, nil)
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

	items := buildFollowItems(convs, nil, nil, followedDMs, nil)
	require.Len(t, items, 1)
	assert.Equal(t, "peer1", items[0].TargetID)
	assert.Equal(t, 5, items[0].FollowSort)
}

func TestBuildFollowItems_DMNotFollowed_Excluded(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		makeIMConv("peer2", common.ChannelTypePerson.Uint8(), nowRecent()),
	}
	followedDMs := map[string]*convext.Model{} // no entry for peer2

	items := buildFollowItems(convs, nil, nil, followedDMs, nil)
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
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1},
	}
	threadExtMap := map[string]*convext.Model{
		threadChannelID: {TargetID: threadChannelID, FollowSort: 2},
	}

	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap)
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
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1},
	}
	threadExtMap := map[string]*convext.Model{} // no ext for this thread

	items := buildFollowItems(convs, categorySetting, nil, nil, threadExtMap)
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
			"g1": {GroupNo: "g1", CategoryID: &cat, CategorySort: 1},
		},
		map[string]struct{}{}
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
	lastMsgAtMap := map[string]*time.Time{
		"th2": timePtr(time.Now().Add(-30 * time.Minute)),
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
	result := mergeThreadEntries(existing, threadExtRows, nil, cat, unfollowed)
	assert.Len(t, result, 1) // no duplicate
}

func TestMergeThreadEntries_EmptyExt(t *testing.T) {
	existing := []*SidebarItem{}
	cat, unfollowed := followedG1()
	result := mergeThreadEntries(existing, nil, nil, cat, unfollowed)
	assert.Len(t, result, 0)
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

	result := mergeThreadEntries(existing, threadExtRows, nil, cat, unfollowed)
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

	result := mergeThreadEntries(existing, threadExtRows, nil, cat, unfollowed)
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
	items := buildFollowItems(nil, nil, nil, nil, nil)
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
		"g1": {GroupNo: "g1", CategoryID: strPtr("cat1"), CategorySort: 1},
	}
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 1},
	}
	threadExtMap := map[string]*convext.Model{
		"g1____th1": {TargetID: "g1____th1", FollowSort: 1},
	}

	items := buildFollowItems(convs, categorySetting, nil, followedDMs, threadExtMap)
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
	catID := int64(42)
	followedDMs := map[string]*convext.Model{
		"peer1": {TargetID: "peer1", FollowedDM: 1, FollowSort: 3, DMCategoryID: &catID},
	}
	items := buildFollowItems(convs, nil, nil, followedDMs, nil)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].CategoryID)
	assert.Equal(t, "42", *items[0].CategoryID)
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
// mergeThreadEntries — nil lastMsgAtMap
// ---------------------------------------------------------------------------

// retained: nil lastMsgAtMap is valid; merge still succeeds with ts=0.
func TestMergeThreadEntries_NilLastMsgAtMap(t *testing.T) {
	existing := []*SidebarItem{}
	extRows := []*convext.Model{
		{TargetID: "g1____th1", FollowSort: 1},
	}
	cat, unfollowed := followedG1()
	result := mergeThreadEntries(existing, extRows, nil, cat, unfollowed)
	require.Len(t, result, 1)
	assert.Equal(t, "g1____th1", result[0].TargetID)
	assert.Equal(t, int64(0), result[0].Timestamp) // no timestamp available
}

// ---------------------------------------------------------------------------
// helpers used only in tests
// ---------------------------------------------------------------------------

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }
