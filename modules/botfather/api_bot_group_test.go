package botfather

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/assert"

	// Ensure dependent modules register SQL migrations and HTTP routes.
	// bot_api owns POST /v1/bot/createGroup (see modules/bot_api/bot_api.go);
	// the rest provide migrations referenced by setupGroupTestEnv's
	// testutil.NewTestServer call. The blank import is required because
	// the bot_api module self-registers via init() and is otherwise
	// pulled in transitively only by the prod entrypoint
	// (internal/modules.go), not by botfather alone.
	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

// setupGroupTestEnv creates a test environment without double-registering routes.
// Returns the route handler, BotFather db session, and context.
func setupGroupTestEnv(t *testing.T) (http.Handler, *config.Context) {
	s, ctx := testutil.NewTestServer()
	// NewTestServer cleans tables on first call, subsequent calls reuse the same instance.
	// Each test creates unique data (unique group_no via UUID) so no cross-test pollution.
	return s.GetRoute(), ctx
}

// insertTestUser creates a user in the test database.
func insertTestUser(t *testing.T, ctx *config.Context, uid, name string) {
	// Use InsertBySql with IGNORE to avoid duplicate key errors across tests
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO user (uid, name, username, status, short_no, zone, phone) VALUES (?, ?, ?, 1, ?, '', '')",
		uid, name, uid, util.GenerUUID()[:8],
	).Exec()
	assert.NoError(t, err)
}

// insertTestBot creates a bot in the test database and returns the bot_token.
func insertTestBot(t *testing.T, ctx *config.Context, robotID, creatorUID string) string {
	botToken := "bf_" + robotID
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO robot (app_id, robot_id, username, token, version, status, creator_uid, description, bot_token, im_token_cache, bot_commands) VALUES (?, ?, ?, 'test_token', 1, 1, ?, 'test robot', ?, '', '[]')",
		robotID, robotID, robotID, creatorUID, botToken,
	).Exec()
	assert.NoError(t, err)
	return botToken
}

// Also insert into user table so bot is recognized as a user
func insertTestBotUser(t *testing.T, ctx *config.Context, robotID string) {
	// Insert bot as user with robot=1. Use INSERT IGNORE for idempotency.
	_, _ = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO user (uid, name, username, status, robot, short_no, zone, phone) VALUES (?, ?, ?, 1, 1, ?, '', '')",
		robotID, robotID, robotID, util.GenerUUID()[:8],
	).Exec()
	// Also ensure robot flag is set if user already existed
	ctx.DB().UpdateBySql("UPDATE user SET robot=1 WHERE uid=?", robotID).Exec()
}

// botReq builds an HTTP request with Bot token authentication.
func botReq(method, path, botToken string, body interface{}) *http.Request {
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	return req
}

// doRequest executes a request and returns the recorder.
func doRequest(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// jsonResult unmarshals response body into a map.
func jsonResult(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	var result map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &result)
	assert.NoError(t, err, "response body: %s", w.Body.String())
	return result
}

const grpTestBotID = "grptest_bot"

// =====================================================================
// botGroupCreate
// =====================================================================

func TestBotGroupCreate_HappyPath(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Test Group",
		"members": []string{"user_a", "user_b"},
		"creator": testutil.UID,
	}))

	t.Logf("Status: %d, Body: %s", w.Code, w.Body.String())
	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.NotEmpty(t, result["group_no"])
	assert.Equal(t, "Test Group", result["name"])
}

func TestBotGroupCreate_EmptyMembers(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestUser(t, ctx, testutil.UID, "owner")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Empty",
		"members": []string{},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIRequestInvalid.DefaultMessage)
}

func TestBotGroupCreate_EmptyCreator(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, "user_a", "Alice")

	// creator 为空时默认 members[0] 为群主
	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "No Creator",
		"members": []string{"user_a"},
		"creator": "",
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.NotEmpty(t, result["group_no"])
}

func TestBotGroupCreate_AutoName(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"members": []string{"user_a"},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	name := result["name"].(string)
	assert.Contains(t, name, "owner")
}

// =====================================================================
// botGroupUpdate
// =====================================================================

// createGroupViaAPI creates a group and returns group_no.
func createGroupViaAPI(t *testing.T, handler http.Handler, botToken string, members []string) string {
	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Test",
		"members": members,
		"creator": testutil.UID,
	}))
	t.Logf("createGroup: status=%d body=%s", w.Code, w.Body.String())
	assert.Equal(t, http.StatusOK, w.Code)
	return jsonResult(t, w)["group_no"].(string)
}

func TestBotGroupUpdate_HappyPath(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// Bot 创建群后自动加入并成为 bot_admin，无需手动设置
	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("PUT", fmt.Sprintf("/v1/bot/groups/%s/info", groupNo), botToken, map[string]interface{}{
		"name": "Updated",
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)
}

func TestBotGroupUpdate_NotMember(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// 用另一个 Bot 来请求（它没参与建群，不在群内）
	otherBotID := "other_bot_update"
	otherBotToken := insertTestBot(t, ctx, otherBotID, testutil.UID)
	insertTestBotUser(t, ctx, otherBotID)

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("PUT", fmt.Sprintf("/v1/bot/groups/%s/info", groupNo), otherBotToken, map[string]interface{}{
		"name": "Fail",
	}))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBotGroupUpdate_BotAutoAdmin(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// Bot 创建群后自动成为 bot_admin，无需手动设置即可更新
	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("PUT", fmt.Sprintf("/v1/bot/groups/%s/info", groupNo), botToken, map[string]interface{}{
		"name": "AutoAdmin Updated",
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)
}

// =====================================================================
// botGroupMemberAdd
// =====================================================================

func TestBotGroupMemberAdd_HappyPath(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a", grpTestBotID})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_b"},
	}))
	t.Logf("addMembers: status=%d body=%s", w.Code, w.Body.String())

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["added"])
}

func TestBotGroupMemberAdd_Dedup(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a", grpTestBotID})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_b", "user_b"},
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["added"])
}

func TestBotGroupMemberAdd_NotMember(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	// 用另一个 Bot 来请求（它没参与建群，不在群内）
	otherBotID := "other_bot_add"
	otherBotToken := insertTestBot(t, ctx, otherBotID, testutil.UID)
	insertTestBotUser(t, ctx, otherBotID)

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), otherBotToken, map[string]interface{}{
		"members": []string{"user_b"},
	}))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// botGroupMemberRemove
// =====================================================================

func TestBotGroupMemberRemove_HappyPath(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	// Bot 创建群后自动成为 bot_admin
	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a", "user_b"})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/remove", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_b"},
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["removed"])
}

func TestBotGroupMemberRemove_CannotRemoveCreator(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// Bot 创建群后自动成为 bot_admin
	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/remove", groupNo), botToken, map[string]interface{}{
		"members": []string{testutil.UID},
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(0), result["removed"])
}

func TestBotGroupMemberRemove_NotBotAdmin(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// 用另一个 Bot（不在群内，没有 bot_admin 权限）
	otherBotID := "other_bot_rm"
	otherBotToken := insertTestBot(t, ctx, otherBotID, testutil.UID)
	insertTestBotUser(t, ctx, otherBotID)

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/remove", groupNo), otherBotToken, map[string]interface{}{
		"members": []string{"user_a"},
	}))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// botSpaceMembers
// =====================================================================

func TestBotSpaceMembers_HappyPath(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	spaceID := "test_space_grp"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Test Space", testutil.UID, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, grpTestBotID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_a", 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_b", 0, 1).Exec()

	w := doRequest(handler, botReq("GET", fmt.Sprintf("/v1/bot/space/members?space_id=%s", spaceID), botToken, nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var members []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &members)
	assert.GreaterOrEqual(t, len(members), 2)
}

// =====================================================================
// 针对修复的测试
// =====================================================================

// 验证创建群时，members 中包含不存在的 UID 不会导致崩溃
// QueryByUIDs 会过滤掉不存在的用户，只插入有效成员
func TestBotGroupCreate_WithNonExistentMember(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Partial Members",
		"members": []string{"user_a", "nonexistent_uid_12345"},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.NotEmpty(t, result["group_no"])

	// 验证群成员：应该有 creator + user_a + bot，没有 nonexistent_uid
	groupNo := result["group_no"].(string)
	var memberCount int
	ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_deleted=0", groupNo).LoadOne(&memberCount)
	assert.Equal(t, 3, memberCount) // owner + user_a + bot
}

// 验证创建群时 members 中有重复的 UID，不会重复插入
func TestBotGroupCreate_DuplicateMembers(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Dedup Test",
		"members": []string{"user_a", "user_a", "user_a"},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	groupNo := result["group_no"].(string)

	var memberCount int
	ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_deleted=0", groupNo).LoadOne(&memberCount)
	// creator + user_a(一次) + bot = 3，不应该有重复
	assert.Equal(t, 3, memberCount)
}

// 验证创建群时 creator 也在 members 列表中，不会重复插入
func TestBotGroupCreate_CreatorInMembers(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "Creator In Members",
		"members": []string{testutil.UID, "user_a"},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	groupNo := result["group_no"].(string)

	var memberCount int
	ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_deleted=0", groupNo).LoadOne(&memberCount)
	assert.Equal(t, 3, memberCount) // creator + user_a + bot（creator 不重复）
}

// 验证创建群后 Bot 自动成为 bot_admin，可以直接执行管理操作
func TestBotGroupCreate_BotIsAutoAdmin(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a", "user_b"})

	// 验证 DB 中 bot 是 bot_admin
	var botAdmin int
	ctx.DB().SelectBySql(
		"SELECT IFNULL(bot_admin, 0) FROM group_member WHERE group_no=? AND uid=?", groupNo, grpTestBotID,
	).LoadOne(&botAdmin)
	assert.Equal(t, 1, botAdmin)

	// 验证 bot 能执行管理操作：移除成员
	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/remove", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_b"},
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["removed"])
}

// 验证移除成员后重新添加（走 ExistMemberDelete 恢复路径）
func TestBotGroupMemberAdd_ReaddRemovedMember(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_readd", "ReaddUser")

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a", "user_readd"})

	// 先移除
	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/remove", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_readd"},
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"removed":1`)

	// 验证已被软删除
	var isDeleted int
	ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, "user_readd",
	).LoadOne(&isDeleted)
	assert.Equal(t, 1, isDeleted)

	// 重新添加
	w = doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_readd"},
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["added"])

	// 验证已恢复
	ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, "user_readd",
	).LoadOne(&isDeleted)
	assert.Equal(t, 0, isDeleted)
}

// 验证编辑群同时传 name 和 notice 都能生效
func TestBotGroupUpdate_NameAndNotice(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	newName := "New Name"
	newNotice := "Hello World"
	w := doRequest(handler, botReq("PUT", fmt.Sprintf("/v1/bot/groups/%s/info", groupNo), botToken, map[string]interface{}{
		"name":   newName,
		"notice": newNotice,
	}))
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证 DB 中两个字段都更新了
	var dbName, dbNotice string
	ctx.DB().SelectBySql("SELECT name FROM `group` WHERE group_no=?", groupNo).LoadOne(&dbName)
	ctx.DB().SelectBySql("SELECT notice FROM `group` WHERE group_no=?", groupNo).LoadOne(&dbNotice)
	assert.Equal(t, newName, dbName)
	assert.Equal(t, newNotice, dbNotice)
}

// =====================================================================
// botSpaceMembers (continued)
// =====================================================================

func TestBotSpaceMembers_KeywordSearch(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	spaceID := "test_space_search"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Search Space", testutil.UID, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, grpTestBotID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_a", 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_b", 0, 1).Exec()

	w := doRequest(handler, botReq("GET", fmt.Sprintf("/v1/bot/space/members?space_id=%s&keyword=Alice", spaceID), botToken, nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var members []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &members)
	assert.Equal(t, 1, len(members))
	assert.Equal(t, "Alice", members[0]["name"])
}

// =====================================================================
// Space 权限负例测试
// =====================================================================

// TestBotSpaceMembers_RejectOtherSpace 验证 bot 不能查询不属于自己的 Space
func TestBotSpaceMembers_RejectOtherSpace(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)

	// 创建一个 bot 不在的 Space
	otherSpaceID := "other_space_reject"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(otherSpaceID, "Other Space", testutil.UID, 1).Exec()
	// bot 不加入这个 Space

	w := doRequest(handler, botReq("GET", fmt.Sprintf("/v1/bot/space/members?space_id=%s", otherSpaceID), botToken, nil))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPINotSpaceMember.DefaultMessage)
}

// TestBotGroupCreate_RejectCreatorOutsideSpace 验证 creator 不在 Space 时应失败
func TestBotGroupCreate_RejectCreatorOutsideSpace(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, "user_in_space", "InSpace")
	insertTestUser(t, ctx, "user_outside", "OutSide")

	spaceID := "space_create_test"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Test Space", testutil.UID, 1).Exec()
	// bot 和 user_in_space 在 Space 内
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, grpTestBotID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_in_space", 0, 1).Exec()
	// user_outside 不在 Space 内

	// creator 不在 Space，应失败
	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":     "Reject Test",
		"members":  []string{"user_in_space"},
		"creator":  "user_outside",
		"space_id": spaceID,
	}))

	assert.NotEqual(t, http.StatusOK, w.Code)
}

// TestBotGroupCreate_RejectMemberOutsideSpace 验证不在 Space 的成员不能加入群
func TestBotGroupCreate_RejectMemberOutsideSpace(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_in", "InSpace")
	insertTestUser(t, ctx, "user_out", "OutSpace")

	spaceID := "space_member_test"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Test Space", testutil.UID, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, grpTestBotID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_in", 0, 1).Exec()
	// user_out 不在 Space

	// 创建群，members 包含 user_out（不在 Space）
	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":     "Space Filter",
		"members":  []string{"user_in", "user_out"},
		"creator":  testutil.UID,
		"space_id": spaceID,
	}))

	// 应该成功，但 user_out 不在群内（被 Space 校验过滤）
	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	groupNo := result["group_no"].(string)

	var memberCount int
	ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_deleted=0", groupNo).LoadOne(&memberCount)
	// creator + user_in + bot = 3（user_out 被过滤）
	assert.Equal(t, 3, memberCount)
}

// TestBotGroupCreate_DefaultAllowViewHistoryMsg 验证 bot 建群 allow_view_history_msg=1
func TestBotGroupCreate_DefaultAllowViewHistoryMsg(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	w := doRequest(handler, botReq("POST", "/v1/bot/createGroup", botToken, map[string]interface{}{
		"name":    "HistoryMsg Test",
		"members": []string{"user_a"},
		"creator": testutil.UID,
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	groupNo := result["group_no"].(string)

	var allowViewHistoryMsg int
	ctx.DB().SelectBySql("SELECT allow_view_history_msg FROM `group` WHERE group_no=?", groupNo).LoadOne(&allowViewHistoryMsg)
	assert.Equal(t, 1, allowViewHistoryMsg)
}

// TestBotGroupMemberAdd_RejectMemberOutsideGroupSpace 验证不在群所属 Space 的成员不能被添加
func TestBotGroupMemberAdd_RejectMemberOutsideGroupSpace(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_in", "InSpace")
	insertTestUser(t, ctx, "user_out_add", "OutSpace")

	spaceID := "space_add_test"
	ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "Add Test Space", testutil.UID, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, grpTestBotID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
		Values(spaceID, "user_in", 0, 1).Exec()

	// 先创建群（在 Space 内）
	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_in"})

	// 更新群的 space_id（createGroupViaAPI 不传 space_id）
	ctx.DB().UpdateBySql("UPDATE `group` SET space_id=? WHERE group_no=?", spaceID, groupNo).Exec()

	// 尝试添加不在 Space 的用户
	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_out_add"},
	}))

	// 应该失败：成员不在 Space 内
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// =====================================================================
// Bot 不能拉 Bot 进群
// =====================================================================

// TestBotGroupMemberAdd_RejectPureBotMembers 纯 bot 成员应被拒绝
func TestBotGroupMemberAdd_RejectPureBotMembers(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")

	// 创建另一个 bot
	otherBotID := "other_bot_add_test"
	insertTestBot(t, ctx, otherBotID, testutil.UID)
	insertTestBotUser(t, ctx, otherBotID)

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	// 尝试拉 bot 进群，应被拒绝
	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{otherBotID},
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIMemberNotHuman.DefaultMessage)
}

// TestBotGroupMemberAdd_MixedHumanAndBot 混合传入时 bot 被过滤，人正常添加
func TestBotGroupMemberAdd_MixedHumanAndBot(t *testing.T) {
	handler, ctx := setupGroupTestEnv(t)
	botToken := insertTestBot(t, ctx, grpTestBotID, testutil.UID)
	insertTestBotUser(t, ctx, grpTestBotID)
	insertTestUser(t, ctx, testutil.UID, "owner")
	insertTestUser(t, ctx, "user_a", "Alice")
	insertTestUser(t, ctx, "user_b", "Bob")

	otherBotID := "other_bot_mixed_test"
	insertTestBot(t, ctx, otherBotID, testutil.UID)
	insertTestBotUser(t, ctx, otherBotID)

	groupNo := createGroupViaAPI(t, handler, botToken, []string{"user_a"})

	// 混合传入 human + bot
	w := doRequest(handler, botReq("POST", fmt.Sprintf("/v1/bot/groups/%s/members/add", groupNo), botToken, map[string]interface{}{
		"members": []string{"user_b", otherBotID},
	}))

	assert.Equal(t, http.StatusOK, w.Code)
	result := jsonResult(t, w)
	assert.Equal(t, float64(1), result["added"])
	// bot 应出现在 skipped_bots 中
	skipped := result["skipped_bots"]
	assert.NotNil(t, skipped)
}
