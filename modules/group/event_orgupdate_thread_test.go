package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleOrgOrDeptEmployeeUpdate_DeleteAlsoCleansThreads 回归 YUJ-4185 P1-2：
// 组织/部门结构更新删人（handleOrgOrDeptEmployeeUpdate 的 deleteMembers 分支）原本
// 只摘父群 IM 订阅 + 发 CMD，漏掉子区清理 —— 与已修的 handleOrgEmployeeExit 不对称。
// 修复后对每个被删 uid 调 removeUserFromGroupThreads。DB-level 断言直查 thread_member。
func TestHandleOrgOrDeptEmployeeUpdate_DeleteAlsoCleansThreads(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	insertTestUsers(t, userDB, "ou_owner", "ou_leaver")

	spaceID := "space_orgupdate"
	groupNo := "g_orgupdate_thread"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "orgupdate-thread", Creator: "ou_owner",
		SpaceID: spaceID, Status: 1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "ou_owner", Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "ou_leaver", Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))

	res, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("orgupd_thread", groupNo, "orgupd-sub", "ou_owner", 1, 1).Exec()
	require.NoError(t, err)
	threadID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = f.ctx.DB().InsertInto("thread_member").
		Columns("thread_id", "uid", "role", "version").
		Values(threadID, "ou_leaver", 0, 1).Exec()
	require.NoError(t, err)

	payload := config.MsgOrgOrDeptEmployeeUpdateReq{
		Members: []*config.OrgOrDeptEmployeeVO{
			{
				Operator:     "ou_owner",
				OperatorName: "owner",
				EmployeeUid:  "ou_leaver",
				EmployeeName: "leaver",
				GroupNo:      groupNo,
				Action:       "delete",
			},
		},
	}
	var commitErr error
	committed := false
	f.handleOrgOrDeptEmployeeUpdate([]byte(util.ToJson(payload)), func(err error) {
		committed = true
		commitErr = err
	})
	require.True(t, committed, "handler 必须调用 commit")
	require.NoError(t, commitErr, "组织结构更新删人处理不应报错")

	var postCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("thread_id=? AND uid=?", threadID, "ou_leaver").Load(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 0, postCount, "组织结构更新删人必须同步清理子区成员/订阅（YUJ-4185 P1-2）")
}

func TestHandleOrgOrDeptEmployeeUpdate_SkipsAITeamContainerMutations(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)

	insertTestUsers(t, userDB, "ai_owner", "ai_bot", "org_new_member")
	const groupNo = "g_orgupdate_ai_container"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "AI container", Creator: "ai_owner",
		SpaceID: "space_orgupdate_ai", Status: GroupStatusNormal,
		Purpose: "ai_session_container",
	}))
	for _, member := range []*MemberModel{
		{GroupNo: groupNo, UID: "ai_owner", Role: MemberRoleCreator, Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID())},
		{GroupNo: groupNo, UID: "ai_bot", Role: MemberRoleCommon, Status: 1, Robot: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID())},
	} {
		require.NoError(t, f.db.InsertMember(member))
	}

	payload := config.MsgOrgOrDeptEmployeeUpdateReq{Members: []*config.OrgOrDeptEmployeeVO{
		{Operator: "org", EmployeeUid: "org_new_member", GroupNo: groupNo, Action: "add"},
		{Operator: "org", EmployeeUid: "ai_owner", GroupNo: groupNo, Action: "delete"},
	}}
	var commitErr error
	f.handleOrgOrDeptEmployeeUpdate([]byte(util.ToJson(payload)), func(err error) { commitErr = err })
	require.NoError(t, commitErr)

	var activeUIDs []string
	_, err := f.ctx.DB().Select("uid").From("group_member").
		Where("group_no=? AND is_deleted=0", groupNo).OrderBy("uid").Load(&activeUIDs)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai_bot", "ai_owner"}, activeUIDs,
		"org-sync add/delete actions must leave the two-member AI container unchanged")
}
