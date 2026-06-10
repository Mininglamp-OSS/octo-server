package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleOrgEmployeeExit_AlsoCleansThreads 回归 Issue #27 同型缺口：
// 组织/部门同步退出（handleOrgEmployeeExit）原本只删 group_member + 摘父群
// IM 订阅，从不清理子区 —— 与踢人/退群路径不对称，组织驱动的移除会持续
// 泄漏子区订阅。修复后该 handler 对每个群调用 removeUserFromGroupThreads。
//
// 与 cascade 测试同款 DB-level 断言：直查 thread_member 证明子区清理被执行
// （IM 订阅摘除本身由 thread_cleanup 的单测和共享 helper 保证）。
func TestHandleOrgEmployeeExit_AlsoCleansThreads(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	insertTestUsers(t, userDB, "org_owner", "org_leaver")

	spaceID := "space_orgexit"
	groupNo := "g_orgexit_thread"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "orgexit-thread",
		Creator: "org_owner",
		SpaceID: spaceID,
		Status:  1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "org_owner", Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "org_leaver", Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))

	// 子区 + leaver 的 thread_member 行（DB 可见的清理证据）
	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("orgexit_thread", groupNo, "orgexit-sub", "org_owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "org_leaver", 0, 1).Exec()
	require.NoError(t, err)

	// 触发组织退出事件 handler
	payload := config.OrgEmployeeExitReq{
		Operator: "org_leaver",
		GroupNos: []string{groupNo},
	}
	var commitErr error
	committed := false
	f.handleOrgEmployeeExit([]byte(util.ToJson(payload)), func(err error) {
		committed = true
		commitErr = err
	})
	require.True(t, committed, "handler 必须调用 commit")
	require.NoError(t, commitErr, "组织退出事件处理不应报错")

	// 核心断言：子区成员记录已被清理（修复前 handler 不会触达 thread_member）
	var postCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "org_leaver").Load(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "组织退出必须同步清理子区成员/订阅（Issue #27 同型）")

	// 旁证：群成员已删除，原有语义未回归
	existInGroup, err := f.db.ExistMember("org_leaver", groupNo)
	require.NoError(t, err)
	assert.False(t, existInGroup, "组织退出必须删除 group_member")
}
