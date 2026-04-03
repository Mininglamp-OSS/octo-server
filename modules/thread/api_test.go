package thread

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

func setupTestData(t *testing.T) (*server.Server, *config.Context) {
	s, ctx := testutil.NewTestServer()
	// module.Setup 已注册所有模块路由，无需手动注册

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试用户
	userDB := user.NewDB(ctx)
	err = userDB.Insert(&user.Model{
		UID:     testutil.UID,
		Name:    "测试用户",
		ShortNo: "test10000",
	})
	assert.NoError(t, err)

	err = userDB.Insert(&user.Model{
		UID:     "user2",
		Name:    "用户2",
		ShortNo: "test10002",
	})
	assert.NoError(t, err)

	return s, ctx
}

func createTestGroup(t *testing.T, ctx *config.Context) string {
	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)

	// 直接插入群数据
	err := groupDB.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "测试群",
		Creator: testutil.UID,
		Status:  1,
		Version: 1,
	})
	assert.NoError(t, err)

	// 插入群成员
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     testutil.UID,
		Role:    group.MemberRoleCreator,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     "user2",
		Role:    group.MemberRoleCommon,
		Status:  1,
		Version: 1,
		Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)

	return groupNo
}

// ==================== 创建子区测试 ====================

func TestCreateThread(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "讨论话题1",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"讨论话题1"`)
	assert.Contains(t, w.Body.String(), `"channel_type":5`)
	assert.Contains(t, w.Body.String(), `"short_id"`)
}

func TestCreateThread_WithSourceMessage(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区（带来源消息）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":              "从消息创建的子区",
		"source_message_id": 12345,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"source_message_id":12345`)
}

func TestCreateThread_EmptyName(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区（空名称）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ==================== 列出子区测试 ====================

func TestListThreads(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建两个子区
	for _, name := range []string{"话题A", "话题B"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"name": name,
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// 列出子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"话题A"`)
	assert.Contains(t, w.Body.String(), `"话题B"`)
}

// ==================== 获取子区详情测试 ====================

func TestGetThread(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "测试话题",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 解析 short_id
	var createResp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &createResp)
	shortID := createResp["short_id"].(string)

	// 获取详情
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"测试话题"`)
}

func TestGetThread_InvalidShortID(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 使用无效的 shortID（不包含 / 的测试用例，因为 / 会被 Gin 解析成不同路由）
	invalidShortIDs := []string{
		"invalid",            // 含字母且太短
		"12345",              // 太短（< 15位）
		"12345678901234567a", // 含非数字字符
	}

	for _, shortID := range invalidShortIDs {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code, "shortID: %s should fail", shortID)
		assert.Contains(t, w.Body.String(), "invalid short_id format")
	}
}

// ==================== 删除子区测试 ====================

func TestDeleteThread(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "待删除话题",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var createResp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &createResp)
	shortID := createResp["short_id"].(string)

	// 删除子区
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证已删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "deleted")
}

func TestDeleteThread_InvalidShortID(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 使用无效的 shortID 删除
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/threads/invalid-id", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

// ==================== 归档测试 ====================

func TestArchiveThread(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "待归档话题",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var createResp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &createResp)
	shortID := createResp["short_id"].(string)

	// 归档
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证状态
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":2`)
}

func TestArchiveThread_InvalidShortID(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/invalid/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

// ==================== 取消归档测试 ====================

func TestUnarchiveThread(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建并归档子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "待取消归档话题",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	var createResp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &createResp)
	shortID := createResp["short_id"].(string)

	// 归档
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 取消归档
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/unarchive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证状态恢复
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":1`)
}

func TestUnarchiveThread_InvalidShortID(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/invalid/unarchive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

// ==================== IMDatasource 测试 ====================

// createThreadViaAPI 通过 API 创建子区，返回 shortID
func createThreadViaAPI(t *testing.T, s *server.Server, groupNo, name string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": name,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	util.ReadJsonByByte(w.Body.Bytes(), &resp)
	return resp["short_id"].(string)
}

func TestIMDatasource_HasData(t *testing.T) {
	_, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 直接插入一条 thread 记录
	db := NewDB(ctx)
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	err := db.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "ds测试",
		CreatorUID: testutil.UID,
		Status:     ThreadStatusActive,
		Version:    1,
	})
	assert.NoError(t, err)

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 正常的子区频道 - 应返回有数据
	result := mod.IMDatasource.HasData(channelID, 5)
	assert.True(t, result.Has(register.IMDatasourceTypeSubscribers))
	assert.True(t, result.Has(register.IMDatasourceTypeBlacklist))
	assert.True(t, result.Has(register.IMDatasourceTypeWhitelist))
	assert.True(t, result.Has(register.IMDatasourceTypeChannelInfo))

	// 错误的频道类型 - 应返回 None
	result = mod.IMDatasource.HasData(channelID, 2) // 群频道类型
	assert.Equal(t, register.IMDatasourceTypeNone, result)

	// 不存在的子区
	fakeID := BuildChannelID(groupNo, "9999999999999999999")
	result = mod.IMDatasource.HasData(fakeID, 5)
	assert.Equal(t, register.IMDatasourceTypeNone, result)
}

func TestIMDatasource_Subscribers(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "订阅测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 子区的订阅者应该是父群的成员
	uids, err := mod.IMDatasource.Subscribers(channelID, 5)
	assert.NoError(t, err)
	assert.Len(t, uids, 2) // testutil.UID 和 "user2"
	assert.Contains(t, uids, testutil.UID)
	assert.Contains(t, uids, "user2")

	// 错误的频道类型
	_, err = mod.IMDatasource.Subscribers(channelID, 2)
	assert.Equal(t, register.ErrDatasourceNotProcess, err)
}

func TestIMDatasource_ChannelInfo(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "频道信息测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 活跃子区 - 不应该有 ban
	info, err := mod.IMDatasource.ChannelInfo(channelID, 5)
	assert.NoError(t, err)
	assert.NotContains(t, info, "ban")

	// 归档子区 - 应该 ban=1
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	info, err = mod.IMDatasource.ChannelInfo(channelID, 5)
	assert.NoError(t, err)
	assert.Equal(t, 1, info["ban"])

	// 删除子区 - 也应该 ban=1
	// 先取消归档再删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/unarchive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	info, err = mod.IMDatasource.ChannelInfo(channelID, 5)
	assert.NoError(t, err)
	assert.Equal(t, 1, info["ban"])
}

func TestIMDatasource_Blacklist(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "黑名单测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 测试群无黑名单成员，应返回空列表
	blacklist, err := mod.IMDatasource.Blacklist(channelID, 5)
	assert.NoError(t, err)
	assert.Empty(t, blacklist)

	// 错误的频道类型
	_, err = mod.IMDatasource.Blacklist(channelID, 2)
	assert.Equal(t, register.ErrDatasourceNotProcess, err)
}

func TestIMDatasource_Whitelist(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "白名单测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 群未禁言 - 白名单应为空
	whitelist, err := mod.IMDatasource.Whitelist(channelID, 5)
	assert.NoError(t, err)
	assert.Empty(t, whitelist)

	// 错误的频道类型
	_, err = mod.IMDatasource.Whitelist(channelID, 2)
	assert.Equal(t, register.ErrDatasourceNotProcess, err)
}

// ==================== BussDataSource 测试 ====================

func TestBussDataSource_ChannelGet(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "频道查询测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 获取频道信息
	resp, err := mod.BussDataSource.ChannelGet(channelID, 5, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, channelID, resp.Channel.ChannelID)
	assert.Equal(t, uint8(5), resp.Channel.ChannelType)
	assert.Equal(t, "频道查询测试", resp.Name)
	assert.Equal(t, shortID, resp.Extra["short_id"])
	assert.Equal(t, groupNo, resp.Extra["group_no"])

	// 错误的频道类型
	_, err = mod.BussDataSource.ChannelGet(channelID, 2, testutil.UID)
	assert.Equal(t, register.ErrDatasourceNotProcess, err)

	// 不存在的子区
	fakeID := BuildChannelID(groupNo, "9999999999999999999")
	_, err = mod.BussDataSource.ChannelGet(fakeID, 5, testutil.UID)
	assert.Equal(t, register.ErrDatasourceNotProcess, err)
}

// ==================== 修复验证测试 ====================

// TestCreateThread_TransactionIntegrity 验证 #2: 事务完整性
// 创建子区后，thread 和 member 记录应同时存在
func TestCreateThread_TransactionIntegrity(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "事务测试")

	// 验证 thread 记录存在
	db := NewDB(ctx)
	thread, err := db.QueryByGroupNoAndShortID(groupNo, shortID)
	assert.NoError(t, err)
	assert.NotNil(t, thread)
	assert.Equal(t, "事务测试", thread.Name)
	assert.True(t, thread.Id > 0, "thread.Id should be populated")

	// 验证 creator 作为 member 存在
	members, err := db.QueryMembers(thread.Id)
	assert.NoError(t, err)
	assert.Len(t, members, 1)
	assert.Equal(t, testutil.UID, members[0].UID)
	assert.Equal(t, MemberRoleCreator, members[0].Role)
}

// TestGetThreads_MemberCountBatch 验证 #3: 批量查询 member_count
// GetThreads 应正确返回每个子区的成员数量
func TestGetThreads_MemberCountBatch(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建多个子区
	shortID1 := createThreadViaAPI(t, s, groupNo, "话题1")
	_ = createThreadViaAPI(t, s, groupNo, "话题2")

	// 获取子区列表
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var threads []ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &threads)

	assert.Len(t, threads, 2)

	// 验证每个子区都有 member_count 字段且至少为 1
	for _, thread := range threads {
		assert.GreaterOrEqual(t, thread.MemberCount, 1, "member_count should be at least 1 (creator)")
	}

	// 验证 shortID1 的 member_count = 1（只有创建者）
	for _, thread := range threads {
		if thread.ShortID == shortID1 {
			assert.Equal(t, 1, thread.MemberCount)
		}
	}
}

// TestGetMembers_WithUserName 验证 #7: MemberResp.Name 填充
// GetMembers 应返回成员的用户名
func TestGetMembers_WithUserName(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "成员名称测试")

	// 获取成员列表
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/members", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var members []MemberResp
	util.ReadJsonByByte(w.Body.Bytes(), &members)

	assert.Len(t, members, 1)

	// 验证 name 字段已填充（不是空字符串）
	member := members[0]
	assert.Equal(t, testutil.UID, member.UID)
	assert.NotEmpty(t, member.Name, "member name should not be empty")
	assert.Equal(t, "测试用户", member.Name) // setupTestData 中创建的用户名
}

// TestCreateThread_CreatorAsMember 验证创建者自动成为成员
func TestCreateThread_CreatorAsMember(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "创建者成员测试",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var createResp ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &createResp)

	// 验证返回的 member_count = 1
	assert.Equal(t, 1, createResp.MemberCount)

	// 验证创建者 UID
	assert.Equal(t, testutil.UID, createResp.CreatorUID)
}
