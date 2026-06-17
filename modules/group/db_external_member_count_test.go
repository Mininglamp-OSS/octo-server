package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestQueryExternalMemberCount_ExcludesBots 验证 QueryExternalMemberCountTx 只统计人类外部成员，
// 不把 bot 算进来（YUJ-48 / Mininglamp-OSS/octo-server#1184）。
//
// 设计语义：
//   - 人类外部成员 (is_external=1, robot=0) → 计入 is_external_group 判定
//   - bot 外部成员    (is_external=1, robot=1) → 只用于能力路由，不应把群标记成外部群
//
// 场景：同一个群内 1 个人类外部成员 + 1 个 bot 外部成员 → 计数应为 1。
func TestQueryExternalMemberCount_ExcludesBots(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj48-exclude-bots"

	// 1 个人类外部成员
	err = db.InsertMember(&MemberModel{
		GroupNo:       groupNo,
		UID:           "human-ext-1",
		IsExternal:    1,
		Robot:         0,
		SourceSpaceID: "spaceB",
	})
	assert.NoError(t, err)

	// 1 个 bot 外部成员（orphan bot，典型 T-bot-5 场景）
	err = db.InsertMember(&MemberModel{
		GroupNo:       groupNo,
		UID:           "bot-ext-1",
		IsExternal:    1,
		Robot:         1,
		SourceSpaceID: "ExampleCorp",
	})
	assert.NoError(t, err)

	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	count, err := db.QueryExternalMemberCountTx(groupNo, tx)
	assert.NoError(t, err)
	// 只统计人类外部成员：期望 = 1，bot 不算
	assert.Equal(t, int64(1), count)
}

// TestQueryExternalMemberCount_OnlyBotsReturnsZero 验证当群里只剩 orphan bot
// （人类外部成员都已退群）时，计数为 0 —— 这正是 T-bot-5 E2E 复现的关键路径：
// refreshIsExternalGroup 应据此把 is_external_group 改回 0。
func TestQueryExternalMemberCount_OnlyBotsReturnsZero(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj48-only-bots"

	err = db.InsertMember(&MemberModel{
		GroupNo:       groupNo,
		UID:           "bot-orphan",
		IsExternal:    1,
		Robot:         1,
		SourceSpaceID: "ExampleCorp",
	})
	assert.NoError(t, err)

	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	count, err := db.QueryExternalMemberCountTx(groupNo, tx)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// TestQueryExternalMemberCount_EmptyGroup 边界：空群（0 成员）应返回 0。
func TestQueryExternalMemberCount_EmptyGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	count, err := db.QueryExternalMemberCountTx("g-yuj48-empty", tx)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// TestQueryExternalMemberCount_MixedMulti 多成员混合场景：
// 2 human external + 3 bot external + 1 human internal → 只返回 human external 数（2）。
func TestQueryExternalMemberCount_MixedMulti(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj48-mixed"

	// 2 human external
	for _, uid := range []string{"human-ext-a", "human-ext-b"} {
		err = db.InsertMember(&MemberModel{
			GroupNo: groupNo, UID: uid, IsExternal: 1, Robot: 0, SourceSpaceID: "spaceB",
		})
		assert.NoError(t, err)
	}
	// 3 bot external
	for _, uid := range []string{"bot-ext-a", "bot-ext-b", "bot-ext-c"} {
		err = db.InsertMember(&MemberModel{
			GroupNo: groupNo, UID: uid, IsExternal: 1, Robot: 1, SourceSpaceID: "ExampleCorp",
		})
		assert.NoError(t, err)
	}
	// 1 human internal（is_external=0，不该计入）
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "human-internal", IsExternal: 0, Robot: 0,
	})
	assert.NoError(t, err)

	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	count, err := db.QueryExternalMemberCountTx(groupNo, tx)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), count)
}

// TestQueryExternalMemberCount_DeletedHuman 软删除的人类外部成员必须排除
// （is_deleted=1 AND is_external=1 AND robot=0 → 不算）。
func TestQueryExternalMemberCount_DeletedHuman(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj48-deleted-human"

	// 一个活的人类外部
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "human-active", IsExternal: 1, Robot: 0, IsDeleted: 0,
		SourceSpaceID: "spaceB",
	})
	assert.NoError(t, err)
	// 一个已删除的人类外部
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "human-gone", IsExternal: 1, Robot: 0, IsDeleted: 1,
		SourceSpaceID: "spaceB",
	})
	assert.NoError(t, err)

	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	count, err := db.QueryExternalMemberCountTx(groupNo, tx)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), count)
}
