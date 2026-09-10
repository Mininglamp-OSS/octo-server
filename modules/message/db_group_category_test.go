package message

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sidebarOrderTestDBName = "octo_message_sidebar_order_test"

func newSidebarOrderTestContext(t *testing.T) *config.Context {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true"
	}
	parsed, err := mysqldriver.ParseDSN(addr)
	require.NoError(t, err, "parse MySQL DSN")
	parsed.DBName = ""
	bootstrap, err := sql.Open("mysql", parsed.FormatDSN())
	require.NoError(t, err, "open MySQL bootstrap connection")
	t.Cleanup(func() { require.NoError(t, bootstrap.Close()) })
	_, err = bootstrap.Exec("DROP DATABASE IF EXISTS `" + sidebarOrderTestDBName + "`")
	require.NoError(t, err, "drop isolated sidebar-order database")
	_, err = bootstrap.Exec("CREATE DATABASE `" + sidebarOrderTestDBName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err, "create isolated sidebar-order database")

	parsed.DBName = sidebarOrderTestDBName
	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = parsed.FormatDSN()
	ctx := config.NewContext(cfg)
	t.Cleanup(func() { require.NoError(t, ctx.DB().Close()) })

	for _, statement := range []string{
		"CREATE TABLE group_category (category_id VARCHAR(40) NOT NULL, space_id VARCHAR(40) NOT NULL, uid VARCHAR(40) NOT NULL, name VARCHAR(100) NOT NULL, sort INT NOT NULL, status SMALLINT NOT NULL, is_default SMALLINT NULL, PRIMARY KEY (category_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE group_setting (group_no VARCHAR(40) NOT NULL, uid VARCHAR(40) NOT NULL, category_id VARCHAR(40) NULL, category_sort INT NOT NULL DEFAULT 0, PRIMARY KEY (group_no, uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE octo_sidebar_section (id BIGINT NOT NULL AUTO_INCREMENT, uid VARCHAR(40) NOT NULL, space_id VARCHAR(40) NOT NULL, section_type SMALLINT NOT NULL, ref_id VARCHAR(40) NOT NULL, sort INT NOT NULL, status SMALLINT NOT NULL, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL, PRIMARY KEY (id), UNIQUE KEY uk_sidebar_section (uid, space_id, section_type, ref_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	} {
		_, err = ctx.DB().Exec(statement)
		require.NoError(t, err, "create isolated sidebar-order table")
	}
	return ctx
}

// TestCategorySortsUseSidebarSectionOrder keeps the follow-sidebar's legacy
// group/DM payload on the same source of truth as the new unified sections
// endpoint. The two values intentionally disagree so a fallback to
// group_category.sort cannot accidentally satisfy this test.
func TestCategorySortsUseSidebarSectionOrder(t *testing.T) {
	ctx := newSidebarOrderTestContext(t)

	const (
		uid        = "sidebar-order-uid"
		spaceID    = "sidebar-order-space"
		categoryID = "sidebar-order-category"
		groupNo    = "sidebar-order-group"
	)
	now := time.Now().UTC()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, NULL)",
		categoryID, spaceID, uid, "category", 99,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO octo_sidebar_section (uid, space_id, section_type, ref_id, sort, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?, 1, ?, ?)",
		uid, spaceID, categoryID, 7, now, now,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_setting (uid, group_no, category_id, category_sort) VALUES (?, ?, ?, ?)",
		uid, groupNo, categoryID, 3,
	).Exec()
	require.NoError(t, err)

	db := newGroupCategoryDB(ctx)
	settings, err := db.QueryCategorySettingsByGroupNos([]string{groupNo}, uid)
	require.NoError(t, err)
	require.Len(t, settings, 1)
	assert.Equal(t, 7, settings[0].CategoryGroupSort)

	sorts, err := db.QueryCategorySortsByIDs([]string{categoryID}, uid)
	require.NoError(t, err)
	assert.Equal(t, 7, sorts[categoryID])
}

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
