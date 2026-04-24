package user

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPinnedDBForTest 连接到 testenv-mysql-1 容器中的 pinned_test 库，
// 该库只预先建好 user_pinned_channel 表（见 sql/user-20260424-01.sql）。
// 刻意绕开 testutil.NewTestServer：后者会触发全模块迁移，遇到 Go map
// 随机顺序下的跨模块表依赖（space → group/robot）而 panic。
// 这里只测 DB 层，无需其他模块参与。
func newPinnedDBForTest(t *testing.T) *PinnedDB {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1)/pinned_test?charset=utf8mb4&parseTime=true"
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)
	_, err := ctx.DB().DeleteFrom("user_pinned_channel").Exec()
	require.NoError(t, err, "clean user_pinned_channel before test")
	return NewPinnedDB(ctx)
}

func TestPinnedDB_Add_List_Remove(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c2", 2, pinnedMaxPerSpace))

	list, err := db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "c1", list[0].ChannelID)
	assert.Equal(t, 1, list[0].SortOrder)
	assert.Equal(t, "c2", list[1].ChannelID)
	assert.Equal(t, 2, list[1].SortOrder)

	require.NoError(t, db.Remove(uid, space, "c1", 2))
	list, err = db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "c2", list[0].ChannelID)
}

func TestPinnedDB_Add_Duplicate(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))
	err := db.Add(uid, space, "c1", 2, pinnedMaxPerSpace)
	assert.True(t, errors.Is(err, ErrPinnedAlreadyExists), "expected ErrPinnedAlreadyExists, got %v", err)
}

func TestPinnedDB_Add_LimitExceeded(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	for i := 0; i < pinnedMaxPerSpace; i++ {
		cid := "c" + string(rune('a'+i))
		require.NoError(t, db.Add(uid, space, cid, 2, pinnedMaxPerSpace))
	}

	err := db.Add(uid, space, "overflow", 2, pinnedMaxPerSpace)
	assert.True(t, errors.Is(err, ErrPinnedLimitExceeded), "expected ErrPinnedLimitExceeded, got %v", err)

	list, err := db.List(uid, space)
	require.NoError(t, err)
	assert.Len(t, list, pinnedMaxPerSpace, "overflow insert must have rolled back")
}

func TestPinnedDB_Add_SpaceIsolation(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid = "u1"

	require.NoError(t, db.Add(uid, "sA", "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, "sB", "c1", 2, pinnedMaxPerSpace))

	listA, err := db.List(uid, "sA")
	require.NoError(t, err)
	assert.Len(t, listA, 1)

	listB, err := db.List(uid, "sB")
	require.NoError(t, err)
	assert.Len(t, listB, 1)
}

func TestPinnedDB_UpdateSort_Success(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c2", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c3", 2, pinnedMaxPerSpace))

	// 客户端提交相反的顺序，且 SortOrder 字段填随机值（应被忽略）
	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c3", ChannelType: 2, SortOrder: 999},
		{ChannelID: "c1", ChannelType: 2, SortOrder: -5},
		{ChannelID: "c2", ChannelType: 2, SortOrder: 0},
	})
	require.NoError(t, err)

	list, err := db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "c3", list[0].ChannelID)
	assert.Equal(t, 1, list[0].SortOrder)
	assert.Equal(t, "c1", list[1].ChannelID)
	assert.Equal(t, 2, list[1].SortOrder)
	assert.Equal(t, "c2", list[2].ChannelID)
	assert.Equal(t, 3, list[2].SortOrder)
}

func TestPinnedDB_UpdateSort_RejectUnknownChannel(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))

	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "ghost", ChannelType: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未置顶")

	// 确保原始 sort_order 未被修改
	list, err := db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, 1, list[0].SortOrder)
}

func TestPinnedDB_UpdateSort_RejectDuplicate(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))

	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "c1", ChannelType: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "重复")
}

func TestPinnedDB_UpdateSort_CrossUserIsolation(t *testing.T) {
	db := newPinnedDBForTest(t)
	const space = "s1"

	require.NoError(t, db.Add("u1", space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add("u2", space, "c1", 2, pinnedMaxPerSpace))

	// u1 不能通过 UpdateSort 影响 u2 的数据（即使 c1 对 u2 也存在）。
	// 这里构造一个只 u2 有、u1 没有的频道来验证。
	require.NoError(t, db.Add("u2", space, "c_only_u2", 2, pinnedMaxPerSpace))

	err := db.UpdateSort("u1", space, []PinnedSortItem{
		{ChannelID: "c_only_u2", ChannelType: 2},
	})
	require.Error(t, err, "u1 不应能排序 u2 才有的频道")
}

func TestPinnedDB_RemoveByUIDAndChannel_AllSpaces(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid = "u1"

	require.NoError(t, db.Add(uid, "sA", "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, "sB", "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, "sA", "c2", 2, pinnedMaxPerSpace))

	require.NoError(t, db.RemoveByUIDAndChannel(uid, "c1", 2))

	listA, err := db.List(uid, "sA")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	assert.Equal(t, "c2", listA[0].ChannelID)

	listB, err := db.List(uid, "sB")
	require.NoError(t, err)
	assert.Empty(t, listB)
}
