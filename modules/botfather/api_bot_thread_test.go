package botfather

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"

	// 导入依赖模块以确保迁移按正确顺序执行
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

// setupBotThreadTestData 创建 Bot Thread 测试所需的基础数据：robot + group + membership
func setupBotThreadTestData(t *testing.T) (s *server.Server, bf *BotFather, robotID, groupNo, botToken string) {
	t.Helper()
	s, bf = setupTestBotFather(t)

	robotID = "thread_test_bot"
	botToken = "bf_" + robotID
	ownerUID := "group_owner_001"

	createTestUser(t, bf, ownerUID, "群主")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// robot 也需要 user 记录（thread service 查 member name 时需要）
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status", "robot",
	).Values(
		robotID, "TestBot", robotID, "sn_"+robotID, 1, 1,
	).Exec()
	assert.NoError(t, err)

	// 创建群
	groupNo = strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(bf.ctx)
	err = groupDB.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "测试群",
		Creator: ownerUID,
		Status:  1,
		Version: 1,
	})
	assert.NoError(t, err)

	// 群主
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     ownerUID,
		Role:    group.MemberRoleCreator,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	// bot 作为群成员
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     robotID,
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	return
}

// botRequest 发起 Bot API 请求
func botRequest(t *testing.T, s *server.Server, method, path, botToken string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req, _ = http.NewRequest(method, path, bytes.NewReader([]byte(util.ToJson(body))))
	} else {
		req, _ = http.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// createBotThread 通过 Bot API 创建子区，返回 short_id
func createBotThread(t *testing.T, s *server.Server, groupNo, botToken, name string) string {
	t.Helper()
	w := botRequest(t, s, "POST", "/v1/bot/groups/"+groupNo+"/threads", botToken, map[string]interface{}{
		"name": name,
	})
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &resp)
	return resp["short_id"].(string)
}

// ==================== 创建子区 ====================

func TestBotCreateThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	w := botRequest(t, s, "POST", "/v1/bot/groups/"+groupNo+"/threads", botToken, map[string]interface{}{
		"name": "Bot创建的子区",
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"Bot创建的子区"`)
	assert.Contains(t, w.Body.String(), `"short_id"`)
	assert.Contains(t, w.Body.String(), `"channel_type":5`)
}

func TestBotCreateThread_NotGroupMember(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, _, _ := setupBotThreadTestData(t)

	// 创建一个不在群内的 bot
	outsiderID := "outsider_bot"
	outsiderToken := "bf_" + outsiderID
	createTestRobot(t, bf, outsiderID, "group_owner_001", 0)

	w := botRequest(t, s, "POST", "/v1/bot/groups/"+"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"+"/threads", outsiderToken, map[string]interface{}{
		"name": "不应该创建成功",
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not a member")
}

func TestBotCreateThread_EmptyName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	w := botRequest(t, s, "POST", "/v1/bot/groups/"+groupNo+"/threads", botToken, map[string]interface{}{
		"name": "",
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ==================== 列出子区 ====================

func TestBotListThreads(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	// 创建两个子区
	createBotThread(t, s, groupNo, botToken, "话题A")
	createBotThread(t, s, groupNo, botToken, "话题B")

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads", botToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"话题A"`)
	assert.Contains(t, w.Body.String(), `"话题B"`)
}

// TestBotListThreads_BackwardCompat_NoParams 不传分页参数时返回裸数组
func TestBotListThreads_BackwardCompat_NoParams(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	createBotThread(t, s, groupNo, botToken, "A")
	createBotThread(t, s, groupNo, botToken, "B")

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	body := strings.TrimSpace(w.Body.String())
	assert.True(t, strings.HasPrefix(body, "["), "不传分页参数时响应必须以数组开头，实际: %s", body)
}

// TestBotListThreads_EnvelopeWithParams 传分页参数时返回 {count, list} 信封
func TestBotListThreads_EnvelopeWithParams(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	createBotThread(t, s, groupNo, botToken, "A")
	createBotThread(t, s, groupNo, botToken, "B")
	createBotThread(t, s, groupNo, botToken, "C")

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads?page_index=1&page_size=2", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	body := strings.TrimSpace(w.Body.String())
	assert.True(t, strings.HasPrefix(body, "{"), "传分页参数时响应应为信封")
	assert.Contains(t, body, `"count":3`)
	assert.Contains(t, body, `"list":`)
}

// ==================== 获取子区详情 ====================

func TestBotGetThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	shortID := createBotThread(t, s, groupNo, botToken, "详情测试")

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID, botToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"详情测试"`)
	assert.Contains(t, w.Body.String(), `"short_id":"`+shortID+`"`)
}

func TestBotGetThread_InvalidShortID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/invalid", botToken, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

// ==================== 删除子区 ====================

func TestBotDeleteThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	shortID := createBotThread(t, s, groupNo, botToken, "待删除")

	w := botRequest(t, s, "DELETE", "/v1/bot/groups/"+groupNo+"/threads/"+shortID, botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证已被删除（获取详情应失败）
	w = botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID, botToken, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBotDeleteThread_NotCreator(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, groupNo, botToken := setupBotThreadTestData(t)

	// 用另一个 bot 创建子区
	otherBotID := "other_bot"
	otherToken := "bf_" + otherBotID
	createTestRobot(t, bf, otherBotID, "group_owner_001", 0)
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status", "robot",
	).Values(
		otherBotID, "OtherBot", otherBotID, "sn_"+otherBotID, 1, 1,
	).Exec()
	assert.NoError(t, err)

	groupDB := group.NewDB(bf.ctx)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     otherBotID,
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	// other_bot 创建子区
	shortID := createBotThread(t, s, groupNo, otherToken, "别人的子区")

	// thread_test_bot 尝试删除（非创建者、非管理员）
	w := botRequest(t, s, "DELETE", "/v1/bot/groups/"+groupNo+"/threads/"+shortID, botToken, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ==================== 成员列表 ====================

func TestBotListThreadMembers(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	shortID := createBotThread(t, s, groupNo, botToken, "成员测试")

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/members", botToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	// 创建者自动成为成员
	assert.Contains(t, w.Body.String(), `"thread_test_bot"`)
}

// ==================== 加入/离开子区 ====================

func TestBotJoinAndLeaveThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, groupNo, botToken := setupBotThreadTestData(t)

	// 用另一个 bot 创建子区
	otherBotID := "join_test_bot"
	otherToken := "bf_" + otherBotID
	createTestRobot(t, bf, otherBotID, "group_owner_001", 0)
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status", "robot",
	).Values(
		otherBotID, "JoinTestBot", otherBotID, "sn_"+otherBotID, 1, 1,
	).Exec()
	assert.NoError(t, err)

	groupDB := group.NewDB(bf.ctx)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     otherBotID,
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	shortID := createBotThread(t, s, groupNo, otherToken, "加入离开测试")

	// thread_test_bot 加入子区
	w := botRequest(t, s, "POST", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/join", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证成员列表包含两个 bot
	w = botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/members", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"thread_test_bot"`)
	assert.Contains(t, w.Body.String(), `"join_test_bot"`)

	// thread_test_bot 离开子区
	w = botRequest(t, s, "POST", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/leave", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ==================== 认证测试 ====================

func TestBotThreadAPI_Unauthorized(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, _ := setupBotThreadTestData(t)

	// 无 token
	req, _ := http.NewRequest("GET", "/v1/bot/groups/"+groupNo+"/threads", nil)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ==================== Bot Thread GROUP.md 测试 ====================

// setupBotThreadMdTestData 创建 Bot Thread GROUP.md 测试数据
// 返回 server, bf, robotID(bot_admin), groupNo, botToken, shortID
func setupBotThreadMdTestData(t *testing.T) (s *server.Server, bf *BotFather, robotID, groupNo, botToken, shortID string) {
	t.Helper()
	s, bf, robotID, groupNo, botToken = setupBotThreadTestData(t)

	// 将 bot 设置为 bot_admin
	_, err := bf.db.session.UpdateBySql(
		"UPDATE group_member SET bot_admin=1 WHERE group_no=? AND uid=?",
		groupNo, robotID,
	).Exec()
	assert.NoError(t, err)

	// 创建子区
	shortID = createBotThread(t, s, groupNo, botToken, "md测试子区")
	return
}

func TestBotGetThreadMd_NotSet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content":""`)
	assert.Contains(t, w.Body.String(), `"version":0`)
	assert.Contains(t, w.Body.String(), `"updated_by":""`)
}

func TestBotUpdateThreadMd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	// 更新
	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, map[string]interface{}{
		"content": "# Bot 管理的子区文档",
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version":1`)

	// 验证内容
	w = botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Bot 管理的子区文档")
	assert.Contains(t, w.Body.String(), `"version":1`)
}

func TestBotUpdateThreadMd_VersionIncrement(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	// 更新两次
	for i := 1; i <= 2; i++ {
		w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, map[string]interface{}{
			"content": "version " + strings.Repeat("x", i),
		})
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// 验证最终版本
	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version":2`)
}

func TestBotUpdateThreadMd_NotBotAdmin(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, groupNo, _, shortID := setupBotThreadMdTestData(t)

	// 创建另一个 bot（不是 bot_admin）
	nonAdminBotID := "non_admin_bot"
	nonAdminToken := "bf_" + nonAdminBotID
	createTestRobot(t, bf, nonAdminBotID, "group_owner_001", 0)
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status", "robot",
	).Values(
		nonAdminBotID, "NonAdminBot", nonAdminBotID, "sn_"+nonAdminBotID, 1, 1,
	).Exec()
	assert.NoError(t, err)

	groupDB := group.NewDB(bf.ctx)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     nonAdminBotID,
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	// 非 bot_admin 尝试更新
	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", nonAdminToken, map[string]interface{}{
		"content": "不应该成功",
	})

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "bot_admin")
}

func TestBotUpdateThreadMd_EmptyContent(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	// 空内容
	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, map[string]interface{}{
		"content": "",
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "content must not be empty")
}

func TestBotUpdateThreadMd_ExceedsMaxSize(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	bigContent := strings.Repeat("x", 10241)
	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, map[string]interface{}{
		"content": bigContent,
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "exceeds max size")
}

func TestBotGetThreadMd_NotGroupMember(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, groupNo, _, shortID := setupBotThreadMdTestData(t)

	// 创建不在群内的 bot
	outsiderID := "outsider_md_bot"
	outsiderToken := "bf_" + outsiderID
	createTestRobot(t, bf, outsiderID, "group_owner_001", 0)

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", outsiderToken, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not a member")
}

func TestBotGetThreadMd_GroupMemberCanRead(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, bf, _, groupNo, botToken, shortID := setupBotThreadMdTestData(t)

	// bot_admin 先设置内容
	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", botToken, map[string]interface{}{
		"content": "# 可读取的内容",
	})
	assert.Equal(t, http.StatusOK, w.Code)

	// 创建一个非 bot_admin 的群成员 bot
	readerBotID := "reader_bot"
	readerToken := "bf_" + readerBotID
	createTestRobot(t, bf, readerBotID, "group_owner_001", 0)
	_, err := bf.db.session.InsertInto("user").Columns(
		"uid", "name", "username", "short_no", "status", "robot",
	).Values(
		readerBotID, "ReaderBot", readerBotID, "sn_"+readerBotID, 1, 1,
	).Exec()
	assert.NoError(t, err)

	groupDB := group.NewDB(bf.ctx)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     readerBotID,
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	// 普通群成员 bot 可以读取
	w = botRequest(t, s, "GET", "/v1/bot/groups/"+groupNo+"/threads/"+shortID+"/md", readerToken, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "可读取的内容")
}

func TestBotUpdateThreadMd_InvalidShortID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken, _ := setupBotThreadMdTestData(t)

	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+groupNo+"/threads/invalid/md", botToken, map[string]interface{}{
		"content": "不应该成功",
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

// TestBotGetGroupMd_RejectsThreadChannelID verifies that calling the main-group
// GROUP.md endpoint with a thread channel id (parent____short) is rejected with
// a 400 and an English hint pointing at the thread endpoint, instead of being
// silently treated as a non-existent group.
func TestBotGetGroupMd_RejectsThreadChannelID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	threadChannelID := groupNo + "____2049000369341599744"

	w := botRequest(t, s, "GET", "/v1/bot/groups/"+threadChannelID+"/md", botToken, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "thread channel id")
	assert.Contains(t, w.Body.String(), "/threads/")
}

// TestBotUpdateGroupMd_RejectsThreadChannelID mirrors the GET case for PUT.
func TestBotUpdateGroupMd_RejectsThreadChannelID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _, groupNo, botToken := setupBotThreadTestData(t)

	threadChannelID := groupNo + "____2049000369341599744"

	w := botRequest(t, s, "PUT", "/v1/bot/groups/"+threadChannelID+"/md", botToken, map[string]interface{}{
		"content": "should not be accepted",
	})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "thread channel id")
	assert.Contains(t, w.Body.String(), "/threads/")
}
