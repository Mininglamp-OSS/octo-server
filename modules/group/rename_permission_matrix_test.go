package group

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

// seedRenameMatrixGroup 建一个由 creatorUID 创建的群，并把 token 对应用户
// （testutil.UID）作为一名成员写入——角色 role、成员状态 memberStatus、user.robot=robot。
// 这样 groupUpdate 的分档权限校验（活跃人类成员 / 龙虾 / 黑名单 / 管理员）都能被精确复现。
//
// 通过直插 DB 而非走 CreateGroup/AddMember，避免依赖已停的 IM(:5001)，与
// avatar_update_test.go 的 seedCreatorGroup 同风格。
func seedRenameMatrixGroup(t *testing.T, g *Group, groupNo, creatorUID string, role, memberStatus, robot int) {
	t.Helper()
	seedRenameMatrixGroupExternal(t, g, groupNo, creatorUID, role, memberStatus, robot, 0)
}

// seedRenameMatrixGroupExternal 是 seedRenameMatrixGroup 的扩展变体，额外指定操作者成员行的
// is_external 档位，用于复现「跨 Space 外部成员改名应被拒绝」的边界（YUJ-231 / GH#1289）。
func seedRenameMatrixGroupExternal(t *testing.T, g *Group, groupNo, creatorUID string, role, memberStatus, robot, isExternal int) {
	t.Helper()

	uniq := util.GenerUUID()[:8]
	require.NoError(t, g.userDB.Insert(&user.Model{UID: creatorUID, Name: "创建者", ShortNo: "rm_c_" + uniq}))
	// token 对应的操作者（testutil.UID）——按 robot 档位决定是否为龙虾。
	require.NoError(t, g.userDB.Insert(&user.Model{UID: testutil.UID, Name: "操作者", ShortNo: "rm_o_" + uniq, Robot: robot}))

	require.NoError(t, g.db.Insert(&Model{GroupNo: groupNo, Name: "原始群名", Creator: creatorUID, Status: GroupStatusNormal, Version: 1}))
	// 群主成员行。
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: creatorUID, Role: MemberRoleCreator, Status: int(common.GroupMemberStatusNormal),
		Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	// 操作者成员行。
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: role, Status: memberStatus, IsExternal: isExternal,
		Version: 2, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
}

// TestGroupRename_NormalActiveHumanMember_NameOnly_Allowed 覆盖新行为①：
// 仅改 name 时，普通活跃人类成员即可改名，无需管理员/群主。
func TestGroupRename_NormalActiveHumanMember_NameOnly_Allowed(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	wireI18nRendererForGroupTest(s)
	g := New(ctx)

	const groupNo = "rm_name_ok"
	// 操作者是普通成员（非创建者/管理员）、活跃、人类。
	seedRenameMatrixGroup(t, g, groupNo, "rm_creator_1", MemberRoleCommon, int(common.GroupMemberStatusNormal), 0)

	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{"name": "新群名"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "新群名", got.Name, "普通活跃人类成员应能仅改名成功")
}

// TestGroupRename_Robot_NameOnly_Denied 覆盖新行为②：
// 龙虾(robot)即使是活跃成员也禁止改名。
func TestGroupRename_Robot_NameOnly_Denied(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	wireI18nRendererForGroupTest(s)
	g := New(ctx)

	const groupNo = "rm_name_bot"
	// 操作者是活跃普通成员，但 user.robot=1。
	seedRenameMatrixGroup(t, g, groupNo, "rm_creator_2", MemberRoleCommon, int(common.GroupMemberStatusNormal), 1)

	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{"name": "龙虾改名"})
	require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.group.not_group_member",
		"龙虾改名应被拒绝, body=%s", w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "原始群名", got.Name, "龙虾改名应被拒绝，群名不变")
}

// TestGroupRename_BlacklistMember_NameOnly_Denied 覆盖新行为③：
// 黑名单成员（status=blacklist）非活跃成员，禁止改名。
func TestGroupRename_BlacklistMember_NameOnly_Denied(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	wireI18nRendererForGroupTest(s)
	g := New(ctx)

	const groupNo = "rm_name_black"
	// 操作者被拉黑（人类），ExistMemberActive 为 false。
	seedRenameMatrixGroup(t, g, groupNo, "rm_creator_3", MemberRoleCommon, int(common.GroupMemberStatusBlacklist), 0)

	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{"name": "黑名单改名"})
	require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.group.not_group_member",
		"黑名单成员改名应被拒绝, body=%s", w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "原始群名", got.Name, "黑名单成员改名应被拒绝，群名不变")
}

// TestGroupRename_ExternalMember_NameOnly_Denied 覆盖安全边界：
// 跨 Space 外部成员（is_external=1）即便活跃、人类、状态正常，也禁止改名——放宽后的
// 门禁必须保留旧 QueryIsGroupManagerOrCreator 的 is_external=0 边界（YUJ-231 / GH#1289，P1）。
func TestGroupRename_ExternalMember_NameOnly_Denied(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	wireI18nRendererForGroupTest(s)
	g := New(ctx)

	const groupNo = "rm_name_ext"
	// 操作者活跃、人类、状态正常，但 is_external=1（跨 Space 外部成员）。
	seedRenameMatrixGroupExternal(t, g, groupNo, "rm_creator_ext", MemberRoleCommon, int(common.GroupMemberStatusNormal), 0, 1)

	w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{"name": "外部成员改名"})
	require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.group.not_group_member",
		"外部成员改名应被拒绝, body=%s", w.Body.String())

	got, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.Equal(t, "原始群名", got.Name, "外部成员改名应被拒绝，群名不变")
}

// TestGroupRename_NormalMemberWithAdvancedFields_Denied 覆盖新行为④：
// 普通成员带高级字段（name + notice/invite/avatar 混合）时取最高档，仍需管理员/群主。
func TestGroupRename_NormalMemberWithAdvancedFields_Denied(t *testing.T) {
	// 混合请求里的每一种高级字段都应把权限档位抬到「需要管理员」。
	cases := []struct {
		name string
		body map[string]string
	}{
		{"name+notice", map[string]string{"name": "混合1", "notice": "新公告"}},
		{"name+invite", map[string]string{"name": "混合2", "invite": "1"}},
		{"name+avatar_text", map[string]string{"name": "混合3", attrKeyAvatarText: "研发"}},
		{"name+avatar_color", map[string]string{"name": "混合4", attrKeyAvatarColor: "5"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := newTestServer(t)
			require.NoError(t, testutil.CleanAllTables(ctx))
			wireI18nRendererForGroupTest(s)
			g := New(ctx)

			groupNo := fmt.Sprintf("rm_adv_%d", i)
			// 操作者是活跃普通人类成员——仅改名可放行，但带了高级字段就必须是管理员。
			seedRenameMatrixGroup(t, g, groupNo, fmt.Sprintf("rm_creator_adv_%d", i), MemberRoleCommon, int(common.GroupMemberStatusNormal), 0)

			w := putGroupUpdate(t, s.GetRoute(), groupNo, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
			require.Contains(t, w.Body.String(), "err.server.group.manager_only",
				"普通成员带高级字段应要求管理员, body=%s", w.Body.String())

			got, err := g.db.QueryWithGroupNo(groupNo)
			require.NoError(t, err)
			require.Equal(t, "原始群名", got.Name, "被拒绝时群名不应被写入")
		})
	}
}

// TestGroupUpdate_EmptyBodyOrUnknownField_FailClosed 覆盖新行为⑤：
// 空 body → request_invalid；只含未知字段（既无 name 也无高级字段）→ fail-closed 走管理员，
// 普通成员被拒。
func TestGroupUpdate_EmptyBodyOrUnknownField_FailClosed(t *testing.T) {
	// 空 body：请求非法。
	t.Run("empty body -> request_invalid", func(t *testing.T) {
		s, ctx := newTestServer(t)
		require.NoError(t, testutil.CleanAllTables(ctx))
		wireI18nRendererForGroupTest(s)
		g := New(ctx)

		const groupNo = "rm_empty"
		seedRenameMatrixGroup(t, g, groupNo, "rm_creator_empty", MemberRoleCommon, int(common.GroupMemberStatusNormal), 0)

		w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{})
		require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
		require.Contains(t, w.Body.String(), "err.server.group.request_invalid",
			"空 body 应是请求非法, body=%s", w.Body.String())
	})

	// 只含未知字段：不落入「仅改名」放开分支，fail-closed 走管理员校验；普通成员被拒。
	t.Run("unknown field only -> manager required (fail-closed)", func(t *testing.T) {
		s, ctx := newTestServer(t)
		require.NoError(t, testutil.CleanAllTables(ctx))
		wireI18nRendererForGroupTest(s)
		g := New(ctx)

		const groupNo = "rm_unknown"
		seedRenameMatrixGroup(t, g, groupNo, "rm_creator_unknown", MemberRoleCommon, int(common.GroupMemberStatusNormal), 0)

		w := putGroupUpdate(t, s.GetRoute(), groupNo, map[string]string{"totally_unknown_field": "x"})
		require.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
		require.Contains(t, w.Body.String(), "err.server.group.manager_only",
			"仅含未知字段应 fail-closed 走管理员校验, body=%s", w.Body.String())
	})
}
