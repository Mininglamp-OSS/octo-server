package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetSubscribableMemberUIDs_ExcludesBlacklist 验证 YUJ-4185 P0-2 根因收口：
// 子区/父群的 IM Subscribers 数据源必须排除 status=blacklist 成员。GetMembers
// （queryMembersWithGroupNo）只过滤 is_deleted=0，会把黑名单成员一并返回，导致
// WuKongIM 重载订阅时把被拉黑用户加回订阅列表 → 拉黑不自愈。GetSubscribableMemberUIDs
// 必须只返回 status=normal AND is_deleted=0 的成员。
func TestGetSubscribableMemberUIDs_ExcludesBlacklist(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := NewDB(ctx)
	svc := NewService(ctx)
	const groupNo = "g-yuj4185-p0-2"

	// 正常成员
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "normal-1", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 1,
	}))
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "normal-2", Role: MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 2,
	}))
	// 被拉黑成员（is_deleted=0，status=blacklist）—— 必须被排除
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "blacklisted", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusBlacklist), Version: 3,
	}))

	// GetMembers 仍返回全部非删除成员（语义不变，不能破坏其它调用方）
	all, err := svc.GetMembers(groupNo)
	require.NoError(t, err)
	allUIDs := make([]string, 0, len(all))
	for _, m := range all {
		allUIDs = append(allUIDs, m.UID)
	}
	assert.ElementsMatch(t, []string{"normal-1", "normal-2", "blacklisted"}, allUIDs,
		"GetMembers 语义不变：返回所有非删除成员，包括黑名单")

	// GetSubscribableMemberUIDs 排除黑名单
	subs, err := svc.GetSubscribableMemberUIDs(groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"normal-1", "normal-2"}, subs,
		"GetSubscribableMemberUIDs 必须排除 status=blacklist 成员（YUJ-4185 P0-2）")

	// GetBlacklistMemberUIDs 仍只返回黑名单成员（兼容性：Blacklist 数据源回调不受影响）
	bl, err := svc.GetBlacklistMemberUIDs(groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"blacklisted"}, bl,
		"GetBlacklistMemberUIDs 兼容性不变：只返回黑名单成员")
}

// TestGetSubscribableMemberUIDs_ExcludesDeleted 防御：is_deleted=1 的成员同样排除。
func TestGetSubscribableMemberUIDs_ExcludesDeleted(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := NewDB(ctx)
	svc := NewService(ctx)
	const groupNo = "g-yuj4185-p0-2-del"

	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "active", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 1,
	}))
	deleted := &MemberModel{
		GroupNo: groupNo, UID: "deleted", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 2, IsDeleted: 1,
	}
	require.NoError(t, db.InsertMember(deleted))

	subs, err := svc.GetSubscribableMemberUIDs(groupNo)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"active"}, subs,
		"GetSubscribableMemberUIDs 必须排除 is_deleted=1 成员")
}
