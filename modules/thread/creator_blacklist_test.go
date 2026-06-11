package thread

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCreatorBlacklistTest 建一个父群 + 一个由 creator-u（普通成员、子区创建者）
// 建好的子区。返回 service、ctx、groupNo、子区 shortID，供「creator 建区后被拉黑」
// 的授权回归用：测试通过把 creator-u 的 group_member.status 改成 Blacklist 来模拟
// 「先 Normal 后被拉黑」的状态转换。
//
// 验证 YUJ-4229 必修 1：canOperate / UpdateName 在给 creator/admin 特权之前先
// require 父群 active 成员身份，被拉黑创建者 fail-closed。
func setupCreatorBlacklistTest(t *testing.T) (*Service, *config.Context, string, string) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	userDB := user.NewDB(ctx)
	require.NoError(t, userDB.Insert(&user.Model{UID: "owner-u", Name: "owner", ShortNo: "sn_owner"}))
	require.NoError(t, userDB.Insert(&user.Model{UID: "creator-u", Name: "creator", ShortNo: "sn_creator"}))

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "父群", Creator: "owner-u", Status: 1, Version: 1}))
	// 群主
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: "owner-u", Role: group.MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
	}))
	// 子区创建者：建区时是 Normal 普通成员
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: "creator-u", Role: group.MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 2, Vercode: util.GenerUUID(),
	}))

	svc := NewService(ctx).(*Service)
	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "我的子区", CreatorUID: "creator-u", CreatorName: "creator",
	})
	require.NoError(t, err)
	require.NotNil(t, th)

	return svc, ctx, groupNo, th.ShortID
}

// blacklistMember 把 group_member.status 切到 Blacklist（is_deleted 仍=0），
// 模拟「先是正常成员、后被拉黑」的转换。
func blacklistMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), groupNo, uid,
	).Exec()
	require.NoError(t, err)
}

// TestCanOperate_CreatorBlacklisted_Denied creator 建区后被拉黑，
// canOperate 必须拒（不能再 short-circuit 给 creator 特权）。
func TestCanOperate_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	// 正常 creator 仍放行（基线）。
	ok, err := svc.canOperate(groupNo, shortID, "creator-u")
	require.NoError(t, err)
	assert.True(t, ok, "正常 creator 应有操作权")

	// 被拉黑后必须拒。
	blacklistMember(t, ctx, groupNo, "creator-u")
	ok, err = svc.canOperate(groupNo, shortID, "creator-u")
	require.NoError(t, err)
	assert.False(t, ok, "被拉黑的 creator 不应再有操作权")
}

// TestArchiveThread_CreatorBlacklisted_Denied 被拉黑 creator 不能归档自己建的子区。
func TestArchiveThread_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	// 正常 creator 先验证可归档。
	require.NoError(t, svc.ArchiveThread(groupNo, shortID, "creator-u"))
	require.NoError(t, svc.UnarchiveThread(groupNo, shortID, "creator-u"))

	blacklistMember(t, ctx, groupNo, "creator-u")
	err := svc.ArchiveThread(groupNo, shortID, "creator-u")
	assert.Error(t, err, "被拉黑 creator 归档必须被拒")
	assert.Contains(t, err.Error(), "no permission")
}

// TestUnarchiveThread_CreatorBlacklisted_Denied 被拉黑 creator 不能取消归档。
func TestUnarchiveThread_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	require.NoError(t, svc.ArchiveThread(groupNo, shortID, "creator-u"))

	blacklistMember(t, ctx, groupNo, "creator-u")
	err := svc.UnarchiveThread(groupNo, shortID, "creator-u")
	assert.Error(t, err, "被拉黑 creator 取消归档必须被拒")
	assert.Contains(t, err.Error(), "no permission")
}

// TestDeleteThread_CreatorBlacklisted_Denied 被拉黑 creator 不能删除自己建的子区。
func TestDeleteThread_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	blacklistMember(t, ctx, groupNo, "creator-u")
	err := svc.DeleteThread(groupNo, shortID, "creator-u")
	assert.Error(t, err, "被拉黑 creator 删除必须被拒")
	assert.Contains(t, err.Error(), "no permission")
}

// TestDeleteThread_CreatorNormal_Allowed 正常 creator 仍可删除（不被 over-block）。
func TestDeleteThread_CreatorNormal_Allowed(t *testing.T) {
	svc, _, groupNo, shortID := setupCreatorBlacklistTest(t)
	assert.NoError(t, svc.DeleteThread(groupNo, shortID, "creator-u"),
		"正常 creator 删除不应被拦")
}

// TestUpdateName_CreatorBlacklisted_Denied 被拉黑 creator 不能改子区名。
func TestUpdateName_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	// 正常 creator 先验证可改名。
	require.NoError(t, svc.UpdateName(groupNo, shortID, "creator-u", "新名字"))

	blacklistMember(t, ctx, groupNo, "creator-u")
	err := svc.UpdateName(groupNo, shortID, "creator-u", "越权改名")
	assert.Error(t, err, "被拉黑 creator 改名必须被拒")
	assert.Contains(t, err.Error(), "no permission")
}

// TestCanEditThreadMd_CreatorBlacklisted_Denied 被拉黑 creator 不能 edit/delete GROUP.md。
func TestCanEditThreadMd_CreatorBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	ok, err := svc.CanEditThreadMd(groupNo, shortID, "creator-u")
	require.NoError(t, err)
	assert.True(t, ok, "正常 creator 应能编辑 GROUP.md")

	blacklistMember(t, ctx, groupNo, "creator-u")
	ok, err = svc.CanEditThreadMd(groupNo, shortID, "creator-u")
	require.NoError(t, err)
	assert.False(t, ok, "被拉黑 creator 不应能编辑/删除 GROUP.md")
}

// TestCanOperate_GroupOwnerBlacklisted_Denied 群主自己被拉黑后也不能管理子区
// （admin 特权同样在 active 校验之后）。
func TestCanOperate_GroupOwnerBlacklisted_Denied(t *testing.T) {
	svc, ctx, groupNo, shortID := setupCreatorBlacklistTest(t)

	// 群主（非 creator）正常时可操作。
	ok, err := svc.canOperate(groupNo, shortID, "owner-u")
	require.NoError(t, err)
	assert.True(t, ok, "正常群主应可管理子区")

	blacklistMember(t, ctx, groupNo, "owner-u")
	ok, err = svc.canOperate(groupNo, shortID, "owner-u")
	require.NoError(t, err)
	assert.False(t, ok, "被拉黑群主不应再能管理子区")
}
