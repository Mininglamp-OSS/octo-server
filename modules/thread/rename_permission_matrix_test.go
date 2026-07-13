package thread

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

// seedRenameThread 建一个父群 + 一名操作者成员 + 一个活跃子区（子区创建者是另一个人，
// 用来证明「改名不再要求创建者/管理员」）。操作者的成员状态 memberStatus、user.robot=robot
// 由调用方指定，覆盖活跃人类 / 龙虾 / 黑名单三档。
//
// 直插 DB（不走 CreateThread）以规避已停的 IM(:5001)——与 group 侧 seed 同策略，
// 只验证 UpdateName 的权限分档，不依赖频道创建。
func seedRenameThread(t *testing.T, memberStatus, robot int) (*Service, string, string, string) {
	t.Helper()
	return seedRenameThreadExternal(t, memberStatus, robot, 0)
}

// seedRenameThreadExternal 是 seedRenameThread 的扩展变体，额外指定操作者父群成员行的
// is_external 档位，用于复现「跨 Space 外部成员改子区名应被拒绝」的边界（YUJ-231 / GH#1289）。
func seedRenameThreadExternal(t *testing.T, memberStatus, robot, isExternal int) (*Service, string, string, string) {
	t.Helper()

	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	uniq := util.GenerUUID()[:8]
	operatorUID := "op_" + uniq
	threadCreatorUID := "tc_" + uniq

	userDB := user.NewDB(ctx)
	// 操作者：按档位决定成员状态与是否龙虾。
	require.NoError(t, userDB.Insert(&user.Model{UID: operatorUID, Name: "操作者", ShortNo: "t_o_" + uniq, Robot: robot}))
	// 子区创建者：另一名普通成员。
	require.NoError(t, userDB.Insert(&user.Model{UID: threadCreatorUID, Name: "子区创建者", ShortNo: "t_c_" + uniq}))

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "父群", Creator: threadCreatorUID, Status: 1, Version: 1}))
	// 子区创建者是活跃群成员。
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: threadCreatorUID, Role: group.MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
	}))
	// 操作者成员行——状态与 is_external 按档位。
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: operatorUID, Role: group.MemberRoleCommon, IsExternal: isExternal,
		Status: memberStatus, Version: 2, Vercode: util.GenerUUID(),
	}))

	// 活跃子区，创建者是 threadCreatorUID（非操作者）。
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	tdb := NewDB(ctx)
	require.NoError(t, tdb.Insert(&Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       "原始子区名",
		CreatorUID: threadCreatorUID,
		Status:     ThreadStatusActive,
		Version:    1,
	}))

	svc := NewService(ctx).(*Service)
	return svc, groupNo, shortID, operatorUID
}

// TestThreadUpdateName_NormalActiveHumanMember_Allowed 覆盖新行为：
// 父群普通活跃人类成员（非子区创建者/管理员）即可改子区名。
func TestThreadUpdateName_NormalActiveHumanMember_Allowed(t *testing.T) {
	svc, groupNo, shortID, operatorUID := seedRenameThread(t, int(common.GroupMemberStatusNormal), 0)

	err := svc.UpdateName(groupNo, shortID, operatorUID, "新子区名")
	require.NoError(t, err, "普通活跃人类成员应能改子区名")

	got, err := svc.GetThread(groupNo, shortID, operatorUID)
	require.NoError(t, err)
	require.Equal(t, "新子区名", got.Name)
}

// TestThreadUpdateName_Robot_Denied 覆盖新行为：龙虾(robot)禁止改子区名。
func TestThreadUpdateName_Robot_Denied(t *testing.T) {
	svc, groupNo, shortID, operatorUID := seedRenameThread(t, int(common.GroupMemberStatusNormal), 1)

	err := svc.UpdateName(groupNo, shortID, operatorUID, "龙虾改名")
	require.Error(t, err, "龙虾应被拒绝改子区名")
	require.Contains(t, err.Error(), "no permission")

	got, err := svc.GetThread(groupNo, shortID, operatorUID)
	require.NoError(t, err)
	require.Equal(t, "原始子区名", got.Name, "龙虾被拒后子区名不变")
}

// TestThreadUpdateName_BlacklistMember_Denied 覆盖新行为：
// 被父群拉黑的成员（非活跃）禁止改子区名。
func TestThreadUpdateName_BlacklistMember_Denied(t *testing.T) {
	svc, groupNo, shortID, operatorUID := seedRenameThread(t, int(common.GroupMemberStatusBlacklist), 0)

	err := svc.UpdateName(groupNo, shortID, operatorUID, "黑名单改名")
	require.Error(t, err, "黑名单成员应被拒绝改子区名")
	require.Contains(t, err.Error(), "no permission")

	got, err := svc.GetThread(groupNo, shortID, operatorUID)
	require.NoError(t, err)
	require.Equal(t, "原始子区名", got.Name, "黑名单成员被拒后子区名不变")
}

// TestThreadUpdateName_ExternalMember_Denied 覆盖安全边界：
// 跨 Space 外部成员（is_external=1）即便活跃、人类、状态正常，也禁止改子区名——放宽后的
// 门禁必须保留 is_external=0 边界（YUJ-231 / GH#1289，P1）。
func TestThreadUpdateName_ExternalMember_Denied(t *testing.T) {
	svc, groupNo, shortID, operatorUID := seedRenameThreadExternal(t, int(common.GroupMemberStatusNormal), 0, 1)

	err := svc.UpdateName(groupNo, shortID, operatorUID, "外部成员改名")
	require.Error(t, err, "外部成员应被拒绝改子区名")
	require.Contains(t, err.Error(), "no permission")

	got, err := svc.GetThread(groupNo, shortID, operatorUID)
	require.NoError(t, err)
	require.Equal(t, "原始子区名", got.Name, "外部成员被拒后子区名不变")
}
