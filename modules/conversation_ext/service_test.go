//go:build integration

package conversation_ext

import (
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
