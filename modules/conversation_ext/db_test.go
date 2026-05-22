//go:build integration

package conversation_ext

import (
	"os"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDBForTest 连接到 testenv-mysql-1 容器中的测试库。
// 环境变量 CONV_EXT_TEST_MYSQL_ADDR 覆盖默认连接串。
// 同 db_pinned_test.go 的模式：跳过全模块迁移，仅操作单张表。
func newDBForTest(t *testing.T) *DB {
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
	require.NoError(t, err, "clean "+table+" before test")
	// followVersionTable 必须一起清掉，否则上一个 test 留下的 follow_version
	// 会让本 test 的 expectedVersion=0 失败为 ErrVersionConflict。
	_, err = ctx.DB().DeleteFrom(followVersionTable).Exec()
	require.NoError(t, err, "clean "+followVersionTable+" before test")
	return NewDB(ctx)
}

// int8Ptr 等是 table-driven 测试中提高可读性的小辅助。
func int8Ptr(v int8) *int8    { return &v }
func int64Ptr(v int64) *int64 { return &v }
func intPtr(v int) *int       { return &v }

// ---------------------------------------------------------------------------
// Upsert + Get
// ---------------------------------------------------------------------------

func TestDB_Upsert_Get_BasicInsert(t *testing.T) {
	db := newDBForTest(t)

	err := db.Upsert("u1", "s1", 1, "dm-target", ConvExtFields{
		FollowedDM: int8Ptr(1),
		FollowSort: intPtr(10),
	})
	require.NoError(t, err)

	m, err := db.Get("u1", "s1", 1, "dm-target")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "u1", m.UID)
	assert.Equal(t, "s1", m.SpaceID)
	assert.Equal(t, uint8(1), m.TargetType)
	assert.Equal(t, "dm-target", m.TargetID)
	assert.Equal(t, int8(1), m.FollowedDM)
	assert.Equal(t, 10, m.FollowSort)
}

func TestDB_Get_NotFound_ReturnsNilNil(t *testing.T) {
	db := newDBForTest(t)

	m, err := db.Get("ghost", "s1", 1, "nobody")
	require.NoError(t, err)
	assert.Nil(t, m, "not-found row should return nil model without error")
}

func TestDB_Upsert_Update_ExistingRow(t *testing.T) {
	db := newDBForTest(t)

	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-x", ConvExtFields{
		FollowedDM: int8Ptr(0),
		FollowSort: intPtr(5),
	}))

	// 更新：follow_dm 翻转，sort 変更
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-x", ConvExtFields{
		FollowedDM: int8Ptr(1),
		FollowSort: intPtr(99),
	}))

	m, err := db.Get("u1", "s1", 1, "dm-x")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.FollowedDM)
	assert.Equal(t, 99, m.FollowSort)
}

func TestDB_Upsert_DMCategoryID(t *testing.T) {
	db := newDBForTest(t)

	catID := "cat-uuid-42"
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-cat", ConvExtFields{
		FollowedDM:   int8Ptr(1),
		DMCategoryID: &catID,
	}))

	m, err := db.Get("u1", "s1", 1, "dm-cat")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.NotNil(t, m.DMCategoryID)
	assert.Equal(t, catID, *m.DMCategoryID)

	// ClearDMCategory → NULL
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-cat", ConvExtFields{
		ClearDMCategory: true,
	}))

	m2, err := db.Get("u1", "s1", 1, "dm-cat")
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Nil(t, m2.DMCategoryID)
}

func TestDB_Upsert_GroupUnfollowed(t *testing.T) {
	db := newDBForTest(t)

	require.NoError(t, db.Upsert("u1", "s1", 2, "grp-1", ConvExtFields{
		GroupUnfollowed: int8Ptr(1),
	}))

	m, err := db.Get("u1", "s1", 2, "grp-1")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.GroupUnfollowed)
}

func TestDB_Upsert_AutoFollowThreads(t *testing.T) {
	db := newDBForTest(t)

	// Insert：通过新字段把群行写成"已关注且自动级联子区"。
	require.NoError(t, db.Upsert("u1", "s1", 2, "grp-cascade", ConvExtFields{
		GroupUnfollowed:   int8Ptr(0),
		AutoFollowThreads: int8Ptr(1),
	}))

	m, err := db.Get("u1", "s1", 2, "grp-cascade")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.GroupUnfollowed)
	assert.Equal(t, int8(1), m.AutoFollowThreads,
		"AutoFollowThreads 字段应从 ConvExtFields 落库并通过 Get 读出")

	// Update：再次 upsert 关掉级联开关，验证 ON DUPLICATE KEY UPDATE 路径。
	require.NoError(t, db.Upsert("u1", "s1", 2, "grp-cascade", ConvExtFields{
		AutoFollowThreads: int8Ptr(0),
	}))
	m, err = db.Get("u1", "s1", 2, "grp-cascade")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(0), m.AutoFollowThreads)
	// GroupUnfollowed 未传，保持上次值 0。
	assert.Equal(t, int8(0), m.GroupUnfollowed)
}

func TestDB_Upsert_NilFields_DoNotOverwrite(t *testing.T) {
	db := newDBForTest(t)

	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-nil", ConvExtFields{
		FollowedDM: int8Ptr(1),
		FollowSort: intPtr(7),
	}))

	// Upsert with empty fields — original values must survive
	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-nil", ConvExtFields{}))

	m, err := db.Get("u1", "s1", 1, "dm-nil")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, int8(1), m.FollowedDM)
	assert.Equal(t, 7, m.FollowSort)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDB_Delete_ExistingRow(t *testing.T) {
	db := newDBForTest(t)

	require.NoError(t, db.Upsert("u1", "s1", 1, "dm-del", ConvExtFields{FollowedDM: int8Ptr(1)}))

	m, err := db.Get("u1", "s1", 1, "dm-del")
	require.NoError(t, err)
	require.NotNil(t, m)

	require.NoError(t, db.Delete("u1", "s1", 1, "dm-del"))

	m2, err := db.Get("u1", "s1", 1, "dm-del")
	require.NoError(t, err)
	assert.Nil(t, m2)
}

func TestDB_Delete_NotFound_NoError(t *testing.T) {
	db := newDBForTest(t)
	err := db.Delete("ghost", "s1", 1, "nothing")
	require.NoError(t, err, "deleting a non-existent row should not error")
}

// ---------------------------------------------------------------------------
// ListFollowedDM
// ---------------------------------------------------------------------------

func TestDB_ListFollowedDM_BasicOrder(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	cat1 := "cat-uuid-1"
	cat2 := "cat-uuid-2"

	// 関注 DM：異なる category + sort
	require.NoError(t, db.Upsert(uid, space, 1, "dm-a", ConvExtFields{FollowedDM: int8Ptr(1), DMCategoryID: &cat2, FollowSort: intPtr(5)}))
	require.NoError(t, db.Upsert(uid, space, 1, "dm-b", ConvExtFields{FollowedDM: int8Ptr(1), DMCategoryID: &cat1, FollowSort: intPtr(3)}))
	require.NoError(t, db.Upsert(uid, space, 1, "dm-c", ConvExtFields{FollowedDM: int8Ptr(1), DMCategoryID: &cat1, FollowSort: intPtr(1)}))
	// 未关注的 DM（不应出现在列表里）
	require.NoError(t, db.Upsert(uid, space, 1, "dm-unfollowed", ConvExtFields{FollowedDM: int8Ptr(0)}))
	// 群（type=2，不应出现在列表里）
	require.NoError(t, db.Upsert(uid, space, 2, "grp-1", ConvExtFields{FollowedDM: int8Ptr(0)}))

	list, err := db.ListFollowedDM(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 3)

	// 期望排序：(dm_category_id ASC, follow_sort ASC)
	// cat1/sort1 → cat1/sort3 → cat2/sort5
	assert.Equal(t, "dm-c", list[0].TargetID)
	assert.Equal(t, "dm-b", list[1].TargetID)
	assert.Equal(t, "dm-a", list[2].TargetID)
}

func TestDB_ListFollowedDM_Empty(t *testing.T) {
	db := newDBForTest(t)
	list, err := db.ListFollowedDM("u_nobody", "s1")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestDB_ListFollowedDM_SpaceIsolation(t *testing.T) {
	db := newDBForTest(t)
	const uid = "u1"

	require.NoError(t, db.Upsert(uid, "sA", 1, "dm-a", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert(uid, "sB", 1, "dm-b", ConvExtFields{FollowedDM: int8Ptr(1)}))

	listA, err := db.ListFollowedDM(uid, "sA")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	assert.Equal(t, "dm-a", listA[0].TargetID)

	listB, err := db.ListFollowedDM(uid, "sB")
	require.NoError(t, err)
	require.Len(t, listB, 1)
	assert.Equal(t, "dm-b", listB[0].TargetID)
}

// ---------------------------------------------------------------------------
// ListUnfollowedGroups
// ---------------------------------------------------------------------------

func TestDB_ListUnfollowedGroups_Basic(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, 2, "grp-unfollowed", ConvExtFields{GroupUnfollowed: int8Ptr(1)}))
	require.NoError(t, db.Upsert(uid, space, 2, "grp-followed", ConvExtFields{GroupUnfollowed: int8Ptr(0)}))
	// DM（type=1）不应出现
	require.NoError(t, db.Upsert(uid, space, 1, "dm-x", ConvExtFields{GroupUnfollowed: int8Ptr(1)}))

	list, err := db.ListUnfollowedGroups(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "grp-unfollowed", list[0].TargetID)
}

func TestDB_ListUnfollowedGroups_Empty(t *testing.T) {
	db := newDBForTest(t)
	list, err := db.ListUnfollowedGroups("u_nobody", "s1")
	require.NoError(t, err)
	assert.Empty(t, list)
}

// ---------------------------------------------------------------------------
// UpdateSort (CAS)
// ---------------------------------------------------------------------------

func TestDB_UpdateSort_Success(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, 1, "dm-1", ConvExtFields{FollowedDM: int8Ptr(1), FollowSort: intPtr(1)}))
	require.NoError(t, db.Upsert(uid, space, 1, "dm-2", ConvExtFields{FollowedDM: int8Ptr(1), FollowSort: intPtr(2)}))
	require.NoError(t, db.Upsert(uid, space, 1, "dm-3", ConvExtFields{FollowedDM: int8Ptr(1), FollowSort: intPtr(3)}))

	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: 1, TargetID: "dm-3"},
		{TargetType: 1, TargetID: "dm-1"},
		{TargetType: 1, TargetID: "dm-2"},
	}, 0 /* expectedVersion */)
	require.NoError(t, err)

	// dm-3 排到首位（follow_sort=1）。
	// Phase 3 之后 per-row version 不再被 UpdateSort 推进，所以这里不断言 m.Version。
	m, err := db.Get(uid, space, 1, "dm-3")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, 1, m.FollowSort)

	m2, err := db.Get(uid, space, 1, "dm-1")
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Equal(t, 2, m2.FollowSort)

	m3, err := db.Get(uid, space, 1, "dm-2")
	require.NoError(t, err)
	require.NotNil(t, m3)
	assert.Equal(t, 3, m3.FollowSort)
}

func TestDB_UpdateSort_VersionConflict(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, 1, "dm-1", ConvExtFields{FollowedDM: int8Ptr(1)}))

	// 正常推进 follow_version 0→1
	require.NoError(t, db.UpdateSort(uid, space, []SortItem{
		{TargetType: 1, TargetID: "dm-1"},
	}, 0))

	// 用旧 version=0 重试 → ErrVersionConflict
	err := db.UpdateSort(uid, space, []SortItem{
		{TargetType: 1, TargetID: "dm-1"},
	}, 0)
	assert.ErrorIs(t, err, ErrVersionConflict)
}

func TestDB_UpdateSort_EmptyItems_NoOp(t *testing.T) {
	db := newDBForTest(t)
	// items 为空时跳过版本校验直接返回 nil
	err := db.UpdateSort("u1", "s1", []SortItem{}, 0)
	require.NoError(t, err)
}

func TestDB_UpdateSort_VersionBumps_Sequential(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"
	fvDB := NewFollowVersionDB(newCtxForTest(t))
	// Wipe so the user starts at follow_version=0.
	_, _ = db.session.DeleteFrom(followVersionTable).Exec()

	require.NoError(t, db.Upsert(uid, space, 1, "dm-seq", ConvExtFields{FollowedDM: int8Ptr(1)}))

	// PR review Round-3 Blocking #1/#2: CAS anchor is user_follow_version, not
	// per-row version. Each successful UpdateSort bumps follow_version by 1.
	for expectedVer := int64(0); expectedVer < 3; expectedVer++ {
		require.NoError(t, db.UpdateSort(uid, space, []SortItem{
			{TargetType: 1, TargetID: "dm-seq"},
		}, expectedVer))

		v, err := fvDB.Get(uid, space)
		require.NoError(t, err)
		assert.Equal(t, expectedVer+1, v, "follow_version must advance monotonically")
	}
}

// ---------------------------------------------------------------------------
// Concurrent UpdateSort — race detection
// ---------------------------------------------------------------------------

func TestDB_UpdateSort_ConcurrentConflict(t *testing.T) {
	db := newDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Upsert(uid, space, 1, "dm-race", ConvExtFields{FollowedDM: int8Ptr(1)}))

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	successCh := make(chan struct{}, workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			err := db.UpdateSort(uid, space, []SortItem{
				{TargetType: 1, TargetID: "dm-race"},
			}, 0)
			if err == nil {
				successCh <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(successCh)

	// 起始 version=0 时只允许 1 次成功
	successes := 0
	for range successCh {
		successes++
	}
	assert.Equal(t, 1, successes, "only one concurrent UpdateSort(version=0) should succeed")
}

// ---------------------------------------------------------------------------
// Unique key / isolation
// ---------------------------------------------------------------------------

func TestDB_Upsert_UniqueKey_SameKeyDifferentUsers(t *testing.T) {
	db := newDBForTest(t)

	// 同 target、不同 uid 应各自独立成行
	require.NoError(t, db.Upsert("u1", "s1", 1, "shared-dm", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert("u2", "s1", 1, "shared-dm", ConvExtFields{FollowedDM: int8Ptr(0)}))

	m1, err := db.Get("u1", "s1", 1, "shared-dm")
	require.NoError(t, err)
	require.NotNil(t, m1)
	assert.Equal(t, int8(1), m1.FollowedDM)

	m2, err := db.Get("u2", "s1", 1, "shared-dm")
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Equal(t, int8(0), m2.FollowedDM)
}

func TestDB_Upsert_UniqueKey_SameKeyDifferentTypes(t *testing.T) {
	db := newDBForTest(t)

	// 同 target_id 但 target_type 不同时是不同的行
	require.NoError(t, db.Upsert("u1", "s1", 1, "target-x", ConvExtFields{FollowedDM: int8Ptr(1)}))
	require.NoError(t, db.Upsert("u1", "s1", 2, "target-x", ConvExtFields{GroupUnfollowed: int8Ptr(1)}))

	m1, err := db.Get("u1", "s1", 1, "target-x")
	require.NoError(t, err)
	require.NotNil(t, m1)
	assert.Equal(t, int8(1), m1.FollowedDM)

	m2, err := db.Get("u1", "s1", 2, "target-x")
	require.NoError(t, err)
	require.NotNil(t, m2)
	assert.Equal(t, int8(1), m2.GroupUnfollowed)
}
