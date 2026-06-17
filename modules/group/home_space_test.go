package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestFillSpaceRelatedFields_ExternalAndInternal 核心规则验证（YUJ-63 / #1208）：
//
//	external member → home_space_id = source_space_id, home_space_name = source space name
//	internal member → home_space_id = group.space_id,  home_space_name = group space name
//
// 同时确认 is_external / source_space_id / source_space_name 语义未被改写，
// 且一次调用内合并了 source_space_name 回填（Jerry-Xin review #1209 优化 1：
// 只打 1 次 group 查询兜底 + 1 次 space WHERE IN 批量）。
func TestFillSpaceRelatedFields_ExternalAndInternal(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupSpaceID := "space_group_home"
	groupSpaceName := "GroupHomeSpace"
	srcSpaceID := "space_example"
	srcSpaceName := "ExampleCorp"

	// 写入两条 Space 行（群归属 + 外部来源），便于 fillSpaceRelatedFields 反查名称。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1), (?, ?, 1)",
		groupSpaceID, groupSpaceName, srcSpaceID, srcSpaceName,
	).Exec()
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj63-home-space"

	// 创建群（带 space_id）
	err = db.Insert(&Model{GroupNo: groupNo, SpaceID: groupSpaceID, Status: 1})
	assert.NoError(t, err)

	// 1 内部成员 + 1 外部成员
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "internal-uid", IsExternal: 0,
	})
	assert.NoError(t, err)
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "ext-uid", IsExternal: 1, SourceSpaceID: srcSpaceID,
	})
	assert.NoError(t, err)

	// 构造 resps，故意不预先填 SourceSpaceName，验证合并后的批量查询也回填了原字段。
	resps := []memberDetailResp{
		{UID: "internal-uid", IsExternal: 0},
		{UID: "ext-uid", IsExternal: 1, SourceSpaceID: srcSpaceID},
	}

	g := New(ctx)
	// 走 groupSpaceID="" 的兜底路径：内部触发一次 group 表查询。
	g.fillSpaceRelatedFields(groupNo, "", resps)

	// 内部成员：home 等于群自身 space
	assert.Equal(t, groupSpaceID, resps[0].HomeSpaceID,
		"内部成员 home_space_id 应等于 group.space_id")
	assert.Equal(t, groupSpaceName, resps[0].HomeSpaceName,
		"内部成员 home_space_name 应等于群归属 Space 名称")
	// 原语义保留
	assert.Equal(t, 0, resps[0].IsExternal)
	assert.Equal(t, "", resps[0].SourceSpaceID)
	assert.Equal(t, "", resps[0].SourceSpaceName)

	// 外部成员：home 等于来源 space，且 source_space_name 也被同一次查询填充
	assert.Equal(t, srcSpaceID, resps[1].HomeSpaceID,
		"外部成员 home_space_id 应等于 source_space_id")
	assert.Equal(t, srcSpaceName, resps[1].HomeSpaceName,
		"外部成员 home_space_name 应等于 source space 名称")
	// 原语义保留（合并后仍然正确回填 source_space_name）
	assert.Equal(t, 1, resps[1].IsExternal)
	assert.Equal(t, srcSpaceID, resps[1].SourceSpaceID)
	assert.Equal(t, srcSpaceName, resps[1].SourceSpaceName,
		"合并后 source_space_name 仍应由同一次 space 批量查询回填（原语义保留）")
}

// TestFillSpaceRelatedFields_NoInternalMembers 纯外部群（没有内部成员）：
// fillSpaceRelatedFields 不应查询群资料也不应写 group space，纯按 source 计算。
// 这个用例保证即便 group 行意外缺失也不会 panic / 被错误填充。
func TestFillSpaceRelatedFields_NoInternalMembers(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	srcSpaceID := "space_example_only"
	srcSpaceName := "ExampleCorpOnly"
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1)",
		srcSpaceID, srcSpaceName,
	).Exec()
	assert.NoError(t, err)

	resps := []memberDetailResp{
		{UID: "ext-1", IsExternal: 1, SourceSpaceID: srcSpaceID},
	}

	g := New(ctx)
	// 故意传不存在的 groupNo + 空 groupSpaceID；因为没有内部成员，逻辑不应访问 group 表。
	g.fillSpaceRelatedFields("g-nonexistent", "", resps)

	assert.Equal(t, srcSpaceID, resps[0].HomeSpaceID)
	assert.Equal(t, srcSpaceName, resps[0].HomeSpaceName)
	assert.Equal(t, srcSpaceName, resps[0].SourceSpaceName,
		"纯外部成员 source_space_name 也应被合并查询填充")
}

// TestFillSpaceRelatedFields_GroupSpaceIDPassedIn 验证 Jerry-Xin review #1209 优化 2：
// 调用方（如 syncMembers）已经查过 group 时，把 groupSpaceID 显式传入，
// fillSpaceRelatedFields 不应再做 group 表查询，依然正确填充内部成员的 home_space_*。
// 为了证明没有 fallback 到 group 表，故意传一个数据库里不存在的 groupNo。
func TestFillSpaceRelatedFields_GroupSpaceIDPassedIn(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupSpaceID := "space_group_passthrough"
	groupSpaceName := "GroupPassthroughSpace"

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1)",
		groupSpaceID, groupSpaceName,
	).Exec()
	assert.NoError(t, err)

	// 注意：没有 Insert group 行。如果 fillSpaceRelatedFields 误用 fallback
	// 去查 group 表，它会拿不到 SpaceID，内部成员的 home_space_* 就会是空 —— 断言失败。
	resps := []memberDetailResp{
		{UID: "internal-only", IsExternal: 0},
	}

	g := New(ctx)
	g.fillSpaceRelatedFields("g-does-not-exist", groupSpaceID, resps)

	assert.Equal(t, groupSpaceID, resps[0].HomeSpaceID,
		"外层传入 groupSpaceID 后应直接使用，不依赖 group 表查询")
	assert.Equal(t, groupSpaceName, resps[0].HomeSpaceName)
}

// TestGetMemberExternalMarkers_HomeSpace 验证 Service 层 GetMemberExternalMarkers
// 同时暴露 HomeSpaceID / HomeSpaceName，供消息同步热路径使用（YUJ-63 / #1208）。
func TestGetMemberExternalMarkers_HomeSpace(t *testing.T) {
	_, ctx := newTestServer(t)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	groupSpaceID := "space_group_msg"
	groupSpaceName := "GroupMsgSpace"
	srcSpaceID := "space_example_msg"
	srcSpaceName := "ExampleCorpMsg"

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1), (?, ?, 1)",
		groupSpaceID, groupSpaceName, srcSpaceID, srcSpaceName,
	).Exec()
	assert.NoError(t, err)

	db := NewDB(ctx)
	groupNo := "g-yuj63-markers"
	err = db.Insert(&Model{GroupNo: groupNo, SpaceID: groupSpaceID, Status: 1})
	assert.NoError(t, err)

	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "inside", IsExternal: 0,
	})
	assert.NoError(t, err)
	err = db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "outside", IsExternal: 1, SourceSpaceID: srcSpaceID,
	})
	assert.NoError(t, err)

	svc := NewService(ctx).(*Service)
	markers, err := svc.GetMemberExternalMarkers(groupNo)
	assert.NoError(t, err)

	inside, ok := markers["inside"]
	assert.True(t, ok)
	assert.Equal(t, 0, inside.IsExternal)
	assert.Equal(t, "", inside.SourceSpaceName, "内部成员不暴露 source_space_name")
	assert.Equal(t, groupSpaceID, inside.HomeSpaceID,
		"内部成员 home_space_id 应等于群 space_id")
	assert.Equal(t, groupSpaceName, inside.HomeSpaceName)

	outside, ok := markers["outside"]
	assert.True(t, ok)
	assert.Equal(t, 1, outside.IsExternal)
	assert.Equal(t, srcSpaceName, outside.SourceSpaceName)
	assert.Equal(t, srcSpaceID, outside.HomeSpaceID,
		"外部成员 home_space_id 应等于 source_space_id")
	assert.Equal(t, srcSpaceName, outside.HomeSpaceName)
}
