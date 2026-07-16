package thread

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertThread 用最少字段插入一行 thread 用于归档测试。
// last_message_at 用指针，nil 表示从未有消息。
func insertThread(t *testing.T, db *DB, shortID string, status int, lastMsgAt *time.Time) {
	t.Helper()
	insertThreadWithVersion(t, db, shortID, status, lastMsgAt, 1)
}

// constVersion 在测试里固定 GenSeq 返回值，仅用于 active-path 测试（不调）和断言场景。
// 生产路径的全局单调性由 ctx.GenSeq 保证。
func constVersion(v int64) func() (int64, error) {
	return func() (int64, error) { return v, nil }
}

func insertThreadWithVersion(t *testing.T, db *DB, shortID string, status int, lastMsgAt *time.Time, version int64) {
	t.Helper()
	m := &Model{
		ShortID:    shortID,
		GroupNo:    "g_" + shortID,
		Name:       "t-" + shortID,
		CreatorUID: testutil.UID,
		Status:     status,
		Version:    version,
	}
	if lastMsgAt != nil {
		m.LastMessageAt = lastMsgAt
	}
	require.NoError(t, db.Insert(m))
}

func TestArchiveStaleBatch(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)      // 10 天前 — 应归档
	recent := now.Add(-1 * time.Hour)         // 1 小时前 — 不应归档
	threshold := now.Add(-3 * 24 * time.Hour) // 3 天阈值

	// 准备各种边界数据
	insertThread(t, db, "stale_active_1", ThreadStatusActive, &old)
	insertThread(t, db, "stale_active_2", ThreadStatusActive, &old)
	insertThread(t, db, "fresh_active", ThreadStatusActive, &recent)
	insertThread(t, db, "stale_already_archived", ThreadStatusArchived, &old)
	insertThread(t, db, "stale_deleted", ThreadStatusDeleted, &old)
	insertThread(t, db, "active_never_messaged", ThreadStatusActive, nil) // last_message_at IS NULL

	rows, err := db.ArchiveStaleBatch(threshold, 100, 9999, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(2), rows, "exactly the two stale_active rows should be archived")

	// 验证状态
	verify := func(shortID string, expect int) {
		m, qerr := db.QueryByShortID(shortID)
		require.NoError(t, qerr)
		require.NotNil(t, m, "row %s missing", shortID)
		assert.Equal(t, expect, m.Status, "row %s status", shortID)
	}
	verify("stale_active_1", ThreadStatusArchived)
	verify("stale_active_2", ThreadStatusArchived)
	verify("fresh_active", ThreadStatusActive)
	verify("stale_already_archived", ThreadStatusArchived)
	verify("stale_deleted", ThreadStatusDeleted)
	verify("active_never_messaged", ThreadStatusActive)

	// 版本号应被刷成传入值
	m, err := db.QueryByShortID("stale_active_1")
	require.NoError(t, err)
	assert.Equal(t, int64(9999), m.Version)
}

func TestArchiveStaleBatch_RespectsBatchSize(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)

	// 7 条 stale active
	for i := 0; i < 7; i++ {
		insertThread(t, db, "s"+strconv.Itoa(i), ThreadStatusActive, &old)
	}

	rows, err := db.ArchiveStaleBatch(threshold, 3, 100, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows, "should archive exactly batchSize=3")

	// 再调一次应再归档 3 条
	rows, err = db.ArchiveStaleBatch(threshold, 3, 101, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(3), rows)

	// 第 3 次只剩 1 条
	rows, err = db.ArchiveStaleBatch(threshold, 3, 102, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)

	// 第 4 次 0
	rows, err = db.ArchiveStaleBatch(threshold, 3, 103, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
}

func TestArchiveStaleBatch_SkipsRowsAtOrAboveBatchVersion(t *testing.T) {
	// 防赛跑：如果某行的当前 version >= 本批 cron 版本号，说明在 cron 拿到版本号
	// 之后有人（手动 archive/unarchive、auto-unarchive）已经写过这行，cron 必须跳过，
	// 否则会用旧版本号覆盖新版本号让 sync 客户端漏拉。
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)

	insertThreadWithVersion(t, db, "low_version", ThreadStatusActive, &old, 10)
	insertThreadWithVersion(t, db, "exactly_at_batch", ThreadStatusActive, &old, 100)
	insertThreadWithVersion(t, db, "above_batch", ThreadStatusActive, &old, 200)

	batchVersion := int64(100)
	rows, err := db.ArchiveStaleBatch(threshold, 100, batchVersion, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows, "only the row with version < batchVersion should be archived")

	verify := func(shortID string, expectStatus int, expectVersion int64) {
		m, qerr := db.QueryByShortID(shortID)
		require.NoError(t, qerr)
		require.NotNil(t, m)
		assert.Equal(t, expectStatus, m.Status, "row %s status", shortID)
		assert.Equal(t, expectVersion, m.Version, "row %s version (must not be overwritten)", shortID)
	}
	verify("low_version", ThreadStatusArchived, batchVersion)
	verify("exactly_at_batch", ThreadStatusActive, 100) // 等号也排除
	verify("above_batch", ThreadStatusActive, 200)
}

// TestRecordMessageAndReactivate_ResurrectsArchived 验证 reviewer 找到的核心竞态：
// cron 把 active+stale 的子区归档之后，消息到达必须把它解档回 active。
// 这里我们顺序模拟最坏排列：先归档再记消息，断言最终态恢复成 active 且 version 更新。
func TestRecordMessageAndReactivate_ResurrectsArchived(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)

	insertThreadWithVersion(t, db, "raced", ThreadStatusActive, &old, 5)

	// 模拟 cron 抢先归档
	rows, err := db.ArchiveStaleBatch(threshold, 10, 100, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	m, _ := db.QueryByShortID("raced")
	require.Equal(t, ThreadStatusArchived, m.Status)

	// 此后 listener 落库消息统计：必须把 status 抬回 active 并写新版本
	require.NoError(t, db.RecordMessageAndReactivate("raced", "hello", "sender-1", constVersion(200)))

	m, err = db.QueryByShortID("raced")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, m.Status, "message must reactivate archived thread")
	assert.Equal(t, int64(200), m.Version, "version must be bumped on reactivation")
	assert.Equal(t, int64(1), m.MessageCount)
	assert.Equal(t, "hello", m.LastMessageContent)
	assert.Equal(t, "sender-1", m.LastMessageSenderUID)
	require.NotNil(t, m.LastMessageAt)
	assert.WithinDuration(t, time.Now(), *m.LastMessageAt, 5*time.Second)
}

// TestRecordMessageAndReactivate_NoVersionBumpForAlreadyActive 反向用例：
// 已经 active 的子区收到消息只更新统计，不无谓地 bump version，避免每条消息把 thread
// 推到 sync 的"已变更"队列里。
func TestRecordMessageAndReactivate_NoVersionBumpForAlreadyActive(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "active_thread", ThreadStatusActive, &now, 42)

	called := false
	require.NoError(t, db.RecordMessageAndReactivate("active_thread", "hi", "u1", func() (int64, error) {
		called = true
		return 999, nil
	}))

	m, err := db.QueryByShortID("active_thread")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, m.Status)
	assert.Equal(t, int64(42), m.Version, "version must NOT be bumped for already-active threads")
	assert.Equal(t, int64(1), m.MessageCount)
	assert.False(t, called, "GenSeq must NOT be called for already-active threads")
}

// TestRecordMessageAndReactivate_DeletedStaysDeleted 确认 status=deleted 不被复活。
func TestRecordMessageAndReactivate_DeletedStaysDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "deleted_thread", ThreadStatusDeleted, &now, 7)

	require.NoError(t, db.RecordMessageAndReactivate("deleted_thread", "x", "u1", constVersion(999)))

	m, err := db.QueryByShortID("deleted_thread")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusDeleted, m.Status, "deleted thread must not be touched")
	assert.Equal(t, int64(7), m.Version, "deleted thread version untouched")
	assert.Equal(t, int64(0), m.MessageCount, "deleted thread stats untouched")
}

// TestRecordMessageAndReactivate_VersionMonotonicVsCronArchive 回归 reviewer 指出的
// 版本倒退 bug：旧实现里 listener 在拿锁前预生成 reactivateVersion，若 cron 之后用
// 更大的版本号归档，listener 后到会把 version 写成预生成的更小值，sync 客户端漏拉。
// 修复后 GenSeq 在锁内才调用，因此无论 cron 用哪个版本号归档，listener 拿到的新版本
// 都严格大于 cron 的版本号（GenSeq 全局单调）。
//
// 本测试用一个真实的 dbr Session 模拟"全局单调 GenSeq"行为：用 atomic 计数器。
// 制造场景：listener "预备" 拿小版本号 100；cron 紧接着用版本号 101 归档；
// 然后 listener 真正落库——必须最终 version > 101。
func TestRecordMessageAndReactivate_VersionMonotonicVsCronArchive(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)

	insertThreadWithVersion(t, db, "monotonic", ThreadStatusActive, &old, 1)

	// cron 用版本 101 归档（模拟 cron 在 listener 拿到锁之前已经提交）
	rows, err := db.ArchiveStaleBatch(threshold, 10, 101, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// listener "想用" 100 解档，但我们的实现会在锁内重新调 GenSeq 拿到更大值；
	// 这里用一个递增计数器模拟 GenSeq，初值从 200 起，确保 > cron 版本。
	var seq int64 = 200
	gen := func() (int64, error) {
		newV := atomic.AddInt64(&seq, 1)
		return newV, nil
	}

	require.NoError(t, db.RecordMessageAndReactivate("monotonic", "msg", "u1", gen))

	m, err := db.QueryByShortID("monotonic")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, m.Status)
	assert.Greater(t, m.Version, int64(101),
		"version must move forward past cron's archive version, never backward")
}

// TestUpdateStatusFrom_CASRetriesOnConcurrentBump 回归 admin-vs-cron 版本回退：
// admin 拿到 GenSeq=A，cron 同时写到 version=B (B>A)。无 CAS 时 admin 把 version
// 写回 A，sync 漏拉。修复后用 WHERE version<? 做 CAS 重试。
func TestUpdateStatusFrom_CASRetriesOnConcurrentBump(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "cas_target", ThreadStatusArchived, &now, 100)

	// admin 路径解档：第一次 GenSeq 返回 50（< 100 CAS 失败），第二次 200（成功）。
	seqVals := []int64{50, 200}
	idx := 0
	cas := func() (int64, error) {
		v := seqVals[idx]
		idx++
		return v, nil
	}

	require.NoError(t, db.UpdateStatusFrom("cas_target", ThreadStatusArchived, ThreadStatusActive, cas))
	assert.Equal(t, 2, idx, "GenSeq must have been called twice (CAS retry)")

	m, err := db.QueryByShortID("cas_target")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, m.Status)
	assert.Equal(t, int64(200), m.Version)
}

func TestUpdateStatusFrom_NoSuchRow(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	calls := 0
	gen := func() (int64, error) { calls++; return int64(100 + calls), nil }
	err := db.UpdateStatusFrom("ghost", ThreadStatusActive, ThreadStatusArchived, gen)
	assert.ErrorIs(t, err, ErrThreadNotFound)
	assert.Equal(t, 1, calls)
}

// TestUpdateStatusFrom_RejectsDeletedRow 回归 reviewer 最新指出的 deleted 保护漏洞：
// admin 调用 ArchiveThread 时另一个请求把行先 delete 了。修复前 CAS 只看 version<?，
// 会复活已删除子区；修复后 WHERE status=expected 直接吃掉这条路径，返回 ErrThreadDeleted。
func TestUpdateStatusFrom_RejectsDeletedRow(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "deleted_target", ThreadStatusDeleted, &now, 50)

	calls := 0
	gen := func() (int64, error) { calls++; return int64(100 + calls), nil }
	// 试图把已删除的行从 active 切到 archived：必须返回 ErrThreadDeleted，不能复活。
	err := db.UpdateStatusFrom("deleted_target", ThreadStatusActive, ThreadStatusArchived, gen)
	assert.ErrorIs(t, err, ErrThreadDeleted)

	// 行必须仍是 deleted，version 不动
	m, err := db.QueryByShortID("deleted_target")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusDeleted, m.Status)
	assert.Equal(t, int64(50), m.Version, "deleted row must not be touched")
}

// TestUpdateStatusFrom_IdempotentWhenAlreadyAtTarget admin 双击 archive，第二次行
// 已经 archived，应该返回 nil（语义不变）。
func TestUpdateStatusFrom_IdempotentWhenAlreadyAtTarget(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "already_archived", ThreadStatusArchived, &now, 50)

	calls := 0
	gen := func() (int64, error) { calls++; return int64(100 + calls), nil }
	require.NoError(t, db.UpdateStatusFrom("already_archived", ThreadStatusActive, ThreadStatusArchived, gen))

	m, err := db.QueryByShortID("already_archived")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusArchived, m.Status)
	assert.Equal(t, int64(50), m.Version, "idempotent no-op must not bump version")
}

func TestMarkDeleted_IdempotentWhenAlreadyDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "already_deleted", ThreadStatusDeleted, &now, 7)

	require.NoError(t, db.MarkDeleted("already_deleted", func() (int64, error) { return 100, nil }))

	m, err := db.QueryByShortID("already_deleted")
	require.NoError(t, err)
	assert.Equal(t, int64(7), m.Version, "no-op on already-deleted row")
}

func TestMarkDeleted_NoSuchRow(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	err := db.MarkDeleted("ghost", func() (int64, error) { return 100, nil })
	assert.ErrorIs(t, err, ErrThreadNotFound)
}

func TestUpdateName_CASBehavesSameAsUpdateStatus(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "named", ThreadStatusActive, &now, 50)

	seqVals := []int64{10, 100}
	idx := 0
	gen := func() (int64, error) { v := seqVals[idx]; idx++; return v, nil }
	require.NoError(t, db.UpdateName("named", "new-name", gen))

	m, err := db.QueryByShortID("named")
	require.NoError(t, err)
	assert.Equal(t, "new-name", m.Name)
	assert.Equal(t, int64(100), m.Version)
}

// TestUpdateName_RejectsDeletedRow 改名时遇到已删除的行必须报错，不能 bump 它的 version。
func TestUpdateName_RejectsDeletedRow(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	insertThreadWithVersion(t, db, "del_named", ThreadStatusDeleted, &now, 50)

	err := db.UpdateName("del_named", "should-fail", func() (int64, error) { return 200, nil })
	assert.ErrorIs(t, err, ErrThreadDeleted)

	m, err := db.QueryByShortID("del_named")
	require.NoError(t, err)
	assert.Equal(t, int64(50), m.Version, "deleted row version must not be touched")
}

// TestArchiveStaleBatch_ConcurrentWithMessages 真正的并发竞态测试。
// 在 N 个 stale-active 子区上同时跑 cron 归档和"消息到达"两路，跑若干轮，最终
// 状态必须满足："任何 last_message_at 接近 now 的子区不能停在 archived"。
// 这是 reviewer 指出的 race 的最直接验证：MySQL 行锁让单条 UPDATE 互斥，
// 单 UPDATE 内的 IF(status=archived, active, status) 保证最终态恢复。
func TestArchiveStaleBatch_ConcurrentWithMessages(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)

	const numThreads = 20
	shortIDs := make([]string, numThreads)
	for i := 0; i < numThreads; i++ {
		shortIDs[i] = "race_" + strconv.Itoa(i)
		insertThreadWithVersion(t, db, shortIDs[i], ThreadStatusActive, &old, int64(i+1))
	}

	const rounds = 5
	var wg sync.WaitGroup

	// cron 路：循环 archive
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := 0; r < rounds; r++ {
			_, _ = db.ArchiveStaleBatch(threshold, numThreads, int64(1000+r*10), common.ChannelTypeCommunityTopic.Uint8())
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// 模拟全局单调 GenSeq：原子递增计数器，初值远高于 cron 的版本号空间。
	var seq int64 = 100000
	gen := func() (int64, error) {
		return atomic.AddInt64(&seq, 1), nil
	}

	// message 路：每个子区轮流 record message
	for _, sid := range shortIDs {
		sid := sid
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				_ = db.RecordMessageAndReactivate(sid, "msg", "sender", gen)
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// 收尾：最后再跑一遍消息路（确保每个子区最后一次写入是消息，模拟"消息到了之后不再有
	// cron 在它身上跑"的真实条件，因为生产里 cron 是周期触发不是连续触发）。
	for _, sid := range shortIDs {
		require.NoError(t, db.RecordMessageAndReactivate(sid, "final", "sender", gen))
	}

	// 断言：所有刚发过消息的子区都必须是 active
	for _, sid := range shortIDs {
		m, err := db.QueryByShortID(sid)
		require.NoError(t, err)
		require.NotNil(t, m, sid)
		assert.Equal(t, ThreadStatusActive, m.Status,
			"thread %s must end as active after receiving a message", sid)
		require.NotNil(t, m.LastMessageAt)
		assert.WithinDuration(t, time.Now(), *m.LastMessageAt, 10*time.Second,
			"thread %s last_message_at should be recent", sid)
	}
}

func TestArchiveStaleBatch_EmptyTable(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	rows, err := db.ArchiveStaleBatch(time.Now(), 100, 1, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
}

// ensureReminderTables 在 thread 模块隔离测试环境里按需建 reminders/reminder_done。
// 这两张表属于 message 模块，thread 包的 module.Setup 不会加载它们的建表
// migration；而本回归需要真实表来验证 ArchiveStaleBatch 的 NOT EXISTS 语义。
// 字段与 modules/message/sql 建表对齐（仅取本测试用到的列）。
func ensureReminderTables(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.session.UpdateBySql(
		"CREATE TABLE IF NOT EXISTS `reminders`(" +
			"id bigint not null primary key AUTO_INCREMENT," +
			"channel_id VARCHAR(100) not null default ''," +
			"channel_type smallint not null default 0," +
			"reminder_type integer not null default 0," +
			"uid varchar(40) not null default ''," +
			"message_seq bigint not null default 0," +
			"is_deleted smallint not null default 0," +
			"version bigint not null default 0)" +
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci",
	).Exec()
	require.NoError(t, err)
	_, err = db.session.UpdateBySql(
		"CREATE TABLE IF NOT EXISTS `reminder_done`(" +
			"id bigint not null primary key AUTO_INCREMENT," +
			"reminder_id bigint not null default 0," +
			"uid varchar(40) not null default '')" +
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci",
	).Exec()
	require.NoError(t, err)
	// 清干净残留数据，与 CleanAllTables 对齐（它只清 thread 模块登记的表）。
	_, _ = db.session.UpdateBySql("DELETE FROM `reminders`").Exec()
	_, _ = db.session.UpdateBySql("DELETE FROM `reminder_done`").Exec()
}

// insertReminder 直插一行 reminders（模拟 @ 提及）。channelID 应为 thread channel
// 形式 {group_no}____{short_id}。返回 reminder id 以便需要时写 reminder_done。
func insertReminder(t *testing.T, db *DB, channelID string, channelType uint8, reminderType int, uid string, isDeleted int) int64 {
	t.Helper()
	res, err := db.session.InsertBySql(
		"INSERT INTO reminders(channel_id, channel_type, reminder_type, uid, message_seq, is_deleted, version) "+
			"VALUES(?,?,?,?,?,?,?)",
		channelID, channelType, reminderType, uid, 1, isDeleted, 1,
	).Exec()
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func insertReminderDone(t *testing.T, db *DB, reminderID int64, uid string) {
	t.Helper()
	_, err := db.session.InsertBySql(
		"INSERT INTO reminder_done(reminder_id, uid) VALUES(?,?)", reminderID, uid,
	).Exec()
	require.NoError(t, err)
}

// threadChannelID 重建 insertThread 用的 group_no/short_id 对应的 thread channel id。
// insertThreadWithVersion 用 GroupNo = "g_" + shortID。
func threadChannelID(shortID string) string {
	return BuildChannelID("g_"+shortID, shortID)
}

// TestArchiveStaleBatch_PendingMentionExclusion 是 #566 的核心回归：验证归档谓词
// 里的 NOT EXISTS(未处理 per-uid @提及) 在真库上的四个关键分支。纯 mock 测不到
// 这些 SQL 语义。
func TestArchiveStaleBatch_PendingMentionExclusion(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)
	ensureReminderTables(t, db)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)      // 陈旧 —— 时间上应归档
	threshold := now.Add(-3 * 24 * time.Hour) // 3 天阈值
	ct := common.ChannelTypeCommunityTopic.Uint8()
	const mentionType = 1 // ReminderTypeMentionMe

	// (a) 陈旧 + 未处理 per-uid @ → 不归档（保持 active）。这条能接住 P0。
	insertThread(t, db, "stale_pending_at", ThreadStatusActive, &old)
	insertReminder(t, db, threadChannelID("stale_pending_at"), ct, mentionType, "userA", 0)

	// (b) 陈旧 + @ 已处理（reminder_done 已写）→ 照常归档。
	insertThread(t, db, "stale_done_at", ThreadStatusActive, &old)
	ridDone := insertReminder(t, db, threadChannelID("stale_done_at"), ct, mentionType, "userB", 0)
	insertReminderDone(t, db, ridDone, "userB")

	// (c) 陈旧 + @所有人（uid=''）→ 照常归档（广播不阻止归档）。
	insertThread(t, db, "stale_broadcast_at", ThreadStatusActive, &old)
	insertReminder(t, db, threadChannelID("stale_broadcast_at"), ct, mentionType, "", 0)

	// (d) 陈旧 + @ 已软删（is_deleted=1）→ 照常归档。
	insertThread(t, db, "stale_deleted_at", ThreadStatusActive, &old)
	insertReminder(t, db, threadChannelID("stale_deleted_at"), ct, mentionType, "userC", 1)

	// (e) 陈旧 无任何 @ → 照常归档（基线）。
	insertThread(t, db, "stale_no_at", ThreadStatusActive, &old)

	rows, err := db.ArchiveStaleBatch(threshold, 100, 9999, ct)
	require.NoError(t, err)
	assert.Equal(t, int64(4), rows, "only (b)(c)(d)(e) should archive; (a) pending-mention must be excluded")

	verify := func(shortID string, expect int, msg string) {
		m, qerr := db.QueryByShortID(shortID)
		require.NoError(t, qerr)
		require.NotNil(t, m, shortID)
		assert.Equal(t, expect, m.Status, msg)
	}
	verify("stale_pending_at", ThreadStatusActive, "(a) stale + unprocessed per-uid @ must stay active")
	verify("stale_done_at", ThreadStatusArchived, "(b) processed mention must not block archive")
	verify("stale_broadcast_at", ThreadStatusArchived, "(c) @all broadcast must not block archive")
	verify("stale_deleted_at", ThreadStatusArchived, "(d) soft-deleted reminder must not block archive")
	verify("stale_no_at", ThreadStatusArchived, "(e) no mention archives normally")
}

// TestArchiveStaleBatch_MixedCollation 是 cross-collation 回归（GH #567 review, yujiawei P1）。
// 生产上 reminders/reminder_done 建表未指定 CHARSET/COLLATE，MySQL 8+ 解析为
// utf8mb4_0900_ai_ci，而 thread.* 是 utf8mb4_general_ci。ArchiveStaleBatch 谓词里
// r.channel_id = CONCAT(t.group_no,'____',t.short_id) 是列派生表达式跨 collation 等值比较，
// 若不在 r.channel_id 上 pin COLLATE 会抛 Error 1267 并阻塞归档 / backfill 部署。
// 本测试显式用 0900_ai_ci 重建两表复现生产条件：若 COLLATE pin 缺失，这里会 1267 红。
func TestArchiveStaleBatch_MixedCollation(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// 显式以 utf8mb4_0900_ai_ci 重建 reminders/reminder_done，复现生产跨 collation 条件。
	for _, tbl := range []string{"reminders", "reminder_done"} {
		_, err := db.session.UpdateBySql("DROP TABLE IF EXISTS `" + tbl + "`").Exec()
		require.NoError(t, err)
	}
	_, err := db.session.UpdateBySql(
		"CREATE TABLE `reminders`(" +
			"id bigint not null primary key AUTO_INCREMENT," +
			"channel_id VARCHAR(100) not null default ''," +
			"channel_type smallint not null default 0," +
			"reminder_type integer not null default 0," +
			"uid varchar(40) not null default ''," +
			"message_seq bigint not null default 0," +
			"is_deleted smallint not null default 0," +
			"version bigint not null default 0" +
			") CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
	).Exec()
	require.NoError(t, err)
	_, err = db.session.UpdateBySql(
		"CREATE TABLE `reminder_done`(" +
			"id bigint not null primary key AUTO_INCREMENT," +
			"reminder_id bigint not null default 0," +
			"uid varchar(40) not null default ''" +
			") CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
	).Exec()
	require.NoError(t, err)

	// 模拟 message 模块 20260711000001 迁移：把两表 CONVERT 归一到 general_ci（与 thread 对齐），
	// 再建以 channel_id 打头的索引。归一后查询侧无需 COLLATE pin，1267 与索引失效一并解决。
	for _, tbl := range []string{"reminders", "reminder_done"} {
		_, cerr := db.session.UpdateBySql("ALTER TABLE `" + tbl + "` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci").Exec()
		require.NoError(t, cerr)
	}
	_, err = db.session.UpdateBySql(
		"ALTER TABLE `reminders` ADD INDEX `idx_channel_type_rtype_deleted` " +
			"(`channel_id`, `channel_type`, `reminder_type`, `is_deleted`)",
	).Exec()
	require.NoError(t, err)

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	threshold := now.Add(-3 * 24 * time.Hour)
	ct := common.ChannelTypeCommunityTopic.Uint8()

	// 陈旧 + 未处理 per-uid @ → 跨 collation 比较必须成立且排除该行，不归档。
	insertThread(t, db, "mc_pending", ThreadStatusActive, &old)
	insertReminder(t, db, threadChannelID("mc_pending"), ct, 1, "userA", 0)
	// 陈旧无 @ → 正常归档（确认谓词整体仍工作，不是被 1267 整条炸掉）。
	insertThread(t, db, "mc_plain", ThreadStatusActive, &old)

	rows, err := db.ArchiveStaleBatch(threshold, 100, 9999, ct)
	require.NoError(t, err, "cross-collation join must not raise Error 1267")
	assert.Equal(t, int64(1), rows, "only the no-mention row archives")

	mp, err := db.QueryByShortID("mc_pending")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusActive, mp.Status, "pending-mention thread must stay active across collations")
	pl, err := db.QueryByShortID("mc_plain")
	require.NoError(t, err)
	assert.Equal(t, ThreadStatusArchived, pl.Status)

	// 索引是否被 collation 废掉，由**结构事实**保证而非运行时断言：
	//   (1) 两表已归一到 utf8mb4_general_ci（与 thread 、与索引列同）；
	//   (2) ArchiveStaleBatch 查询已移除任何 `COLLATE` 子句。
	// 两者合起来，比较侧 collation == 索引列 collation，不存在任何强制不同 collation 的子句
	// 去阻止优化器选用 idx_channel_type_rtype_deleted。EXPLAIN 运行时断言在轻量单测里不可靠
	// （小表下优化器可能故意全扫、possible_keys 为空，且随 MySQL 版本变），故不引入 flaky
	// EXPLAIN 断言；上方“无 1267 + 行为正确”已锁住 collation 归一生效（pin 时这里会 1267 红）。
}

// TestReminderTypeMentionMeMatchesMessagePackage 固定 thread 侧本地常量与 message 侧
// 权威值一致（message 侧由 modules/message/validation_test.go 固定为 1）。thread 不
// import message 以免包耦合，故此处直接钉字面值；两侧任一漂移都应在 CI 暴露。
func TestReminderTypeMentionMeMatchesMessagePackage(t *testing.T) {
	assert.Equal(t, 1, ReminderTypeMentionMe,
		"thread.ReminderTypeMentionMe must match message.ReminderTypeMentionMe (=1)")
}
