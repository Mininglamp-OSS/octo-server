package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestQueryIsGroupManagerOrCreator_ExternalMemberFailSafe 验证 fail-safe 语义
// (YUJ-231 / GH#1289)：即使 DB 里历史脏数据存在 is_external=1 且 role=manager
// 的成员（例如 managerAdd 外部成员校验加入前遗留的数据），
// QueryIsGroupManagerOrCreator 也必须返回 false，从而在角色判定层阻断 8 项敏感操作。
func TestQueryIsGroupManagerOrCreator_ExternalMemberFailSafe(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj231-failsafe"

	// 正常 internal creator（应被识别）
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "internal-creator",
		Role:       MemberRoleCreator,
		IsExternal: 0,
		Status:     int(common.GroupMemberStatusNormal),
		Version:    1,
	})
	assert.NoError(t, err)

	// 历史脏数据：外部成员 + role=manager（老数据或绕过入口写入）
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "external-dirty-manager",
		Role:       MemberRoleManager,
		IsExternal: 1,
		Status:     int(common.GroupMemberStatusNormal),
		Version:    2,
	})
	assert.NoError(t, err)

	// 历史脏数据：外部成员 + role=creator（极端情况，转让入口漏洞）
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "external-dirty-creator",
		Role:       MemberRoleCreator,
		IsExternal: 1,
		Status:     int(common.GroupMemberStatusNormal),
		Version:    3,
	})
	assert.NoError(t, err)

	// 普通成员对照
	err = db.InsertMember(&MemberModel{
		GroupNo:    groupNo,
		UID:        "common-member",
		Role:       MemberRoleCommon,
		IsExternal: 0,
		Status:     int(common.GroupMemberStatusNormal),
		Version:    4,
	})
	assert.NoError(t, err)

	// Internal creator → true
	ok, err := db.QueryIsGroupManagerOrCreator(groupNo, "internal-creator")
	assert.NoError(t, err)
	assert.True(t, ok, "internal creator 应识别为管理者")

	// External dirty manager → false（fail-safe 拦截）
	ok, err = db.QueryIsGroupManagerOrCreator(groupNo, "external-dirty-manager")
	assert.NoError(t, err)
	assert.False(t, ok, "外部成员即使 role=manager 也不应识别为管理者")

	// External dirty creator → false（fail-safe 拦截）
	ok, err = db.QueryIsGroupManagerOrCreator(groupNo, "external-dirty-creator")
	assert.NoError(t, err)
	assert.False(t, ok, "外部成员即使 role=creator 也不应识别为管理者")

	// Common member → false
	ok, err = db.QueryIsGroupManagerOrCreator(groupNo, "common-member")
	assert.NoError(t, err)
	assert.False(t, ok)
}
