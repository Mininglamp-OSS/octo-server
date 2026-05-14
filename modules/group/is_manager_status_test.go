package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestQueryIsGroupManagerOrCreator_BlacklistedFailSafe 验证 fail-safe 语义
// （PR #31 round-3，Jerry-Xin）：status=Blacklist / Quit 但 is_deleted=0 的
// 历史/异常成员，即使 role 仍是 creator/manager，也不应被 QueryIsGroupManagerOrCreator
// 识别为有效管理者。否则被踢出的管理员可继续调用所有依赖该 query 的敏感 API
// （转让群主、邀请、踢人、修改群设置、incoming webhook 管理 …）。
//
// 配对 [[is_manager_external_test]]：两条 fail-safe 都在同一处 WHERE 上叠加。
func TestQueryIsGroupManagerOrCreator_BlacklistedFailSafe(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-pr31-status-failsafe"

	// 正常 internal creator（应被识别）
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "normal-creator",
		Role:       MemberRoleCreator,
		IsExternal: 0,
		Status:     int(common.GroupMemberStatusNormal),
		Version:    1,
	})
	assert.NoError(t, err)

	// 黑名单 + role=creator（is_deleted=0，模拟脏数据 / 历史残留）
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "blacklisted-creator",
		Role:       MemberRoleCreator,
		IsExternal: 0,
		Status:     int(common.GroupMemberStatusBlacklist),
		Version:    2,
	})
	assert.NoError(t, err)

	// 黑名单 + role=manager
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "blacklisted-manager",
		Role:       MemberRoleManager,
		IsExternal: 0,
		Status:     int(common.GroupMemberStatusBlacklist),
		Version:    3,
	})
	assert.NoError(t, err)

	// 正常 creator → true
	ok, err := db.QueryIsGroupManagerOrCreator(groupNo, "normal-creator")
	assert.NoError(t, err)
	assert.True(t, ok, "normal creator 应识别为管理者")

	// 黑名单 creator → false（fail-safe 拦截）
	ok, err = db.QueryIsGroupManagerOrCreator(groupNo, "blacklisted-creator")
	assert.NoError(t, err)
	assert.False(t, ok, "黑名单成员即便 role=creator 也不应识别为管理者")

	// 黑名单 manager → false（fail-safe 拦截）
	ok, err = db.QueryIsGroupManagerOrCreator(groupNo, "blacklisted-manager")
	assert.NoError(t, err)
	assert.False(t, ok, "黑名单成员即便 role=manager 也不应识别为管理者")
}
