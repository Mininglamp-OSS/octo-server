package space

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func strPtr(s string) *string {
	return &s
}

func TestGetInvitePreview(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	_, err = ctx.DB().InsertInto("user").Columns("uid", "name", "avatar").
		Values(botUID, "AI 助手", "https://example.com/bot.png").Exec()
	assert.NoError(t, err)

	// 在 robot 表中注册 Bot
	_, err = ctx.DB().InsertInto("robot").Columns("robot_id", "token", "status").
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
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

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
	f.Route(s.GetRoute())

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试群组
	groupNo := "test-group-001"
	_, err = ctx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
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
		_, err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, testutil.UID).Load(&count)
		return err == nil && count == 1
	}, time.Second, 10*time.Millisecond, "用户应该已自动加入预置群")
}

func TestJoinSpaceWithNoPresetGroup(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试 Space（不带预置群）
	spaceId := "test-space-no-preset"
	inviteCode := "nopreset1"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:        spaceId,
		Name:           "无预置群的空间",
		PresetGroupIds: nil, // 没有预置群
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
	f.Route(s.GetRoute())

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建测试群组
	groupNo := "test-group-idem"
	_, err = ctx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
		Values(groupNo, "幂等测试群", "admin", 1).Exec()
	assert.NoError(t, err)

	// 用户已在群中
	_, err = ctx.DB().InsertInto("group_member").
		Columns("group_no", "uid", "role", "is_deleted", "status").
		Values(groupNo, testutil.UID, 0, 0, 1).Exec()
	assert.NoError(t, err)

	// 创建测试 Space（带预置群）
	spaceId := "test-space-idem"
	inviteCode := "idem1234"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:        spaceId,
		Name:           "幂等测试空间",
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
		_, err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", groupNo, testutil.UID).Load(&count)
		return err == nil && count == 1
	}, time.Second, 10*time.Millisecond, "群成员记录应该只有一条（幂等）")
}

func TestJoinSpacePresetGroupDisbanded(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())

	// 清空旧数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 创建已解散的群组（status=2 表示解散）
	groupNo := "test-group-disbanded"
	_, err = ctx.DB().InsertInto("group").Columns("group_no", "name", "creator", "status").
		Values(groupNo, "已解散的群", "admin", 2).Exec()
	assert.NoError(t, err)

	// 创建测试 Space（带已解散的预置群）
	spaceId := "test-space-disbanded"
	inviteCode := "disband1"
	err = f.db.insertSpaceNoTx(&SpaceModel{
		SpaceId:        spaceId,
		Name:           "预置群已解散的空间",
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
	_, err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=?", groupNo, testutil.UID).Load(&count)
	assert.NoError(t, err)
	assert.Equal(t, 0, count, "用户不应该加入已解散的群")

	// 验证用户已加入 Space
	member, err := f.db.queryMember(spaceId, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, member)
}
