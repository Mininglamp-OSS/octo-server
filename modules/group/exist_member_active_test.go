package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExistMemberActive_ExcludesBlacklist 验证子区(CommunityTopic)读/发门禁的单群
// 白名单校验：拉黑只把 status 翻成 Blacklist、is_deleted 仍为 0，普通 ExistMember
// 仍返回 true（被拉黑用户可越权读子区历史/会话）。ExistMemberActive 必须额外要求
// status=Normal，把黑名单成员挡在门外（YUJ-4212 CR 整改）。
func TestExistMemberActive_ExcludesBlacklist(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := NewDB(ctx)
	const groupNo = "g-yuj4212-active"

	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "normal", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 1,
	}))
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "blacklisted", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusBlacklist), Version: 2,
	}))
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "deleted", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 3, IsDeleted: 1,
	}))

	// 现状对照：旧 ExistMember 对被拉黑用户仍返回 true（正是越权读的口子）。
	blExist, err := db.ExistMember("blacklisted", groupNo)
	require.NoError(t, err)
	assert.True(t, blExist, "ExistMember 只过滤 is_deleted，黑名单仍过门（对照基线）")

	// ExistMemberActive：正常成员过门
	ok, err := db.ExistMemberActive("normal", groupNo)
	require.NoError(t, err)
	assert.True(t, ok, "正常成员必须过 ExistMemberActive")

	// ExistMemberActive：被拉黑用户被拒
	ok, err = db.ExistMemberActive("blacklisted", groupNo)
	require.NoError(t, err)
	assert.False(t, ok, "被拉黑成员必须被 ExistMemberActive 拒绝（子区越权读门禁）")

	// ExistMemberActive：已删除用户被拒
	ok, err = db.ExistMemberActive("deleted", groupNo)
	require.NoError(t, err)
	assert.False(t, ok, "已删除成员必须被 ExistMemberActive 拒绝")

	// ExistMemberActive：非成员被拒
	ok, err = db.ExistMemberActive("stranger", groupNo)
	require.NoError(t, err)
	assert.False(t, ok, "非成员必须被 ExistMemberActive 拒绝")
}

// TestExistMembersActive_ExcludesBlacklist 验证子区批量读门禁的白名单校验：
// 旧 ExistMembers 把被拉黑用户所在群也算进“仍是成员”的集合，使其会话列表/sidebar
// 仍透出子区。ExistMembersActive 必须只返回 status=Normal AND is_deleted=0 的群编号
// （YUJ-4212 CR 整改）。
func TestExistMembersActive_ExcludesBlacklist(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))

	db := NewDB(ctx)
	const uid = "u-yuj4212"
	const gNormal = "g-active-normal"
	const gBlack = "g-active-black"
	const gDeleted = "g-active-deleted"
	const gStranger = "g-active-stranger"

	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: gNormal, UID: uid, Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 1,
	}))
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: gBlack, UID: uid, Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusBlacklist), Version: 2,
	}))
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: gDeleted, UID: uid, Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 3, IsDeleted: 1,
	}))
	// gStranger 里 uid 完全不存在（只放一个别的成员占位）
	require.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: gStranger, UID: "someone-else", Role: MemberRoleCommon,
		Status: int(common.GroupMemberStatusNormal), Version: 4,
	}))

	groupNos := []string{gNormal, gBlack, gDeleted, gStranger}

	// 现状对照：旧 ExistMembers 把黑名单群也算成员。
	legacy, err := db.existMembers(groupNos, uid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{gNormal, gBlack}, legacy,
		"existMembers 只过滤 is_deleted：黑名单群仍在结果里（对照基线）")

	// ExistMembersActive 只返回正常成员所在群。
	active, err := db.existMembersActive(groupNos, uid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{gNormal}, active,
		"existMembersActive 必须排除黑名单 / 已删除 / 非成员群（子区批量越权读门禁）")
}

// TestExistMemberActive 验证子区(CommunityTopic)读/写门禁所依赖的活跃成员判定：
// status=Normal AND is_deleted=0 才放行；Blacklist / Quit / 已删除一律 fail-closed。
// 这是 YUJ-4219 把 thread 模块 + channel_files + mutualDelete 子区分支统一切到
// ExistMemberActive 后，所有这些门禁共享的授权原语，故在 DB 层兜底双向覆盖。
func TestExistMemberActive(t *testing.T) {
	_, ctx := newTestServer(t)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	db := NewDB(ctx)
	groupNo := "g-yuj4219-active"

	// 正常成员
	assert.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo,
		UID:     "normal",
		Role:    MemberRoleCommon,
		Status:  int(common.GroupMemberStatusNormal),
		Version: 1,
	}))
	// 被拉黑成员（is_deleted=0，仅 status=Blacklist）—— 越权读的核心攻击面
	assert.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo,
		UID:     "blacklisted",
		Role:    MemberRoleCommon,
		Status:  int(common.GroupMemberStatusBlacklist),
		Version: 2,
	}))

	// normal → 放行
	ok, err := db.ExistMemberActive("normal", groupNo)
	assert.NoError(t, err)
	assert.True(t, ok, "正常成员应放行")

	// blacklist → 拒
	ok, err = db.ExistMemberActive("blacklisted", groupNo)
	assert.NoError(t, err)
	assert.False(t, ok, "被拉黑成员(is_deleted=0,status=Blacklist)必须拒绝越权读")

	// 不存在的用户 → 拒
	ok, err = db.ExistMemberActive("ghost", groupNo)
	assert.NoError(t, err)
	assert.False(t, ok, "非成员应拒绝")

	// 对照：旧的 permissive ExistMember 对被拉黑成员仍会放行，证明二者语义差异
	permissive, err := db.ExistMember("blacklisted", groupNo)
	assert.NoError(t, err)
	assert.True(t, permissive, "ExistMember(permissive) 对被拉黑成员仍 true —— 正是必须改用 Active 的原因")
}
