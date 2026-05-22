//go:build integration

package conversation_ext

import (
	"os"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// intToStrAFT 短手包装 strconv.Itoa，仅供 auto_follow_threads 测试块内使用。
func intToStrAFT(i int) string { return strconv.Itoa(i) }

// readFollowVersion 直接读 user_follow_version 行的值（行不存在返回 0）。
// 在 svc 上读，复用同一个 *dbr.Session，避免再开 ctx。
func readFollowVersion(t *testing.T, svc *Service, uid, spaceID string) int64 {
	t.Helper()
	var v int64
	err := svc.session.SelectBySql(
		"SELECT version FROM "+followVersionTable+" WHERE uid=? AND space_id=?",
		uid, spaceID,
	).LoadOne(&v)
	if err != nil {
		// 行不存在时 dbr.ErrNotFound — 视为 0。
		return 0
	}
	return v
}

// newServiceForTest creates a Service connected to the test MySQL instance and
// wipes the table so every test starts from a clean slate.
func newServiceForTest(t *testing.T) *Service {
	t.Helper()
	addr := os.Getenv("CONV_EXT_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/conv_ext_test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)
	_, err := ctx.DB().DeleteFrom(table).Exec()
	require.NoError(t, err, "clean "+table+" before service test")
	// 同时清 follow_version 行 —— 否则前一个测试留下的 (uid, space_id, version=N)
	// 让本测试的 FollowChannel bump 后 version 不再是 1，断言会假报失败。
	// 与 newDBForTest 行为对齐。
	_, err = ctx.DB().DeleteFrom(followVersionTable).Exec()
	require.NoError(t, err, "clean "+followVersionTable+" before service test")
	return NewService(ctx)
}

// seedTestCategory inserts a status=1 row into group_category owned by uid in
// spaceID with the given catID, bootstrapping the table if missing. Required
// for any FollowDM(..., &categoryID) call after PR #79 because
// authorizeDMCategoryInTx now demands a real, status=1, owned row in the
// same transaction. Pre-PR these tests passed only because no
// DMCategoryChecker was injected via SetDMCategoryChecker — that hook is
// gone, the in-tx lock is now the sole authority.
//
// The schema definition mirrors the category module's migration
// (modules/category/sql/20260403000001_category_legacy01.sql) at the
// minimum columns FollowDM cares about. CREATE TABLE IF NOT EXISTS keeps
// this idempotent against a DB that already has the real schema applied.
func seedTestCategory(t *testing.T, svc *Service, uid, spaceID, catID string) {
	t.Helper()
	rawDB := svc.session.DB
	_, err := rawDB.Exec(`CREATE TABLE IF NOT EXISTS group_category (
		id          BIGINT       AUTO_INCREMENT PRIMARY KEY,
		category_id VARCHAR(32)  NOT NULL,
		space_id    VARCHAR(40)  NOT NULL,
		uid         VARCHAR(40)  NOT NULL,
		name        VARCHAR(100) NOT NULL,
		sort        INT          NOT NULL DEFAULT 0,
		status      TINYINT      NOT NULL DEFAULT 1,
		is_default  TINYINT      NULL,
		UNIQUE KEY uk_category_id (category_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	require.NoError(t, err, "ensure group_category table")
	_, err = rawDB.Exec(
		"INSERT IGNORE INTO group_category (category_id, space_id, uid, name) VALUES (?, ?, ?, ?)",
		catID, spaceID, uid, "test",
	)
	require.NoError(t, err, "seed group_category row")
}

// ---------------------------------------------------------------------------
// Input validation
// ---------------------------------------------------------------------------

func TestService_FollowChannel_EmptyUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowChannel("", "s1", "grp-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uid")
}

func TestService_FollowChannel_EmptySpaceID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowChannel("u1", "", "grp-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "space_id")
}

func TestService_FollowChannel_EmptyGroupNo(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowChannel("u1", "s1", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group_no")
}

func TestService_UnfollowChannel_EmptyUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.UnfollowChannel("", "s1", "grp-1")
	require.Error(t, err)
}

func TestService_FollowThread_EmptyUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowThread("", "s1", "grp-1____thr-1")
	require.Error(t, err)
}

func TestService_FollowThread_InvalidChannelID_NoSeparator(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowThread("u1", "s1", "invalid-no-separator")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread_channel_id")
}

func TestService_FollowThread_InvalidChannelID_EmptyGroupNo(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowThread("u1", "s1", "____shortID")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread_channel_id")
}

func TestService_FollowThread_InvalidChannelID_EmptyShortID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowThread("u1", "s1", "grp-1____")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread_channel_id")
}

func TestService_UnfollowThread_InvalidChannelID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.UnfollowThread("u1", "s1", "bad-channel")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread_channel_id")
}

func TestService_FollowDM_EmptyUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowDM("", "s1", "peer1", nil)
	require.Error(t, err)
}

func TestService_FollowDM_EmptyPeerUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.FollowDM("u1", "s1", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peer_uid")
}

func TestService_UnfollowDM_EmptyUID(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.UnfollowDM("", "s1", "peer1")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// FollowChannel happy path
// ---------------------------------------------------------------------------

func TestService_FollowChannel_ClearGroupUnfollowed(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-100"

	// Pre-condition: group already unfollowed
	require.NoError(t, svc.UnfollowChannel(uid, space, grp))
	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.GroupUnfollowed)

	// Re-follow
	require.NoError(t, svc.FollowChannel(uid, space, grp))

	m2, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Equal(t, int8(0), m2.GroupUnfollowed)
}

func TestService_FollowChannel_NoExistingRow_CreatesRow(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-200"

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.GroupUnfollowed)
}

// ---------------------------------------------------------------------------
// FollowChannel cascade: auto_follow_threads + ThreadEnumerator
// ---------------------------------------------------------------------------

// stubThreadEnumerator 是 service_test 内用的 ThreadEnumerator 桩实现：
//   - groups[groupNo] 决定枚举返回值；
//   - lastLimit 记录最近一次调用收到的 limit，便于断言 cap 透传；
//   - callCount 用来确认 FollowChannel 不在 nil-enumerator 之外的路径多次调用。
type stubThreadEnumerator struct {
	groups    map[string][]string
	lastLimit int
	callCount int
}

func (s *stubThreadEnumerator) EnumerateActiveShortIDs(groupNo string, limit int) ([]string, error) {
	s.callCount++
	s.lastLimit = limit
	ids := s.groups[groupNo]
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func TestService_FollowChannel_SetsAutoFollowThreads(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-1"

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.GroupUnfollowed)
	assert.Equal(t, int8(1), m.AutoFollowThreads, "FollowChannel 应把 auto_follow_threads 置 1")
}

func TestService_FollowChannel_MaterializesActiveThreads(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-2"

	enum := &stubThreadEnumerator{groups: map[string][]string{
		grp: {"thr-a", "thr-b", "thr-c"},
	}}
	svc.SetThreadEnumerator(enum)

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	assert.Equal(t, 1, enum.callCount, "FollowChannel 应只查询一次 ThreadEnumerator")

	for _, shortID := range []string{"thr-a", "thr-b", "thr-c"} {
		channelID := grp + threadSeparator + shortID
		row, err := svc.db.Get(uid, space, targetTypeThread, channelID)
		require.NoError(t, err)
		require.NotNil(t, row, "thread ext row for %s should exist after FollowChannel", channelID)
		assert.Equal(t, 0, row.FollowSort,
			"fanout 物化行 follow_sort 应保持默认 0（未手动排序），由客户端按规则聚拢渲染")
	}
}

func TestService_FollowChannel_RespectsCap(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-3"

	// 模拟有 600 个 active 子区。
	ids := make([]string, 600)
	for i := range ids {
		ids[i] = "thr-" + intToStrAFT(i)
	}
	enum := &stubThreadEnumerator{groups: map[string][]string{grp: ids}}
	svc.SetThreadEnumerator(enum)

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	assert.Equal(t, maxAutoFollowThreadsPerChannel, enum.lastLimit,
		"FollowChannel 应把 cap 透传给 ThreadEnumerator")

	// 验前 500 个物化、500..599 未物化。
	var got []*Model
	got, err := svc.db.ListThreadExts(uid, space)
	require.NoError(t, err)
	assert.Equal(t, maxAutoFollowThreadsPerChannel, len(got),
		"超过 cap 的子区不应被物化（fanout 会在后续 OnThreadCreated 持续补齐）")
}

func TestService_FollowChannel_NoEnumerator_NoMaterialization(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-4"

	// 不注入 enumerator —— 与现有 FollowChannel 行为兼容。
	require.NoError(t, svc.FollowChannel(uid, space, grp))

	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.AutoFollowThreads)

	rows, err := svc.db.ListThreadExts(uid, space)
	require.NoError(t, err)
	assert.Empty(t, rows, "无 enumerator 时不应物化子区行")
}

func TestService_FollowChannel_VersionBumpedOnce(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-5"

	enum := &stubThreadEnumerator{groups: map[string][]string{
		grp: {"thr-1", "thr-2", "thr-3", "thr-4"},
	}}
	svc.SetThreadEnumerator(enum)

	require.NoError(t, svc.FollowChannel(uid, space, grp))

	v := readFollowVersion(t, svc, uid, space)
	assert.Equal(t, int64(1), v,
		"FollowChannel 即便物化 N 个子区，也只应 bump follow_version 一次（user-level 单调序列）")
}

func TestService_FollowChannel_PreservesExistingThreadSort(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-af-6"
	preThread := grp + threadSeparator + "thr-pre"

	// 预先：用户手动关注过该子区并拖拽到 sort=42。
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, preThread, ConvExtFields{
		FollowSort: intPtr(42),
	}))

	enum := &stubThreadEnumerator{groups: map[string][]string{
		grp: {"thr-pre", "thr-new"},
	}}
	svc.SetThreadEnumerator(enum)
	require.NoError(t, svc.FollowChannel(uid, space, grp))

	// thr-pre 的手动 sort 必须保留（fanout 走 INSERT IGNORE）。
	pre, err := svc.db.Get(uid, space, targetTypeThread, preThread)
	require.NoError(t, err)
	require.NotNil(t, pre)
	assert.Equal(t, 42, pre.FollowSort, "已有手动排序的 thread 行不应被 fanout 覆盖")

	// thr-new 是新物化的，follow_sort=0（未排序）。
	newRow, err := svc.db.Get(uid, space, targetTypeThread, grp+threadSeparator+"thr-new")
	require.NoError(t, err)
	require.NotNil(t, newRow)
	assert.Equal(t, 0, newRow.FollowSort)
}

// ---------------------------------------------------------------------------
// UnfollowChannel happy path + cascade
// ---------------------------------------------------------------------------

func TestService_UnfollowChannel_SetsGroupUnfollowed(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-300"

	require.NoError(t, svc.UnfollowChannel(uid, space, grp))

	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.GroupUnfollowed)
}

func TestService_UnfollowChannel_ClearsAutoFollowThreads(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "grp-uaf-1"

	// 先关注 channel（auto_follow_threads=1）
	require.NoError(t, svc.FollowChannel(uid, space, grp))
	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, int8(1), m.AutoFollowThreads)

	// 取消关注后 auto_follow_threads 必须置回 0，
	// 防止后续 OnThreadCreated 把已取关的用户当作 fanout 目标。
	require.NoError(t, svc.UnfollowChannel(uid, space, grp))
	m2, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Equal(t, int8(1), m2.GroupUnfollowed)
	assert.Equal(t, int8(0), m2.AutoFollowThreads,
		"UnfollowChannel 必须把 auto_follow_threads 清零，否则 fanout 还会找到该用户")
}

func TestService_UnfollowChannel_CascadeDeletesThreadExtRows(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "400"

	// Insert several thread ext rows under this group
	thread1 := grp + "____thr-a"
	thread2 := grp + "____thr-b"
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, thread1, ConvExtFields{}))
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, thread2, ConvExtFields{}))
	// A thread row for a different group — must NOT be deleted
	otherThread := "999____thr-x"
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, otherThread, ConvExtFields{}))

	require.NoError(t, svc.UnfollowChannel(uid, space, grp))

	// Group row must be marked unfollowed
	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.GroupUnfollowed)

	// Thread rows under the group must be gone
	m1, err := svc.db.Get(uid, space, targetTypeThread, thread1)
	require.NoError(t, err)
	assert.Nil(t, m1, "thread ext row should have been cascade-deleted")

	m2, err := svc.db.Get(uid, space, targetTypeThread, thread2)
	require.NoError(t, err)
	assert.Nil(t, m2, "thread ext row should have been cascade-deleted")

	// Thread from different group must survive
	m3, err := svc.db.Get(uid, space, targetTypeThread, otherThread)
	require.NoError(t, err)
	assert.NotNil(t, m3, "thread ext row for different group must be preserved")
}

func TestService_UnfollowChannel_ThreadsOtherUsersNotAffected(t *testing.T) {
	svc := newServiceForTest(t)
	const space, grp = "s1", "500"
	thread := grp + "____thr-z"

	// uid1 and uid2 both have a thread row
	require.NoError(t, svc.db.Upsert("uid1", space, targetTypeThread, thread, ConvExtFields{}))
	require.NoError(t, svc.db.Upsert("uid2", space, targetTypeThread, thread, ConvExtFields{}))

	// Only uid1 unfollows the channel
	require.NoError(t, svc.UnfollowChannel("uid1", space, grp))

	// uid2's thread row must be untouched
	m, err := svc.db.Get("uid2", space, targetTypeThread, thread)
	require.NoError(t, err)
	assert.NotNil(t, m, "other user's thread ext row must not be affected")
}

// ---------------------------------------------------------------------------
// FollowThread happy path
// ---------------------------------------------------------------------------

func TestService_FollowThread_CreatesExtRowAndClearsParentUnfollowed(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "600"
	threadChannelID := grp + "____thr-1"

	// Pre-condition: parent group is unfollowed
	require.NoError(t, svc.UnfollowChannel(uid, space, grp))
	m, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.GroupUnfollowed)

	// FollowThread should clear parent unfollow flag and create thread ext row
	require.NoError(t, svc.FollowThread(uid, space, threadChannelID))

	// Parent group must now be followed (group_unfollowed=0)
	parentRow, err := svc.db.Get(uid, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, parentRow)
	assert.Equal(t, int8(0), parentRow.GroupUnfollowed)

	// Thread ext row must exist
	threadRow, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.NotNil(t, threadRow, "thread ext row should have been created")
}

func TestService_FollowThread_ParentGroupNotPreviouslyUnfollowed_StillCreatesThreadRow(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "700"
	threadChannelID := grp + "____thr-2"

	require.NoError(t, svc.FollowThread(uid, space, threadChannelID))

	threadRow, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.NotNil(t, threadRow, "thread ext row should have been created even if parent was not unfollowed")
}

// ---------------------------------------------------------------------------
// UnfollowThread happy path
// ---------------------------------------------------------------------------

func TestService_UnfollowThread_DeletesExtRow(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, grp = "u1", "s1", "800"
	threadChannelID := grp + "____thr-3"

	require.NoError(t, svc.FollowThread(uid, space, threadChannelID))
	threadRow, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	require.NotNil(t, threadRow)

	require.NoError(t, svc.UnfollowThread(uid, space, threadChannelID))

	threadRow2, err := svc.db.Get(uid, space, targetTypeThread, threadChannelID)
	require.NoError(t, err)
	assert.Nil(t, threadRow2, "thread ext row should have been deleted")
}

func TestService_UnfollowThread_NotExisting_NoError(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.UnfollowThread("u1", "s1", "grp-900____thr-ghost")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// FollowDM happy path
// ---------------------------------------------------------------------------

func TestService_FollowDM_WithoutCategory(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, peer = "u1", "s1", "peer-dm-1"

	require.NoError(t, svc.FollowDM(uid, space, peer, nil))

	m, err := svc.db.Get(uid, space, targetTypeDM, peer)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.FollowedDM)
	assert.Nil(t, m.DMCategoryID)
}

func TestService_FollowDM_WithCategory(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, peer = "u1", "s1", "peer-dm-2"
	catID := "cat-uuid-77"
	seedTestCategory(t, svc, uid, space, catID)

	require.NoError(t, svc.FollowDM(uid, space, peer, &catID))

	m, err := svc.db.Get(uid, space, targetTypeDM, peer)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.FollowedDM)
	require.NotNil(t, m.DMCategoryID)
	assert.Equal(t, catID, *m.DMCategoryID)
}

func TestService_FollowDM_Idempotent_UpdatesCategory(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, peer = "u1", "s1", "peer-dm-3"
	catA := "cat-uuid-A"
	catB := "cat-uuid-B"
	seedTestCategory(t, svc, uid, space, catA)
	seedTestCategory(t, svc, uid, space, catB)

	require.NoError(t, svc.FollowDM(uid, space, peer, &catA))
	require.NoError(t, svc.FollowDM(uid, space, peer, &catB))

	m, err := svc.db.Get(uid, space, targetTypeDM, peer)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.FollowedDM)
	require.NotNil(t, m.DMCategoryID)
	assert.Equal(t, catB, *m.DMCategoryID)
}

// ---------------------------------------------------------------------------
// UnfollowDM happy path
// ---------------------------------------------------------------------------

func TestService_UnfollowDM_DeletesRow(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space, peer = "u1", "s1", "peer-dm-4"

	require.NoError(t, svc.FollowDM(uid, space, peer, nil))

	m, err := svc.db.Get(uid, space, targetTypeDM, peer)
	require.NoError(t, err)
	require.NotNil(t, m)

	require.NoError(t, svc.UnfollowDM(uid, space, peer))

	m2, err := svc.db.Get(uid, space, targetTypeDM, peer)
	require.NoError(t, err)
	assert.Nil(t, m2)
}

func TestService_UnfollowDM_NotExisting_NoError(t *testing.T) {
	svc := newServiceForTest(t)
	err := svc.UnfollowDM("u1", "s1", "ghost-peer")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Special-character groupNo in LIKE (LIKE-escape correctness)
// ---------------------------------------------------------------------------

func TestService_UnfollowChannel_GroupNoWithUnderscore_DoesNotMatchOtherGroups(t *testing.T) {
	svc := newServiceForTest(t)
	// groupNo contains underscores which are LIKE wildcards
	const uid, space = "u1", "s1"
	const grpA = "1_2" // contains underscore
	const grpB = "1X2" // differs only in that position

	threadA := grpA + "____thr-a"
	threadB := grpB + "____thr-b"

	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, threadA, ConvExtFields{}))
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, threadB, ConvExtFields{}))

	// Unfollow grpA — must only delete threadA's row, not threadB's
	require.NoError(t, svc.UnfollowChannel(uid, space, grpA))

	mA, err := svc.db.Get(uid, space, targetTypeThread, threadA)
	require.NoError(t, err)
	assert.Nil(t, mA, "thread for grpA must be deleted")

	mB, err := svc.db.Get(uid, space, targetTypeThread, threadB)
	require.NoError(t, err)
	assert.NotNil(t, mB, "thread for grpB must survive")
}

// PR review follow-up：threadSeparator 里的 4 个下划线如果没 escape，会被当作
// 任意 4 字符通配，导致变长 groupNo 之间相互越界匹配。这里构造一个 28 字符的
// "受害者" groupNo 加上一个 32 字符的 "攻击者" groupNo（差正好 4 个字符），
// 验证修复后两者不会互相级联删除。
func TestService_UnfollowChannel_SeparatorEscaped_LengthCollisionSafe(t *testing.T) {
	svc := newServiceForTest(t)
	const uid, space = "u1", "s1"
	// 28 字符 victim：unfollow 它不应触及更长的 attacker。
	const victim = "AAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 28
	// 32 字符 attacker：victim 后 4 个通配若未 escape，会去匹配 attacker 的中间 4 字符。
	const attacker = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAXXXX" // 32

	victimThread := victim + "____v-thr"
	attackerThread := attacker + "____a-thr"

	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, victimThread, ConvExtFields{}))
	require.NoError(t, svc.db.Upsert(uid, space, targetTypeThread, attackerThread, ConvExtFields{}))

	require.NoError(t, svc.UnfollowChannel(uid, space, victim))

	mV, err := svc.db.Get(uid, space, targetTypeThread, victimThread)
	require.NoError(t, err)
	assert.Nil(t, mV, "victim 的 thread 必须被级联删除")

	mA, err := svc.db.Get(uid, space, targetTypeThread, attackerThread)
	require.NoError(t, err)
	assert.NotNil(t, mA,
		"attacker 的 thread 必须留存——4 个下划线不应被当作通配跨群匹配")
}
