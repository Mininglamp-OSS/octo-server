//go:build integration

package message

// =============================================================================
// Sidebar per-user thread visibility — DB-backed tests (plan T3/T4)
//
// computeEffectiveStatus runs the four-level arbitration MUTE > P1 > P2 > P3 and
// folds in the T4 Unread second filter. These tests seed real thread rows,
// thread_setting (mute), thread_user_state (archive_intent) and reminders /
// reminder_done (P1 unhandled per-uid @), then drive the same helper Sidebar.Sync
// uses (flag=on path). flag=off equivalence is covered separately.
//
// Build-tagged `integration` like api_sidebar_status_test.go: these spin up
// testutil.NewTestServer against the shared `test` DB.
// =============================================================================

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupPerUserEnv sets the env the message-package test server needs before
// NewTestServer: OCTO_MASTER_KEY (common.Setup requires it) and DM_THREAD_ON
// (thread API + migrations apply cleanly). Must be called first in each test.
func setupPerUserEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DM_THREAD_ON", "true")
}

// seedUserArchiveIntent writes a per-user archive intent row via the thread DB.
func seedUserArchiveIntent(t *testing.T, ctx *config.Context, uid, groupNo, shortID string, intent int) {
	t.Helper()
	tdb := thread.NewDB(ctx)
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	require.NoError(t, tdb.UpsertArchiveIntentTx(tx, uid, groupNo, shortID, intent, 1))
	require.NoError(t, tx.Commit())
}

// seedMute writes a per-user mute setting via the thread DB.
func seedMute(t *testing.T, ctx *config.Context, uid, groupNo, shortID string, mute int) {
	t.Helper()
	tdb := thread.NewDB(ctx)
	require.NoError(t, tdb.UpsertSetting(&thread.SettingModel{
		GroupNo: groupNo, ShortID: shortID, UID: uid, Mute: mute, Version: 1,
	}))
}

// seedReminder inserts a reminders row for a thread channel (channel_type=5).
// uid="" means an @所有人 broadcast (must NOT trigger P1 for individuals).
func seedReminder(t *testing.T, ctx *config.Context, uid, groupNo, shortID string) int64 {
	t.Helper()
	channelID := groupNo + "____" + shortID
	res, err := ctx.DB().InsertBySql(
		"INSERT INTO reminders (channel_id, channel_type, reminder_type, uid, is_deleted, version) VALUES (?,?,?,?,0,1)",
		channelID, uint8(common.ChannelTypeCommunityTopic), 1, uid,
	).Exec()
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// markReminderDone inserts a reminder_done row for (reminderID, uid).
func markReminderDone(t *testing.T, ctx *config.Context, reminderID int64, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO reminder_done (reminder_id, uid) VALUES (?,?)", reminderID, uid,
	).Exec()
	require.NoError(t, err)
}

func threadItem(groupNo, shortID string, unread int) *SidebarItem {
	return &SidebarItem{
		TargetType: int(common.ChannelTypeCommunityTopic),
		TargetID:   groupNo + "____" + shortID,
		Unread:     unread,
	}
}

// itemByID finds a SidebarItem by TargetID.
func itemByID(items []*SidebarItem, targetID string) *SidebarItem {
	for _, it := range items {
		if it.TargetID == targetID {
			return it
		}
	}
	return nil
}

// TestComputeEffectiveStatus_PerUserIsolation — gate 1: 子区内 @Alice 未处理 →
// Alice effective status=1；同群未被@的 Bob effective status=2（手工归档）。
// uid='' 广播不触发 P1。
func TestComputeEffectiveStatus_PerUserIsolation(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp1"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusActive, &now)

	// Alice 有未处理 per-uid @；Bob 手工归档了 t1。
	seedReminder(t, ctx, "alice", g, "t1")
	seedUserArchiveIntent(t, ctx, "bob", g, "t1", 1)
	// @所有人 广播（uid=''）——不该把任何人拉成 P1。
	seedReminder(t, ctx, "", g, "t1")

	sb := NewSidebar(ctx)
	statusMap := map[string]int{threadChannelID(g, "t1"): thread.ThreadStatusActive}

	// Alice 视角：P1 命中 → active。
	aliceItems := []*SidebarItem{threadItem(g, "t1", 3)}
	sb.computeEffectiveStatus("alice", aliceItems, statusMap)
	assert.Equal(t, thread.ThreadStatusActive, itemByID(aliceItems, threadChannelID(g, "t1")).Status)
	assert.Equal(t, 3, itemByID(aliceItems, threadChannelID(g, "t1")).Unread, "P1 keeps unread")

	// Bob 视角：无 P1、有 archive_intent → archived + Unread 清零。
	bobItems := []*SidebarItem{threadItem(g, "t1", 5)}
	sb.computeEffectiveStatus("bob", bobItems, statusMap)
	assert.Equal(t, thread.ThreadStatusArchived, itemByID(bobItems, threadChannelID(g, "t1")).Status)
	assert.Equal(t, 0, itemByID(bobItems, threadChannelID(g, "t1")).Unread, "archived-hidden -> unread 0")

	// Carol 视角：无 P1/P2 → 回落全局 status (active)。
	carolItems := []*SidebarItem{threadItem(g, "t1", 2)}
	sb.computeEffectiveStatus("carol", carolItems, statusMap)
	assert.Equal(t, thread.ThreadStatusActive, itemByID(carolItems, threadChannelID(g, "t1")).Status)
}

// TestComputeEffectiveStatus_NoFlapping — gate 1: 连续多轮 sync（模拟 worker 只改全局 P3）
// 同一 uid 可见性不翻烧饼。
func TestComputeEffectiveStatus_NoFlapping(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp2"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusActive, &now)
	seedReminder(t, ctx, "alice", g, "t1") // Alice 有未处理@

	sb := NewSidebar(ctx)
	// worker 把全局 status 翻来覆去（P3），Alice 的 P1 应稳定压过它。
	for _, globalStatus := range []int{1, 2, 1, 2, 2} {
		items := []*SidebarItem{threadItem(g, "t1", 1)}
		statusMap := map[string]int{threadChannelID(g, "t1"): globalStatus}
		sb.computeEffectiveStatus("alice", items, statusMap)
		assert.Equal(t, thread.ThreadStatusActive, itemByID(items, threadChannelID(g, "t1")).Status,
			"P1 must stay active regardless of global status churn (global=%d)", globalStatus)
	}
}

// TestComputeEffectiveStatus_MuteOverP1 — gate 2: Alice 静音且有未处理@ →
// 不被 P1 强制拉成 active、Unread 清零（suppressBadge）。
func TestComputeEffectiveStatus_MuteOverP1(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp3"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusArchived, &now) // 全局归档

	seedReminder(t, ctx, "alice", g, "t1") // 有未处理@（P1）
	seedMute(t, ctx, "alice", g, "t1", 1)  // 但静音

	sb := NewSidebar(ctx)
	statusMap := map[string]int{threadChannelID(g, "t1"): thread.ThreadStatusArchived}
	items := []*SidebarItem{threadItem(g, "t1", 4)}
	sb.computeEffectiveStatus("alice", items, statusMap)

	got := itemByID(items, threadChannelID(g, "t1"))
	// MUTE 压过 P1：不被强制拉成 active，走 P3（全局 archived），且 Unread 清零。
	assert.Equal(t, thread.ThreadStatusArchived, got.Status, "mute suppresses P1 forced-visible")
	assert.Equal(t, 0, got.Unread, "mute suppresses badge -> unread 0")
}

// TestComputeEffectiveStatus_MuteWithIntent — MUTE + 本人手工归档意图：静音走 P2。
func TestComputeEffectiveStatus_MuteWithIntent(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp3b"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusActive, &now)
	seedReminder(t, ctx, "alice", g, "t1")            // P1
	seedMute(t, ctx, "alice", g, "t1", 1)             // 静音
	seedUserArchiveIntent(t, ctx, "alice", g, "t1", 1) // 本人归档意图

	sb := NewSidebar(ctx)
	statusMap := map[string]int{threadChannelID(g, "t1"): thread.ThreadStatusActive}
	items := []*SidebarItem{threadItem(g, "t1", 4)}
	sb.computeEffectiveStatus("alice", items, statusMap)

	got := itemByID(items, threadChannelID(g, "t1"))
	assert.Equal(t, thread.ThreadStatusArchived, got.Status, "mute -> P2 intent archived")
	assert.Equal(t, 0, got.Unread)
}

// TestComputeEffectiveStatus_P1AfterDone — reminder_done 后 P1 消失，回落 P2/P3。
func TestComputeEffectiveStatus_P1AfterDone(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp4"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusActive, &now)
	seedUserArchiveIntent(t, ctx, "alice", g, "t1", 1) // 本人已归档
	rid := seedReminder(t, ctx, "alice", g, "t1")      // 但有未处理@ → P1 拉回

	sb := NewSidebar(ctx)
	statusMap := map[string]int{threadChannelID(g, "t1"): thread.ThreadStatusActive}

	// done 前：P1 压过 intent → active。
	items := []*SidebarItem{threadItem(g, "t1", 2)}
	sb.computeEffectiveStatus("alice", items, statusMap)
	assert.Equal(t, thread.ThreadStatusActive, itemByID(items, threadChannelID(g, "t1")).Status)

	// done 后：P1 消失 → 回落 P2 (archived) + Unread 清零。
	markReminderDone(t, ctx, rid, "alice")
	items = []*SidebarItem{threadItem(g, "t1", 2)}
	sb.computeEffectiveStatus("alice", items, statusMap)
	got := itemByID(items, threadChannelID(g, "t1"))
	assert.Equal(t, thread.ThreadStatusArchived, got.Status, "after done, fall back to intent archived")
	assert.Equal(t, 0, got.Unread)
}

// TestComputeEffectiveStatus_FailOpen — gate 4: per-uid 查询失败 → 保持全局 status（不隐藏）。
// 这里用 loginUID="" 触发的早退 + 空 refs 场景验证 fail-open 回落到 backfill。
func TestComputeEffectiveStatus_FailOpenEmptyUID(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp5"
	sb := NewSidebar(ctx)
	statusMap := map[string]int{threadChannelID(g, "t1"): thread.ThreadStatusArchived}
	items := []*SidebarItem{threadItem(g, "t1", 7)}
	// loginUID 空 → fail-open：回落全局 status，Unread 不动（除非全局归档）。
	sb.computeEffectiveStatus("", items, statusMap)
	got := itemByID(items, threadChannelID(g, "t1"))
	assert.Equal(t, thread.ThreadStatusArchived, got.Status, "empty uid falls back to global status")
}

// TestComputeEffectiveStatus_EmptyTableFallback — gate 4b: thread_user_state 空表 →
// 所有 thread 回落全局 status（== 现状），零回填。
func TestComputeEffectiveStatus_EmptyTableFallback(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp6"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "t1", thread.ThreadStatusActive, &now)
	seedThread(t, ctx, g, "t2", thread.ThreadStatusArchived, &now)

	sb := NewSidebar(ctx)
	statusMap := map[string]int{
		threadChannelID(g, "t1"): thread.ThreadStatusActive,
		threadChannelID(g, "t2"): thread.ThreadStatusArchived,
	}
	items := []*SidebarItem{threadItem(g, "t1", 1), threadItem(g, "t2", 2)}
	sb.computeEffectiveStatus("nobody", items, statusMap)

	assert.Equal(t, thread.ThreadStatusActive, itemByID(items, threadChannelID(g, "t1")).Status)
	assert.Equal(t, thread.ThreadStatusArchived, itemByID(items, threadChannelID(g, "t2")).Status)
	// 全局归档条目在列表内 Unread 清零（列表内一致）。
	assert.Equal(t, 0, itemByID(items, threadChannelID(g, "t2")).Unread)
}

// TestComputeEffectiveStatus_UnreadSecondFilter — gate 3: per-user 归档隐藏条目 Unread=0；
// P1 拉回可见条目 Unread 保留。
func TestComputeEffectiveStatus_UnreadSecondFilter(t *testing.T) {
	setupPerUserEnv(t)
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const g = "gp7"
	now := time.Now().Add(-time.Hour)
	seedThread(t, ctx, g, "hidden", thread.ThreadStatusActive, &now)
	seedThread(t, ctx, g, "pulled", thread.ThreadStatusActive, &now)

	seedUserArchiveIntent(t, ctx, "alice", g, "hidden", 1) // 归档隐藏
	seedReminder(t, ctx, "alice", g, "pulled")             // P1 拉回

	sb := NewSidebar(ctx)
	statusMap := map[string]int{
		threadChannelID(g, "hidden"): thread.ThreadStatusActive,
		threadChannelID(g, "pulled"): thread.ThreadStatusActive,
	}
	items := []*SidebarItem{threadItem(g, "hidden", 9), threadItem(g, "pulled", 6)}
	sb.computeEffectiveStatus("alice", items, statusMap)

	assert.Equal(t, 0, itemByID(items, threadChannelID(g, "hidden")).Unread, "archived-hidden unread zeroed")
	assert.Equal(t, thread.ThreadStatusArchived, itemByID(items, threadChannelID(g, "hidden")).Status)
	assert.Equal(t, 6, itemByID(items, threadChannelID(g, "pulled")).Unread, "P1 pulled item keeps unread")
	assert.Equal(t, thread.ThreadStatusActive, itemByID(items, threadChannelID(g, "pulled")).Status)
}
