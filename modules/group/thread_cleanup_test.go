package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryThreadShortIDsForCleanup_ReturnsAllNonDeletedRegardlessOfMembership
// 这是 Issue #27 的核心回归：群下有 active+archived 子区，但 uid 未在 thread_member
// 表里出现（Bot 入群后不会主动 JoinThread 的常见情况）。修复前 SQL 用 JOIN
// thread_member 过滤导致返回空切片，IMRemoveSubscriber 永远不被调用 → Bot 被踢后
// 仍订阅子区频道。修复后应返回所有非 deleted 子区的 short_id。
func TestQueryThreadShortIDsForCleanup_ReturnsAllNonDeletedRegardlessOfMembership(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	const groupNo = "g_issue27_basic"
	// active
	_, err := ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_active", groupNo, "active", "owner", 1, 1).Exec()
	require.NoError(t, err)
	// archived (status=2) — 可被 unarchive 重激活，不能漏摘订阅
	_, err = ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_archived", groupNo, "archived", "owner", 2, 1).Exec()
	require.NoError(t, err)
	// deleted (status=3) — 必须排除（IM 频道已销毁）
	_, err = ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_deleted", groupNo, "deleted", "owner", 3, 1).Exec()
	require.NoError(t, err)
	// 另一个群下的子区，不应被返回
	_, err = ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_other_group", "g_other", "other", "owner", 1, 1).Exec()
	require.NoError(t, err)

	// 关键：不插入任何 thread_member 行 —— 模拟 Bot 入群后未 JoinThread
	shortIDs, err := queryThreadShortIDsForCleanup(ctx, groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"th_active", "th_archived"}, shortIDs,
		"必须返回所有非 deleted 子区，不能依赖 thread_member（Issue #27）")
}

// TestQueryThreadShortIDsForCleanup_EmptyGroupNo 防御：空 group_no 直接返回空。
func TestQueryThreadShortIDsForCleanup_EmptyGroupNo(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	shortIDs, err := queryThreadShortIDsForCleanup(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestQueryThreadShortIDsForCleanup_NoThreads 群下没子区 → 空切片。
func TestQueryThreadShortIDsForCleanup_NoThreads(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	shortIDs, err := queryThreadShortIDsForCleanup(ctx, "g_nothreads")
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestQueryThreadShortIDsForCleanup_OnlyDeleted 群下只有已删除子区 → 空切片。
// 保证已删除子区永不被回流为 IMRemoveSubscriber 调用对象。
func TestQueryThreadShortIDsForCleanup_OnlyDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	const groupNo = "g_only_deleted"
	_, err := ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_dead", groupNo, "dead", "owner", 3, 1).Exec()
	require.NoError(t, err)

	shortIDs, err := queryThreadShortIDsForCleanup(ctx, groupNo)
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestQueryThreadShortIDsForCleanup_IgnoresThreadMember 回归：thread_member 表的内容
// 不应影响结果 —— 不管目标 uid 自己在不在 thread_member、不管别的 uid 在不在，
// 输出只由 thread.group_no + thread.status 决定（防止 JOIN 过滤式 bug 复发）。
func TestQueryThreadShortIDsForCleanup_IgnoresThreadMember(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	const groupNo = "g_with_members"
	const targetUID = "target_uid"
	// 两个活跃子区：一个 target 自己 join 过，一个 target 没 join
	res1, err := ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_joined", groupNo, "joined", "owner", 1, 1).Exec()
	require.NoError(t, err)
	threadIDJoined, err := res1.LastInsertId()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_not_joined", groupNo, "not-joined", "owner", 1, 1).Exec()
	require.NoError(t, err)

	// target 自己在 th_joined 上有 thread_member 行
	_, err = ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadIDJoined, targetUID, 0, 1).Exec()
	require.NoError(t, err)
	// 另一个无关用户在同一个子区上也有 thread_member 行（防止 GROUP BY / DISTINCT 误折叠）
	_, err = ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadIDJoined, "some_other_user", 0, 1).Exec()
	require.NoError(t, err)

	shortIDs, err := queryThreadShortIDsForCleanup(ctx, groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"th_joined", "th_not_joined"}, shortIDs,
		"无论 uid 在不在 thread_member，结果都必须是该群所有非 deleted 子区，且每个子区只出现一次")
}

// TestRemoveUserFromGroupThreadsCleanup_EmptyInputs 验证 uid/groupNo 防御守卫：
// 空 uid 或空 groupNo 时直接 no-op，绝不下发任何 SQL/IM 调用。
func TestRemoveUserFromGroupThreadsCleanup_EmptyInputs(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ensureThreadTablesByCtx(t, ctx)

	// 准备一个子区 + 一个 thread_member 行，用来证明守卫触发时不会被清理
	res, err := ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_guard", "g_guard", "guard", "owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "u", 0, 1).Exec()
	require.NoError(t, err)

	logger := log.NewTLog("thread_cleanup_test")

	removeUserFromGroupThreadsCleanup(ctx, logger, "", "u", "sp")
	removeUserFromGroupThreadsCleanup(ctx, logger, "g_guard", "", "sp")

	var count int
	_, err = ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "u").Load(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "空参数时 helper 必须立即返回、不下发 DELETE/IMRemoveSubscriber")
}
