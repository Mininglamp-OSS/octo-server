package user

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// 这些用例必须打真实 MySQL。DesktopOnlineUIDs 的第一版把 device_flag 列表写成
// []uint8，而 []uint8 就是 []byte —— dbr 当作 blob 而非 IN 列表，生成的 SQL 里
// `?` 不被展开，直接 Error 1064。上层 fail-open 会把这个错误吞掉、静音全部失效，
// 因此只有打到真实数据库的断言才能挡住这类回归。
func TestDesktopOnlineUIDs(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	s := NewService(ctx)

	seed := func(uid string, flag config.DeviceFlag, online int) {
		_, err := ctx.DB().InsertInto("user_online").
			Columns("uid", "device_flag", "online").
			Values(uid, flag.Uint8(), online).Exec()
		assert.NoError(t, err)
	}

	seed("u_pc", config.PC, 1)     // PC 在线 → 命中
	seed("u_web", config.Web, 1)   // Web 在线 → 命中
	seed("u_app", config.APP, 1)   // 仅手机在线 → 不命中
	seed("u_pc_off", config.PC, 0) // PC 记录存在但已离线 → 不命中
	seed("u_both", config.APP, 1)  // 同一用户多端：手机在线
	seed("u_both", config.Web, 1)  // + Web 在线 → 命中且不重复

	got, err := s.DesktopOnlineUIDs([]string{"u_pc", "u_web", "u_app", "u_pc_off", "u_both", "u_absent"})
	assert.NoError(t, err, "查询不得报错——第一版在这里返回 Error 1064")

	assert.True(t, got["u_pc"], "PC 在线应判定为桌面端在线")
	assert.True(t, got["u_web"], "Web 在线应判定为桌面端在线")
	assert.True(t, got["u_both"], "多端在线只要含桌面端即命中")
	assert.False(t, got["u_app"], "仅 APP 在线不算桌面端会话")
	assert.False(t, got["u_pc_off"], "online=0 的 PC 记录不算在线")
	assert.False(t, got["u_absent"], "无记录的用户不算在线")
	assert.Len(t, got, 3, "只应返回三个桌面端在线用户")
}

// 空入参不查库、不报错。
func TestDesktopOnlineUIDs_EmptyInput(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	got, err := NewService(ctx).DesktopOnlineUIDs(nil)
	assert.NoError(t, err)
	assert.Empty(t, got)
}
