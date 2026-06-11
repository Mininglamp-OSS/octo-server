package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 创建者在群闸的安全不变量：cachedCreatorMembership【绝不缓存负结果】。一旦 false
// 被缓存，刚退群的创建者会被"粘"在拒绝态之外的反面——更糟的是若实现反转，退群成员
// 可能被粘在放行态。这里钉住：非成员查询后缓存无条目（下次仍回源 DB），成员查询后
// 缓存有条目且值为 isAdmin（PR #340 review，yujiawei P2#3）。
func TestCreatorMembershipCache_NeverCachesNegative(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	w := New(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	const uid = "cm_cache_uid"
	key := groupNo + "|" + uid

	// 非成员：返回 false，且不得写入缓存。
	member, admin, err := w.cachedCreatorMembership(groupNo, uid)
	require.NoError(t, err)
	assert.False(t, member)
	assert.False(t, admin)
	_, hit := w.memberCache.get(key)
	assert.False(t, hit, "negative membership must never be cached")

	// 普通成员：返回 (true, false) 并缓存，条目值=isAdmin=false。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 0, 1, 0, 1)",
		groupNo, uid).Exec()
	require.NoError(t, err)
	member, admin, err = w.cachedCreatorMembership(groupNo, uid)
	require.NoError(t, err)
	assert.True(t, member)
	assert.False(t, admin)
	isAdmin, hit := w.memberCache.get(key)
	assert.True(t, hit, "positive membership must be cached")
	assert.False(t, isAdmin)

	// 管理员成员：条目值=isAdmin=true（push 覆盖判权依赖该值）。
	const adminUID = "cm_cache_admin"
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 2, 1, 0, 1)",
		groupNo, adminUID).Exec()
	require.NoError(t, err)
	member, admin, err = w.cachedCreatorMembership(groupNo, adminUID)
	require.NoError(t, err)
	assert.True(t, member)
	assert.True(t, admin)
}
