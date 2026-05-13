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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区（带来源消息和 payload）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":                   "从消息创建的子区",
		"source_message_id":      12345,
		"source_message_payload": map[string]interface{}{"type": 1, "content": "原始消息内容"},
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"source_message_id":12345`)
}

func TestCreateThread_EmptyName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

// threadListResp 分页列表响应
type threadListResp struct {
	Count int64         `json:"count"`
	List  []*ThreadResp `json:"list"`
}

func TestListThreads(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

	// 列出子区（不传分页参数 → 兼容旧格式：裸数组）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var list []*ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &list)
	assert.Len(t, list, 2)
	assert.Contains(t, w.Body.String(), `"话题A"`)
	assert.Contains(t, w.Body.String(), `"话题B"`)
}

// TestListThreads_Pagination 验证分页参数 page_index / page_size 正确工作
func TestListThreads_Pagination(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建 25 个子区，数量大于默认 page_size (15)
	const total = 25
	for i := 0; i < total; i++ {
		createThreadViaAPI(t, s, groupNo, fmt.Sprintf("话题%02d", i))
	}

	// 场景 1：传 page_size=15，返回信封格式
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_size=15", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var page1 threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &page1)
	assert.Equal(t, int64(total), page1.Count, "count 应为全部子区数")
	assert.Len(t, page1.List, 15, "page_size 应为 15")

	// 场景 2：page_size=10, page_index=1
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_index=1&page_size=10", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	var p1 threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &p1)
	assert.Equal(t, int64(total), p1.Count)
	assert.Len(t, p1.List, 10)

	// 场景 3：page_index=2，返回第二页
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_index=2&page_size=10", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	var p2 threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &p2)
	assert.Len(t, p2.List, 10)
	// 与第一页不应有重叠
	seen := map[string]bool{}
	for _, it := range p1.List {
		seen[it.ShortID] = true
	}
	for _, it := range p2.List {
		assert.False(t, seen[it.ShortID], "第二页与第一页不应重复")
	}

	// 场景 4：page_index=3，仅剩 5 条
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_index=3&page_size=10", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	var p3 threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &p3)
	assert.Len(t, p3.List, 5)

	// 场景 5：page_size 超上限（>100）夹到最大值 100
	// 此处仅创建了 25 条子区，所以实际返回 25 条
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_size=9999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	var pOver threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &pOver)
	assert.Len(t, pOver.List, total, "page_size>100 应夹到 100，返回全部 25 条")
}

// TestListThreads_BackwardCompat_NoParams 不传分页参数时返回裸数组（向后兼容旧客户端）
func TestListThreads_BackwardCompat_NoParams(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	for _, name := range []string{"A", "B", "C"} {
		createThreadViaAPI(t, s, groupNo, name)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	body := strings.TrimSpace(w.Body.String())
	assert.True(t, strings.HasPrefix(body, "["), "不传分页参数时响应必须以数组开头，实际: %s", body)
	assert.False(t, strings.HasPrefix(body, `{"count"`), "不传分页参数时响应不应为信封")

	var list []*ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &list)
	assert.Len(t, list, 3)
}

// TestListThreads_EnvelopeWithPageIndex 只传 page_index 也触发信封格式
func TestListThreads_EnvelopeWithPageIndex(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	for i := 0; i < 3; i++ {
		createThreadViaAPI(t, s, groupNo, fmt.Sprintf("t%d", i))
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_index=1", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := strings.TrimSpace(w.Body.String())
	assert.True(t, strings.HasPrefix(body, "{"), "传分页参数时响应应为信封")
	var resp threadListResp
	util.ReadJsonByByte(w.Body.Bytes(), &resp)
	assert.Equal(t, int64(3), resp.Count)
	assert.Len(t, resp.List, 3)
}

// TestListThreads_DBPagination 直接在 DB 层验证 offset/limit 参数
func TestListThreads_DBPagination(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	db := NewDB(ctx)
	for i := 0; i < 5; i++ {
		m := &Model{
			ShortID: fmt.Sprintf("148910429168%04d", 1000+i),
			GroupNo: groupNo,
			Name:    fmt.Sprintf("t%d", i),
			Status:  ThreadStatusActive,
			Version: 1,
		}
		err := db.Insert(m)
		assert.NoError(t, err)
	}

	// 取前 2 条
	list, err := db.QueryByGroupNo(groupNo, 0, 2)
	assert.NoError(t, err)
	assert.Len(t, list, 2)

	// 跳过 2 条，再取 2 条
	list2, err := db.QueryByGroupNo(groupNo, 2, 2)
	assert.NoError(t, err)
	assert.Len(t, list2, 2)
	assert.NotEqual(t, list[0].ShortID, list2[0].ShortID)

	// 总数
	count, err := db.CountByGroupNo(groupNo)
	assert.NoError(t, err)
	assert.Equal(t, int64(5), count)
}

// TestListThreads_StatusFilter PR-B：listThreads 支持 ?status=active|archived|all
func TestListThreads_StatusFilter(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	db := NewDB(ctx)
	// 2 active + 3 archived + 1 deleted
	mk := func(name string, status int) string {
		shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
		err := db.Insert(&Model{
			ShortID: shortID, GroupNo: groupNo, Name: name,
			CreatorUID: testutil.UID, Status: status, Version: 1,
		})
		assert.NoError(t, err)
		return shortID
	}
	mk("A1", ThreadStatusActive)
	mk("A2", ThreadStatusActive)
	mk("R1", ThreadStatusArchived)
	mk("R2", ThreadStatusArchived)
	mk("R3", ThreadStatusArchived)
	mk("D1", ThreadStatusDeleted)

	get := func(query string) *threadListResp {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?page_size=100"+query, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
		var resp threadListResp
		util.ReadJsonByByte(w.Body.Bytes(), &resp)
		return &resp
	}

	// 默认（不传 status）只返回 active
	def := get("")
	assert.Equal(t, int64(2), def.Count)
	assert.Len(t, def.List, 2)

	// ?status=active 等价默认
	act := get("&status=active")
	assert.Equal(t, int64(2), act.Count)
	assert.Len(t, act.List, 2)

	// ?status=archived 只返回 archived
	arch := get("&status=archived")
	assert.Equal(t, int64(3), arch.Count)
	assert.Len(t, arch.List, 3)
	for _, it := range arch.List {
		assert.Equal(t, ThreadStatusArchived, it.Status)
	}

	// ?status=all 返回 active + archived（不含 deleted）
	all := get("&status=all")
	assert.Equal(t, int64(5), all.Count)
	assert.Len(t, all.List, 5)
	for _, it := range all.List {
		assert.NotEqual(t, ThreadStatusDeleted, it.Status)
	}

	// 非法 status 值 → 400
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?status=bogus", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestListThreads_StatusFilter_BackwardCompat 不传 page_index/page_size 但传 status=archived，
// 仍走兼容旧客户端的裸数组格式（向后兼容性回归）。
func TestListThreads_StatusFilter_BackwardCompat(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	db := NewDB(ctx)
	for i, st := range []int{ThreadStatusActive, ThreadStatusArchived, ThreadStatusArchived} {
		err := db.Insert(&Model{
			ShortID:    fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64()),
			GroupNo:    groupNo,
			Name:       fmt.Sprintf("t%d", i),
			CreatorUID: testutil.UID,
			Status:     st,
			Version:    1,
		})
		assert.NoError(t, err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads?status=archived", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	body := strings.TrimSpace(w.Body.String())
	assert.True(t, strings.HasPrefix(body, "["), "不传分页参数时即使带 status= 也应返回裸数组，实际: %s", body)
	var list []*ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &list)
	assert.Len(t, list, 2)
	for _, it := range list {
		assert.Equal(t, ThreadStatusArchived, it.Status)
	}
}

// ==================== 获取子区详情测试 ====================

func TestGetThread(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "频道信息测试")

	mod := register.GetModuleByName("thread", ctx)
	channelID := BuildChannelID(groupNo, shortID)

	// 活跃子区 - 不应该有 ban
	info, err := mod.IMDatasource.ChannelInfo(channelID, 5)
	assert.NoError(t, err)
	assert.NotContains(t, info, "ban")

	// 归档子区 - 不应 ban（归档子区允许发消息，发消息后自动解档）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads/"+shortID+"/archive", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	info, err = mod.IMDatasource.ChannelInfo(channelID, 5)
	assert.NoError(t, err)
	assert.NotContains(t, info, "ban")
	assert.Equal(t, ThreadStatusArchived, info["status"])

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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

// ==================== 修改子区名称测试 ====================

func TestUpdateThreadName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "原始名称")

	// 修改名称
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID, bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "新名称",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证名称已更新
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"新名称"`)
}

func TestUpdateThreadName_EmptyName(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	shortID := createThreadViaAPI(t, s, groupNo, "原始名称")

	// 空名称
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID, bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateThreadName_InvalidShortID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/invalid", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "新名称",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

func TestUpdateThreadName_NoPermission(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// testutil.UID 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "原始名称")

	// 为 user2 设置 token
	user2Token := "token_user2"
	err := ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+user2Token, "user2@test")
	assert.NoError(t, err)

	// user2 修改名称（非创建者、非管理员）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID, bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "新名称",
	}))))
	req.Header.Set("token", user2Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no permission")
}

// ==================== 统计字段测试 ====================

// TestListThreads_WithStats 验证列表返回消息统计字段
func TestListThreads_WithStats(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "统计测试")

	// 模拟收到消息，触发 onMessages 更新统计
	api := New(ctx)
	api.onMessages([]*config.MessageResp{
		{
			ChannelID:   BuildChannelID(groupNo, shortID),
			ChannelType: 5,
			FromUID:     testutil.UID,
			Payload:     []byte(`{"type":1,"content":"你好世界"}`),
		},
	})

	// 列出子区（不传分页参数 → 裸数组）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var list []*ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &list)
	assert.Len(t, list, 1)

	thread := list[0]
	assert.Equal(t, int64(1), thread.MessageCount)
	assert.Equal(t, "你好世界", thread.LastMessageContent)
	assert.NotEmpty(t, thread.LastMessageSenderName)
	assert.NotEmpty(t, thread.LastMessageAt)
	assert.NotEqual(t, thread.CreatedAt, thread.LastMessageAt, "last_message_at should differ from created_at when messages exist")
}

// TestGetThread_WithStats 验证详情返回消息统计字段
func TestGetThread_WithStats(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	shortID := createThreadViaAPI(t, s, groupNo, "详情统计测试")

	// 模拟收到消息
	api := New(ctx)
	api.onMessages([]*config.MessageResp{
		{
			ChannelID:   BuildChannelID(groupNo, shortID),
			ChannelType: 5,
			FromUID:     testutil.UID,
			Payload:     []byte(`{"type":1,"content":"详情消息"}`),
		},
	})

	// 获取子区详情
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &resp)

	assert.Equal(t, int64(1), resp.MessageCount)
	assert.Equal(t, "详情消息", resp.LastMessageContent)
	assert.NotEmpty(t, resp.LastMessageSenderName)
	assert.NotEmpty(t, resp.LastMessageAt)
}

// TestCreateThread_ThreadCreatedMessagePayload 验证 ThreadCreated 消息包含 participants
func TestCreateThread_ThreadCreatedMessagePayload(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "参与者测试",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证返回的响应包含新的统计字段
	var resp ThreadResp
	util.ReadJsonByByte(w.Body.Bytes(), &resp)
	assert.Equal(t, int64(0), resp.MessageCount)
	assert.Equal(t, 1, resp.MemberCount)
	assert.Equal(t, resp.CreatedAt, resp.LastMessageAt, "last_message_at should equal created_at when no messages")
}

// ==================== 修复验证测试 ====================

// TestCreateThread_TransactionIntegrity 验证 #2: 事务完整性
// 创建子区后，thread 和 member 记录应同时存在
func TestCreateThread_TransactionIntegrity(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建多个子区
	shortID1 := createThreadViaAPI(t, s, groupNo, "话题1")
	_ = createThreadViaAPI(t, s, groupNo, "话题2")

	// 获取子区列表（不传分页参数 → 裸数组）
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var threads []*ThreadResp
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

// ==================== 消息监听器测试 ====================

// TestOnMessages_AutoUnarchive 验证归档子区收到消息后自动解档
func TestOnMessages_AutoUnarchive(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建并归档子区
	shortID := createThreadViaAPI(t, s, groupNo, "自动解档测试")
	archiveThread(t, s, groupNo, shortID)

	// 验证已归档
	db := NewDB(ctx)
	thread, _ := db.QueryByGroupNoAndShortID(groupNo, shortID)
	assert.Equal(t, ThreadStatusArchived, thread.Status)

	// 模拟收到消息
	api := New(ctx)
	api.onMessages([]*config.MessageResp{
		{
			ChannelID:   BuildChannelID(groupNo, shortID),
			ChannelType: 5, // ChannelTypeCommunityTopic
			FromUID:     testutil.UID,
		},
	})

	// 验证自动解档
	thread, _ = db.QueryByGroupNoAndShortID(groupNo, shortID)
	assert.Equal(t, ThreadStatusActive, thread.Status)
}

// TestOnMessages_AutoJoin 验证发送者不是子区成员时自动加入
func TestOnMessages_AutoJoin(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区（testutil.UID 是创建者，自动成为成员）
	shortID := createThreadViaAPI(t, s, groupNo, "自动加入测试")

	// 验证 user2 不是子区成员
	db := NewDB(ctx)
	thread, _ := db.QueryByGroupNoAndShortID(groupNo, shortID)
	isMember, _ := db.ExistMember(thread.Id, "user2")
	assert.False(t, isMember)

	// 模拟 user2 发送消息
	api := New(ctx)
	api.onMessages([]*config.MessageResp{
		{
			ChannelID:   BuildChannelID(groupNo, shortID),
			ChannelType: 5, // ChannelTypeCommunityTopic
			FromUID:     "user2",
		},
	})

	// 验证 user2 自动加入子区
	isMember, _ = db.ExistMember(thread.Id, "user2")
	assert.True(t, isMember, "user2 should be auto-joined as member")
}

// TestOnMessages_IgnoreNonThreadChannel 验证忽略非子区频道
func TestOnMessages_IgnoreNonThreadChannel(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := setupTestData(t)

	api := New(ctx)

	// 不应 panic 或报错
	api.onMessages([]*config.MessageResp{
		{
			ChannelID:   "some-group-id",
			ChannelType: 2, // ChannelTypeGroup
			FromUID:     testutil.UID,
		},
	})
}

// archiveThread 归档子区
func archiveThread(t *testing.T, s *server.Server, groupNo, shortID string) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/v1/groups/%s/threads/%s/archive", groupNo, shortID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ==================== 子区 GROUP.md API 测试 ====================

func TestThreadMdGet_NotSet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "md读取测试")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content":""`)
	assert.Contains(t, w.Body.String(), `"version":0`)
	assert.Contains(t, w.Body.String(), `"updated_by":""`)
}

func TestThreadMdUpdate(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "md更新测试")

	// 更新 GROUP.md
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 子区规范\n## 代码风格",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"version":1`)

	// 验证内容
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content":"# 子区规范\n## 代码风格"`)
	assert.Contains(t, w.Body.String(), `"version":1`)
	assert.Contains(t, w.Body.String(), `"updated_by":"`+testutil.UID+`"`)
}

func TestThreadMdUpdate_VersionIncrement(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "版本递增测试")

	// 更新两次
	for i := 1; i <= 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"content": fmt.Sprintf("version %d", i),
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), fmt.Sprintf(`"version":%d`, i))
	}
}

func TestThreadMdUpdate_EmptyContent(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "空内容测试")

	// 空字符串
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "content must not be empty")

	// 纯空白
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "   \n\t  ",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "content must not be empty")
}

func TestThreadMdUpdate_ExceedsMaxSize(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "大小限制测试")

	// 创建超大内容
	bigContent := strings.Repeat("x", 10241) // 默认 10240 字节
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": bigContent,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "exceeds max size")
}

func TestThreadMdDelete(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "md删除测试")

	// 先设置内容
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 待删除",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证已删除（内容为空，版本号递增）
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"content":""`)
	assert.Contains(t, w.Body.String(), `"version":2`)
}

// TestThreadMdGet_GroupMemberCanRead 验证群成员可以正常读取子区 GROUP.md
// 注：权限拒绝路径（非群成员被拒绝）需要多用户 token 支持后补充
func TestThreadMdGet_GroupMemberCanRead(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "权限测试")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestThreadMdUpdate_CreatorCanEdit 验证群创建者可以编辑子区 GROUP.md
// 注：权限拒绝路径（普通成员不能编辑）需要多用户 token 支持后补充
func TestThreadMdUpdate_CreatorCanEdit(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 用 testutil.UID（群创建者）创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "权限测试")

	// 群创建者编辑 GROUP.md
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "创建者编辑的内容",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestThreadMdDelete_CreatorCanDelete 验证群创建者可以删除子区 GROUP.md
// 注：权限拒绝路径（普通成员不能删除）需要多用户 token 支持后补充
func TestThreadMdDelete_CreatorCanDelete(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "删除权限测试")

	// 先设置内容
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 内容",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 群创建者可以删除
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestThreadMdGet_InvalidGroupNo(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _ := setupTestData(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/invalid/threads/123456789012345/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid group_no format")
}

func TestThreadMdGet_InvalidShortID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/invalid/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid short_id format")
}

func TestThreadMd_ArchivedThread_CanReadAndEdit(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)
	shortID := createThreadViaAPI(t, s, groupNo, "归档md测试")

	// 先设置 GROUP.md
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 归档前设置",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 归档子区
	archiveThread(t, s, groupNo, shortID)

	// 归档后仍可读取
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "归档前设置")

	// 归档后仍可编辑
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 归档后更新",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestThreadResp_HasThreadMdFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	// 创建子区
	shortID := createThreadViaAPI(t, s, groupNo, "ThreadResp测试")

	// 获取详情，验证初始 GROUP.md 字段
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"has_thread_md":false`)
	assert.Contains(t, w.Body.String(), `"thread_md_version":0`)

	// 设置 GROUP.md
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/v1/groups/"+groupNo+"/threads/"+shortID+"/md", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"content": "# 规范",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 再次获取详情，验证 GROUP.md 字段更新
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/groups/"+groupNo+"/threads/"+shortID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"has_thread_md":true`)
	assert.Contains(t, w.Body.String(), `"thread_md_version":1`)
	assert.Contains(t, w.Body.String(), `"thread_md_updated_at"`)
}
