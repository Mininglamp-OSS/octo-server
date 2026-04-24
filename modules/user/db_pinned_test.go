package user

import (
	"errors"
	"fmt"
	"os"
	"sync"
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
	addr := os.Getenv("PINNED_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/pinned_test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
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

// TestPinnedDB_Add_ConcurrentLimit 验证并发 Add 不会越过 pinnedMaxPerSpace。
// 这是 reviewer 指出的 Critical 问题的回归测试：
// REPEATABLE READ 下 consistent read 的 COUNT 看不到其他事务的插入，
// 必须用 SELECT ... FOR UPDATE 才能在并发下保证上限。
func TestPinnedDB_Add_ConcurrentLimit(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"
	const workers = 20

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			cid := fmt.Sprintf("c-%02d", i)
			_ = db.Add(uid, space, cid, 2, pinnedMaxPerSpace)
		}()
	}
	wg.Wait()

	list, err := db.List(uid, space)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(list), pinnedMaxPerSpace,
		"concurrent Add exceeded limit: got %d, max %d", len(list), pinnedMaxPerSpace)
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

	// 客户端提交相反顺序；服务端按数组下标重编号
	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c3", ChannelType: 2},
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "c2", ChannelType: 2},
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
	require.NoError(t, db.Add(uid, space, "c2", 2, pinnedMaxPerSpace))

	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "ghost", ChannelType: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未置顶")

	// 确保原始 sort_order 未被修改
	list, err := db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, 1, list[0].SortOrder)
	assert.Equal(t, 2, list[1].SortOrder)
}

func TestPinnedDB_UpdateSort_RejectDuplicate(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c2", 2, pinnedMaxPerSpace))

	// 全量长度匹配（2 == 2），但 items 内重复
	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "c1", ChannelType: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "重复")
}

func TestPinnedDB_UpdateSort_RejectPartialSubmit(t *testing.T) {
	db := newPinnedDBForTest(t)
	const uid, space = "u1", "s1"

	require.NoError(t, db.Add(uid, space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c2", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add(uid, space, "c3", 2, pinnedMaxPerSpace))

	// 只提交 2/3，应被拒绝
	err := db.UpdateSort(uid, space, []PinnedSortItem{
		{ChannelID: "c2", ChannelType: 2},
		{ChannelID: "c1", ChannelType: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "必须提交所有")

	// 原始排序未被修改
	list, err := db.List(uid, space)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "c1", list[0].ChannelID)
	assert.Equal(t, "c2", list[1].ChannelID)
	assert.Equal(t, "c3", list[2].ChannelID)
}

func TestPinnedDB_UpdateSort_CrossUserIsolation(t *testing.T) {
	db := newPinnedDBForTest(t)
	const space = "s1"

	require.NoError(t, db.Add("u1", space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add("u2", space, "c1", 2, pinnedMaxPerSpace))
	require.NoError(t, db.Add("u2", space, "c_only_u2", 2, pinnedMaxPerSpace))

	// u1 只有 c1；提交 c_only_u2（u2 的频道）应报未置顶错。
	err := db.UpdateSort("u1", space, []PinnedSortItem{
		{ChannelID: "c_only_u2", ChannelType: 2},
	})
	require.Error(t, err, "u1 不应能排序 u2 才有的频道")
	assert.Contains(t, err.Error(), "未置顶")
}

func TestPinnedDB_AreSpaceMembers(t *testing.T) {
	db := newPinnedDBForTest(t)

	// 清表并准备夹具
	for _, tbl := range []string{"space_member", "space"} {
		_, err := db.session.DeleteFrom(tbl).Where("1=1").Exec()
		require.NoError(t, err)
	}
	_, err := db.session.InsertBySql("INSERT INTO space(space_id,status) VALUES ('sp_active',1),('sp_disabled',0)").Exec()
	require.NoError(t, err)
	_, err = db.session.InsertBySql(
		"INSERT INTO space_member(space_id,uid,status) VALUES " +
			"('sp_active','u1',1),('sp_active','u2',1),('sp_active','u3',0)," +
			"('sp_disabled','u1',1),('sp_disabled','u2',1)",
	).Exec()
	require.NoError(t, err)

	tests := []struct {
		name       string
		spaceID    string
		uid1, uid2 string
		want       bool
	}{
		{"both active members", "sp_active", "u1", "u2", true},
		{"one inactive member", "sp_active", "u1", "u3", false},
		{"peer not a member", "sp_active", "u1", "u_other", false},
		{"space disabled", "sp_disabled", "u1", "u2", false},
		{"empty spaceID", "", "u1", "u2", false},
		{"empty uid", "sp_active", "u1", "", false},
		{"same uid twice", "sp_active", "u1", "u1", false}, // COUNT(DISTINCT uid) = 1
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := db.AreSpaceMembers(tc.spaceID, tc.uid1, tc.uid2)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}

func TestPinnedDB_QueryPeerRobotInfo(t *testing.T) {
	db := newPinnedDBForTest(t)

	for _, tbl := range []string{"robot", "user"} {
		_, err := db.session.DeleteFrom(tbl).Where("1=1").Exec()
		require.NoError(t, err)
	}
	_, err := db.session.InsertBySql(
		"INSERT INTO `user`(uid,robot) VALUES " +
			"('human_u',0),('bot_active',1),('bot_disabled',1),('bot_no_row',1)",
	).Exec()
	require.NoError(t, err)
	_, err = db.session.InsertBySql(
		"INSERT INTO robot(robot_id,creator_uid,status) VALUES " +
			"('bot_active','creator_x',1),('bot_disabled','creator_y',0)",
	).Exec()
	require.NoError(t, err)

	tests := []struct {
		name        string
		peerUID     string
		wantIsRobot bool
		wantCreator string
	}{
		{"regular user", "human_u", false, ""},
		{"active bot", "bot_active", true, "creator_x"},
		{"disabled bot falls back to empty creator", "bot_disabled", true, ""},
		{"user.robot=1 with no robot row", "bot_no_row", true, ""},
		{"unknown peer treated as non-robot", "ghost", false, ""},
		{"empty peer", "", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isRobot, creator, err := db.QueryPeerRobotInfo(tc.peerUID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantIsRobot, isRobot)
			assert.Equal(t, tc.wantCreator, creator)
		})
	}
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
