//go:build integration

package conversation_ext

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// initGlobalConvExtDBForTest resets the singleton and re-initialises it using
// the test database context.  Must be called at the start of every cleanup test
// that touches globalConvExtDB to keep tests hermetic.
func initGlobalConvExtDBForTest(t *testing.T, ctx *config.Context) *DB {
	t.Helper()
	return initGlobalConvExtDBForTestTB(t, ctx)
}

// initGlobalConvExtDBForTestTB is the testing.TB-accepting variant used by
// benchmarks (Jerry-Xin review round-1) — same body, no &testing.T{} reach.
func initGlobalConvExtDBForTestTB(tb testing.TB, ctx *config.Context) *DB {
	tb.Helper()
	globalConvExtDBOnce = sync.Once{}
	globalConvExtDB = nil
	InitGlobalConvExtDB(ctx)
	require.NotNil(tb, globalConvExtDB, "globalConvExtDB must be non-nil after Init")
	return globalConvExtDB
}

// insertRow is a test helper that inserts a raw row into user_conversation_ext.
func insertRow(t *testing.T, db *DB, uid, spaceID string, targetType uint8, targetID string) {
	t.Helper()
	_, err := db.session.InsertBySql(
		"INSERT IGNORE INTO "+table+
			" (uid, space_id, target_type, target_id) VALUES (?, ?, ?, ?)",
		uid, spaceID, targetType, targetID,
	).Exec()
	require.NoError(t, err, "insertRow failed")
}

// countRows counts rows matching uid + space_id + target_type + target_id.
func countRows(t *testing.T, db *DB, uid, spaceID string, targetType uint8, targetID string) int {
	t.Helper()
	var n int
	_, err := db.session.SelectBySql(
		"SELECT COUNT(*) FROM "+table+
			" WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
		uid, spaceID, targetType, targetID,
	).Load(&n)
	require.NoError(t, err)
	return n
}

// countRowsByUID counts all rows for a given uid regardless of target.
func countRowsByUID(t *testing.T, db *DB, uid string) int {
	t.Helper()
	var n int
	_, err := db.session.SelectBySql(
		"SELECT COUNT(*) FROM "+table+" WHERE uid=?", uid,
	).Load(&n)
	require.NoError(t, err)
	return n
}

// countRowsByChannel counts all rows for a given channel (target_type + target_id)
// regardless of uid.
func countRowsByChannel(t *testing.T, db *DB, targetType uint8, targetID string) int {
	t.Helper()
	var n int
	_, err := db.session.SelectBySql(
		"SELECT COUNT(*) FROM "+table+" WHERE target_type=? AND target_id=?",
		targetType, targetID,
	).Load(&n)
	require.NoError(t, err)
	return n
}

// ---------------------------------------------------------------------------
// TestInitGlobalConvExtDB
// ---------------------------------------------------------------------------

func TestInitGlobalConvExtDB_NonNil(t *testing.T) {
	ctx := newCtxForTest(t)
	initGlobalConvExtDBForTest(t, ctx)
	assert.NotNil(t, globalConvExtDB)
}

func TestInitGlobalConvExtDB_Idempotent(t *testing.T) {
	ctx := newCtxForTest(t)
	initGlobalConvExtDBForTest(t, ctx)
	first := globalConvExtDB
	InitGlobalConvExtDB(ctx)
	assert.Same(t, first, globalConvExtDB, "Init must be idempotent")
}

// ---------------------------------------------------------------------------
// TestRemoveConvExtForUserInSpace
// ---------------------------------------------------------------------------

// TestRemoveConvExtForUserInSpace_HappyPath verifies that calling the function
// deletes the group ext row for the given user+space+channel.
func TestRemoveConvExtForUserInSpace_HappyPath(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	uid, spaceID, channelID := "u1", "sp1", "grp1"
	insertRow(t, db, uid, spaceID, targetTypeGroup, channelID)
	require.Equal(t, 1, countRows(t, db, uid, spaceID, targetTypeGroup, channelID))

	RemoveConvExtForUserInSpace(uid, spaceID, channelID, targetTypeGroup)

	assert.Equal(t, 0, countRows(t, db, uid, spaceID, targetTypeGroup, channelID),
		"group ext row must be deleted")
}

// TestRemoveConvExtForUserInSpace_CascadesThreads verifies that child thread rows
// (target_type=5, target_id starts with "{channelID}____") are also deleted.
func TestRemoveConvExtForUserInSpace_CascadesThreads(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	uid, spaceID, groupID := "u2", "sp1", "grp2"
	threadID1 := groupID + "____t1"
	threadID2 := groupID + "____t2"
	otherGroupThreadID := "grpOther____t1"

	insertRow(t, db, uid, spaceID, targetTypeGroup, groupID)
	insertRow(t, db, uid, spaceID, targetTypeThread, threadID1)
	insertRow(t, db, uid, spaceID, targetTypeThread, threadID2)
	// Row that must NOT be deleted — different group's thread
	insertRow(t, db, uid, spaceID, targetTypeThread, otherGroupThreadID)

	RemoveConvExtForUserInSpace(uid, spaceID, groupID, targetTypeGroup)

	assert.Equal(t, 0, countRows(t, db, uid, spaceID, targetTypeGroup, groupID), "group row gone")
	assert.Equal(t, 0, countRows(t, db, uid, spaceID, targetTypeThread, threadID1), "thread1 row gone")
	assert.Equal(t, 0, countRows(t, db, uid, spaceID, targetTypeThread, threadID2), "thread2 row gone")
	assert.Equal(t, 1, countRows(t, db, uid, spaceID, targetTypeThread, otherGroupThreadID),
		"other group's thread must be untouched")
}

// TestRemoveConvExtForUserInSpace_ThreadChannelType verifies behaviour when
// channelType is for a thread (target_type=5): only that specific thread row is
// deleted, not the parent group row.
func TestRemoveConvExtForUserInSpace_ThreadChannelType(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	uid, spaceID, groupID := "u3", "sp1", "grp3"
	threadID := groupID + "____tA"

	insertRow(t, db, uid, spaceID, targetTypeGroup, groupID)
	insertRow(t, db, uid, spaceID, targetTypeThread, threadID)

	RemoveConvExtForUserInSpace(uid, spaceID, threadID, targetTypeThread)

	assert.Equal(t, 1, countRows(t, db, uid, spaceID, targetTypeGroup, groupID),
		"parent group row must be untouched when cleaning a specific thread")
	assert.Equal(t, 0, countRows(t, db, uid, spaceID, targetTypeThread, threadID), "thread row gone")
}

// TestRemoveConvExtForUserInSpace_NilGlobalDB verifies no panic when the
// global DB singleton has not been initialised.
func TestRemoveConvExtForUserInSpace_NilGlobalDB(t *testing.T) {
	globalConvExtDB = nil
	globalConvExtDBOnce = sync.Once{}
	assert.NotPanics(t, func() {
		RemoveConvExtForUserInSpace("u", "sp", "ch", 2)
	})
}

// TestRemoveConvExtForUserInSpace_NotExist verifies no error/panic when the row
// does not exist (idempotency).
func TestRemoveConvExtForUserInSpace_NotExist(t *testing.T) {
	ctx := newCtxForTest(t)
	initGlobalConvExtDBForTest(t, ctx)
	assert.NotPanics(t, func() {
		RemoveConvExtForUserInSpace("nonexistent", "sp99", "ch99", 2)
	})
}

// ---------------------------------------------------------------------------
// TestRemoveConvExtForUser
// ---------------------------------------------------------------------------

// TestRemoveConvExtForUser_HappyPath verifies the DM ext row for the peer is
// deleted across all spaces.
func TestRemoveConvExtForUser_HappyPath(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	uid, peerUID := "ua", "ub"
	// Two DM rows in different spaces — both must be deleted.
	insertRow(t, db, uid, "spA", targetTypeDM, peerUID)
	insertRow(t, db, uid, "spB", targetTypeDM, peerUID)
	// Row that must NOT be deleted — different peer.
	insertRow(t, db, uid, "spA", targetTypeDM, "ucOther")

	RemoveConvExtForUser(uid, peerUID)

	assert.Equal(t, 0, countRows(t, db, uid, "spA", targetTypeDM, peerUID), "spA DM row gone")
	assert.Equal(t, 0, countRows(t, db, uid, "spB", targetTypeDM, peerUID), "spB DM row gone")
	assert.Equal(t, 1, countRows(t, db, uid, "spA", targetTypeDM, "ucOther"),
		"other peer DM row must be untouched")
}

// TestRemoveConvExtForUser_NilGlobalDB verifies no panic when singleton is nil.
func TestRemoveConvExtForUser_NilGlobalDB(t *testing.T) {
	globalConvExtDB = nil
	globalConvExtDBOnce = sync.Once{}
	assert.NotPanics(t, func() {
		RemoveConvExtForUser("ua", "ub")
	})
}

// TestRemoveConvExtForUser_NotExist verifies idempotency.
func TestRemoveConvExtForUser_NotExist(t *testing.T) {
	ctx := newCtxForTest(t)
	initGlobalConvExtDBForTest(t, ctx)
	assert.NotPanics(t, func() {
		RemoveConvExtForUser("noone", "nobody")
	})
}

// ---------------------------------------------------------------------------
// TestRemoveConvExtForChannel
// ---------------------------------------------------------------------------

// TestRemoveConvExtForChannel_GroupHappyPath verifies that all user ext rows for
// a group channel (target_type=2) are deleted across all users.
func TestRemoveConvExtForChannel_GroupHappyPath(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	groupID := "grp_dissolve"
	insertRow(t, db, "u10", "sp1", targetTypeGroup, groupID)
	insertRow(t, db, "u11", "sp1", targetTypeGroup, groupID)
	// Unrelated row — must not be deleted.
	insertRow(t, db, "u10", "sp1", targetTypeGroup, "grpOther")

	RemoveConvExtForChannel(groupID, targetTypeGroup)

	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeGroup, groupID), "all group rows gone")
	assert.Equal(t, 1, countRowsByChannel(t, db, targetTypeGroup, "grpOther"), "other group untouched")
}

// TestRemoveConvExtForChannel_GroupCascadesThreads verifies that when
// channelType=2 (group), child thread rows are also deleted.
func TestRemoveConvExtForChannel_GroupCascadesThreads(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	groupID := "grp_full"
	threadID1 := groupID + "____s1"
	threadID2 := groupID + "____s2"
	otherGroupThread := "grpX____s1"

	for _, uid := range []string{"u20", "u21"} {
		insertRow(t, db, uid, "sp1", targetTypeGroup, groupID)
		insertRow(t, db, uid, "sp1", targetTypeThread, threadID1)
		insertRow(t, db, uid, "sp1", targetTypeThread, threadID2)
	}
	insertRow(t, db, "u20", "sp1", targetTypeThread, otherGroupThread)

	RemoveConvExtForChannel(groupID, targetTypeGroup)

	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeGroup, groupID), "group rows gone")
	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeThread, threadID1), "thread1 rows gone")
	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeThread, threadID2), "thread2 rows gone")
	assert.Equal(t, 1, countRowsByChannel(t, db, targetTypeThread, otherGroupThread),
		"other group's thread must be untouched")
}

// TestRemoveConvExtForChannel_ThreadHappyPath verifies that for a thread channel
// (target_type=5) all user ext rows for that exact thread are deleted; no cascade.
func TestRemoveConvExtForChannel_ThreadHappyPath(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)

	groupID := "grpT"
	threadID := groupID + "____sT"

	insertRow(t, db, "u30", "sp1", targetTypeThread, threadID)
	insertRow(t, db, "u31", "sp1", targetTypeThread, threadID)
	// Parent group row — must NOT be deleted.
	insertRow(t, db, "u30", "sp1", targetTypeGroup, groupID)

	RemoveConvExtForChannel(threadID, targetTypeThread)

	assert.Equal(t, 0, countRowsByChannel(t, db, targetTypeThread, threadID), "thread rows gone")
	assert.Equal(t, 1, countRowsByChannel(t, db, targetTypeGroup, groupID),
		"parent group row must be untouched")
}

// TestRemoveConvExtForChannel_NilGlobalDB verifies no panic when singleton is nil.
func TestRemoveConvExtForChannel_NilGlobalDB(t *testing.T) {
	globalConvExtDB = nil
	globalConvExtDBOnce = sync.Once{}
	assert.NotPanics(t, func() {
		RemoveConvExtForChannel("ch", 2)
	})
}

// ---------------------------------------------------------------------------
// UnfollowGroupsTx must clear auto_follow_threads (bug fix #1)
//
// 删除分类时 category 模块调用 UnfollowGroupsTx 把分类下的群批量取关。
// 若漏掉 auto_follow_threads=0，后续这些群里新建子区时 OnThreadCreated 仍会按
// auto_follow_threads=1 把已取关的用户当作 fanout 目标，违反"取消关注 =
// 不再自动跟随新子区"的语义。本测试与 service.UnfollowChannel 同语义守护。
// ---------------------------------------------------------------------------

func TestUnfollowGroupsTx_ClearsAutoFollowThreads(t *testing.T) {
	ctx := newCtxForTest(t)
	db := initGlobalConvExtDBForTest(t, ctx)
	_, err := ctx.DB().DeleteFrom(table).Exec()
	require.NoError(t, err)

	const uid, space, grp1, grp2 = "u-cat-uf", "sp-cat", "grp-cat-1", "grp-cat-2"
	// 模拟用户对两个群都开启了级联关注（FollowChannel 写入的状态）。
	one := int8(1)
	zero := int8(0)
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, grp1, ConvExtFields{
		GroupUnfollowed:   &zero,
		AutoFollowThreads: &one,
	}))
	require.NoError(t, db.Upsert(uid, space, targetTypeGroup, grp2, ConvExtFields{
		GroupUnfollowed:   &zero,
		AutoFollowThreads: &one,
	}))

	tx, err := db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	require.NoError(t, UnfollowGroupsTx(tx, uid, space, []string{grp1, grp2}))
	require.NoError(t, tx.Commit())

	for _, grp := range []string{grp1, grp2} {
		row, err := db.Get(uid, space, targetTypeGroup, grp)
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.Equal(t, int8(1), row.GroupUnfollowed, "%s 应被标记为取消关注", grp)
		assert.Equal(t, int8(0), row.AutoFollowThreads,
			"%s 必须清掉 auto_follow_threads，否则 OnThreadCreated 还会把已取关用户当目标", grp)
	}
}
