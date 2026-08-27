package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// seedBotMember 插入一个 bot user + robot 行 + group_member 行（robot=1）。
// 返回 bot UID（便于断言）。creatorUID 非空时建立 robot.creator_uid 绑定，
// 为空时模拟孤儿 bot（无 robot 行），此时 D-2 cascade 不应将其误带走。
func seedBotMember(t *testing.T, f *Service, groupNo, botUID, botName, creatorUID string) {
	t.Helper()
	err := f.userDB.Insert(&user.Model{UID: botUID, Name: botName, ShortNo: "sn_" + botUID, Robot: 1})
	assert.NoError(t, err)
	if creatorUID != "" {
		_, err = f.ctx.DB().InsertBySql(
			"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
			botUID, creatorUID,
		).Exec()
		assert.NoError(t, err)
	}
	err = f.db.InsertMember(&MemberModel{
		GroupNo: groupNo,
		UID:     botUID,
		Role:    MemberRoleCommon,
		Robot:   1,
		Status:  1,
		Version: 1,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)
}

// TestQueryBotsInvitedByUIDTx_BasicMatch 验证 SQL 只挑出「属于该 inviter 且活跃」的 bot。
func TestQueryBotsInvitedByUIDTx_BasicMatch(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1"},
		Name:    "bot-cascade-query",
	})
	assert.NoError(t, err)

	s := svc.(*Service)
	// m1 拉入的 bot
	seedBotMember(t, s, resp.GroupNo, "bot_m1_1", "bot-m1-1", "m1")
	seedBotMember(t, s, resp.GroupNo, "bot_m1_2", "bot-m1-2", "m1")
	// 群主拉入的 bot（不应被列为 m1 的 bot）
	seedBotMember(t, s, resp.GroupNo, "bot_creator", "bot-creator", testutil.UID)
	// 孤儿 bot：group_member.robot=1 但 robot 表没行（T6 orphan 场景）
	seedBotMember(t, s, resp.GroupNo, "bot_orphan", "bot-orphan", "")

	tx, err := s.ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	uids, err := s.db.QueryBotsInvitedByUIDTx(resp.GroupNo, "m1", false, tx)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"bot_m1_1", "bot_m1_2"}, uids)

	// 不存在的 inviter → 空切片
	uids, err = s.db.QueryBotsInvitedByUIDTx(resp.GroupNo, "nobody", false, tx)
	assert.NoError(t, err)
	assert.Empty(t, uids)

	// 空 groupNo / inviterUID → nil，no error（防御式短路）
	uids, err = s.db.QueryBotsInvitedByUIDTx("", "m1", false, tx)
	assert.NoError(t, err)
	assert.Nil(t, uids)
	uids, err = s.db.QueryBotsInvitedByUIDTx(resp.GroupNo, "", false, tx)
	assert.NoError(t, err)
	assert.Nil(t, uids)
}

// TestQueryBotsInvitedByUIDTx_SkipsInactiveRobot 验证被禁用 (robot.status=0) 的 bot
// 也不被级联（与 checkBotOwnership 的 fail-closed 语义对齐）。
func TestQueryBotsInvitedByUIDTx_SkipsInactiveRobot(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1"},
		Name:    "bot-cascade-inactive",
	})
	assert.NoError(t, err)

	s := svc.(*Service)
	// 活跃 bot
	seedBotMember(t, s, resp.GroupNo, "bot_active", "active", "m1")
	// 禁用 bot：手工覆盖 status=0
	seedBotMember(t, s, resp.GroupNo, "bot_disabled", "disabled", "m1")
	_, err = s.ctx.DB().UpdateBySql(
		"UPDATE robot SET status=0 WHERE robot_id=?", "bot_disabled",
	).Exec()
	assert.NoError(t, err)

	tx, err := s.ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	uids, err := s.db.QueryBotsInvitedByUIDTx(resp.GroupNo, "m1", false, tx)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"bot_active"}, uids)
}

// TestRemoveGroupMembers_CascadeRemovesInvitedBots 验证踢人路径：被踢的成员
// 所拉入的所有活跃 bot 同事务一并带走，removedUIDs 包含 bot。群主拉的 bot 留下。
func TestRemoveGroupMembers_CascadeRemovesInvitedBots(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1", "m2")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1", "m2"},
		Name:    "cascade-remove-kick",
	})
	assert.NoError(t, err)

	s := svc.(*Service)
	// m1 拉入 2 个 bot；m2 拉入 1 个 bot；群主拉入 1 个 bot（应留下）；1 个孤儿 bot（应留下）
	seedBotMember(t, s, resp.GroupNo, "bot_m1_a", "bm1a", "m1")
	seedBotMember(t, s, resp.GroupNo, "bot_m1_b", "bm1b", "m1")
	seedBotMember(t, s, resp.GroupNo, "bot_m2_a", "bm2a", "m2")
	seedBotMember(t, s, resp.GroupNo, "bot_creator", "bcreator", testutil.UID)
	seedBotMember(t, s, resp.GroupNo, "bot_orphan", "borph", "")

	removeResp, err := svc.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      resp.GroupNo,
		Members:      []string{"m1"},
		OperatorUID:  testutil.UID,
		OperatorName: "creator",
	})
	assert.NoError(t, err)

	// 期望移除 = m1 + bot_m1_a + bot_m1_b = 3
	assert.Equal(t, 3, removeResp.Removed)
	assert.ElementsMatch(t, []string{"m1", "bot_m1_a", "bot_m1_b"}, removeResp.RemovedUIDs)

	// m1 的 bot 不再在群内
	exist, err := s.db.ExistMember("bot_m1_a", resp.GroupNo)
	assert.NoError(t, err)
	assert.False(t, exist, "m1 的 bot_m1_a 应被级联移除")
	exist, err = s.db.ExistMember("bot_m1_b", resp.GroupNo)
	assert.NoError(t, err)
	assert.False(t, exist, "m1 的 bot_m1_b 应被级联移除")

	// m2 的 bot 不受影响
	exist, err = s.db.ExistMember("bot_m2_a", resp.GroupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "m2 的 bot 不应被误伤")

	// 群主 bot / 孤儿 bot 留在群里
	exist, err = s.db.ExistMember("bot_creator", resp.GroupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "群主自己拉的 bot 不应被级联")
	exist, err = s.db.ExistMember("bot_orphan", resp.GroupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "孤儿 bot（无 robot 行）不应被级联")
}

// TestRemoveGroupMembers_NoBotsCascade_NoOp 对照组：被踢成员没有拉入 bot 时，
// 级联逻辑不改变 removedUIDs，不泄露 bot UID。
func TestRemoveGroupMembers_NoBotsCascade_NoOp(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1", "m2")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1", "m2"},
		Name:    "cascade-noop",
	})
	assert.NoError(t, err)

	s := svc.(*Service)
	// 一个「他人」的 bot 存在群内（但不是 m1 拉的，m1 没拉任何 bot）
	seedBotMember(t, s, resp.GroupNo, "bot_m2", "bm2", "m2")

	removeResp, err := svc.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      resp.GroupNo,
		Members:      []string{"m1"},
		OperatorUID:  testutil.UID,
		OperatorName: "creator",
	})
	assert.NoError(t, err)

	// 只移除 m1，不碰任何 bot
	assert.Equal(t, 1, removeResp.Removed)
	assert.Equal(t, []string{"m1"}, removeResp.RemovedUIDs)

	exist, err := s.db.ExistMember("bot_m2", resp.GroupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "他人 bot 无关联关系，不应被移除")
}

// TestRemoveGroupMembers_MultipleLeaversCascadeDistinct 验证批量踢人时，
// 每个 leaver 各自的 bot 都被带走，不会互相干扰或重复。
func TestRemoveGroupMembers_MultipleLeaversCascadeDistinct(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1", "m2", "m3")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1", "m2", "m3"},
		Name:    "cascade-batch",
	})
	assert.NoError(t, err)

	s := svc.(*Service)
	seedBotMember(t, s, resp.GroupNo, "bot_m1", "bm1", "m1")
	seedBotMember(t, s, resp.GroupNo, "bot_m2", "bm2", "m2")
	seedBotMember(t, s, resp.GroupNo, "bot_m3", "bm3", "m3") // 留下

	removeResp, err := svc.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      resp.GroupNo,
		Members:      []string{"m1", "m2"},
		OperatorUID:  testutil.UID,
		OperatorName: "creator",
	})
	assert.NoError(t, err)

	// m1 + bot_m1 + m2 + bot_m2 = 4
	assert.Equal(t, 4, removeResp.Removed)
	assert.ElementsMatch(t, []string{"m1", "m2", "bot_m1", "bot_m2"}, removeResp.RemovedUIDs)

	exist, err := s.db.ExistMember("bot_m3", resp.GroupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "m3 的 bot 与本次批量踢人无关")
}

// --- QuitGroup (groupExit) cascade simulation ---
//
// groupExit 的 HTTP handler 在单测里无法直接驱动（ctx.Event 尚未初始化会 panic），
// 已是 baseline 行为（TestGroupMdGet_NoContent 同因）。此处直接调用 cascadeRemoveBotsInvitedByUIDTx，
// 覆盖和 QuitGroup 路径完全相同的语义；HTTP 层面的正确性由 E2E T-bot-5 / BrowserWing 保障。

// TestQuitGroup_CascadeRemovesInvitedBots 模拟主动退群路径：在事务中执行
// DeleteMemberTx(leaver) + cascadeRemoveBotsInvitedByUIDTx，验证 leaver 自己的 bot 被带走。
func TestQuitGroup_CascadeRemovesInvitedBots(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "owner", "friend")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: "owner",
		Members: []string{testutil.UID, "friend"},
		Name:    "quit-cascade-main",
	})
	assert.NoError(t, err)
	groupNo := resp.GroupNo

	s := svc.(*Service)
	// user-c 拉的 bot
	seedBotMember(t, s, groupNo, "bot_userc_1", "userc-bot-1", testutil.UID)
	seedBotMember(t, s, groupNo, "bot_userc_2", "userc-bot-2", testutil.UID)
	// friend 拉的 bot（不应被级联）
	seedBotMember(t, s, groupNo, "bot_friend", "friend-bot", "friend")
	// owner / orphan bot（不应被级联）
	seedBotMember(t, s, groupNo, "bot_owner", "owner-bot", "owner")
	seedBotMember(t, s, groupNo, "bot_orphan", "orphan-bot", "")

	// 模拟 groupExit 事务：DeleteMember(leaver) + cascade
	tx, err := s.ctx.DB().Begin()
	assert.NoError(t, err)
	leaverVersion, _ := s.ctx.GenSeq(common.GroupMemberSeqKey)
	assert.NoError(t, s.db.DeleteMemberTx(groupNo, testutil.UID, leaverVersion, tx))
	cascaded, err := cascadeRemoveBotsInvitedByUIDTx(s.db, s.ctx, groupNo, testutil.UID, false, tx)
	assert.NoError(t, err)
	assert.NoError(t, tx.Commit())

	assert.ElementsMatch(t, []string{"bot_userc_1", "bot_userc_2"}, cascaded)

	// 退群方和其 bot 均已离群
	for _, uid := range []string{testutil.UID, "bot_userc_1", "bot_userc_2"} {
		exist, err := s.db.ExistMember(uid, groupNo)
		assert.NoError(t, err)
		assert.False(t, exist, "%s 应已离群", uid)
	}
	// 其他 bot 留在群
	for _, uid := range []string{"bot_friend", "bot_owner", "bot_orphan"} {
		exist, err := s.db.ExistMember(uid, groupNo)
		assert.NoError(t, err)
		assert.True(t, exist, "%s 不应被级联", uid)
	}
}

// TestQuitGroup_CreatorOwnsNoBot_NoOp 对照组：退群的人没有拉入任何 bot，级联为 no-op。
// 进一步验证：即便群里存在他人 bot，不会被误伤。
func TestQuitGroup_CreatorOwnsNoBot_NoOp(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "owner", "peer")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: "owner",
		Members: []string{testutil.UID, "peer"},
		Name:    "quit-cascade-noop",
	})
	assert.NoError(t, err)
	groupNo := resp.GroupNo

	s := svc.(*Service)
	// 他人 bot 存在，但和 user-c 无关
	seedBotMember(t, s, groupNo, "bot_peer", "peer-bot", "peer")

	tx, err := s.ctx.DB().Begin()
	assert.NoError(t, err)
	leaverVersion, _ := s.ctx.GenSeq(common.GroupMemberSeqKey)
	assert.NoError(t, s.db.DeleteMemberTx(groupNo, testutil.UID, leaverVersion, tx))
	cascaded, err := cascadeRemoveBotsInvitedByUIDTx(s.db, s.ctx, groupNo, testutil.UID, false, tx)
	assert.NoError(t, err)
	assert.NoError(t, tx.Commit())

	assert.Empty(t, cascaded, "user-c 没有 bot，级联应为 no-op")

	// user-c 离群，peer 和 bot_peer 都还在
	exist, err := s.db.ExistMember(testutil.UID, groupNo)
	assert.NoError(t, err)
	assert.False(t, exist)
	exist, err = s.db.ExistMember("bot_peer", groupNo)
	assert.NoError(t, err)
	assert.True(t, exist, "他人 bot 无关，不应被误伤")
}

// TestQuitGroup_CreatorCascadesBots 验证 #354 产品决策：bot 永远跟随其主人，无角色例外。
// 群主退群时（角色先转让给 newGrouper），其名下 bot 同样被级联带走；
// 新群主人选必须排除退群群主名下的 bot（否则新群主在同一事务内即被级联删除）。
// 本测试按 groupExit 的新事务序列组装：选主（排除 leaver 的 bot）→ 转让 → 删 leaver → cascade。
func TestQuitGroup_CreatorCascadesBots(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1"},
		Name:    "quit-cascade-creator",
	})
	assert.NoError(t, err)
	groupNo := resp.GroupNo

	s := svc.(*Service)
	// 群主自己拉的 bot；把 bot 的 created_at 提前，使其成为「第二元老」，
	// 验证选主 SQL 确实排除 leaver 名下 bot 而选中人类成员 m1。
	seedBotMember(t, s, groupNo, "bot_by_creator", "creator-bot", testutil.UID)
	_, err = s.ctx.DB().UpdateBySql(
		"UPDATE group_member SET created_at = DATE_SUB(NOW(), INTERVAL 1 DAY) WHERE group_no=? AND uid=?",
		groupNo, "bot_by_creator",
	).Exec()
	assert.NoError(t, err)

	member, err := s.db.QueryMemberWithUID(testutil.UID, groupNo)
	assert.NoError(t, err)
	assert.Equal(t, MemberRoleCreator, member.Role)

	// 选主：必须跳过 leaver 名下的 bot，选中 m1
	newGrouper, err := s.db.QuerySecondOldestMemberExcludingBotsOf(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.NotNil(t, newGrouper)
	assert.Equal(t, "m1", newGrouper.UID, "新群主不能是退群群主名下的 bot")

	tx, err := s.ctx.DB().Begin()
	assert.NoError(t, err)
	version, _ := s.ctx.GenSeq(common.GroupMemberSeqKey)
	assert.NoError(t, s.db.UpdateMemberRoleTx(groupNo, newGrouper.UID, MemberRoleCreator, version, tx))
	assert.NoError(t, s.db.DeleteMemberTx(groupNo, testutil.UID, version, tx))
	// #354：cascade 无角色例外，群主退群同样触发
	cascaded, err := cascadeRemoveBotsInvitedByUIDTx(s.db, s.ctx, groupNo, testutil.UID, false, tx)
	assert.NoError(t, err)
	assert.NoError(t, tx.Commit())

	assert.ElementsMatch(t, []string{"bot_by_creator"}, cascaded, "群主的 bot 必须随群主离群（#354）")

	exist, err := s.db.ExistMember("bot_by_creator", groupNo)
	assert.NoError(t, err)
	assert.False(t, exist, "creator 退群其 bot 必须被级联带走，无角色例外（#354）")

	// 新群主 m1 仍在群且为 creator
	m1Member, err := s.db.QueryMemberWithUID("m1", groupNo)
	assert.NoError(t, err)
	assert.NotNil(t, m1Member)
	assert.Equal(t, MemberRoleCreator, m1Member.Role)
	assert.Equal(t, 0, m1Member.IsDeleted)
}

// TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft 群里除群主外只剩 bot 时，
// 选主返回 nil（群成为无主空群），不能把 bot 提为新群主。
//
// 此前这条用例的后半段断言的是相反的契约：「他人的 bot 不在排除范围内（与旧
// QuerySecondOldestMember 语义一致）」。那条断言现已**推翻**——群主握有改群信息、
// 加/removeManager、再转让的全部敏感权限，交给机器人账号等于群实际无人管理而权限
// 仍然活着。理由与另外两条排除（外部成员、非正常 status）一并写在
// QuerySecondOldestMemberExcludingBotsOf 的注释里。
func TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "m1")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"m1"},
		Name:    "quit-cascade-only-bots",
	})
	assert.NoError(t, err)
	groupNo := resp.GroupNo

	s := svc.(*Service)
	// m1 离群，群里只剩群主 + 群主自己的 bot
	version, _ := s.ctx.GenSeq(common.GroupMemberSeqKey)
	assert.NoError(t, s.db.DeleteMember(groupNo, "m1", version))
	seedBotMember(t, s, groupNo, "bot_only", "only-bot", testutil.UID)

	newGrouper, err := s.db.QuerySecondOldestMemberExcludingBotsOf(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, newGrouper, "群里只剩 leaver 自己的 bot 时不应选出新群主")

	// 他人的 bot 同样不得当选：群主权限不该落到任何机器人账号上。
	seedBotMember(t, s, groupNo, "bot_other", "other-bot", "someone_else")
	newGrouper, err = s.db.QuerySecondOldestMemberExcludingBotsOf(groupNo, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, newGrouper, "他人的 bot 也不得被选为新群主——群主权限不该交给机器人账号")
}
