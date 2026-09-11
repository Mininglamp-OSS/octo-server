package bot_api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PR#355 review 守卫：bot_admin 与人类管理员同权，POST
// /v1/bot/groups/:group_no/members/remove 不得移除群主/管理员。#354 把
// service 层 RemoveGroupMembers 的 manager 豁免下沉移除后，目标角色校验由
// 调用方负责——Web API memberRemove 有 creator/manager 守卫，这里验证 Bot
// API 路径的对等守卫：
//   - 目标含 manager → 403 cannot_remove_privileged，且整个请求拒绝（混合
//     列表里的普通成员也不能被"顺带"移除）；
//   - 目标含 creator → 403 cannot_remove_privileged；
//   - 目标全为普通成员 → 正常移除（守卫不误伤）。

const (
	rmGuardGroupNo  = "g_rm_guard_1"
	rmGuardBotID    = "bot_rm_guard"
	rmGuardBotToken = "bf_rm_guard_token"
	rmGuardCreator  = "u_rm_creator"
	rmGuardManager  = "u_rm_manager"
	rmGuardCommon   = "u_rm_common"
)

func setupRemoveGuardEnv(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		rmGuardBotID, rmGuardCreator, rmGuardBotToken).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version) VALUES (?, ?, 1, 1)",
		rmGuardGroupNo, "remove guard group").Exec()
	require.NoError(t, err)

	// creator(role=1) / manager(role=2) / 普通成员(role=0) / bot 管理员(bot_admin=1)
	for _, m := range []struct {
		uid              string
		role, robot, adm int
	}{
		{rmGuardCreator, 1, 0, 0},
		{rmGuardManager, 2, 0, 0},
		{rmGuardCommon, 0, 0, 0},
		{rmGuardBotID, 0, 1, 1},
	} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO group_member (group_no, uid, role, robot, bot_admin, vercode, is_deleted, status, version) VALUES (?, ?, ?, ?, ?, ?, 0, 1, 1)",
			rmGuardGroupNo, m.uid, m.role, m.robot, m.adm, util.GenerUUID()).Exec()
		require.NoError(t, err)
	}

	return s.GetRoute(), ctx
}

func rmGuardIsActiveMember(t *testing.T, handler http.Handler, uid string) bool {
	t.Helper()
	w := doBot(handler, botReq(t, "GET", "/v1/bot/groups/"+rmGuardGroupNo+"/members", rmGuardBotToken, nil))
	require.Equalf(t, http.StatusOK, w.Code, "list members body: %s", w.Body.String())
	// GET /members 返回成员数组；uid 可能出现在响应的其他字段里，所以逐项
	// 比对而不是对响应体做字符串包含判断。
	var members []struct {
		UID string `json:"uid"`
	}
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &members), "list members body: %s", w.Body.String())
	for _, m := range members {
		if m.UID == uid {
			return true
		}
	}
	return false
}

func TestBotGroupMemberRemove_ManagerTargetForbidden(t *testing.T) {
	handler, _ := setupRemoveGuardEnv(t)

	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardManager}}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "cannot be removed through the bot API")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardManager), "manager must remain a member")
}

func TestBotGroupMemberRemove_CreatorTargetForbidden(t *testing.T) {
	handler, _ := setupRemoveGuardEnv(t)

	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCreator}}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "cannot be removed through the bot API")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardCreator), "creator must remain a member")
}

func TestBotGroupMemberRemove_MixedListRejectedAtomically(t *testing.T) {
	handler, _ := setupRemoveGuardEnv(t)

	// 混合列表（普通成员 + 管理员）→ 整个请求 403，普通成员也不能被顺带移除。
	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCommon, rmGuardManager}}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "cannot be removed through the bot API")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardManager), "manager must remain a member")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardCommon), "common member must not be removed when the request is rejected")
}

func TestBotGroupMemberRemove_ManagerTargetCaseVariantForbidden(t *testing.T) {
	handler, _ := setupRemoveGuardEnv(t)

	// MySQL utf8mb4_*_ci collation 下 uid 匹配大小写不敏感：大小写变体在
	// service 层仍会命中真实 manager 行，所以守卫必须按 DB 解析行的角色
	// 拦截，而不是在 Go 里做大小写敏感的字符串比对（codex review P1 回归）。
	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{strings.ToUpper(rmGuardManager)}}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "cannot be removed through the bot API")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardManager), "manager must remain a member")
}

func TestBotGroupMemberRemove_CommonTargetStillWorks(t *testing.T) {
	handler, _ := setupRemoveGuardEnv(t)

	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCommon}}))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeBody(t, w)
	assert.Equal(t, true, resp["ok"])
	assert.Equal(t, float64(1), resp["removed"])
	assert.False(t, rmGuardIsActiveMember(t, handler, rmGuardCommon), "common member should be removed")
}

func TestBotGroupMemberRemove_AIContainerForbidden(t *testing.T) {
	handler, ctx := setupRemoveGuardEnv(t)
	_, err := ctx.DB().Update("group").Set("purpose", "ai_session_container").
		Where("group_no=?", rmGuardGroupNo).Exec()
	require.NoError(t, err)

	w := doBot(handler, botReq(t, "POST", "/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCommon}}))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "AI session container")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardCommon))
}

func TestBotGroupMemberRemove_DedicatedAllMemberGroupForbidden(t *testing.T) {
	handler, ctx := setupRemoveGuardEnv(t)
	const projectID = "p_rm_guard_dedicated"

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project "+
			"(project_id, space_id, name, creator, status, all_member_group_no, created_at, updated_at) "+
			"VALUES (?, '', ?, ?, 1, ?, NOW(3), NOW(3))",
		projectID, projectID, rmGuardCreator, rmGuardGroupNo,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("group").Set("project_id", projectID).
		Where("group_no=?", rmGuardGroupNo).Exec()
	require.NoError(t, err)

	w := doBot(handler, botReq(t, "POST",
		"/v1/bot/groups/"+rmGuardGroupNo+"/members/remove", rmGuardBotToken,
		map[string]interface{}{"members": []string{rmGuardCommon}}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "all-member group")
	assert.True(t, rmGuardIsActiveMember(t, handler, rmGuardCommon),
		"Bot API removal of a dedicated member must be rejected without mutation")
}
