package user

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetDeviceOnlines_ExecutesBatchQuery 是对 OCT-61 批量在线查询的 DB-backed 回归。
//
// 必须真跑一次 SQL：这个用例专门守 device_flag IN 参数的元素类型 —— 早期实现用 []uint8，
// 而 Go 的 []uint8 即 []byte，dbr 会当二进制字面量而非 IN 列表展开，MySQL 1064 恒失败、
// GetDeviceOnlines 每次都 fail-open，静音功能形同虚设。build/vet 和纯函数单测都发现不了，
// 只有执行到 SQL 才暴露，故这里必须连库。无 MySQL 时随 user 包其它 DB-backed 用例一起在 CI 跑。
func TestGetDeviceOnlines_ExecutesBatchQuery(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	odb := newOnlineDB(ctx)
	// u1 Web 在线、u2 PC 在线：应命中；u3 APP 在线（设备不在查询集合）、u4 Web 但离线：应被过滤。
	seedOnline(t, ctx, odb, "u1", config.Web, 1)
	seedOnline(t, ctx, odb, "u2", config.PC, 1)
	seedOnline(t, ctx, odb, "u3", config.APP, 1)
	seedOnline(t, ctx, odb, "u4", config.Web, 0)

	svc := NewService(ctx)
	rows, err := svc.GetDeviceOnlines([]string{"u1", "u2", "u3", "u4"}, []config.DeviceFlag{config.Web, config.PC})
	require.NoError(t, err) // []uint8 的旧实现会在此返回 MySQL 1064

	got := map[string]uint8{}
	for _, r := range rows {
		got[r.UID] = r.DeviceFlag
		assert.Equal(t, 1, r.Online, "只应返回 online=1 的记录")
	}
	assert.Equal(t, map[string]uint8{
		"u1": config.Web.Uint8(),
		"u2": config.PC.Uint8(),
	}, got, "只应命中查询设备(Web/PC)且在线的 uid，APP 与离线记录被过滤")

	// 空入参走 0 次查询，直接返回空。
	empty, err := svc.GetDeviceOnlines(nil, []config.DeviceFlag{config.Web})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// seedOnline 往 user_online 写一条指定设备/在线态的记录。
func seedOnline(t *testing.T, ctx *config.Context, odb *onlineDB, uid string, flag config.DeviceFlag, online int) {
	t.Helper()
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	now := int(time.Now().Unix())
	require.NoError(t, odb.insertOrUpdateUserOnlineTx(&onlineStatusModel{
		UID:         uid,
		DeviceFlag:  flag.Uint8(),
		LastOnline:  now,
		LastOffline: now,
		Online:      online,
		Version:     time.Now().UnixNano() / 1000,
	}, tx))
	require.NoError(t, tx.Commit())
}
