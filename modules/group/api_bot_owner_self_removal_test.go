package group

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
