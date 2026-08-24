package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 自助移除的三处收口（PR #805 重审提出，三位评审独立收敛到同一批）。
//
// 共同的形状：事务**外**的快照守卫已经拦住了角色 bot，但事务**内**没有对应的
// 收口，于是窗口内的状态变化、以及不经过守卫的级联路径，都能绕过它。
// 「守卫只在事务外」这件事本身就是缺陷，不取决于当下能不能构造出触发条件。

// ---------- ① 锁内重查必须同时拦住 Manager ----------

// LockRemovableMemberTx 原本只判 `role != Creator`。自助路径在事务外拦了所有
// 非普通角色，但目标若在「快照 → 取锁」这个窗口内被提升为 Manager，锁内重查
// 会放行、行真的被删，而且 Removed 计数对得上，连计数守卫也发现不了 ——
// 正是验收 #12 要消灭的那个形状，只是漏在了锁内那一半。
func TestLockRemovableMemberTx_RejectsManagerWhenCommonRequired(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	f := New(ctx)

	const groupNo = "g_lock_role"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "lock role", Creator: "owner_x",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedRoleMember(t, f, groupNo, "member_common", MemberRoleCommon)
	seedRoleMember(t, f, groupNo, "member_manager", MemberRoleManager)
	seedRoleMember(t, f, groupNo, "owner_x", MemberRoleCreator)

	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// requireCommonRole=false：沿用既有语义（只排除 Creator），管理员可移除。
	ok, err := f.db.LockRemovableMemberTx(groupNo, "member_manager", false, tx)
	require.NoError(t, err)
	assert.True(t, ok, "既有语义不变：非自助路径下 Manager 仍可被移除")

	// requireCommonRole=true：自助路径，必须连 Manager 一起拦。
	ok, err = f.db.LockRemovableMemberTx(groupNo, "member_manager", true, tx)
	require.NoError(t, err)
	assert.False(t, ok, "自助路径下 Manager 角色必须在**锁内**也被拦住")

	ok, err = f.db.LockRemovableMemberTx(groupNo, "owner_x", true, tx)
	require.NoError(t, err)
	assert.False(t, ok, "Creator 任何情况下都不可移除")

	ok, err = f.db.LockRemovableMemberTx(groupNo, "member_common", true, tx)
	require.NoError(t, err)
	assert.True(t, ok, "普通角色目标应放行")
}

// ---------- ② 级联的角色过滤只作用于自助路径 ----------

// #354 是**写下来的产品决策**：「bot 永远跟随其主人，无角色例外」
// （见 api.go groupExit 与 service.go 过滤处的注释）。它覆盖三条既有路径：
// 主动退群、被群主/管理员移除、被拉黑。
//
// 因此级联的角色过滤**不能**做成全局的：那会让「张三被踢 → 他名下的管理员 bot
// 留在群里、而张三已不是成员、自助路径又拒绝角色 bot」，群里凭空多出一个
// 没人管得动的特权 bot —— 正是 #354 要消灭的状态，拉黑流程下还带安全含义。
//
// 需要角色过滤的只有**新开的自助路径**：普通成员不该借级联把一个管理员 bot
// 洗掉。所以判据跟着 BotOwnerSelfRemoval 走，与锁内的 requireCommonRole 同源。
func TestQueryBotsInvitedByUIDTx_RoleFilterOnlyOnSelfServicePath(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	f := New(ctx)

	const groupNo = "g_cascade_role"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "cascade role", Creator: "owner_y",
		Status: GroupStatusNormal, Version: 1,
	}))
	seedRoleMember(t, f, groupNo, "owner_y", MemberRoleCreator)

	// owner_y 名下三个 bot：普通角色、Manager 角色、Creator 角色。
	seedOwnedBot(t, f, groupNo, "bot_plain", "owner_y", MemberRoleCommon)
	seedOwnedBot(t, f, groupNo, "bot_mgr", "owner_y", MemberRoleManager)
	seedOwnedBot(t, f, groupNo, "bot_creator", "owner_y", MemberRoleCreator)

	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// requireCommonRole=false —— 既有三条路径（退群 / 被踢 / 拉黑）。
	// #354：无角色例外，三个都要跟着走。
	uids, err := f.db.QueryBotsInvitedByUIDTx(groupNo, "owner_y", false, tx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bot_plain", "bot_mgr", "bot_creator"}, uids,
		"既有路径必须保持 #354：bot 永远跟随其主人，无角色例外")

	// requireCommonRole=true —— 仅自助路径。
	uids, err = f.db.QueryBotsInvitedByUIDTx(groupNo, "owner_y", true, tx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bot_plain"}, uids,
		"自助路径下只级联普通角色的 bot，角色 bot 的处置权留给群主/管理员")
}

// 端到端形态：管理员踢人时，被踢者名下的**管理员 bot** 仍要被级联带走。
//
// 这条补的是评审点出的测试盲区 —— space_member_removal_test.go 的
// seedInvitedBot 把 role 硬编码成 0，所有级联 fixture 都是普通角色，
// 于是「级联对角色 bot 怎么处理」从来没有测试覆盖，改坏了也不会红。
func TestRemoveGroupMembers_AdminKickStillCascadesManagerBot(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	insertTestUsers(t, userDB, testutil.UID, "leaver")

	resp, err := svc.CreateGroup(&CreateGroupServiceReq{
		Creator: testutil.UID,
		Members: []string{"leaver"},
		Name:    "admin-kick-cascade",
	})
	require.NoError(t, err)
	s := svc.(*Service)

	// leaver 名下两个 bot：一个普通角色、一个被群主提升为 Manager。
	seedBotMember(t, s, resp.GroupNo, "bot_of_leaver", "bol", "leaver")
	seedBotMember(t, s, resp.GroupNo, "bot_mgr_of_leaver", "bmol", "leaver")
	_, err = s.ctx.DB().UpdateBySql(
		"UPDATE group_member SET role=? WHERE group_no=? AND uid=?",
		MemberRoleManager, resp.GroupNo, "bot_mgr_of_leaver",
	).Exec()
	require.NoError(t, err)

	// 群主踢人（非自助路径）。
	removeResp, err := svc.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:      resp.GroupNo,
		Members:      []string{"leaver"},
		OperatorUID:  testutil.UID,
		OperatorName: "creator",
	})
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{"leaver", "bot_of_leaver", "bot_mgr_of_leaver"}, removeResp.RemovedUIDs,
		"#354：被踢者名下的 bot 全部跟着走，Manager 角色也不例外 —— "+
			"否则群里会留下一个主人已不在、谁也管不动的特权 bot")

	for _, uid := range []string{"bot_of_leaver", "bot_mgr_of_leaver"} {
		exist, eerr := s.db.ExistMember(uid, resp.GroupNo)
		require.NoError(t, eerr)
		assert.False(t, exist, "%s 应已被级联移除", uid)
	}
}

// ---------- ③ 实际移除集合要与请求集合比对，而不是比数量 ----------

// 原来的守卫是 `removeResp.Removed < len(req.Members)`。两个问题：
//   - removedUIDs 含级联带走的 bot，数量会被撑大，可以把「某个目标被跳过」
//     在数字上抹平；
//   - 数量对不上时一律报 ErrGroupCannotRemoveOwner，但真实原因也可能是
//     DeleteMemberTx 失败后 service 的 continue（部分提交）。
//
// 改成比对集合，并让调用方能拿到「到底哪个没被移除」。
func TestMissingRemovalTargets(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		removed   []string
		want      []string
	}{
		{
			name:      "全部移除成功",
			requested: []string{"bot_a"},
			removed:   []string{"bot_a"},
			want:      nil,
		},
		{
			name:      "级联带走额外的 bot，不算缺失",
			requested: []string{"bot_a"},
			removed:   []string{"bot_a", "bot_cascaded"},
			want:      nil,
		},
		{
			name:      "级联把被跳过的目标在数量上抹平 —— 计数比较会漏，集合比较不会",
			requested: []string{"bot_a", "bot_b"},
			removed:   []string{"bot_a", "bot_cascaded"},
			want:      []string{"bot_b"},
		},
		{
			name:      "目标被静默跳过",
			requested: []string{"bot_a", "bot_b"},
			removed:   []string{"bot_a"},
			want:      []string{"bot_b"},
		},
		{
			name:      "一个都没移除",
			requested: []string{"bot_a"},
			removed:   nil,
			want:      []string{"bot_a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, missingRemovalTargets(tc.requested, tc.removed))
		})
	}
}

// ---------- 共用脚手架 ----------

func seedRoleMember(t *testing.T, f *Group, groupNo, uid string, role int) {
	t.Helper()
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: uid, Role: role,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
}

// seedOwnedBot 建一个 robot 行 + 指定角色的 bot 群成员行。
func seedOwnedBot(t *testing.T, f *Group, groupNo, botUID, ownerUID string, role int) {
	t.Helper()
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)", botUID, ownerUID,
	).Exec()
	require.NoError(t, err)
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: botUID, Role: role, Robot: 1,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
}
