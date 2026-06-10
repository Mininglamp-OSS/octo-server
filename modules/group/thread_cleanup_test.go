package group

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryThreadShortIDsForCleanup_ReturnsAllNonDeletedRegardlessOfMembership
// 是 Issue #27 的核心回归：群下有 active+archived 子区，但 uid 从未出现在 thread_member
// 表里（Bot 入群后不会主动 JoinThread 的常见情况）。修复前 SQL 用 JOIN thread_member
// 过滤导致返回空切片，IMRemoveSubscriber 永不被调用 → Bot 被踢后仍订阅子区频道。
// 修复后应返回该群所有非 deleted 子区的 short_id。
func TestQueryThreadShortIDsForCleanup_ReturnsAllNonDeletedRegardlessOfMembership(t *testing.T) {
	svc, _ := setupServiceTest(t)
	f := New(svc.(*Service).ctx)
	ensureThreadTables(t, f)

	const groupNo = "g_issue27_basic"
	// active (status=1)
	_, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_active", groupNo, "active", "owner", 1, 1).Exec()
	require.NoError(t, err)
	// archived (status=2) —— 可被 UnarchiveThread 重激活，不能漏摘订阅
	_, err = f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_archived", groupNo, "archived", "owner", 2, 1).Exec()
	require.NoError(t, err)
	// deleted (status=3) —— 必须排除（IM 频道已销毁）
	_, err = f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_deleted", groupNo, "deleted", "owner", 3, 1).Exec()
	require.NoError(t, err)
	// 另一个群下的子区，不应被返回
	_, err = f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_other_group", "g_other", "other", "owner", 1, 1).Exec()
	require.NoError(t, err)

	// 关键：不插入任何 thread_member 行 —— 模拟 Bot 入群后未 JoinThread
	shortIDs, err := queryThreadShortIDsForCleanup(f.ctx, groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"th_active", "th_archived"}, shortIDs,
		"必须返回所有非 deleted 子区，不能依赖 thread_member（Issue #27）")
}

// TestQueryThreadShortIDsForCleanup_EmptyGroupNo 防御：空 group_no 直接返回空。
func TestQueryThreadShortIDsForCleanup_EmptyGroupNo(t *testing.T) {
	svc, _ := setupServiceTest(t)
	f := New(svc.(*Service).ctx)
	ensureThreadTables(t, f)

	shortIDs, err := queryThreadShortIDsForCleanup(f.ctx, "")
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestQueryThreadShortIDsForCleanup_NoThreads 群下没子区 → 空切片。
func TestQueryThreadShortIDsForCleanup_NoThreads(t *testing.T) {
	svc, _ := setupServiceTest(t)
	f := New(svc.(*Service).ctx)
	ensureThreadTables(t, f)

	shortIDs, err := queryThreadShortIDsForCleanup(f.ctx, "g_nothreads")
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestQueryThreadShortIDsForCleanup_OnlyDeleted 群下只有已删除子区 → 空切片。
// 保证已删除子区永不被回流为 IMRemoveSubscriber 调用对象。
func TestQueryThreadShortIDsForCleanup_OnlyDeleted(t *testing.T) {
	svc, _ := setupServiceTest(t)
	f := New(svc.(*Service).ctx)
	ensureThreadTables(t, f)

	const groupNo = "g_only_deleted"
	_, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_dead", groupNo, "dead", "owner", 3, 1).Exec()
	require.NoError(t, err)

	shortIDs, err := queryThreadShortIDsForCleanup(f.ctx, groupNo)
	require.NoError(t, err)
	assert.Empty(t, shortIDs)
}

// TestRemoveUserFromGroupThreadsCleanup_DeletesMemberWithoutOwnRow 验证修复的端到端行为：
// 即便目标 uid 在 thread_member 没有任何行（Bot 场景），cleanup 也要走完整个流程而不是
// 提前 return —— 这是和旧实现最关键的差异。这里用一个“别人 join 了、target 没 join”的
// 子区，断言 cleanup 不会因为 target 没有 thread_member 行而跳过，且 target 自己后来
// 误留的行（如有）会被清掉，别人的行不受影响。
func TestRemoveUserFromGroupThreadsCleanup_DeletesMemberWithoutOwnRow(t *testing.T) {
	svc, _ := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	const groupNo = "g_issue27_e2e"
	const targetUID = "bot_no_join"
	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_e2e", groupNo, "e2e", "owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)

	// 一个无关用户在子区里有 thread_member 行 —— cleanup target 时不能误删
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "someone_else", 0, 1).Exec()
	require.NoError(t, err)

	// target 自己没有 thread_member 行：旧实现会 JOIN 出 0 行直接 return，新实现必须照常处理。
	// IMRemoveSubscriber 在无 IM 服务的测试环境里只记日志、不影响 DELETE 路径与断言。
	s.removeUserFromGroupThreads(groupNo, targetUID, "sp_e2e")

	// 无关用户的 thread_member 行仍在
	var otherCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "someone_else").Load(&otherCount)
	require.NoError(t, err)
	assert.Equal(t, 1, otherCount, "cleanup 只能按 uid 删除，不能波及其他用户的 thread_member")
}

// TestRemoveUserFromGroupThreadsCleanup_CleansSettingWithoutMembership 是 Issue #331（item 2）
// 的回归：用户只对子区设置过 mute（thread_setting 有行）但从未 JoinThread（thread_member 无行），
// 被移出群后 setting 行必须被清掉，否则重新入群时老 mute 静默生效。同时断言按 uid / group_no
// 隔离：其他用户的 setting、该用户在其他群的 setting 都不受影响。
func TestRemoveUserFromGroupThreadsCleanup_CleansSettingWithoutMembership(t *testing.T) {
	svc, _ := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	const groupNo = "g_issue331_setting"
	const targetUID = "mute_only_user"
	_, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_331", groupNo, "setting", "owner", 1, 1).Exec()
	require.NoError(t, err)

	// target 只 mute、未 join：thread_setting 有行，thread_member 无行
	_, err = f.ctx.DB().InsertInto("thread_setting").
		Columns("group_no", "short_id", "uid", "mute", "version").
		Values(groupNo, "th_331", targetUID, 1, 1).Exec()
	require.NoError(t, err)
	// 其他用户在同子区的 setting —— 不能被波及
	_, err = f.ctx.DB().InsertInto("thread_setting").
		Columns("group_no", "short_id", "uid", "mute", "version").
		Values(groupNo, "th_331", "someone_else", 1, 1).Exec()
	require.NoError(t, err)
	// target 在另一个群的 setting —— 不能被波及
	_, err = f.ctx.DB().InsertInto("thread_setting").
		Columns("group_no", "short_id", "uid", "mute", "version").
		Values("g_other_331", "th_other_331", targetUID, 1, 1).Exec()
	require.NoError(t, err)

	s.removeUserFromGroupThreads(groupNo, targetUID, "sp_331")

	var targetCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_setting").
		Where("group_no=? AND uid=?", groupNo, targetUID).Load(&targetCount)
	require.NoError(t, err)
	assert.Equal(t, 0, targetCount, "mute 而未 join 的用户被移出群后，thread_setting 必须被清理（Issue #331）")

	var otherUserCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_setting").
		Where("group_no=? AND uid=?", groupNo, "someone_else").Load(&otherUserCount)
	require.NoError(t, err)
	assert.Equal(t, 1, otherUserCount, "cleanup 只能按 uid 删除，不能波及其他用户的 thread_setting")

	var otherGroupCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_setting").
		Where("group_no=? AND uid=?", "g_other_331", targetUID).Load(&otherGroupCount)
	require.NoError(t, err)
	assert.Equal(t, 1, otherGroupCount, "cleanup 只能按 group_no 删除，不能波及该用户在其他群的 thread_setting")
}

// TestRemoveUserFromGroupThreadsCleanup_EmptyInputs 验证 uid/groupNo 防御守卫：
// 空 uid 或空 groupNo 时直接 no-op，绝不下发任何 SQL/IM 调用。
func TestRemoveUserFromGroupThreadsCleanup_EmptyInputs(t *testing.T) {
	svc, _ := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("th_guard", "g_guard", "guard", "owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "u", 0, 1).Exec()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_setting").
		Columns("group_no", "short_id", "uid", "mute", "version").
		Values("g_guard", "th_guard", "u", 1, 1).Exec()
	require.NoError(t, err)

	removeUserFromGroupThreadsCleanup(s.ctx, s.Log, "", "u", "sp")
	removeUserFromGroupThreadsCleanup(s.ctx, s.Log, "g_guard", "", "sp")

	var count int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "u").Load(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "空参数时 helper 必须立即返回、不下发 DELETE/IMRemoveSubscriber")

	var settingCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_setting").
		Where("group_no=? AND uid=?", "g_guard", "u").Load(&settingCount)
	require.NoError(t, err)
	assert.Equal(t, 1, settingCount, "空参数时 helper 不得删除 thread_setting")
}
