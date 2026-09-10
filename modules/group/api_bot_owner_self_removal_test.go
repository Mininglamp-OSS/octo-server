package group

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 自助移除 bot 的鉴权矩阵（task bot-owner-self-removal / octo-web#1511）。
//
// 与 api_bot_ownership_test.go 的区别：那组测的是**入群侧** ownership
// （checkBotOwnership），本组测的是**移除侧**——调用方 testutil.UID 在这里恒为
// MemberRoleCommon（普通成员），群主另有其人，所以走的正是新开的自助分支。
//
// 最关键的一条是 TestBotOwnerSelfRemoval_RejectsHumanTarget：它钉住「自助路径
// 不得移除人类成员」。若有人日后把判据换成 checkBotOwnership（对非 bot 返回 nil），
// 这条会立刻红——那是一个提权漏洞，不是风格问题。

const selfRemovalGroupNo = "g_bot_self_rm"

// setupBotSelfRemovalGroup 建一个群：testutil.UID 是**普通成员**，群主是 owner_other。
// 返回的 *Group 用于直接做 DB 断言/播种。
func setupBotSelfRemovalGroup(t *testing.T) (*Group, http.Handler, *config.Context) {
	t.Helper()
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	// 移除路由自本任务起挂了 SharedUIDRateLimiter，其桶存活在 Redis 里且不被
	// CleanAllTables 清理；不重置会让同包其它用例的配额溢出到这里。
	resetGroupUIDRateLimit(t, ctx)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "bot-owner", ShortNo: "uc_self_rm"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "owner_other", Name: "group-owner", ShortNo: "uo_self_rm"}))

	require.NoError(t, f.db.Insert(&Model{
		GroupNo: selfRemovalGroupNo, Name: "bot self removal", Creator: "owner_other",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedSelfRemovalMember(t, f, "owner_other", MemberRoleCreator, 0)
	seedSelfRemovalMember(t, f, testutil.UID, MemberRoleCommon, 0)

	wireI18nRendererForGroupTest(s)
	return f, s.GetRoute(), ctx
}

func seedSelfRemovalMember(t *testing.T, f *Group, uid string, role, robot int) {
	t.Helper()
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: selfRemovalGroupNo, UID: uid, Role: role, Robot: robot,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
}

// seedSelfRemovalBot 建 bot 用户 + robot 行 + 群成员行（group_member.robot=1）。
// creatorUID 为空则不建 robot 行 —— 孤儿 bot，任何人都不拥有它。
func seedSelfRemovalBot(t *testing.T, f *Group, uid, name, creatorUID string, robotStatus int) {
	t.Helper()
	require.NoError(t, f.userDB.Insert(&user.Model{UID: uid, Name: name, ShortNo: uid, Robot: 1}))
	if creatorUID != "" {
		_, err := f.ctx.DB().InsertBySql(
			"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, ?, ?)",
			uid, robotStatus, creatorUID,
		).Exec()
		require.NoError(t, err)
	}
	seedSelfRemovalMember(t, f, uid, MemberRoleCommon, 1)
}

func deleteMembersReq(t *testing.T, handler http.Handler, groupNo string, members []string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"members": members})
	req, err := http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/members", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	return w
}

func postMembersDeleteReq(t *testing.T, handler http.Handler, groupNo string, members []string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"members": members})
	req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/members_delete", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	return w
}

// 验收 1：普通成员移除自己名下的 bot → 200，且成员行真的没了。
func TestBotOwnerSelfRemoval_OwnBotSucceeds(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_mine", "my-bot", testutil.UID, 1)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_mine"})
	assert.Equal(t, http.StatusOK, w.Code, "普通成员应能移除自己名下的 bot: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_mine", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.False(t, exist, "bot 应已不在群内")
}

// 验收 2：他人名下的 bot → 拒绝，且成员关系不变。
func TestBotOwnerSelfRemoval_RejectsOthersBot(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_theirs", "their-bot", "owner_other", 1)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_theirs"})
	assert.NotEqual(t, http.StatusOK, w.Code, "不得移除他人名下的 bot")
	assert.Contains(t, w.Body.String(), "member_cannot_remove",
		"应回「无权移除」而不是别的错误（例如查询失败的 5xx），实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_theirs", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "他人的 bot 应仍在群内")
}

// 验收 3（核心回归防线）：普通成员不得借自助路径移除**人类**成员。
//
// 这一条专防「把判据换成 checkBotOwnership」——后者的 SQL 是 WHERE u.robot = 1，
// 人类 UID 查不出行、循环不拒绝，等于放开踢人权限。
func TestBotOwnerSelfRemoval_RejectsHumanTarget(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "human_peer", Name: "peer", ShortNo: "hp_self_rm"}))
	seedSelfRemovalMember(t, f, "human_peer", MemberRoleCommon, 0)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"human_peer"})
	assert.NotEqual(t, http.StatusOK, w.Code, "普通成员不得移除人类成员（提权防线）")
	assert.Contains(t, w.Body.String(), "member_cannot_remove",
		"应回「无权移除」，实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("human_peer", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "人类成员必须仍在群内")
}

// 验收 4：混合批次整批拒绝，不得部分执行。
func TestBotOwnerSelfRemoval_RejectsMixedBatch(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_mine_mix", "my-bot", testutil.UID, 1)
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "human_mix", Name: "peer", ShortNo: "hm_self_rm"}))
	seedSelfRemovalMember(t, f, "human_mix", MemberRoleCommon, 0)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_mine_mix", "human_mix"})
	assert.NotEqual(t, http.StatusOK, w.Code, "混合批次必须整批拒绝")
	assert.Contains(t, w.Body.String(), "member_cannot_remove",
		"应回「无权移除」，实际: %s", w.Body.String())

	// 关键：连自己那只 bot 也不能被顺带移除，否则就是部分执行。
	botExist, err := f.db.ExistMember("bot_mine_mix", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, botExist, "整批拒绝时自己的 bot 也不应被移除（不得部分执行）")
	humanExist, err := f.db.ExistMember("human_mix", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, humanExist, "人类成员不应被移除")
}

// 验收 5：孤儿 bot（无 robot 行）与禁用 bot（status!=1）均 fail-closed。
func TestBotOwnerSelfRemoval_RejectsOrphanAndDisabledBot(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_orphan_rm", "orphan", "", 0)
	seedSelfRemovalBot(t, f, "bot_disabled_rm", "disabled", testutil.UID, 0)

	for _, uid := range []string{"bot_orphan_rm", "bot_disabled_rm"} {
		w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{uid})
		assert.NotEqual(t, http.StatusOK, w.Code, "%s 应 fail-closed 拒绝", uid)
		assert.Contains(t, w.Body.String(), "member_cannot_remove",
			"%s 应回「无权移除」，实际: %s", uid, w.Body.String())
		exist, err := f.db.ExistMember(uid, selfRemovalGroupNo)
		require.NoError(t, err)
		assert.True(t, exist, "%s 应仍在群内", uid)
	}
}

// 验收 8：两条路由（DELETE /members 与别名 POST /members_delete）行为一致。
func TestBotOwnerSelfRemoval_BothRoutesBehaveSame(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_alias", "alias-bot", testutil.UID, 1)

	w := postMembersDeleteReq(t, handler, selfRemovalGroupNo, []string{"bot_alias"})
	assert.Equal(t, http.StatusOK, w.Code, "别名路由应与 DELETE 行为一致: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_alias", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.False(t, exist, "bot 应已被别名路由移除")
}

// 验收 11：跨群作用域——我在 A 群拥有的 bot，不能通过对 B 群发请求而被移除。
// 白名单查询按 groupNo 作用域，这条是防止后续重构把 groupNo 从判据里漏掉的钉子。
func TestBotOwnerSelfRemoval_ScopedToRequestedGroup(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)

	// 另建 B 群：testutil.UID 也是普通成员，但他的 bot 只在 A 群里。
	const otherGroupNo = "g_bot_self_rm_b"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: otherGroupNo, Name: "other group", Creator: "owner_other",
		Status: GroupStatusNormal, Version: 1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: otherGroupNo, UID: testutil.UID, Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	seedSelfRemovalBot(t, f, "bot_in_a", "bot-in-a", testutil.UID, 1)

	// 对 B 群请求移除只存在于 A 群的 bot。
	w := deleteMembersReq(t, handler, otherGroupNo, []string{"bot_in_a"})
	assert.NotEqual(t, http.StatusOK, w.Code, "不得跨群移除")
	assert.Contains(t, w.Body.String(), "not_in_group",
		"目标不在本群，应回「不在群内」，实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_in_a", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "A 群里的 bot 不应被 B 群的请求移除")
}

// 验收 12：自助路径不得静默成功——目标是 Creator 角色的 bot 时显式拒绝，
// 而不是让 service 静默跳过后回 200。
func TestBotOwnerSelfRemoval_RejectsCreatorRoleBot(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)

	// 一个归 testutil.UID 所有、但在群里是 Creator 角色的 bot（构造出的边界态）。
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "bot_creator_role", Name: "creator-bot", ShortNo: "bcr", Robot: 1}))
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		"bot_creator_role", testutil.UID,
	).Exec()
	require.NoError(t, err)
	seedSelfRemovalMember(t, f, "bot_creator_role", MemberRoleCreator, 1)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_creator_role"})
	assert.NotEqual(t, http.StatusOK, w.Code, "Creator 角色目标必须显式拒绝，不能回 200 却没动")
	assert.Contains(t, w.Body.String(), "cannot_remove_owner",
		"应回「不能移除群主」，实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_creator_role", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "Creator 角色的 bot 应仍在群内")
}

// 验收 13：对已不在群内的 bot 重复移除 → 业务错误（不在群内），不得 5xx。
func TestBotOwnerSelfRemoval_RepeatRemovalIsNotServerError(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_twice", "twice-bot", testutil.UID, 1)

	first := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_twice"})
	require.Equal(t, http.StatusOK, first.Code, "首次移除应成功: %s", first.Body.String())

	second := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_twice"})
	assert.Less(t, second.Code, http.StatusInternalServerError,
		"重复移除应是业务错误而非 5xx，实际: %d %s", second.Code, second.Body.String())
	// 必须是「不在群内」而不是「无权移除」：白名单按 is_deleted=0 过滤，已移除的
	// bot 天然掉出白名单，若直接回权限错误，用户会看到「你没有权限移除自己的 bot」。
	assert.Contains(t, second.Body.String(), "not_in_group",
		"重复移除应回「成员不在群内」，实际: %s", second.Body.String())
}

// 验收 7：自助路径不发「你被 X 移除群聊」，改发 owner 视角的 Tip。
//
// 两条文案刻意可区分：旧的是「你被{0}移除群聊」，新的是「X 将机器人 Y 移出了群聊」
// ——「移除群聊」与「移出了群聊」不互相包含，所以下面的片段匹配是准确的。
//
// 这条**不走 HTTP 路由**，而是直接驱动 service —— 与 space_member_removal_test.go
// 里的同类 Tip 断言同一写法。原因是 register.GetModules（octo-lib
// pkg/register/register.go）用进程级 sync.Once 构造模块实例：一个进程里 handler
// 永远持有**第一个** NewTestServer 的 ctx，而 newGroupIMStub 改的是本测试自己那个
// ctx 的 config，于是经路由发出的消息不会落进本测试的桩（只有进程里第一个测试碰巧
// 生效）。这是测试脚手架的限制，不是产品行为——线上只有一个长期存在的 ctx。
//
// 「handler 确实置位了 BotOwnerSelfRemoval」由上面那批 HTTP 用例反证：普通成员能
// 把自己的 bot 删掉、却删不动人类/他人的 bot，只有走自助分支才可能是这个结果。
func TestBotOwnerSelfRemoval_SendsOwnerTipNotKickNotice(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	stub := newGroupIMStub(t, ctx)
	wireI18nRendererForGroupTest(s)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "bot-owner", ShortNo: "uc_tip"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "owner_other", Name: "group-owner", ShortNo: "uo_tip"}))
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: selfRemovalGroupNo, Name: "tip group", Creator: "owner_other",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedSelfRemovalMember(t, f, "owner_other", MemberRoleCreator, 0)
	seedSelfRemovalMember(t, f, testutil.UID, MemberRoleCommon, 0)
	seedSelfRemovalBot(t, f, "bot_tip", "tip-bot", testutil.UID, 1)

	_, err := f.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:             selfRemovalGroupNo,
		Members:             []string{"bot_tip"},
		OperatorUID:         testutil.UID,
		OperatorName:        "bot-owner",
		BotOwnerSelfRemoval: true,
	})
	require.NoError(t, err)

	exist, err := f.db.ExistMember("bot_tip", selfRemovalGroupNo)
	require.NoError(t, err)
	require.False(t, exist, "前置：bot 应已被移除")

	payloads := stub.sentPayloads()
	assert.False(t, payloadsContain(payloads, "移除群聊"),
		"自助移除 bot 不应发出「你被 X 移除群聊」——那是被移除者视角的措辞")
	assert.True(t, payloadsContain(payloads, "将机器人"),
		"应发出 owner 视角的 Tip，让群成员知道 bot 为何消失")
}

// 自助移除必须把 bot 从群频道的 IM 订阅里摘掉。
//
// 这是本功能最要紧的后置条件：删掉 group_member 行只是「名单上没有它了」，
// 若 IM 订阅还在，bot 依然会收到群消息 —— 那等于没移除。
// 既有的 TestGroupCascadeUnsubscribesFromIM 只覆盖了「人走 bot 跟着走」的级联路径，
// 自助路径此前无人断言。
//
// 与 Tip 断言同理走 service 层：register.GetModules 的进程级 sync.Once 让 HTTP
// 路由上的 IM 桩只对进程内第一个测试生效（见本文件 SendsOwnerTipNotKickNotice 的注释）。
func TestBotOwnerSelfRemoval_UnsubscribesBotFromGroupChannel(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	stub := newGroupIMStub(t, ctx)
	wireI18nRendererForGroupTest(s)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "bot-owner", ShortNo: "uc_unsub"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "owner_other", Name: "group-owner", ShortNo: "uo_unsub"}))
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: selfRemovalGroupNo, Name: "unsub group", Creator: "owner_other",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedSelfRemovalMember(t, f, "owner_other", MemberRoleCreator, 0)
	seedSelfRemovalMember(t, f, testutil.UID, MemberRoleCommon, 0)
	seedSelfRemovalBot(t, f, "bot_unsub", "unsub-bot", testutil.UID, 1)

	_, err := f.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:             selfRemovalGroupNo,
		Members:             []string{"bot_unsub"},
		OperatorUID:         testutil.UID,
		OperatorName:        "bot-owner",
		BotOwnerSelfRemoval: true,
	})
	require.NoError(t, err)

	assert.Contains(t, stub.unsubscribed(selfRemovalGroupNo), "bot_unsub",
		"bot 必须从群频道的 IM 订阅里摘掉，否则它还会继续收到群消息")
	// 操作者自己（以及群主）不能被顺带退订。
	assert.NotContains(t, stub.unsubscribed(selfRemovalGroupNo), testutil.UID,
		"发起人不应被退订")
	assert.NotContains(t, stub.unsubscribed(selfRemovalGroupNo), "owner_other",
		"其他成员不应被退订")
}

// bot_admin 不得跨越「撤走再拉回」存活。
//
// DeleteMemberTx 是软删（只置 is_deleted=1），整行连同 bot_admin 都留在表里；
// recoverMemberTx 复活时只重置 remark/role/version/is_deleted/invite_uid/
// is_external/source_space_id/created_at —— **不含 bot_admin**。
//
// 于是：群主给某个 bot 授过 bot_admin → 该 bot 的所有者把它撤走 → 再拉回来，
// 它就悄悄又是 bot 管理员了，全程不需要群主参与。
// 这个缺陷本身早于本功能，但此前普通成员根本撤不走 bot，这个来回做不出来；
// 自助移除让它变得可达，所以由本任务负责堵上。
func TestBotOwnerSelfRemoval_BotAdminDoesNotSurviveReAdd(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	// memberAdd 会读 app_config，缺行会 panic。
	_, _ = ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)",
	).Exec()

	seedSelfRemovalBot(t, f, "bot_readd", "readd-bot", testutil.UID, 1)
	// 群主给它授了 bot_admin。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET bot_admin=1 WHERE group_no=? AND uid=?",
		selfRemovalGroupNo, "bot_readd",
	).Exec()
	require.NoError(t, err)

	// 所有者自助撤走。
	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_readd"})
	require.Equal(t, http.StatusOK, w.Code, "移除应成功: %s", w.Body.String())

	// 再拉回来（memberAdd 的 ownership 校验允许所有者拉自己的 bot）。
	addW := httptest.NewRecorder()
	addBody := util.ToJson(map[string]interface{}{"members": []string{"bot_readd"}})
	addReq, err := http.NewRequest("POST",
		"/v1/groups/"+selfRemovalGroupNo+"/members", bytes.NewReader([]byte(addBody)))
	require.NoError(t, err)
	addReq.Header.Set("token", testutil.Token)
	handler.ServeHTTP(addW, addReq)
	require.Equal(t, http.StatusOK, addW.Code, "重新拉回应成功: %s", addW.Body.String())

	detail, err := f.db.queryMemberWithGroupNoAndUID(selfRemovalGroupNo, "bot_readd")
	require.NoError(t, err)
	require.NotNil(t, detail, "bot 应已重新在群内")
	assert.Equal(t, 0, detail.BotAdmin,
		"bot_admin 不得跨越『撤走再拉回』存活 —— 否则所有者可以绕过群主重新拿到管理员位")
}

// 对照组：同一条 service 路径在**不**置位 BotOwnerSelfRemoval 时，
// 仍然发原来的「被移除」文案——保证新分支没有把既有行为一起改掉。
func TestBotOwnerSelfRemoval_NormalRemovalStillSendsKickNotice(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	stub := newGroupIMStub(t, ctx)
	wireI18nRendererForGroupTest(s)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: "owner_other", Name: "group-owner", ShortNo: "uo_kick"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "victim", Name: "victim", ShortNo: "uv_kick"}))
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: selfRemovalGroupNo, Name: "kick group", Creator: "owner_other",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedSelfRemovalMember(t, f, "owner_other", MemberRoleCreator, 0)
	seedSelfRemovalMember(t, f, "victim", MemberRoleCommon, 0)

	_, err := f.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      selfRemovalGroupNo,
		Members:      []string{"victim"},
		OperatorUID:  "owner_other",
		OperatorName: "group-owner",
	})
	require.NoError(t, err)

	payloads := stub.sentPayloads()
	assert.True(t, payloadsContain(payloads, "移除群聊"),
		"常规踢人仍应发出原有的「被移除」系统消息")
	assert.False(t, payloadsContain(payloads, "将机器人"),
		"常规踢人不应发 bot owner Tip")
}

// 验收 10：bot_owned_by_me 的下发口径——自己的 bot 为 true，
// 他人的 bot 与人类成员恒为 false。
func TestBotOwnerSelfRemoval_MembersGetExposesBotOwnedByMe(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_owned_flag", "mine", testutil.UID, 1)
	seedSelfRemovalBot(t, f, "bot_other_flag", "theirs", "owner_other", 1)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+selfRemovalGroupNo+"/members?page=1&limit=50", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "成员列表应可读: %s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))

	flags := map[string]bool{}
	for _, m := range members {
		uid, _ := m["uid"].(string)
		owned, present := m["bot_owned_by_me"].(bool)
		require.True(t, present, "每个成员行都应带 bot_owned_by_me（uid=%s）", uid)
		flags[uid] = owned
	}
	assert.True(t, flags["bot_owned_flag"], "自己名下的 bot 应为 true")
	assert.False(t, flags["bot_other_flag"], "他人名下的 bot 应为 false")
	assert.False(t, flags[testutil.UID], "人类成员应恒为 false")
	assert.False(t, flags["owner_other"], "人类成员应恒为 false")
}

// 被拉黑的成员（status=Blacklist、is_deleted=0）不得借自助分支获得写操作。
//
// 上面那层 QueryMemberWithUID 只过滤 is_deleted，会把被拉黑成员当作仍在群
// （见 db.go QueryActiveMemberGroupNosWithUID 的约定）；而 QueryBotUIDsOwnedByUIDs
// 故意不过滤 group_member.status（拉黑级联要靠它恢复），所以拉黑门必须由
// handler 显式来把。少了它，被拉黑的人能改群成员表并往群里写一条持久化 Tip。
func TestBotOwnerSelfRemoval_RejectsBlacklistedOperator(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_of_blacklisted", "bot", testutil.UID, 1)

	_, err := f.ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), selfRemovalGroupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_of_blacklisted"})
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑成员不得移除任何成员")
	assert.Contains(t, w.Body.String(), "member_cannot_remove",
		"应回「无权移除」，实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_of_blacklisted", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "bot 应仍在群内")
}

// memberGet 也要如实下发 bot_owned_by_me（验收 10 的「三处同名同型」）。
//
// 这条同时钉住 queryMemberWithGroupNoAndUID 的选择列必须含 group_member.robot：
// 漏选时 Robot 恒为 0，fillBotOwnedByMe 的前置判据直接短路，字段静默恒 false。
func TestBotOwnerSelfRemoval_MemberGetExposesBotOwnedByMe(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_single_get", "mine", testutil.UID, 1)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET",
		"/v1/groups/"+selfRemovalGroupNo+"/members/bot_single_get", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "单成员查询应可读: %s", w.Body.String())

	var resp struct {
		Exists bool `json:"exists"`
		Member struct {
			UID          string `json:"uid"`
			Robot        int    `json:"robot"`
			BotOwnedByMe bool   `json:"bot_owned_by_me"`
		} `json:"member"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Exists)
	assert.Equal(t, 1, resp.Member.Robot, "robot 必须被下发，否则回填会静默失效")
	assert.True(t, resp.Member.BotOwnedByMe, "memberGet 也要如实下发 bot_owned_by_me")
}

// 被授予群角色（Manager / Creator）的 bot 不走自助路径。
//
// 白名单按所有权圈定目标、不过滤 role，而 managerAdd 不排除 robot，所以
// Manager 角色的 bot 构造得出来。若不拦：普通成员能移除一个群管理员，而真正的
// 管理员反而不能移除另一个管理员，权限阶梯倒挂。维护者拍板的「所有权优先」
// 针对的是 bot_admin 列，group_member.role 是更强的授予，不在其覆盖范围。
func TestBotOwnerSelfRemoval_RejectsManagerRoleBot(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: "bot_mgr", Name: "mgr-bot", ShortNo: "bmgr", Robot: 1}))
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)", "bot_mgr", testutil.UID,
	).Exec()
	require.NoError(t, err)
	seedSelfRemovalMember(t, f, "bot_mgr", MemberRoleManager, 1)

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_mgr"})
	assert.NotEqual(t, http.StatusOK, w.Code, "Manager 角色的 bot 不应走自助路径")
	assert.Contains(t, w.Body.String(), "cannot_remove_admin",
		"应回「不能移除管理员」，实际: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_mgr", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "Manager 角色的 bot 应仍在群内")
}

// 回填必须与移除授权同口径：被拉黑的所有者不该拿到 bot_owned_by_me=true。
//
// 移除的授权是两个谓词（活跃成员 + 所有权白名单）。回填若只判所有权，被拉黑的
// 所有者仍能拉到成员列表、看到移除按钮，点下去被拒 —— 按钮与权限打架。
func TestBotOwnerSelfRemoval_BlacklistedOwnerGetsNoOwnershipFlag(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_bl_flag", "mine", testutil.UID, 1)

	_, err := f.ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), selfRemovalGroupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)

	flags := membersGetOwnershipFlags(t, handler, selfRemovalGroupNo)
	assert.False(t, flags["bot_bl_flag"],
		"被拉黑的所有者不应拿到 bot_owned_by_me=true —— 否则按钮可见但请求必被拒")
}

// 角色 bot 同理：既然自助路径拒绝它，回填也不该把它标成可移除。
func TestBotOwnerSelfRemoval_RoleBotGetsNoOwnershipFlag(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: "bot_mgr_flag", Name: "mgr", ShortNo: "bmgrf", Robot: 1}))
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)", "bot_mgr_flag", testutil.UID,
	).Exec()
	require.NoError(t, err)
	seedSelfRemovalMember(t, f, "bot_mgr_flag", MemberRoleManager, 1)
	seedSelfRemovalBot(t, f, "bot_plain_flag", "plain", testutil.UID, 1)

	flags := membersGetOwnershipFlags(t, handler, selfRemovalGroupNo)
	assert.False(t, flags["bot_mgr_flag"], "Manager 角色的 bot 不应被标成可移除")
	assert.True(t, flags["bot_plain_flag"], "对照：普通角色的自有 bot 仍应为 true")
}

// membersync 是三个下发端点里最后一个 —— 也是 Android WKSDK ChannelMember 缓存的
// 唯一增量来源、「缺失=false」降级语义的所在。此前只测了 members 与 members/:uid，
// 而「漏选列导致字段静默恒 false」正是本任务在 memberGet 上真实踩过的坑。
func TestBotOwnerSelfRemoval_MemberSyncExposesBotOwnedByMe(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_sync_mine", "mine", testutil.UID, 1)
	seedSelfRemovalBot(t, f, "bot_sync_other", "theirs", "owner_other", 1)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET",
		"/v1/groups/"+selfRemovalGroupNo+"/membersync?version=0&limit=100", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "membersync 应可读: %s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))
	flags := map[string]bool{}
	for _, m := range members {
		uid, _ := m["uid"].(string)
		owned, present := m["bot_owned_by_me"].(bool)
		require.True(t, present, "membersync 的每一行都应带 bot_owned_by_me（uid=%s）", uid)
		flags[uid] = owned
	}
	assert.True(t, flags["bot_sync_mine"], "自己名下的 bot 应为 true")
	assert.False(t, flags["bot_sync_other"], "他人名下的 bot 应为 false")
	assert.False(t, flags[testutil.UID], "人类成员应恒为 false")
}

// membersGetOwnershipFlags 拉一次成员列表，返回 uid -> bot_owned_by_me。
func membersGetOwnershipFlags(t *testing.T, handler http.Handler, groupNo string) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members?page=1&limit=50", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "成员列表应可读: %s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))
	flags := map[string]bool{}
	for _, m := range members {
		uid, _ := m["uid"].(string)
		owned, _ := m["bot_owned_by_me"].(bool)
		flags[uid] = owned
	}
	return flags
}

// 移除 → 加回 的完整往返：成员身份、所有权标记、以及**再次移除**都要正常。
//
// 自助移除天然是可逆操作（memberAdd 的 ownership 校验允许所有者拉自己的 bot），
// 所以「加回来之后还能不能正常用」和「移除本身」同等重要：
//   - 成员行要真的回来（软删 + recoverMemberTx 复活，不是插新行）；
//   - bot_owned_by_me 要恢复 true，否则前端按钮回不来，用户第二次就撤不掉了；
//   - 再移除一次要能成功，证明这不是个一次性能力。
func TestBotOwnerSelfRemoval_ReAddThenRemoveAgainWorks(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	_, _ = ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)",
	).Exec()
	seedSelfRemovalBot(t, f, "bot_cycle", "cycle-bot", testutil.UID, 1)

	addBack := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		body := util.ToJson(map[string]interface{}{"members": []string{"bot_cycle"}})
		req, err := http.NewRequest("POST",
			"/v1/groups/"+selfRemovalGroupNo+"/members", bytes.NewReader([]byte(body)))
		require.NoError(t, err)
		req.Header.Set("token", testutil.Token)
		handler.ServeHTTP(w, req)
		return w
	}

	// --- 第一轮移除 ---
	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_cycle"})
	require.Equal(t, http.StatusOK, w.Code, "首次移除应成功: %s", w.Body.String())
	exist, err := f.db.ExistMember("bot_cycle", selfRemovalGroupNo)
	require.NoError(t, err)
	require.False(t, exist, "首次移除后 bot 应不在群内")
	assert.False(t, membersGetOwnershipFlags(t, handler, selfRemovalGroupNo)["bot_cycle"],
		"已移除的 bot 不应再出现在成员列表里并带 true")

	// --- 加回来 ---
	addW := addBack()
	require.Equal(t, http.StatusOK, addW.Code, "所有者应能把自己的 bot 加回来: %s", addW.Body.String())
	exist, err = f.db.ExistMember("bot_cycle", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.True(t, exist, "加回后 bot 应重新在群内")

	// 所有权标记必须恢复，否则前端按钮回不来，用户第二次就没有入口了。
	assert.True(t, membersGetOwnershipFlags(t, handler, selfRemovalGroupNo)["bot_cycle"],
		"加回后 bot_owned_by_me 必须恢复 true，否则移除入口一次性失效")

	// --- 第二轮移除：证明这不是一次性能力 ---
	w2 := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_cycle"})
	assert.Equal(t, http.StatusOK, w2.Code, "加回后应能再次移除: %s", w2.Body.String())
	exist, err = f.db.ExistMember("bot_cycle", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.False(t, exist, "第二次移除后 bot 应再次不在群内")
}

// 验收 9：自助移除同样要清理该 bot 在本群所有子区的成员身份。
//
// 这条不是新逻辑，而是 RemoveGroupMembers 既有的 removeUserFromGroupThreads 副作用；
// 钉住它是为了防止后续有人给自助路径抄一条「轻量」删除分支，把清理漏掉——
// 那会留下能经 bot 旁路读子区内容的残留成员行（同 YUJ-52 / #1189 的失败形状）。
func TestBotOwnerSelfRemoval_AlsoCleansThreadMembership(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	ensureThreadTables(t, f)
	seedSelfRemovalBot(t, f, "bot_thread", "thread-bot", testutil.UID, 1)

	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("self_rm_thread", selfRemovalGroupNo, "sub", "owner_other", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "bot_thread", 0, 1).Exec()
	require.NoError(t, err)

	var preCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "bot_thread").Load(&preCount)
	require.NoError(t, err)
	require.Equal(t, 1, preCount, "前置条件：bot 此刻应是子区成员")

	w := deleteMembersReq(t, handler, selfRemovalGroupNo, []string{"bot_thread"})
	require.Equal(t, http.StatusOK, w.Code, "移除应成功: %s", w.Body.String())

	var postCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "bot_thread").Load(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "自助移除后 bot 的子区成员身份也应被清理")
}

// 验收 6 的补充：群主既有能力不受影响——群主仍可移除**他人**名下的 bot。
// （自助分支只在 role 非 Creator/Manager 时才进入，不应挡住原有路径。）
func TestBotOwnerSelfRemoval_CreatorStillRemovesAnyBot(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))
	resetGroupUIDRateLimit(t, ctx)
	newGroupIMStub(t, ctx)
	wireI18nRendererForGroupTest(s)

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "the-owner", ShortNo: "own_any"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "member_x", Name: "member-x", ShortNo: "mx_any"}))
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: selfRemovalGroupNo, Name: "owner removes any", Creator: testutil.UID,
		Status: GroupStatusNormal, Version: 1,
	}))
	seedSelfRemovalMember(t, f, testutil.UID, MemberRoleCreator, 0)
	seedSelfRemovalMember(t, f, "member_x", MemberRoleCommon, 0)
	// bot 归 member_x 所有，不归群主所有。
	seedSelfRemovalBot(t, f, "bot_of_x", "bot-of-x", "member_x", 1)

	w := deleteMembersReq(t, s.GetRoute(), selfRemovalGroupNo, []string{"bot_of_x"})
	assert.Equal(t, http.StatusOK, w.Code, "群主应仍可移除他人名下的 bot: %s", w.Body.String())

	exist, err := f.db.ExistMember("bot_of_x", selfRemovalGroupNo)
	require.NoError(t, err)
	assert.False(t, exist, "群主移除应生效")
}
