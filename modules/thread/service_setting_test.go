package thread

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// ==================== ThreadSetting Service 测试 ====================

func TestUpdateSetting_NotMember(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)

	// 创建子区，testutil.UID 自动成为创建者成员
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// 非群成员不能设置
	err = svc.UpdateSetting(groupNo, thread.ShortID, "outsider", map[string]interface{}{
		"mute": float64(1),
	})
	assert.Error(t, err)
}

func TestUpdateSetting_InsertAndUpdateMute(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// 首次 insert
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": float64(1),
	})
	assert.NoError(t, err)

	settings, err := svc.GetSettingsWithUIDs(groupNo, thread.ShortID, []string{testutil.UID})
	assert.NoError(t, err)
	assert.Len(t, settings, 1)
	assert.Equal(t, 1, settings[0].Mute)
	assert.Equal(t, testutil.UID, settings[0].UID)

	// 再次 update: mute=0
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": float64(0),
	})
	assert.NoError(t, err)

	settings, err = svc.GetSettingsWithUIDs(groupNo, thread.ShortID, []string{testutil.UID})
	assert.NoError(t, err)
	assert.Len(t, settings, 1)
	assert.Equal(t, 0, settings[0].Mute)
}

// TestGetThread_ReturnsMuteForLoginUID 验证 GetThread 返回当前登录用户的 mute 状态。
// Mute 是 *int 三态：nil=未设置（继承父群组）、*0=显式未静音、*1=显式静音。
func TestGetThread_ReturnsMuteForLoginUID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// 未设置时返回 nil（前端继承父群组）
	resp, err := svc.GetThread(groupNo, thread.ShortID, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, resp.Mute, "未设置时 Mute 应为 nil")

	// 显式设置 mute=0：应返回非 nil 指针，值为 0
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": float64(0),
	})
	assert.NoError(t, err)

	resp, err = svc.GetThread(groupNo, thread.ShortID, testutil.UID)
	assert.NoError(t, err)
	if assert.NotNil(t, resp.Mute, "显式设置后 Mute 不应为 nil") {
		assert.Equal(t, 0, *resp.Mute)
	}

	// 设置 mute=1 后应返回 *1
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": float64(1),
	})
	assert.NoError(t, err)

	resp, err = svc.GetThread(groupNo, thread.ShortID, testutil.UID)
	assert.NoError(t, err)
	if assert.NotNil(t, resp.Mute) {
		assert.Equal(t, 1, *resp.Mute)
	}

	// loginUID 为空时不查询 setting，Mute 为 nil
	resp, err = svc.GetThread(groupNo, thread.ShortID, "")
	assert.NoError(t, err)
	assert.Nil(t, resp.Mute)

	// 其他用户读取应得到自己的设置（未设置 → nil），不串号
	resp, err = svc.GetThread(groupNo, thread.ShortID, "other-uid")
	assert.NoError(t, err)
	assert.Nil(t, resp.Mute)
}

func TestUpdateSetting_InvalidMuteValue(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// 非 float64 类型(如 string)应被忽略
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": "yes",
	})
	assert.Error(t, err)
}

func TestUpdateSetting_MuteOutOfRange(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// mute 只允许 0/1
	err = svc.UpdateSetting(groupNo, thread.ShortID, testutil.UID, map[string]interface{}{
		"mute": float64(2),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mute must be 0 or 1")
}

func TestUpdateSetting_ThreadNotFound(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	err := svc.UpdateSetting(groupNo, "999999999999999", testutil.UID, map[string]interface{}{
		"mute": float64(1),
	})
	assert.Error(t, err)
}

func TestGetSettingsWithUIDs_Empty(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// 无设置记录时应返回空列表,不报错
	settings, err := svc.GetSettingsWithUIDs(groupNo, thread.ShortID, []string{testutil.UID, "user2"})
	assert.NoError(t, err)
	assert.Len(t, settings, 0)
}

// 退群清理 thread_setting 的回归(含 mute 而未 join 的场景)已随清理逻辑迁往
// modules/group/thread_cleanup_test.go(removeUserFromGroupThreadsCleanup,Issue #331)。

func TestGetSettingsWithUIDs_Batch(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	svc, groupNo := setupServiceTestData(t)
	thread, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "s1", CreatorUID: testutil.UID, CreatorName: "用户1",
	})
	assert.NoError(t, err)

	// user2 加入并设置 mute
	assert.NoError(t, svc.JoinThread(groupNo, thread.ShortID, "user2"))
	assert.NoError(t, svc.UpdateSetting(groupNo, thread.ShortID, "user2", map[string]interface{}{
		"mute": float64(1),
	}))

	// testutil.UID 未设置,user2 设置为 mute=1
	settings, err := svc.GetSettingsWithUIDs(groupNo, thread.ShortID, []string{testutil.UID, "user2"})
	assert.NoError(t, err)
	assert.Len(t, settings, 1)
	assert.Equal(t, "user2", settings[0].UID)
	assert.Equal(t, 1, settings[0].Mute)
}
