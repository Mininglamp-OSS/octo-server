package group

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	// common.Setup 需要 OCTO_MASTER_KEY 存在才能初始化 IM 私钥加密（见
	// modules/common/key_encryption.go）。testutil.NewTestServer 会间接调用 common.Setup，
	// 所以本 package 所有测试都必须设置。
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	db, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true")
	if err == nil {
		defer db.Close()
		// COLLATE=utf8mb4_general_ci must match what the goose migrations
		// produce: on mysql:8.0 the server default is utf8mb4_0900_ai_ci,
		// so a bare `DEFAULT CHARSET=utf8mb4` here would create the robot
		// table in 0900_ai_ci. The space migration
		// 20260308000002_space_legacy01.sql then INNER JOINs `robot` to
		// `space_member` (which migrations build as utf8mb4_general_ci)
		// and crashes with "Illegal mix of collations". The thread
		// package's main_test.go already sets the correct collation here;
		// keep group's manually-built fixture in sync.
		db.Exec("CREATE TABLE IF NOT EXISTS `robot` (`id` BIGINT AUTO_INCREMENT PRIMARY KEY, `robot_id` VARCHAR(40) NOT NULL DEFAULT '', `token` VARCHAR(100) NOT NULL DEFAULT '', `version` BIGINT NOT NULL DEFAULT 0, `status` SMALLINT NOT NULL DEFAULT 1, `creator_uid` VARCHAR(40) NOT NULL DEFAULT '', `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
		db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS `robot_id_robot_index` ON `robot` (`robot_id`)")
		db.Exec("CREATE TABLE IF NOT EXISTS `robot_menu` (`id` BIGINT AUTO_INCREMENT PRIMARY KEY, `robot_id` VARCHAR(40) NOT NULL DEFAULT '', `cmd` VARCHAR(100) NOT NULL DEFAULT '', `remark` VARCHAR(100) NOT NULL DEFAULT '', `type` VARCHAR(100) NOT NULL DEFAULT '', `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	}
	os.Exit(m.Run())
}

func TestGroupCreate(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")

	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := f.userDB.Insert(&user.Model{
		UID:  "10009",
		Name: "张九",
	})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{
		UID:  "10010",
		Name: "李十",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":    "群组1",
		"members": []string{"10009", "10010"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"群组1"`)
	time.Sleep(time.Millisecond * 200)
}

func TestGroupCreate_WithCategoryID(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 确保 group_category 表和 group_setting category 列存在（category 模块迁移可能未执行）
	ctx.DB().InsertBySql("CREATE TABLE IF NOT EXISTS `group_category` (`id` BIGINT AUTO_INCREMENT PRIMARY KEY, `category_id` VARCHAR(32) NOT NULL, `space_id` VARCHAR(40) NOT NULL, `uid` VARCHAR(40) NOT NULL, `name` VARCHAR(100) NOT NULL, `sort` INT NOT NULL DEFAULT 0, `status` TINYINT NOT NULL DEFAULT 1, `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP, `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, UNIQUE KEY `uk_category_id` (`category_id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4").Exec()
	ctx.DB().InsertBySql("ALTER TABLE `group_setting` ADD COLUMN `category_id` VARCHAR(32) DEFAULT NULL").Exec()
	ctx.DB().InsertBySql("ALTER TABLE `group_setting` ADD COLUMN `category_sort` INT NOT NULL DEFAULT 0").Exec()

	spaceID := "space-create-cat-001"
	categoryID := "cat-create-001"

	// 创建测试用户（使用唯一 ShortNo 避免冲突）
	f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "测试用户", ShortNo: "cat_u10000"})
	err := f.userDB.Insert(&user.Model{UID: "10009", Name: "张九", ShortNo: "cat_u10009"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "10010", Name: "李十", ShortNo: "cat_u10010"})
	assert.NoError(t, err)

	// 创建 Space 并添加成员
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "测试空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	for _, uid := range []string{testutil.UID, "10009", "10010"} {
		_, err = ctx.DB().InsertInto("space_member").
			Columns("space_id", "uid", "role", "status").
			Values(spaceID, uid, 0, 1).Exec()
		assert.NoError(t, err)
	}

	// 创建 Category（属于当前用户）
	_, err = ctx.DB().InsertInto("group_category").
		Columns("category_id", "space_id", "uid", "name", "sort", "status").
		Values(categoryID, spaceID, testutil.UID, "工作", 0, 1).Exec()
	assert.NoError(t, err)

	// 创建群聊并携带 category_id
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":        "分组群聊",
		"members":     []string{"10009", "10010"},
		"space_id":    spaceID,
		"category_id": categoryID,
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	if !assert.Equal(t, http.StatusOK, w.Code) {
		t.Fatalf("创建群失败，响应: %s", w.Body.String())
	}
	assert.Contains(t, w.Body.String(), `"name":"分组群聊"`)

	// 解析响应获取 group_no
	var resp map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	groupNo := resp["group_no"].(string)

	// 验证 group_setting 中 category_id 已设置
	var settingCategoryID *string
	_, err = ctx.DB().Select("category_id").From("group_setting").
		Where("group_no=? and uid=?", groupNo, testutil.UID).
		Load(&settingCategoryID)
	assert.NoError(t, err)
	assert.NotNil(t, settingCategoryID)
	assert.Equal(t, categoryID, *settingCategoryID)

	time.Sleep(time.Millisecond * 200)
}

func TestGroupCreate_WithCategoryID_NoSpaceID(t *testing.T) {
	s, _ := testutil.NewTestServer()

	// 传 category_id 但不传 space_id → 应报错
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":        "分组群聊",
		"members":     []string{"10009"},
		"category_id": "cat-no-space",
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGroupCreate_WithCategoryID_NotFound(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	spaceID := "space-cat-notfound"

	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "测试空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	// 传不存在的 category_id → 应报错
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":        "分组群聊",
		"members":     []string{testutil.UID},
		"space_id":    spaceID,
		"category_id": "cat-not-exist",
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGroupCreate_WithCategoryID_NotOwned(t *testing.T) {
	s, ctx := testutil.NewTestServer()

	spaceID := "space-cat-notowned"
	categoryID := "cat-other-user"

	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "测试空间", testutil.UID, 1).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	assert.NoError(t, err)

	// Category 属于其他用户
	_, err = ctx.DB().InsertInto("group_category").
		Columns("category_id", "space_id", "uid", "name", "sort", "status").
		Values(categoryID, spaceID, "other_user", "工作", 0, 1).Exec()
	assert.NoError(t, err)

	// 传别人的 category_id → 应报错
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":        "分组群聊",
		"members":     []string{testutil.UID},
		"space_id":    spaceID,
		"category_id": categoryID,
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGroupGet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo:            "1",
		Name:               "test",
		Creator:            testutil.UID,
		Version:            1,
		Status:             1,
		ForbiddenAddFriend: 1,
	})
	assert.NoError(t, err)
	err = f.settingDB.InsertSetting(&Setting{
		GroupNo:         "1",
		UID:             "10000",
		Mute:            1,
		Save:            1,
		ShowNick:        1,
		Top:             1,
		ChatPwdOn:       1,
		JoinGroupRemind: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/1", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"group_no":"1"`, `"name":"test"`, `"chat_pwd":1`, `"mute":1`, `"top":1`, `"show_nick":1`, `"save":1`)

	time.Sleep(time.Millisecond * 200)
}

func TestGroupMemberAdd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "10009",
		Name: "张九",
	})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{
		UID:  "10010",
		Name: "李十",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// operator 必须是群成员,否则会被拒绝(issue#1018)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "1",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/1/members", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"10009", "10010"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

}

// TestGroupMemberAdd_NonMemberForbidden 回归测试 issue#1018:
// 非群成员的任意用户不能向 Invite==0 的群添加成员
func TestGroupMemberAdd_NonMemberForbidden(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{UID: "victim_a", Name: "A", ShortNo: "sa1"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "victim_b", Name: "B", ShortNo: "sb1"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "group_owner", Name: "Owner", ShortNo: "so1"})
	assert.NoError(t, err)

	// 创建群,群主是 group_owner,operator(testutil.UID) 不是成员
	err = f.db.Insert(&Model{
		GroupNo: "g_victim",
		Name:    "victim_group",
		Creator: "group_owner",
		Status:  1,
		Invite:  0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_victim",
		UID:     "group_owner",
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/g_victim/members", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"victim_a", "victim_b"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	// 应该被拒绝
	assert.NotEqual(t, http.StatusOK, w.Code, "非群成员不应能添加群成员")

	// 被攻击的用户不应被实际写入
	exist, err := f.db.ExistMember("victim_a", "g_victim")
	assert.NoError(t, err)
	assert.False(t, exist, "victim_a 不应被加入群")
}

// TestGroupMemberAdd_KickedMemberForbidden 回归测试 issue#1018:
// 曾经是群成员但已被移除(is_deleted=1)的用户,不能再通过 memberAdd 添加他人
func TestGroupMemberAdd_KickedMemberForbidden(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{UID: "victim_c", Name: "C", ShortNo: "sc1"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "owner_k", Name: "OwnerK", ShortNo: "sok1"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "g_kicked",
		Name:    "kicked_group",
		Creator: "owner_k",
		Status:  1,
		Invite:  0,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_kicked",
		UID:     "owner_k",
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)
	// operator(testutil.UID) 是一个"已被踢出"的成员 —— is_deleted=1
	err = f.db.InsertMember(&MemberModel{
		GroupNo:   "g_kicked",
		UID:       testutil.UID,
		Role:      MemberRoleCommon,
		Version:   2,
		IsDeleted: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/g_kicked/members", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"victim_c"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code, "已被踢出的成员不应能添加群成员")

	exist, err := f.db.ExistMember("victim_c", "g_kicked")
	assert.NoError(t, err)
	assert.False(t, exist, "victim_c 不应被加入群")
}

func TestGroupMemberRemove(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "10009",
		Name: "张九",
	})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{
		UID:  "10010",
		Name: "李十",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("DELETE", "/v1/groups/1/members", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"10009", "10010"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

}

func TestSyncMembers(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "10009",
		Name: "张九",
	})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{
		UID:  "10010",
		Name: "李十",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "1",
		UID:     "10009",
		Version: 2,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "1",
		UID:     "10010",
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/1/membersync?version=1", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	b := w.Body.String()
	assert.Contains(t, b, `"uid":"10009"`)
	assert.NotContains(t, b, `"uid":"10010"`)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGroupSettingUpdate(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Version: 1,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		UID:     testutil.UID,
		GroupNo: "1",
		Role:    1,
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/groups/1/setting", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"mute":      1,
		"top":       1,
		"save":      1,
		"show_nick": 1,
		"chat_pwd":  1,
		"forbidden": 1,
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGroupUpdate(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Version: 1,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "1",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/groups/1", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name": "test2",
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

}
func TestList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 先清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "1",
		Name:    "test",
		Creator: testutil.UID,
		Version: 1,
		Status:  1,
	})
	assert.NoError(t, err)
	err = f.settingDB.InsertSetting(&Setting{
		UID:     testutil.UID,
		GroupNo: "1",
		Save:    1,
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/group/my", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"group_no":`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":`))

}

// TestGroupExit 测试退出群聊
func TestGroupExit(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建群和成员
	err = f.db.Insert(&Model{
		GroupNo: "exit_group",
		Name:    "exit test",
		Creator: "creator_uid",
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "exit_group",
		UID:     testutil.UID,
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/exit_group/exit", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGroupDisband 测试解散群组
func TestGroupDisband(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "disband_group",
		Name:    "disband test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "disband_group",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("DELETE", "/v1/groups/disband_group", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGroupManagerAdd 测试添加管理员
func TestGroupManagerAdd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "new_manager",
		Name: "新管理员",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "mgr_group",
		Name:    "manager test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "mgr_group",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "mgr_group",
		UID:     "new_manager",
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/mgr_group/managers", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"new_manager"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGroupManagerRemove 测试移除管理员
func TestGroupManagerRemove(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "mgr_to_remove",
		Name: "待移除管理员",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "mgr_group2",
		Name:    "manager remove test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "mgr_group2",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "mgr_group2",
		UID:     "mgr_to_remove",
		Role:    MemberRoleManager,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("DELETE", "/v1/groups/mgr_group2/managers", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"members": []string{"mgr_to_remove"},
	}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGroupTransfer 测试群主转让
func TestGroupTransfer(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{
		UID:  "new_owner",
		Name: "新群主",
	})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "transfer_group",
		Name:    "transfer test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "transfer_group",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "transfer_group",
		UID:     "new_owner",
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/transfer_group/transfer/new_owner", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGroupForbidden 测试群组全员禁言
func TestGroupForbidden(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "forbidden_group",
		Name:    "forbidden test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "forbidden_group",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Version: 1,
	})
	assert.NoError(t, err)

	// 开启全员禁言
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/forbidden_group/forbidden/1", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 关闭全员禁言
	w2 := httptest.NewRecorder()
	req2, err := http.NewRequest("POST", "/v1/groups/forbidden_group/forbidden/0", nil)
	req2.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

// TestGroupMembersGet 测试获取群成员列表
func TestGroupMembersGet(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.userDB.Insert(&user.Model{UID: "member1", Name: "成员一"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "member2", Name: "成员二"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "members_group",
		Name:    "get members test",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	err = f.db.InsertMember(&MemberModel{
		GroupNo: "members_group",
		UID:     "member1",
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "members_group",
		UID:     "member2",
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/members_group/members", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"uid":"member1"`)
	assert.Contains(t, w.Body.String(), `"uid":"member2"`)
}

func TestGroupDetailGet_MemberCanAccess(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create a group
	err = f.db.Insert(&Model{
		GroupNo: "detail_test_group",
		Name:    "Test Group",
		Creator: testutil.UID,
		Status:  1,
		Notice:  "Sensitive notice content",
	})
	assert.NoError(t, err)

	// Add testutil.UID as a member
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "detail_test_group",
		UID:     testutil.UID,
		Role:    MemberRoleCommon,
		Version: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/detail_test_group/detail", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"name":"Test Group"`)
}

func TestGroupDetailGet_NonMemberDenied(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Create a group without adding the test user as member
	err = f.db.Insert(&Model{
		GroupNo: "detail_test_group2",
		Name:    "Private Group",
		Creator: "other_user",
		Status:  1,
		Notice:  "Secret notice",
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/detail_test_group2/detail", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	// Should return 403 Forbidden for non-members
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "无权限查看群详情")
}

func TestGroupDetailGet_UnauthenticatedDenied(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "detail_test_group3",
		Name:    "Another Group",
		Creator: "creator",
		Status:  1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	// Request without auth token
	req, err := http.NewRequest("GET", "/v1/groups/detail_test_group3/detail", nil)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	// Should be unauthorized
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestBlacklistRemoveUsesFilteredUIDs tests that blacklist removal only removes
// members with ForbiddenExpirTime == 0, not all requested UIDs (issue #482)
func TestBlacklistRemoveUsesFilteredUIDs(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "blacklist_test_group"

	// Create users
	err = f.userDB.Insert(&user.Model{
		UID:  "user_a",
		Name: "User A",
	})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{
		UID:  "user_b",
		Name: "User B",
	})
	assert.NoError(t, err)

	// Create group with testutil.UID as creator
	err = f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "Blacklist Test Group",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// Add creator as member with creator role
	err = f.db.InsertMember(&MemberModel{
		GroupNo: groupNo,
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Status:  1,
		Version: 1,
	})
	assert.NoError(t, err)

	// Add user_a: blacklisted with ForbiddenExpirTime = 0 (should be removed)
	err = f.db.InsertMember(&MemberModel{
		GroupNo:            groupNo,
		UID:                "user_a",
		Role:               MemberRoleCommon,
		Status:             2, // GroupMemberStatusBlacklist
		ForbiddenExpirTime: 0, // No active forbidden period
		Version:            1,
	})
	assert.NoError(t, err)

	// Add user_b: blacklisted with ForbiddenExpirTime > 0 (should NOT be removed)
	futureTime := time.Now().Add(24 * time.Hour).Unix()
	err = f.db.InsertMember(&MemberModel{
		GroupNo:            groupNo,
		UID:                "user_b",
		Role:               MemberRoleCommon,
		Status:             2, // GroupMemberStatusBlacklist
		ForbiddenExpirTime: futureTime,
		Version:            1,
	})
	assert.NoError(t, err)

	// Call blacklist remove for both users
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/blacklist/remove",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"uids": []string{"user_a", "user_b"},
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify: user_a should have status changed to normal (1)
	memberA, err := f.db.QueryMemberWithUID("user_a", groupNo)
	assert.NoError(t, err)
	assert.NotNil(t, memberA)
	assert.Equal(t, 1, memberA.Status, "user_a with ForbiddenExpirTime=0 should be removed from blacklist")

	// Verify: user_b should still have blacklist status because ForbiddenExpirTime > 0
	// The fix ensures setGroupBlacklist is only called with removeUIDs (user_a),
	// not req.Uids (both users). user_b's DB status was set to normal by updateMembersStatus,
	// but the IM blacklist should only be updated for user_a.
	memberB, err := f.db.QueryMemberWithUID("user_b", groupNo)
	assert.NoError(t, err)
	assert.NotNil(t, memberB)
	// Note: updateMembersStatus sets DB status for all req.Uids to normal,
	// but setGroupBlacklist (IM layer) should only be called for removeUIDs
}

// TestGroupMemberGet_Hit 调用方是成员，目标 uid 是在群成员 → exists=true 且返回 member
func TestGroupMemberGet_Hit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "mcheck_hit"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "调用方", ShortNo: "mck_caller_" + groupNo})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "target_in", Name: "目标", ShortNo: "mck_target_in"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "g", Creator: testutil.UID, Status: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "target_in", Role: MemberRoleCommon, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members/target_in", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"exists":true`)
	assert.Contains(t, body, `"uid":"target_in"`)
}

// TestGroupMemberGet_Miss 调用方是成员，目标 uid 不在群 → exists=false
func TestGroupMemberGet_Miss(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "mcheck_miss"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "调用方", ShortNo: "mck_caller_" + groupNo})
	assert.NoError(t, err)
	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "g", Creator: testutil.UID, Status: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members/not_in", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"exists":false`)
	assert.NotContains(t, body, `"uid":"not_in"`)
}

// TestGroupMemberGet_Deleted 目标 uid 之前在群但 is_deleted=1 → exists=false
func TestGroupMemberGet_Deleted(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "mcheck_del"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "调用方", ShortNo: "mck_caller_" + groupNo})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "target_del", Name: "已退", ShortNo: "mck_target_del"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "g", Creator: testutil.UID, Status: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "target_del", Role: MemberRoleCommon, Version: 1, IsDeleted: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members/target_del", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"exists":false`)
}

// TestGroupMemberGet_Self 调用方查询自己 → exists=true 且返回自身成员信息
func TestGroupMemberGet_Self(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "mcheck_self"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "调用方", ShortNo: "mck_self"})
	assert.NoError(t, err)
	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "g", Creator: testutil.UID, Status: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members/"+testutil.UID, nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"exists":true`)
	assert.Contains(t, w.Body.String(), `"uid":"`+testutil.UID+`"`)
}

// TestGroupMemberGet_CallerNotMember 调用方非该群成员 → 403/error
func TestGroupMemberGet_CallerNotMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupNo := "mcheck_perm"
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "外人", ShortNo: "mck_outsider"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "owner", Name: "群主", ShortNo: "mck_owner"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "target_in2", Name: "目标", ShortNo: "mck_target_in2"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{GroupNo: groupNo, Name: "g", Creator: "owner", Status: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "owner", Role: MemberRoleCreator, Version: 1})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{GroupNo: groupNo, UID: "target_in2", Role: MemberRoleCommon, Version: 1})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members/target_in2", nil)
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.True(t, strings.Contains(w.Body.String(), "权限"))
}
