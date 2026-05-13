package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestQueryCategorySettingsByGroupNos_WithCategory 测试查询有分类的群组
func TestQueryCategorySettingsByGroupNos_WithCategory(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	uid := testutil.UID
	groupNo := "group-cat-001"
	categoryID := "cat-001"

	// 插入 group_setting 记录（带 category_id）
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "category_id", "category_sort", "revoke_remind", "screenshot", "receipt").
		Values(groupNo, uid, categoryID, 5, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 查询
	results, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, groupNo, results[0].GroupNo)
	assert.NotNil(t, results[0].CategoryID)
	assert.Equal(t, categoryID, *results[0].CategoryID)
	assert.Equal(t, 5, results[0].CategorySort)
}

// TestQueryCategorySettingsByGroupNos_WithoutCategory 测试查询无分类的群组
func TestQueryCategorySettingsByGroupNos_WithoutCategory(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	uid := testutil.UID
	groupNo := "group-nocat-001"

	// 插入 group_setting 记录（无 category_id）
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "revoke_remind", "screenshot", "receipt").
		Values(groupNo, uid, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 查询
	results, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid)
	assert.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, groupNo, results[0].GroupNo)
	assert.Nil(t, results[0].CategoryID) // 无分类时应为 nil
	assert.Equal(t, 0, results[0].CategorySort)
}

// TestQueryCategorySettingsByGroupNos_NoSetting 测试查询无 setting 记录的群组
func TestQueryCategorySettingsByGroupNos_NoSetting(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	uid := testutil.UID
	groupNo := "group-nosetting-001"

	// 不插入任何记录

	// 查询
	results, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid)
	assert.NoError(t, err)
	assert.Len(t, results, 0) // 无记录时返回空数组
}

// TestQueryCategorySettingsByGroupNos_MultipleGroups 测试批量查询多个群组
func TestQueryCategorySettingsByGroupNos_MultipleGroups(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	uid := testutil.UID
	groupNo1 := "group-multi-001"
	groupNo2 := "group-multi-002"
	groupNo3 := "group-multi-003"
	categoryID := "cat-multi-001"

	// 群组1: 有分类
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "category_id", "category_sort", "revoke_remind", "screenshot", "receipt").
		Values(groupNo1, uid, categoryID, 1, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 群组2: 无分类
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "revoke_remind", "screenshot", "receipt").
		Values(groupNo2, uid, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 群组3: 无 setting 记录

	// 查询 3 个群组
	results, err := db.QueryCategorySettingsByGroupNos([]string{groupNo1, groupNo2, groupNo3}, uid)
	assert.NoError(t, err)
	assert.Len(t, results, 2) // 只有 2 个有 setting 记录

	// 构建 map 方便断言
	resultMap := make(map[string]*GroupCategorySetting)
	for _, r := range results {
		resultMap[r.GroupNo] = r
	}

	// 群组1: 有分类
	assert.NotNil(t, resultMap[groupNo1])
	assert.NotNil(t, resultMap[groupNo1].CategoryID)
	assert.Equal(t, categoryID, *resultMap[groupNo1].CategoryID)

	// 群组2: 无分类
	assert.NotNil(t, resultMap[groupNo2])
	assert.Nil(t, resultMap[groupNo2].CategoryID)

	// 群组3: 不在结果中
	assert.Nil(t, resultMap[groupNo3])
}

// TestQueryCategorySettingsByGroupNos_EmptyInput 测试空输入
func TestQueryCategorySettingsByGroupNos_EmptyInput(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	db := newGroupCategoryDB(ctx)

	// 空数组
	results, err := db.QueryCategorySettingsByGroupNos([]string{}, testutil.UID)
	assert.NoError(t, err)
	assert.Nil(t, results)
}

// TestQueryCategorySettingsByGroupNos_DifferentUser 测试不同用户的隔离
func TestQueryCategorySettingsByGroupNos_DifferentUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	uid1 := testutil.UID
	uid2 := "other-user-001"
	groupNo := "group-isolation-001"
	categoryID := "cat-isolation-001"

	// 用户1: 有分类
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "category_id", "category_sort", "revoke_remind", "screenshot", "receipt").
		Values(groupNo, uid1, categoryID, 1, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 用户2: 无分类
	_, err = ctx.DB().InsertInto("group_setting").
		Columns("group_no", "uid", "revoke_remind", "screenshot", "receipt").
		Values(groupNo, uid2, 1, 1, 1).
		Exec()
	assert.NoError(t, err)

	// 查询用户1
	results1, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid1)
	assert.NoError(t, err)
	assert.Len(t, results1, 1)
	assert.NotNil(t, results1[0].CategoryID)
	assert.Equal(t, categoryID, *results1[0].CategoryID)

	// 查询用户2
	results2, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid2)
	assert.NoError(t, err)
	assert.Len(t, results2, 1)
	assert.Nil(t, results2[0].CategoryID) // 用户2 没有设置分类
}
