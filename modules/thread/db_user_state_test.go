package thread

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upsertIntent 是测试内的小工具：开 tx、调 UpsertArchiveIntentTx、提交。
func upsertIntent(t *testing.T, db *DB, uid, groupNo, shortID string, intent int, version int64) {
	t.Helper()
	tx, err := db.session.Begin()
	require.NoError(t, err)
	require.NoError(t, db.UpsertArchiveIntentTx(tx, uid, groupNo, shortID, intent, version))
	require.NoError(t, tx.Commit())
}

// TestUserStateUpsertAndQuery 覆盖 T2 验证：upsert 幂等 + query 命中/未命中(空 map)。
func TestUserStateUpsertAndQuery(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	uid := "u_alice"
	refA := ShortRef{GroupNo: "g1", ShortID: "t1"}
	refB := ShortRef{GroupNo: "g1", ShortID: "t2"}

	// 未写入前：查询返回空 map（未命中）。
	got, err := db.QueryUserStates(uid, []ShortRef{refA, refB})
	require.NoError(t, err)
	assert.Empty(t, got, "no rows yet")

	// 写归档意图 intent=1。
	upsertIntent(t, db, uid, refA.GroupNo, refA.ShortID, 1, 10)
	got, err = db.QueryUserStates(uid, []ShortRef{refA, refB})
	require.NoError(t, err)
	require.Contains(t, got, "g1____t1")
	assert.Equal(t, 1, got["g1____t1"].ArchiveIntent)
	assert.Equal(t, int64(10), got["g1____t1"].Version)
	assert.NotContains(t, got, "g1____t2", "t2 never written")

	// 幂等：同键再 upsert 覆盖为 intent=0、version=20，不产生第二行。
	upsertIntent(t, db, uid, refA.GroupNo, refA.ShortID, 0, 20)
	got, err = db.QueryUserStates(uid, []ShortRef{refA})
	require.NoError(t, err)
	require.Contains(t, got, "g1____t1")
	assert.Equal(t, 0, got["g1____t1"].ArchiveIntent)
	assert.Equal(t, int64(20), got["g1____t1"].Version)

	var cnt int
	_, err = db.session.SelectBySql(
		"SELECT COUNT(*) FROM thread_user_state WHERE uid=? AND group_no=? AND short_id=?",
		uid, refA.GroupNo, refA.ShortID,
	).Load(&cnt)
	require.NoError(t, err)
	assert.Equal(t, 1, cnt, "upsert must not create duplicate rows")

	// per-user 隔离：另一个 uid 查同 refs 未命中。
	got, err = db.QueryUserStates("u_bob", []ShortRef{refA})
	require.NoError(t, err)
	assert.Empty(t, got, "bob has no state")

	// 空 refs / 空 uid 短路。
	got, err = db.QueryUserStates(uid, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = db.QueryUserStates("", []ShortRef{refA})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestDeleteUserStatesForThread 覆盖 T-GC 的 DB 原语：按 thread 清所有用户状态行。
func TestDeleteUserStatesForThread(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	upsertIntent(t, db, "u1", "g1", "t1", 1, 1)
	upsertIntent(t, db, "u2", "g1", "t1", 1, 1)
	upsertIntent(t, db, "u1", "g1", "t2", 1, 1) // 不同 thread，不该被删

	require.NoError(t, db.DeleteUserStatesForThread("g1", "t1"))

	got, err := db.QueryUserStates("u1", []ShortRef{{GroupNo: "g1", ShortID: "t1"}})
	require.NoError(t, err)
	assert.Empty(t, got, "t1 rows gone")
	got, err = db.QueryUserStates("u2", []ShortRef{{GroupNo: "g1", ShortID: "t1"}})
	require.NoError(t, err)
	assert.Empty(t, got, "t1 rows gone for u2 too")
	got, err = db.QueryUserStates("u1", []ShortRef{{GroupNo: "g1", ShortID: "t2"}})
	require.NoError(t, err)
	assert.Contains(t, got, "g1____t2", "t2 survives")
}

// TestQueryMuteForUID 覆盖 T2 的 mute 批量 helper（一个 uid × 多 thread）。
func TestQueryMuteForUID(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := NewDB(ctx)

	// 用现有 UpsertSetting 落 mute 数据。
	require.NoError(t, db.UpsertSetting(&SettingModel{GroupNo: "g1", ShortID: "t1", UID: "u_alice", Mute: 1, Version: 1}))
	require.NoError(t, db.UpsertSetting(&SettingModel{GroupNo: "g1", ShortID: "t2", UID: "u_alice", Mute: 0, Version: 1}))
	require.NoError(t, db.UpsertSetting(&SettingModel{GroupNo: "g1", ShortID: "t1", UID: "u_bob", Mute: 1, Version: 1}))

	refs := []ShortRef{{GroupNo: "g1", ShortID: "t1"}, {GroupNo: "g1", ShortID: "t2"}, {GroupNo: "g1", ShortID: "t3"}}
	m, err := db.QueryMuteForUID("u_alice", refs)
	require.NoError(t, err)
	assert.Equal(t, 1, m["g1____t1"], "alice muted t1")
	// t2 mute=0 命中但值 0；t3 无行不出现。
	v, ok := m["g1____t2"]
	assert.True(t, ok)
	assert.Equal(t, 0, v)
	assert.NotContains(t, m, "g1____t3")

	// per-user 隔离：bob 的 mute 不混进 alice。
	mb, err := db.QueryMuteForUID("u_bob", refs)
	require.NoError(t, err)
	assert.Equal(t, 1, mb["g1____t1"])
	assert.NotContains(t, mb, "g1____t2", "bob has no t2 setting")

	// 空短路。
	empty, err := db.QueryMuteForUID("u_alice", nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}
