package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestGetMemberExternalFields_External YUJ-206 单成员版：外部成员 → home = source。
func TestGetMemberExternalFields_External(t *testing.T) {
	_, ctx := newTestServer(t)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	groupSpaceID := "space-home-206"
	srcSpaceID := "space-src-206"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1), (?, ?, 1)",
		groupSpaceID, "HomeSpace", srcSpaceID, "SrcSpace",
	).Exec()
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj206-ext"
	assert.NoError(t, db.Insert(&Model{GroupNo: groupNo, SpaceID: groupSpaceID, Status: 1}))
	assert.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "u-ext", IsExternal: 1, SourceSpaceID: srcSpaceID,
	}))

	s := NewService(ctx).(*Service)
	isExt, srcID, srcName, homeID, homeName, err := s.GetMemberExternalFields(groupNo, "u-ext")
	assert.NoError(t, err)
	assert.Equal(t, 1, isExt)
	assert.Equal(t, srcSpaceID, srcID)
	assert.Equal(t, "SrcSpace", srcName)
	assert.Equal(t, srcSpaceID, homeID, "外部成员 home_space_id 应 = source_space_id")
	assert.Equal(t, "SrcSpace", homeName)
}

// TestGetMemberExternalFields_Internal YUJ-206 单成员版：内部成员 → home = 群 Space。
func TestGetMemberExternalFields_Internal(t *testing.T) {
	_, ctx := newTestServer(t)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	groupSpaceID := "space-home-206-int"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1)",
		groupSpaceID, "GroupHomeSpace",
	).Exec()
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj206-int"
	assert.NoError(t, db.Insert(&Model{GroupNo: groupNo, SpaceID: groupSpaceID, Status: 1}))
	assert.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "u-int", IsExternal: 0,
	}))

	s := NewService(ctx).(*Service)
	isExt, srcID, srcName, homeID, homeName, err := s.GetMemberExternalFields(groupNo, "u-int")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", srcID)
	assert.Equal(t, "", srcName)
	assert.Equal(t, groupSpaceID, homeID, "内部成员 home_space_id 应 = group.space_id")
	assert.Equal(t, "GroupHomeSpace", homeName)
}

// TestGetMemberExternalFields_MissingOrEmpty 覆盖空入参 / 不存在成员 / 群无 Space
// 三类边界，确保都返回全零值 + nil error，不抛异常。
func TestGetMemberExternalFields_MissingOrEmpty(t *testing.T) {
	_, ctx := newTestServer(t)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	s := NewService(ctx).(*Service)

	// 1. 入参为空
	isExt, srcID, srcName, homeID, homeName, err := s.GetMemberExternalFields("", "u")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", srcID)
	assert.Equal(t, "", srcName)
	assert.Equal(t, "", homeID)
	assert.Equal(t, "", homeName)

	isExt, _, _, homeID, _, err = s.GetMemberExternalFields("g", "")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", homeID)

	// 2. 群无 space_id → 内部成员的 home_space_id 应为空（而非 panic）
	db := NewDB(ctx)
	groupNo := "g-yuj206-nospace"
	assert.NoError(t, db.Insert(&Model{GroupNo: groupNo, SpaceID: "", Status: 1}))
	assert.NoError(t, db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "u-int-nospace", IsExternal: 0,
	}))
	isExt, _, _, homeID, homeName, err = s.GetMemberExternalFields(groupNo, "u-int-nospace")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", homeID)
	assert.Equal(t, "", homeName)

	// 3. uid 不在群内 → 全零值
	isExt, _, _, homeID, _, err = s.GetMemberExternalFields(groupNo, "nonexistent-uid")
	assert.NoError(t, err)
	assert.Equal(t, 0, isExt)
	assert.Equal(t, "", homeID)
}
