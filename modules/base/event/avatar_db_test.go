package event

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"

	_ "github.com/go-sql-driver/mysql"
)

// TestUpdateGeneratedGroupAvatarProtectsManualUpload 验证自动群头像合成的并发保护契约：
// updateGeneratedGroupAvatar 带 WHERE is_upload_avatar=0，只在群头像仍为自动管理时写入
// 合成结果；一旦群主手动上传（is_upload_avatar=1），后到的旧合成事件必须被挡掉、不得覆盖
// 手动头像。这是本次改动最关键的正确性保证。
func TestUpdateGeneratedGroupAvatarProtectsManualUpload(t *testing.T) {
	ctx := newIsolatedAvatarEventTestContext(t)
	db := NewDB(ctx.DB())

	_, err := ctx.DB().UpdateBySql("CREATE TABLE IF NOT EXISTS `group` (" +
		"`group_no` varchar(40) NOT NULL," +
		"`avatar` varchar(255) NOT NULL DEFAULT ''," +
		"`avatar_version` bigint NOT NULL DEFAULT 0," +
		"`is_upload_avatar` smallint NOT NULL DEFAULT 0," +
		"PRIMARY KEY (`group_no`)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().DeleteBySql("DELETE FROM `group`").Exec()
	require.NoError(t, err)

	const groupNo = "G_auto_avatar_1"
	_, err = ctx.DB().InsertBySql("INSERT INTO `group` (`group_no`, `is_upload_avatar`) VALUES (?, 0)", groupNo).Exec()
	require.NoError(t, err)

	// 自动管理状态：合成事件可写入版本化头像。
	state, err := db.queryGroupAvatarState(groupNo)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, 0, state.IsUploadAvatar)

	const autoVersion int64 = 1733300000000000010
	const autoPath = "group/1/G_auto_avatar_1/1733300000000000010.png"
	updated, err := db.updateGeneratedGroupAvatar(groupNo, autoPath, autoVersion)
	require.NoError(t, err)
	require.True(t, updated, "auto-managed avatar should be composable")

	// 群主手动上传后置 is_upload_avatar=1，并写入手动头像版本。
	const manualPath = "group/1/G_auto_avatar_1/1733300000000000020.png"
	const manualVersion int64 = 1733300000000000020
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `group` SET `is_upload_avatar`=1, `avatar`=?, `avatar_version`=? WHERE `group_no`=?",
		manualPath, manualVersion, groupNo).Exec()
	require.NoError(t, err)

	// 旧的合成事件再次尝试写入：必须被 WHERE is_upload_avatar=0 挡掉。
	stale, err := db.updateGeneratedGroupAvatar(groupNo, "group/1/G_auto_avatar_1/1733300000000000030.png", 1733300000000000030)
	require.NoError(t, err)
	require.False(t, stale, "manual avatar must not be overwritten by a stale compose event")

	// 校验手动头像保持不变。
	var got struct {
		Avatar        string `db:"avatar"`
		AvatarVersion int64  `db:"avatar_version"`
	}
	_, err = ctx.DB().Select("avatar", "avatar_version").From("`group`").Where("group_no=?", groupNo).Load(&got)
	require.NoError(t, err)
	require.Equal(t, manualPath, got.Avatar)
	require.Equal(t, manualVersion, got.AvatarVersion)
}

// Keep this test's hand-built `group` table out of the shared test schema so it
// cannot poison package-level migrations in later go test invocations.
func newIsolatedAvatarEventTestContext(t *testing.T) *config.Context {
	t.Helper()

	adminDB, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)

	dbName := fmt.Sprintf("test_event_avatar_%d", time.Now().UnixNano())
	_, err = adminDB.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminDB.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = adminDB.Close()
	})

	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = fmt.Sprintf("root:demo@tcp(127.0.0.1)/%s?charset=utf8mb4&parseTime=true", dbName)
	return config.NewContext(cfg)
}
