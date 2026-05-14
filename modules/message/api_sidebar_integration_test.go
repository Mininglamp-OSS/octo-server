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
	items := buildFollowItems(stubConvs, categorySetting, unfollowedGroups, followedDMs, threadExtMap, nil, nil)
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

	items := buildFollowItems(stubConvs, categorySetting, unfollowedGroups, nil, nil, nil, nil)
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
	items := buildFollowItems(stubConvs, nil /*categorySetting*/, unfollowedGroups, followedDMs, nil, nil, nil)
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
	items := buildFollowItems(stubConvs, categorySetting, nil, nil, threadExtMap, nil, nil)
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

// ---------------------------------------------------------------------------
// Issue #41 end-to-end regression: cross-type drag-sort must survive a reload.
//
// 严格按 issue 的 reproduction script 跑：账号有 1 个 DM (fileHelper) 和 1 个
// 群（category=CA，category_sort=0）。两次反向的 follow_sort 写入必须分别
// 产生 [DM, 群] 和 [群, DM] 两种 sidebar 响应——旧实现两次都恒回 [群, DM]。
//
// 数据流：
//   1. seed group_category（CA, sort=0）+ group_setting（群在 CA 内）+
//      followed-DM ext 行。
//   2. 把 follow_sort 写到 user_conversation_ext（模拟 UpdateSort 的效果）。
//   3. 走真正的 sidebar pipeline：ListFollowedDM → ListGroupExts →
//      QueryCategorySettingsByGroupNos → QueryCategorySortsByIDs →
//      buildFollowItems → sortFollowItems。
//   4. 断言返回顺序与提交顺序一致。
// ---------------------------------------------------------------------------

func TestIntegration_Sidebar_Issue41_CrossTypeDragSurvivesReload(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	const uid, space = "i41-uid", "i41-space"
	const dmID = "fileHelper"
	const groupNo = "i41-grp"
	const catID = "i41-cat-ca"

	// 1a. seed user_conversation_ext：DM followed_dm=1，群 ext 行（为 follow_sort 准备）。
	db := convext.NewDB(ctx)
	one := int8(1)
	require.NoError(t, db.Upsert(uid, space, 1 /* DM */, dmID, convext.ConvExtFields{
		FollowedDM: &one,
	}))
	require.NoError(t, db.Upsert(uid, space, 2 /* Group */, groupNo, convext.ConvExtFields{}))

	// 1b. 准备 IM stub：同 reproduction，两者都来自 IM。
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: dmID, ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 1_700_000_100},
		{ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Timestamp: 1_700_000_200},
	}

	// 1c. categorySetting 在进程内构造（conv_ext_test 没有 group_setting 表，
	//     与其他 integration tests 一致的处理：参见 Scene 7）。issue #41 的根因在
	//     sidebar 的读取 + 排序路径，group_setting 的写入由 category 模块负责，
	//     在这一层做 stub 不影响 regression 验证。
	catCopy := catID
	categorySetting := map[string]*GroupCategorySetting{
		groupNo: {
			GroupNo:           groupNo,
			CategoryID:        &catCopy,
			CategorySort:      0, // group_setting.category_sort
			CategoryGroupSort: 0, // group_category.sort —— issue 中两者都在 0 桶
		},
	}

	// runPipeline 复现 Sidebar.Sync 的剩余数据装配 + 排序步骤。
	runPipeline := func() []*SidebarItem {
		unfollowedList, err := db.ListUnfollowedGroups(uid, space)
		require.NoError(t, err)
		unfollowedGroups := map[string]struct{}{}
		for _, m := range unfollowedList {
			unfollowedGroups[m.TargetID] = struct{}{}
		}

		dmList, err := db.ListFollowedDM(uid, space)
		require.NoError(t, err)
		followedDMs := map[string]*convext.Model{}
		for _, m := range dmList {
			followedDMs[m.TargetID] = m
		}

		// Issue #41 fix #1: load group exts for FollowSort.
		groupExtList, err := db.ListGroupExts(uid, space)
		require.NoError(t, err)
		groupExts := map[string]*convext.Model{}
		for _, m := range groupExtList {
			groupExts[m.TargetID] = m
		}

		// 本 case DM 没绑 category（issue #41 reproduction 的 fileHelper 也没有），
		// dmCategorySorts 直接传 nil，避免依赖 group_setting 表。
		items := buildFollowItems(stubConvs, categorySetting, unfollowedGroups, followedDMs, nil, groupExts, nil)
		sortFollowItems(items)
		return items
	}

	writeFollowSort := func(targetType uint8, targetID string, sort int) {
		t.Helper()
		s := sort
		require.NoError(t, db.Upsert(uid, space, targetType, targetID, convext.ConvExtFields{
			FollowSort: &s,
		}))
	}

	// === Round 1: PUT [DM=1, group=2] → sidebar must return [DM, group] ===
	writeFollowSort(1, dmID, 1)
	writeFollowSort(2, groupNo, 2)

	items := runPipeline()
	require.Len(t, items, 2)
	assert.Equal(t, dmID, items[0].TargetID,
		"Round 1 (issue #41 reproduction): FollowSort=1 的 DM 必须在 FollowSort=2 的群前面")
	assert.Equal(t, groupNo, items[1].TargetID)

	// === Round 2: PUT [group=1, DM=2] → sidebar must return [group, DM] ===
	writeFollowSort(1, dmID, 2)
	writeFollowSort(2, groupNo, 1)

	items = runPipeline()
	require.Len(t, items, 2)
	assert.Equal(t, groupNo, items[0].TargetID,
		"Round 2 (issue #41 reproduction): FollowSort=1 的群必须在 FollowSort=2 的 DM 前面 —— 旧实现两次响应都恒回 [群, DM]")
	assert.Equal(t, dmID, items[1].TargetID)
}

// Issue #41 fix #2 端到端：DM 带 dm_category_id 时必须从 group_category.sort
// 读到对应的 CategorySort，与同 category 群进入同一排序桶。
func TestIntegration_Sidebar_Issue41_DMCategorySortLoadedFromGroupCategory(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	cleanConvExtTable(t, ctx)

	const uid, space = "i41b-uid", "i41b-space"
	const dmID = "i41b-peer"
	const catID = "i41b-cat"
	const catSort = 77

	_, err := ctx.DB().DeleteFrom("group_category").
		Where("uid=? OR space_id=?", uid, space).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status) VALUES (?, ?, ?, ?, ?, 1)",
		catID, space, uid, "DM-cat", catSort,
	).Exec()
	require.NoError(t, err)

	db := convext.NewDB(ctx)
	one := int8(1)
	catCopy := catID
	require.NoError(t, db.Upsert(uid, space, 1, dmID, convext.ConvExtFields{
		FollowedDM:   &one,
		DMCategoryID: &catCopy,
	}))

	dmList, err := db.ListFollowedDM(uid, space)
	require.NoError(t, err)
	require.Len(t, dmList, 1)
	require.NotNil(t, dmList[0].DMCategoryID)

	groupCategoryDB := newGroupCategoryDB(ctx)
	sorts, err := groupCategoryDB.QueryCategorySortsByIDs([]string{*dmList[0].DMCategoryID}, uid)
	require.NoError(t, err)
	got, ok := sorts[catID]
	require.True(t, ok, "DM 的 dm_category_id 必须能从 group_category 查到")
	assert.Equal(t, catSort, got, "QueryCategorySortsByIDs 必须返回真实的 group_category.sort")

	followedDMs := map[string]*convext.Model{dmID: dmList[0]}
	stubConvs := []*config.SyncUserConversationResp{
		{ChannelID: dmID, ChannelType: common.ChannelTypePerson.Uint8(), Timestamp: 100},
	}
	items := buildFollowItems(stubConvs, nil, nil, followedDMs, nil, nil, sorts)
	require.Len(t, items, 1)
	assert.Equal(t, catSort, items[0].CategorySort,
		"带 dm_category_id 的 DM 必须把 group_category.sort 写到 SidebarItem.CategorySort")
}
