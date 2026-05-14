//go:build integration

package message

// =============================================================================
// Sidebar E2E integration test — scene 7 (issue #337)
//
// Strategy: because IMSyncUserConversation is a direct network call on
// *config.Context (not an interface), we cannot inject a stub via the HTTP
// handler without modifying business code.  Instead we test the aggregation
// layer end-to-end at the function level:
//
//   1. Write real data into user_conversation_ext (the only new table from #337).
//   2. Load that data through the same DB helpers Sidebar.Sync uses
//      (convExtDB.ListFollowedDM, convExtDB.ListUnfollowedGroups, ListThreadExts).
//   3. Build synthetic IM conversation slice (stub the IM call result).
//   4. Build categorySetting in-process (avoids needing group_setting in conv_ext_test DB).
//   5. Pass all through the pure-function pipeline:
//        buildFollowItems → mergeThreadEntries → sortFollowItems
//   6. Assert the final item list is correct.
//
// This gives full coverage of the DB→aggregation→sort path for the follow tab
// without needing a live IM server or the main `test` database.
//
// Run:
//   go test -race -tags=integration ./modules/message/...
// =============================================================================

import (
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// test DB helpers
// ---------------------------------------------------------------------------

// newSidebarIntegCtx builds a *config.Context pointing at the test MySQL.
// Uses the conv_ext_test DSN which is guaranteed to have user_conversation_ext.
func newSidebarIntegCtx(t *testing.T) *config.Context {
	t.Helper()
	addr := os.Getenv("SIDEBAR_INTEG_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = os.Getenv("CONV_EXT_TEST_MYSQL_ADDR")
	}
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/conv_ext_test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
	cfg.DB.Migration = false
	return config.NewContext(cfg)
}

// cleanConvExtTable deletes all rows from user_conversation_ext.
func cleanConvExtTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().DeleteFrom("user_conversation_ext").Exec()
	require.NoError(t, err, "clean user_conversation_ext before sidebar integration test")
}

// ---------------------------------------------------------------------------
// Scene 7: v2 follow-tab sidebar smoke test — DB-backed, IM-stubbed
//
// Data written to DB:
//   - uid follows DM "s7-peer"  (followed_dm=1 ext row)
//   - uid follows thread "s7-grp____s7-thr"  (thread ext row)
//   - NO group_unfollowed row → group is NOT blacklisted
//
// categorySetting is built in-process (avoids group_setting table dependency).
// Stub IM result returns all three conversation types.
//
// Expected: 3 SidebarItems with correct target_type values.
// ---------------------------------------------------------------------------

func TestIntegration_Sidebar_FollowTab_BasicSmoke(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	const uid, space = "s7-uid", "s7-space"
	const groupNo = "s7-grp"
	const peerUID = "s7-peer"
	const threadChannelID = groupNo + "____s7-thr"
	const catID = "cat-s7"
	const catSort = 3

	// 1. Write ext rows to user_conversation_ext via DB layer.
	db := convext.NewDB(ctx)

	followedDMFlag := int8(1)
	require.NoError(t, db.Upsert(uid, space, 1 /* DM */, peerUID, convext.ConvExtFields{
		FollowedDM: &followedDMFlag,
	}), "insert DM ext row")

	require.NoError(t, db.Upsert(uid, space, 5 /* Thread */, threadChannelID, convext.ConvExtFields{}),
		"insert thread ext row")

	// No group_unfollowed row inserted → group is not blacklisted.

	// 2. Load ancillary data exactly as Sidebar.Sync does (from real DB).
	unfollowedGroupList, err := db.ListUnfollowedGroups(uid, space)
	require.NoError(t, err)
	unfollowedGroups := map[string]struct{}{}
	for _, m := range unfollowedGroupList {
		unfollowedGroups[m.TargetID] = struct{}{}
	}
	assert.NotContains(t, unfollowedGroups, groupNo,
		"precondition: group must not be in unfollowed set")

	followedDMList, err := db.ListFollowedDM(uid, space)
	require.NoError(t, err)
	followedDMs := map[string]*convext.Model{}
	for _, m := range followedDMList {
		followedDMs[m.TargetID] = m
	}
	require.Contains(t, followedDMs, peerUID, "DM ext row must be loaded from DB")

	threadExtRows, err := db.ListThreadExts(uid, space)
	require.NoError(t, err)
	threadExtMap := map[string]*convext.Model{}
	for _, m := range threadExtRows {
		threadExtMap[m.TargetID] = m
	}
	require.Contains(t, threadExtMap, threadChannelID, "thread ext row must be loaded from DB")

	// 3. Build categorySetting in-process — simulates what group_setting DB would return.
	// This avoids a dependency on the group_setting table in conv_ext_test DB.
	catIDCopy := catID
	categorySetting := map[string]*GroupCategorySetting{
		groupNo: {GroupNo: groupNo, CategoryID: &catIDCopy, CategorySort: catSort, CategoryGroupSort: catSort},
	}

	// 4. Stub IM conversation result (replaces IMSyncUserConversation network call).
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1_700_000_100},
		{ChannelID: peerUID, ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 1_700_000_200},
		{ChannelID: threadChannelID, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 1_700_000_300},
	}

	// 5. Run the same pure-function pipeline as Sidebar.Sync follow branch.
	items := buildFollowItems(stubConvs, categorySetting, unfollowedGroups, followedDMs, threadExtMap)
	// mergeThreadEntries: thread is already in IM result, so no new item added.
	items = mergeThreadEntries(items, threadExtRows, map[string]*time.Time{}, categorySetting, unfollowedGroups)
	sortFollowItems(items)

	// 6. Assert exactly 3 items with correct target_type.
	require.Len(t, items, 3,
		"follow tab must contain exactly 3 items (1 group + 1 DM + 1 thread)")

	typeCount := map[int]int{}
	for _, it := range items {
		typeCount[it.TargetType]++
		assert.True(t, it.IsFollowed,
			"all follow-tab items must have IsFollowed=true, got false for %s", it.TargetID)
	}
	assert.Equal(t, 1, typeCount[int(common.ChannelTypeGroup)],
		"must have exactly 1 group item")
	assert.Equal(t, 1, typeCount[int(common.ChannelTypePerson)],
		"must have exactly 1 DM item")
	assert.Equal(t, 1, typeCount[int(common.ChannelTypeCommunityTopic)],
		"must have exactly 1 thread item")

	// Verify group item has category fields correctly populated.
	var groupItem *SidebarItem
	for _, it := range items {
		if it.TargetID == groupNo {
			groupItem = it
			break
		}
	}
	require.NotNil(t, groupItem, "group item must be present")
	require.NotNil(t, groupItem.CategoryID, "group item must have category_id")
	assert.Equal(t, catID, *groupItem.CategoryID,
		"category_id must match what categorySetting provides")
	assert.Equal(t, catSort, groupItem.CategorySort,
		"category_sort must match what categorySetting provides")
}

// ---------------------------------------------------------------------------
// Scene 7b: follow tab excludes blacklisted group even when IM returns it
//
// Verifies the DB-loaded unfollowedGroups map correctly gates buildFollowItems.
// ---------------------------------------------------------------------------

func TestIntegration_Sidebar_FollowTab_BlacklistedGroupExcluded(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	const uid, space = "s7b-uid", "s7b-space"
	const groupNo = "s7b-grp"

	// Write group_unfollowed=1 ext row (blacklist) to real DB.
	db := convext.NewDB(ctx)
	unfollowedVal := int8(1)
	require.NoError(t, db.Upsert(uid, space, 2 /* Group */, groupNo, convext.ConvExtFields{
		GroupUnfollowed: &unfollowedVal,
	}))

	// Load unfollowed groups from real DB (the key assertion: DB state → filter).
	unfollowedGroupList, err := db.ListUnfollowedGroups(uid, space)
	require.NoError(t, err)
	unfollowedGroups := map[string]struct{}{}
	for _, m := range unfollowedGroupList {
		unfollowedGroups[m.TargetID] = struct{}{}
	}
	assert.Contains(t, unfollowedGroups, groupNo,
		"precondition: group must be loaded as blacklisted from DB")

	// categorySetting has the group (so it would pass the category check).
	catIDStr := "cat-s7b"
	categorySetting := map[string]*GroupCategorySetting{
		groupNo: {GroupNo: groupNo, CategoryID: &catIDStr, CategorySort: 1, CategoryGroupSort: 1},
	}

	// Stub IM returns the group.
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 100},
	}

	items := buildFollowItems(stubConvs, categorySetting, unfollowedGroups, nil, nil)
	assert.Len(t, items, 0,
		"blacklisted group (group_unfollowed=1 in DB) must be excluded from follow tab")
}

// ---------------------------------------------------------------------------
// Scene 7c: follow tab with no ext rows → empty result
//
// Verifies that when the DB has no ext rows, the follow tab returns nothing
// even if the IM stub returns conversations.
// ---------------------------------------------------------------------------

func TestIntegration_Sidebar_FollowTab_NoExtRows_ReturnsEmpty(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	db := convext.NewDB(ctx)

	// Load data sets — both must be empty.
	followedDMList, err := db.ListFollowedDM("nobody", "nowhere")
	require.NoError(t, err)
	followedDMs := map[string]*convext.Model{}
	for _, m := range followedDMList {
		followedDMs[m.TargetID] = m
	}
	assert.Len(t, followedDMs, 0)

	unfollowedGroups := map[string]struct{}{}

	// IM returns one group + one DM.
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: "grp-x", ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 100},
		{ChannelID: "peer-x", ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 200},
	}

	// No category → group excluded; no followed_dm row → DM excluded.
	items := buildFollowItems(stubConvs, nil /*categorySetting*/, unfollowedGroups, followedDMs, nil)
	assert.Len(t, items, 0, "follow tab with no ext data must return 0 items")
}

// ---------------------------------------------------------------------------
// Scene 7d: mergeThreadEntries correctly adds DB-loaded thread rows that
// the IM stub did NOT return, with proper deduplication.
// ---------------------------------------------------------------------------

func TestIntegration_Sidebar_MergeThreadEntries_AddsDBOnlyThreads(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	const uid, space = "s7d-uid", "s7d-space"
	const groupNo = "s7d-grp"
	const threadInIM = groupNo + "____thr-im"   // returned by IM
	const threadDBOnly = groupNo + "____thr-db" // NOT returned by IM, has ext row

	db := convext.NewDB(ctx)

	// Insert ext rows for both threads.
	require.NoError(t, db.Upsert(uid, space, 5, threadInIM, convext.ConvExtFields{}))
	require.NoError(t, db.Upsert(uid, space, 5, threadDBOnly, convext.ConvExtFields{}))

	// Load thread ext rows from real DB.
	threadExtRows, err := db.ListThreadExts(uid, space)
	require.NoError(t, err)
	require.Len(t, threadExtRows, 2, "both thread ext rows must be loaded from DB")

	threadExtMap := map[string]*convext.Model{}
	for _, m := range threadExtRows {
		threadExtMap[m.TargetID] = m
	}

	// categorySetting: parent group is in follow set.
	catIDStr := "cat-s7d"
	categorySetting := map[string]*GroupCategorySetting{
		groupNo: {GroupNo: groupNo, CategoryID: &catIDStr, CategorySort: 1, CategoryGroupSort: 1},
	}

	// IM only returns threadInIM (not threadDBOnly).
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: threadInIM, ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Timestamp: 500},
	}

	// buildFollowItems picks up threadInIM (has ext row + parent in follow set).
	items := buildFollowItems(stubConvs, categorySetting, nil, nil, threadExtMap)
	require.Len(t, items, 1, "buildFollowItems must include threadInIM")

	// mergeThreadEntries appends threadDBOnly (not yet in items).
	// PR review Round-3 Blocking #4: parent-follow predicate also applies here.
	// lastMsgAtMap 必须为 ext 行登记活跃记录，否则生产代码会按 "幽灵 thread"
	// 规则 skip — 这里给 threadDBOnly 一个活跃时间戳，让 merge 真正生效。
	alive := time.Unix(800, 0)
	lastMsgAtMap := map[string]*time.Time{
		threadInIM:   &alive,
		threadDBOnly: &alive,
	}
	items = mergeThreadEntries(items, threadExtRows, lastMsgAtMap, categorySetting, map[string]struct{}{})
	require.Len(t, items, 2, "mergeThreadEntries must add the DB-only thread")

	// Both thread IDs must be present.
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.TargetID] = true
	}
	assert.True(t, ids[threadInIM], "threadInIM must be in final items")
	assert.True(t, ids[threadDBOnly], "threadDBOnly must be in final items")

	// No duplicates.
	assert.Len(t, items, 2, "no duplicates must exist after merge")
}
