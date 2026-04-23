package space

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

var (
	testSrv     *server.Server
	testCtx     *config.Context
	testSpaceDB *DB
)

// TestMain 确保 space 迁移所依赖的外部表存在，并创建共享测试服务器
func TestMain(m *testing.M) {
	db, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true")
	if err != nil {
		panic("连接测试数据库失败: " + err.Error())
	}

	// space 迁移脚本依赖 group 和 robot 表
	depDDLs := []string{
		"CREATE TABLE IF NOT EXISTS `group` (id BIGINT AUTO_INCREMENT PRIMARY KEY, group_no VARCHAR(40) NOT NULL DEFAULT '', name VARCHAR(100) DEFAULT '', creator VARCHAR(40) DEFAULT '', status SMALLINT DEFAULT 1, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, UNIQUE KEY idx_group_no(group_no)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE IF NOT EXISTS group_member (id BIGINT AUTO_INCREMENT PRIMARY KEY, group_no VARCHAR(40) DEFAULT '', uid VARCHAR(40) DEFAULT '', role INT DEFAULT 0, is_deleted SMALLINT DEFAULT 0, status SMALLINT DEFAULT 1, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE IF NOT EXISTS robot (id BIGINT AUTO_INCREMENT PRIMARY KEY, robot_id VARCHAR(40) NOT NULL DEFAULT '', token VARCHAR(200) DEFAULT '', status SMALLINT DEFAULT 1, creator_uid VARCHAR(40) DEFAULT '', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, UNIQUE KEY idx_robot_id(robot_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE IF NOT EXISTS `user` (id BIGINT AUTO_INCREMENT PRIMARY KEY, uid VARCHAR(40) NOT NULL DEFAULT '', name VARCHAR(100) DEFAULT '', email VARCHAR(200) DEFAULT '', avatar VARCHAR(200) DEFAULT '', robot SMALLINT DEFAULT 0, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, UNIQUE KEY idx_uid(uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	}
	for _, ddl := range depDDLs {
		if _, err := db.Exec(ddl); err != nil {
			panic("创建依赖表失败: " + err.Error())
		}
	}
	db.Close()

	// 创建共享测试服务器（只初始化一次，避免路由重复注册）
	s, ctx := testutil.NewTestServer()
	testSrv = s
	testCtx = ctx
	testSpaceDB = NewDB(ctx)

	os.Exit(m.Run())
}

func strPtr(s string) *string { return &s }

// setup 返回共享的测试服务器和 Space 实例，并清理表数据
func setup(t *testing.T) (*server.Server, *Space, error) {
	t.Helper()
	err := testutil.CleanAllTables(testCtx)
	assert.NoError(t, err)
	return testSrv, New(testCtx), err
}

func TestGetInvitePreview(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space
	spaceId := "test-space-001"
	inviteCode := "abc12345"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:     spaceId,
		Name:        "测试空间",
		Description: "这是一个测试空间描述",
		Logo:        "https://example.com/logo.png",
		Creator:     testutil.UID,
		Status:      1,
	})
	assert.NoError(t, err)

	// 添加空间成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    testutil.UID,
		MaxUses:    10,
		UsedCount:  2,
		Status:     1,
	})
	assert.NoError(t, err)

	// 测试获取邀请预览（公开接口，无需 token）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/space/invite/"+inviteCode+"/preview", nil)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"space_name":"测试空间"`)
	assert.Contains(t, body, `"description":"这是一个测试空间描述"`)
	assert.Contains(t, body, `"logo":"https://example.com/logo.png"`)
	assert.Contains(t, body, `"bots":`)
	assert.Contains(t, body, `"member_count":1`)
}

func TestGetInvitePreviewWithBots(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space
	spaceId := "test-space-002"
	inviteCode := "xyz98765"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:     spaceId,
		Name:        "带 Bot 的空间",
		Description: "测试 Bot 列表",
		Logo:        "",
		Creator:     testutil.UID,
		Status:      1,
	})
	assert.NoError(t, err)

	// 添加空间成员（人类用户）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建一个 Bot 用户
	botUID := "bot-001"
	_, err = testCtx.DB().InsertInto("user").Columns("uid", "name", "avatar").
		Values(botUID, "AI 助手", "https://example.com/bot.png").Exec()
	assert.NoError(t, err)

	// 在 robot 表中注册 Bot
	_, err = testCtx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
		Values(botUID, "test-token", 1).Exec()
	assert.NoError(t, err)

	// 将 Bot 添加为空间成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     botUID,
		Role:    0,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    testutil.UID,
		Status:     1,
	})
	assert.NoError(t, err)

	// 测试获取邀请预览
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/space/invite/"+inviteCode+"/preview", nil)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"space_name":"带 Bot 的空间"`)
	assert.Contains(t, body, `"robot_id":"bot-001"`)
	assert.Contains(t, body, `"name":"AI 助手"`)
	assert.Contains(t, body, `"member_count":2`)
}

func TestGetInvitePreviewInvalidCode(t *testing.T) {
	s, _, err := setup(t)

	// 测试无效邀请码
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/space/invite/invalid-code/preview", nil)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请码无效")
}

func TestUpdateInvite(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space
	spaceId := "test-space-003"
	inviteCode := "upd12345"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    "更新邀请码测试",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// 添加空间成员（管理员）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    1, // 管理员
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    testutil.UID,
		MaxUses:    0,
		Status:     1,
	})
	assert.NoError(t, err)

	// 测试更新邀请码设置
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/"+inviteCode,
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"max_uses":   100,
			"expires_at": "2026-12-31 23:59:59",
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证更新生效
	invitation, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.NotNil(t, invitation)
	assert.Equal(t, 100, invitation.MaxUses)
	assert.NotNil(t, invitation.ExpiresAt)
	expiresAt := time.Time(*invitation.ExpiresAt)
	assert.Equal(t, 2026, expiresAt.Year())
	assert.Equal(t, time.December, expiresAt.Month())
	assert.Equal(t, 31, expiresAt.Day())
}

func TestUpdateInviteNoPermission(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space
	spaceId := "test-space-004"
	inviteCode := "nop12345"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    "权限测试",
		Creator: "other-user",
		Status:  1,
	})
	assert.NoError(t, err)

	// 添加空间成员（普通成员，Role=0）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    0,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    "other-user",
		Status:     1,
	})
	assert.NoError(t, err)

	// 测试普通成员尝试更新邀请码（应该失败）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/"+inviteCode,
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"max_uses": 50,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "无权限")
}

func TestUpdateInviteInvalidCode(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space
	spaceId := "test-space-005"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId,
		Name:    "无效邀请码测试",
		Creator: testutil.UID,
		Status:  1,
	})
	assert.NoError(t, err)

	// 添加空间成员（管理员）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     testutil.UID,
		Role:    1,
		Status:  1,
	})
	assert.NoError(t, err)

	// 测试更新不存在的邀请码
	w := httptest.NewRecorder()
	req, err := http.NewRequest("PUT", "/v1/space/"+spaceId+"/invite/invalid-code",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"max_uses": 50,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "邀请码不存在")
}

func TestJoinSpaceFullReturnsSpaceFullError(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space（max_users=1，只允许1人）
	spaceId := "test-space-full"
	inviteCode := "fullinvite"
	ownerUID := "owner-uid"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:  spaceId,
		Name:     "满员空间",
		Creator:  ownerUID,
		MaxUsers: 1,
		Status:   1,
	})
	assert.NoError(t, err)

	// 添加空间拥有者（占用唯一名额）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     ownerUID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    ownerUID,
		Status:     1,
	})
	assert.NoError(t, err)

	// 新用户尝试加入（应返回 SPACE_FULL）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"status":"SPACE_FULL"`)
	assert.Contains(t, body, "空间已满")
}

func TestJoinSpaceSuccessWithCapacity(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space（max_users=2，允许2人）
	spaceId := "test-space-cap"
	inviteCode := "capinvite"
	ownerUID := "owner-uid-2"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:  spaceId,
		Name:     "有空位的空间",
		Creator:  ownerUID,
		MaxUsers: 2,
		Status:   1,
	})
	assert.NoError(t, err)

	// 添加空间拥有者（占用1个名额）
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     ownerUID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    ownerUID,
		Status:     1,
	})
	assert.NoError(t, err)

	// 新用户加入（应成功）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"space_id":"test-space-cap"`)

	// 验证成员数
	count, err := f.db.countActiveMembers(spaceId)
	assert.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestJoinSpaceUnlimitedCapacity(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space（max_users=0，不限制）
	spaceId := "test-space-unlimited"
	inviteCode := "unlimitedinvite"
	ownerUID := "owner-uid-3"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:  spaceId,
		Name:     "不限人数空间",
		Creator:  ownerUID,
		MaxUsers: 0, // 不限制
		Status:   1,
	})
	assert.NoError(t, err)

	// 添加空间拥有者
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     ownerUID,
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    ownerUID,
		Status:     1,
	})
	assert.NoError(t, err)

	// 新用户加入（应成功，不受限制）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"space_id":"test-space-unlimited"`)
}

// === Preset Group Tests (PR #529) ===

func TestJoinSpaceWithPresetGroup(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试群组
	groupNo := "test-group-001"
	_, err = testCtx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
		Values(groupNo, "测试预置群", "admin", 1).Exec()
	assert.NoError(t, err)

	// 创建测试 Space（带预置群）
	spaceId := "test-space-preset"
	inviteCode := "preset123"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:        spaceId,
		Name:           "带预置群的空间",
		PresetGroupIds: strPtr(`["` + groupNo + `"]`),
		Creator:        "admin",
		Status:         1,
	})
	assert.NoError(t, err)

	// 添加管理员成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     "admin",
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    "admin",
		Status:     1,
	})
	assert.NoError(t, err)

	// 新用户加入 Space
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), spaceId)

	// 验证用户已加入预置群（使用 Eventually 等待异步操作完成）
	assert.Eventually(t, func() bool {
		var count int
		_, err := testCtx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, testutil.UID).Load(&count)
		return err == nil && count == 1
	}, time.Second, 10*time.Millisecond, "用户应该已自动加入预置群")
}

func TestJoinSpaceWithNoPresetGroup(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space（不带预置群）
	spaceId := "test-space-no-preset"
	inviteCode := "nopreset1"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:        spaceId,
		Name:           "无预置群的空间",
		PresetGroupIds: strPtr(""), // 没有预置群
		Creator:        "admin",
		Status:         1,
	})
	assert.NoError(t, err)

	// 添加管理员成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     "admin",
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    "admin",
		Status:     1,
	})
	assert.NoError(t, err)

	// 新用户加入 Space
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), spaceId)

	// 验证用户已加入 Space
	member, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, member)
}

func TestJoinSpacePresetGroupIdempotent(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试群组
	groupNo := "test-group-idem"
	_, err = testCtx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
		Values(groupNo, "幂等测试群", "admin", 1).Exec()
	assert.NoError(t, err)

	// 用户已在群中
	_, err = testCtx.DB().InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status").
		Values(groupNo, testutil.UID, 0, 0, 1).Exec()
	assert.NoError(t, err)

	// 创建测试 Space（带预置群）
	spaceId := "test-space-idem"
	inviteCode := "idem1234"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:       spaceId,
		Name:          "幂等测试空间",
		PresetGroupIds: strPtr(`["` + groupNo + `"]`),
		Creator:       "admin",
		Status:        1,
	})
	assert.NoError(t, err)

	// 添加管理员成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     "admin",
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    "admin",
		Status:     1,
	})
	assert.NoError(t, err)

	// 用户加入 Space（已在群中）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	// 加入 Space 应该成功（不应因为已在群中而失败）
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证群成员记录仍然只有一条（使用 Eventually 等待异步操作完成）
	assert.Eventually(t, func() bool {
		var count int
		_, err := testCtx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", groupNo, testutil.UID).Load(&count)
		return err == nil && count == 1
	}, time.Second, 10*time.Millisecond, "群成员记录应该只有一条（幂等）")
}

func TestJoinSpacePresetGroupDisbanded(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建已解散的群组（status=2 表示解散）
	groupNo := "test-group-disbanded"
	_, err = testCtx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
		Values(groupNo, "已解散的群", "admin", 2).Exec()
	assert.NoError(t, err)

	// 创建测试 Space（带已解散的预置群）
	spaceId := "test-space-disbanded"
	inviteCode := "disband1"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:       spaceId,
		Name:          "预置群已解散的空间",
		PresetGroupIds: strPtr(`["` + groupNo + `"]`),
		Creator:       "admin",
		Status:        1,
	})
	assert.NoError(t, err)

	// 添加管理员成员
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId,
		UID:     "admin",
		Role:    2,
		Status:  1,
	})
	assert.NoError(t, err)

	// 创建邀请码
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    "admin",
		Status:     1,
	})
	assert.NoError(t, err)

	// 用户加入 Space
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	assert.NoError(t, err)
	s.GetRoute().ServeHTTP(w, req)

	// 加入 Space 应该成功（预置群解散不影响主流程）
	assert.Equal(t, http.StatusOK, w.Code)

	// 验证用户没有加入已解散的群（使用 Eventually 确保异步操作已完成）
	// 注意：这里验证的是 count == 0，需要等待足够时间确保如果会加入，已经加入了
	time.Sleep(50 * time.Millisecond) // 给异步操作一点时间
	var count int
	_, err = testCtx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", groupNo, testutil.UID).Load(&count)
	assert.NoError(t, err)
	assert.Equal(t, 0, count, "用户不应该加入已解散的群")

	// 验证用户已加入 Space
	member, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, member)
}

// === Join Apply (Approval Flow) Tests ===

func TestJoinSpaceApprovalMode_CreatesPendingApply(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-approve"
	inviteCode := "appr1234"
	ownerUID := "owner-approve"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:  spaceId,
		Name:     "需审批空间",
		Creator:  ownerUID,
		JoinMode: 1,
		Status:   1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: ownerUID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: ownerUID, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{
			"invite_code": inviteCode,
		}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"status":"NEED_APPROVAL"`)
	assert.Contains(t, body, spaceId)

	// 验证用户没有成为成员
	mbr, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, mbr, "用户不应该直接成为成员")

	// 验证申请记录已创建
	apply, err := f.db.queryPendingApplyBySpaceAndUID(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, apply)
	assert.Equal(t, 0, apply.Status)
	assert.Equal(t, inviteCode, apply.InviteCode)

	// 验证邀请码使用次数没有增加
	invitation, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.Equal(t, 0, invitation.UsedCount, "审批模式不应消耗邀请码次数")
}

func TestJoinSpaceApprovalMode_DuplicateApply(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-dup-apply"
	inviteCode := "dup12345"
	ownerUID := "owner-dup"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "重复申请测试", Creator: ownerUID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: ownerUID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: ownerUID, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{"invite_code": inviteCode}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"NEED_APPROVAL"`)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{"invite_code": inviteCode}))))
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), `"status":"PENDING"`)
}

func TestJoinSpaceApprovalMode_AlreadyMember(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-already"
	inviteCode := "alrd1234"
	ownerUID := "owner-already"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "已是成员测试", Creator: ownerUID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: ownerUID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: ownerUID, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{"invite_code": inviteCode}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "已经是该空间成员")
}

func TestJoinApplies_ListPending(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-list-apply"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "申请列表测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "applicant-1", InviteCode: "inv1", Status: 0,
	})
	assert.NoError(t, err)
	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "applicant-2", InviteCode: "inv2", Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/join-applies", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"count":2`)
	assert.Contains(t, body, `"applicant-1"`)
	assert.Contains(t, body, `"applicant-2"`)
}

func TestJoinApplies_NoPermission(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-noperm"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "无权限测试", Creator: "other", JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/"+spaceId+"/join-applies", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "无权限")
}

func TestApproveJoinApply_Success(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-approve-ok"
	applicantUID := "applicant-approve"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "审批通过测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: "apprinv1", Creator: testutil.UID, Status: 1,
	}))

	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "apprinv1", Status: 0,
	})
	assert.NoError(t, err)

	apply, err := f.db.queryPendingApplyBySpaceAndUID(spaceId, applicantUID)
	assert.NoError(t, err)
	assert.NotNil(t, apply)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, apply.Id), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	mbr, err := f.db.queryMember(spaceId, applicantUID)
	assert.NoError(t, err)
	assert.NotNil(t, mbr, "审批通过后用户应成为成员")
	assert.Equal(t, 0, mbr.Role)

	updatedApply, err := f.db.queryJoinApplyByID(apply.Id)
	assert.NoError(t, err)
	assert.Equal(t, 1, updatedApply.Status)
	assert.Equal(t, testutil.UID, updatedApply.ReviewerUID)
}

func TestRejectJoinApply_Success(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-reject"
	applicantUID := "applicant-reject"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "拒绝测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 1, Status: 1,
	})
	assert.NoError(t, err)

	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "rejinv1", Status: 0,
	})
	assert.NoError(t, err)

	apply, err := f.db.queryPendingApplyBySpaceAndUID(spaceId, applicantUID)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/reject", spaceId, apply.Id), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	mbr, err := f.db.queryMember(spaceId, applicantUID)
	assert.NoError(t, err)
	assert.Nil(t, mbr, "被拒绝的用户不应成为成员")

	updatedApply, err := f.db.queryJoinApplyByID(apply.Id)
	assert.NoError(t, err)
	assert.Equal(t, 2, updatedApply.Status)
}

func TestApproveJoinApply_SpaceFull(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-approve-full"
	applicantUID := "applicant-full"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "满员审批测试", Creator: testutil.UID,
		JoinMode: 1, MaxUsers: 1, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: "fullinv1", Creator: testutil.UID, Status: 1,
	}))

	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "fullinv1", Status: 0,
	})
	assert.NoError(t, err)

	apply, err := f.db.queryPendingApplyBySpaceAndUID(spaceId, applicantUID)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, apply.Id), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "空间已满")
}

func TestJoinSpaceDirectMode_StillWorks(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-direct"
	inviteCode := "direct12"
	ownerUID := "owner-direct"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "直接加入空间", Creator: ownerUID, JoinMode: 0, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: ownerUID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: ownerUID, Status: 1,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{"invite_code": inviteCode}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"space_id"`)
	assert.NotContains(t, body, `"pending"`)

	mbr, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, mbr)
}

// === H5 Approve Flow Tests ===

func TestJoinApproveDetail_ValidAuthCode(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-h5"
	applicantUID := "applicant-h5"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "H5审批测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	_, err = f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "h5inv1",
	})
	assert.NoError(t, err)

	apply, err := f.db.queryPendingApplyBySpaceAndUID(spaceId, applicantUID)
	assert.NoError(t, err)

	// 写入 auth_code 到 Redis
	authCode := "test-auth-code-1"
	authData := util.ToJson(map[string]interface{}{
		"apply_id": apply.Id,
		"space_id": spaceId,
		"type":     "spaceJoinApprove",
	})
	err = testCtx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), authData, time.Minute*5)
	assert.NoError(t, err)

	// GET 审批详情
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/join-approve/detail?auth_code="+authCode, nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, applicantUID)
	assert.Contains(t, body, spaceId)
}

func TestJoinApproveDetail_InvalidAuthCode(t *testing.T) {
	s, _, _ := setup(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/join-approve/detail?auth_code=invalid-code", nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestJoinApproveSure_Approve(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-h5-approve"
	applicantUID := "applicant-h5-approve"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "H5审批通过", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: "h5inv2", Creator: testutil.UID, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "h5inv2",
	})
	assert.NoError(t, err)

	// 写入 auth_code
	authCode := "test-auth-approve"
	authData := util.ToJson(map[string]interface{}{
		"apply_id":     applyID,
		"space_id":     spaceId,
		"reviewer_uid": testutil.UID,
		"type":         "spaceJoinApprove",
	})
	err = testCtx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), authData, time.Minute*5)
	assert.NoError(t, err)

	// POST 审批通过
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join-approve/sure?auth_code="+authCode+"&action=approve", nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证用户已成为成员
	member, err := f.db.queryMember(spaceId, applicantUID)
	assert.NoError(t, err)
	assert.NotNil(t, member)

	// auth_code 保留不删除，审批后仍可查看详情
	val, _ := testCtx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	assert.NotEmpty(t, val, "auth_code 应保留到自然过期")
}

func TestJoinApproveSure_Reject(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-h5-reject"
	applicantUID := "applicant-h5-reject"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "H5审批拒绝", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "h5inv3",
	})
	assert.NoError(t, err)

	authCode := "test-auth-reject"
	authData := util.ToJson(map[string]interface{}{
		"apply_id":     applyID,
		"space_id":     spaceId,
		"reviewer_uid": testutil.UID,
		"type":         "spaceJoinApprove",
	})
	err = testCtx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), authData, time.Minute*5)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join-approve/sure?auth_code="+authCode+"&action=reject", nil)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// 验证用户没有成为成员
	member, err := f.db.queryMember(spaceId, applicantUID)
	assert.NoError(t, err)
	assert.Nil(t, member)

	// 验证申请状态为拒绝
	apply, err := f.db.queryJoinApplyByID(applyID)
	assert.NoError(t, err)
	assert.Equal(t, 2, apply.Status)
}

// Bug: rejectJoinApply 缺少 spaceId 校验，可跨空间拒绝
func TestRejectJoinApply_CrossSpaceBlocked(t *testing.T) {
	s, f, err := setup(t)

	// Space A: 有申请记录
	spaceA := "test-space-a"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceA, Name: "Space A", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)
	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceA, UID: "victim-uid", InviteCode: "inv-a",
	})
	assert.NoError(t, err)

	// Space B: testutil.UID 是管理员
	spaceB := "test-space-b"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceB, Name: "Space B", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceB, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	// Space B 的管理员尝试拒绝 Space A 的申请 → 应被拒绝
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/reject", spaceB, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "跨空间拒绝应被阻止")
	assert.Contains(t, w.Body.String(), "不属于当前空间")

	// 验证申请状态未被修改
	apply, err := f.db.queryJoinApplyByID(applyID)
	assert.NoError(t, err)
	assert.Equal(t, 0, apply.Status, "申请状态不应被修改")
}

// auth_code 不再删除，依靠 DB status 防止重放
func TestJoinApproveSure_ReplayBlockedByDBStatus(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-authcode-order"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "AuthCode顺序", Creator: testutil.UID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: "inv-ac", Creator: testutil.UID, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "applicant-authcode", InviteCode: "inv-ac",
	})
	assert.NoError(t, err)

	authCode := "test-auth-consume"
	authData := util.ToJson(map[string]interface{}{
		"apply_id":     applyID,
		"space_id":     spaceId,
		"reviewer_uid": testutil.UID,
		"type":         "spaceJoinApprove",
	})
	err = testCtx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), authData, time.Minute*5)
	assert.NoError(t, err)

	// 审批通过
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		"/v1/space/join-approve/sure?auth_code="+authCode+"&action=approve", nil)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// auth_code 应保留（不再删除）
	val, _ := testCtx.GetRedisConn().GetString(
		fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	assert.NotEmpty(t, val, "auth_code 应保留到自然过期")

	// 用同一个 auth_code 再次请求应被 DB status 拦截
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST",
		"/v1/space/join-approve/sure?auth_code="+authCode+"&action=approve", nil)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusBadRequest, w2.Code, "重放应被 DB status 拒绝")
	assert.Contains(t, w2.Body.String(), "已被处理")
}

// Fix: 审批后 detail 仍可查看，返回 reviewer 信息
func TestJoinApproveDetail_AfterApproval_ShowsReviewer(t *testing.T) {
	s, f, err := setup(t)

	spaceId := "test-space-detail-after"
	applicantUID := "applicant-detail-after"
	reviewerUID := testutil.UID
	reviewerName := "审批管理员"

	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "详情回看", Creator: reviewerUID, JoinMode: 1, Status: 1,
	})
	assert.NoError(t, err)
	err = f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: reviewerUID, Role: 2, Status: 1,
	})
	assert.NoError(t, err)

	// 插入 reviewer 用户记录
	_, err = testCtx.DB().InsertBySql(
		"INSERT IGNORE INTO `user` (uid, name) VALUES (?, ?)", reviewerUID, reviewerName,
	).Exec()
	assert.NoError(t, err)

	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: "inv-detail", Creator: reviewerUID, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: "inv-detail",
	})
	assert.NoError(t, err)

	authCode := "test-auth-detail-after"
	authData := util.ToJson(map[string]interface{}{
		"apply_id":     applyID,
		"space_id":     spaceId,
		"reviewer_uid": reviewerUID,
		"type":         "spaceJoinApprove",
	})
	err = testCtx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), authData, time.Minute*5)
	assert.NoError(t, err)

	// 先审批通过
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		"/v1/space/join-approve/sure?auth_code="+authCode+"&action=approve", nil)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 用同一个 auth_code 查看详情 — 应返回已通过状态和审批人
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET",
		"/v1/space/join-approve/detail?auth_code="+authCode, nil)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)

	var resp map[string]interface{}
	err = json.Unmarshal(w2.Body.Bytes(), &resp)
	assert.NoError(t, err)

	statusVal, _ := resp["status"].(float64)
	assert.Equal(t, float64(1), statusVal, "状态应为已通过")
	assert.Equal(t, reviewerUID, resp["reviewer_uid"], "应返回审批人UID")
	assert.Equal(t, reviewerName, resp["reviewer_name"], "应返回审批人名称")
}

// ==================== P2: 全局开关测试 ====================

// TestIsUserCreateDisabled_Parsing 覆盖 env 解析分支
func TestIsUserCreateDisabled_Parsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"random", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"ON", true},
		{" true ", true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv(envDisableUserCreateSpace, tc.val)
			assert.Equal(t, tc.want, IsUserCreateDisabled())
		})
	}
}

func TestCreateSpace_AllowedByDefault(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	// 确保开关关闭（默认）
	t.Setenv(envDisableUserCreateSpace, "")

	body := util.ToJson(map[string]interface{}{
		"name":      "p2-normal",
		"join_mode": 0,
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/create", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"name":"p2-normal"`)

	var resp map[string]interface{}
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	spaceID, _ := resp["space_id"].(string)
	assert.NotEmpty(t, spaceID, "space_id 应返回")

	// owner 成员已写入
	mem, err := testSpaceDB.queryMember(spaceID, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, mem)
	assert.Equal(t, 2, mem.Role)
}

func TestCreateSpace_DisabledByEnv(t *testing.T) {
	s, _, err := setup(t)
	assert.NoError(t, err)

	t.Setenv(envDisableUserCreateSpace, "true")

	body := util.ToJson(map[string]interface{}{
		"name":      "p2-blocked",
		"join_mode": 0,
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/create", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "应返回 403")
	assert.Contains(t, w.Body.String(), "已关闭")

	// 不应有新空间入库：按 name 反查
	var count int
	_, err = testCtx.DB().SelectBySql("SELECT COUNT(*) FROM space WHERE name=?", "p2-blocked").Load(&count)
	assert.NoError(t, err)
	assert.Equal(t, 0, count, "开关开启时不应写入任何 space 记录")
}

// === Issue #1140 follow-up: ErrAlreadyMember 路径不应消耗邀请码名额 ===

// TestJoinSpaceDirect_AlreadyMemberRefundsInvite 直接加入模式下，若用户已是成员，
// 不应消耗邀请码名额（executeJoinSpace 返回 ErrAlreadyMember 时归还已 increment 的名额）。
func TestJoinSpaceDirect_AlreadyMemberRefundsInvite(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-direct-already"
	inviteCode := "direct-al-1"
	ownerUID := "owner-direct-al"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "重复加入", Creator: ownerUID, JoinMode: 0, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: ownerUID, Role: 2, Status: 1,
	}))
	// testutil.UID 已经是成员
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 0, Status: 1,
	}))
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: ownerUID,
		MaxUses: 5, UsedCount: 0, Status: 1,
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/space/join",
		bytes.NewReader([]byte(util.ToJson(map[string]string{"invite_code": inviteCode}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "你已经是该空间成员")

	inv, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.Equal(t, 0, inv.UsedCount, "重复加入失败不应消耗邀请码名额")
}

// === Issue #1140: approve 路径消耗邀请码名额 ===

// TestApproveJoinApply_IncrementsInviteUsedCount 审批通过后 used_count 应递增。
func TestApproveJoinApply_IncrementsInviteUsedCount(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-apprv-inc"
	inviteCode := "apprv-inc-1"
	applicantUID := "u-apprv-inc"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "消耗测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	}))
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: testutil.UID,
		MaxUses: 2, UsedCount: 0, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: applicantUID, InviteCode: inviteCode, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	inv, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.Equal(t, 1, inv.UsedCount, "审批通过应递增 used_count")
}

// TestApproveJoinApply_InviteExhaustedBlocksApproval max_uses 用尽后再审批应被拒绝且 apply 回滚。
func TestApproveJoinApply_InviteExhaustedBlocksApproval(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-apprv-exh"
	inviteCode := "apprv-exh-1"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "用尽测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	}))
	// max_uses=1 已用满
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: testutil.UID,
		MaxUses: 1, UsedCount: 1, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "u-apprv-exh", InviteCode: inviteCode, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "已用尽")

	// 申请状态应回滚为 0，保留 owner 后续处理余地
	updated, err := f.db.queryJoinApplyByID(applyID)
	assert.NoError(t, err)
	assert.Equal(t, 0, updated.Status, "审批失败应回滚申请状态")

	// 用户未成为成员
	mbr, err := f.db.queryMember(spaceId, "u-apprv-exh")
	assert.NoError(t, err)
	assert.Nil(t, mbr)
}

// TestApproveJoinApply_InviteDisabledBlocksApproval 邀请码被禁用后审批应被拒。
func TestApproveJoinApply_InviteDisabledBlocksApproval(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-apprv-dis"
	inviteCode := "apprv-dis-1"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "禁用测试", Creator: testutil.UID, JoinMode: 1, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	}))
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: testutil.UID,
		MaxUses: 10, UsedCount: 0, Status: 0, // disabled
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "u-apprv-dis", InviteCode: inviteCode, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "已失效")

	updated, err := f.db.queryJoinApplyByID(applyID)
	assert.NoError(t, err)
	assert.Equal(t, 0, updated.Status)
}

// TestApproveJoinApply_SpaceFullRefundsInvite 空间满员导致加入失败时，已消耗的名额应回滚。
func TestApproveJoinApply_SpaceFullRefundsInvite(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-apprv-full-refund"
	inviteCode := "apprv-full-1"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "满员退款测试", Creator: testutil.UID,
		JoinMode: 1, MaxUsers: 1, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	}))
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: testutil.UID,
		MaxUses: 5, UsedCount: 0, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "u-apprv-full", InviteCode: inviteCode, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/approve", spaceId, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "空间已满")

	// 邀请码名额应被回滚
	inv, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.Equal(t, 0, inv.UsedCount, "加入失败时应回滚 used_count")
}

// TestRejectJoinApply_DoesNotConsumeInvite 拒绝不应消耗邀请码名额。
func TestRejectJoinApply_DoesNotConsumeInvite(t *testing.T) {
	s, f, err := setup(t)
	assert.NoError(t, err)

	spaceId := "sp-rej-noconsume"
	inviteCode := "rej-noc-1"

	assert.NoError(t, f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "拒绝不消耗", Creator: testutil.UID, JoinMode: 1, Status: 1,
	}))
	assert.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceId, UID: testutil.UID, Role: 2, Status: 1,
	}))
	assert.NoError(t, f.db.insertInvitation(&InvitationModel{
		SpaceId: spaceId, InviteCode: inviteCode, Creator: testutil.UID,
		MaxUses: 3, UsedCount: 0, Status: 1,
	}))

	applyID, err := f.db.upsertJoinApply(&spaceJoinApplyModel{
		SpaceId: spaceId, UID: "u-rej-noc", InviteCode: inviteCode, Status: 0,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/v1/space/%s/join-applies/%d/reject", spaceId, applyID), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	inv, err := f.db.queryInvitationByCode(inviteCode)
	assert.NoError(t, err)
	assert.Equal(t, 0, inv.UsedCount, "拒绝不消耗名额")

	// reviewer 已记录
	updated, err := f.db.queryJoinApplyByID(applyID)
	assert.NoError(t, err)
	assert.Equal(t, 2, updated.Status)
	assert.Equal(t, testutil.UID, updated.ReviewerUID)
}
