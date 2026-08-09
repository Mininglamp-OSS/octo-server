package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// db_common_group_test.go —— ExistCommonGroup 谓词的直接单测。
//
// 为什么单列在 group 包：该谓词是对象级资料可见性判定的一条腿
// （见 modules/channel/service/profile_visibility.go）。channel 侧的路由测试直接注入
// 生产实现 ch.groupService.ExistCommonGroup，能覆盖"群已解散"；但 modules/user 侧走
// 注册钩子，且其测试为消除全局状态污染注册了自写替身，**无法**覆盖生产谓词。谓词的
// 语义必须在它自己的包里钉住，否则改 SQL 不会有任何测试变红。
//
// 每条断言都做过变异验证：删掉对应谓词，对应用例必须失败。

func newCommonGroupDB(t *testing.T) *DB {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))
	return NewDB(ctx)
}

func seedCG(t *testing.T, d *DB, groupNo string, groupStatus int, members map[string]int) {
	t.Helper()
	_, err := d.session.InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES (?,?,'cg_creator',?,1)",
		groupNo, "cg-"+groupNo, groupStatus,
	).Exec()
	assert.NoError(t, err)
	for uid, memberStatus := range members {
		_, err := d.session.InsertBySql(
			"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES (?,?,0,?,1,?,0)",
			groupNo, uid, memberStatus, "vc-"+groupNo+"-"+uid,
		).Exec()
		assert.NoError(t, err)
	}
}

func TestExistCommonGroup(t *testing.T) {
	d := newCommonGroupDB(t)

	// 正常群 + 双方 status=Normal → 构成共同群
	seedCG(t, d, "cg_ok", GroupStatusNormal, map[string]int{"cg_a": 1, "cg_b": 1})
	// 已解散群（status=Disband，成员行仍在——disband 只翻父行、成员异步清理）
	seedCG(t, d, "cg_disband", GroupStatusDisband, map[string]int{"cg_a": 1, "cg_dis": 1})
	// 管理员禁用群（status=Disabled）——**仍应算共同群**：全模块 liveness 检查只排除
	// Disband，禁用是可逆的独立状态，用白名单会静默丢失互相可见性。
	seedCG(t, d, "cg_disabled", GroupStatusDisabled, map[string]int{"cg_a": 1, "cg_dsb": 1})
	// 调用方在群内被拉黑（status=Blacklist，is_deleted 仍为 0）
	seedCG(t, d, "cg_blk_self", GroupStatusNormal, map[string]int{"cg_a": 2, "cg_blk1": 1})
	// 目标在群内被拉黑
	seedCG(t, d, "cg_blk_peer", GroupStatusNormal, map[string]int{"cg_a": 1, "cg_blk2": 2})

	cases := []struct {
		name string
		a, b string
		want bool
		why  string
	}{
		{"normal_group_both_active", "cg_a", "cg_b", true, "正常群 + 双方正常"},
		{"disbanded_group", "cg_a", "cg_dis", false,
			"已解散群不构成关系：成员行会在异步清理窗口期残留，授权不能 fail-open 依赖清理"},
		{"disabled_group_still_counts", "cg_a", "cg_dsb", true,
			"管理员禁用群仍是活着的群（全模块只排除 Disband）；用 status=Normal 白名单会误伤"},
		{"caller_blacklisted", "cg_a", "cg_blk1", false, "调用方被该群拉黑 → 该群不再赋予可达关系"},
		{"peer_blacklisted", "cg_a", "cg_blk2", false, "目标被该群拉黑 → 同上（口径对称）"},
		{"no_shared_group", "cg_b", "cg_dis", false, "无共同群"},
		{"empty_uid", "", "cg_b", false, "空 uid 直接判否，不查库"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := d.ExistCommonGroup(c.a, c.b)
			assert.NoError(t, err)
			assert.Equal(t, c.want, got, c.why)
		})
	}
}

// 已退群（is_deleted=1，成员行保留）不构成共同群。
func TestExistCommonGroup_LeftMemberExcluded(t *testing.T) {
	d := newCommonGroupDB(t)

	_, err := d.session.InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('cg_left','cg-left','cg_creator',?,1)",
		GroupStatusNormal,
	).Exec()
	assert.NoError(t, err)
	_, err = d.session.InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES ('cg_left','cg_x',0,1,1,'vc1',1)",
	).Exec()
	assert.NoError(t, err)
	_, err = d.session.InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES ('cg_left','cg_y',0,1,1,'vc2',0)",
	).Exec()
	assert.NoError(t, err)

	got, err := d.ExistCommonGroup("cg_x", "cg_y")
	assert.NoError(t, err)
	assert.False(t, got, "已退群成员不构成共同群")
}
