package group

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleGroupDisbandEvent_PreservesThreadMembers 验证企业微信式解散语义
// （产品决策 2026-06，语义相对 YUJ-4185 反转）：群解散后频道与历史**保留**，
// 子区结构 / thread_member / thread_setting 不再被清空——成员仍能查看历史、
// 会话仍留在 sidebar。发送拦截改由 WuKongIM 的 disband flag 承担（见
// modules/group/1module.go 与 modules/thread/1module.go 的 ChannelInfo datasource），
// 不再 IMDelChannel。
//
// DB-level 断言：解散事件提交后 thread_member 行仍在。
func TestHandleGroupDisbandEvent_PreservesThreadMembers(t *testing.T) {
	svc, userDB := setupServiceTest(t)
	s := svc.(*Service)
	f := New(s.ctx)
	ensureThreadTables(t, f)

	insertTestUsers(t, userDB, "db_owner", "db_member")

	spaceID := "space_disband"
	groupNo := "g_disband_thread"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "disband-thread", Creator: "db_owner",
		SpaceID: spaceID, Status: 1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "db_owner", Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: "db_member", Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))

	// 两个子区（active + archived），解散后都应保留（历史可看）
	resActive, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("disband_active", groupNo, "active-sub", "db_owner", 1, 1).Exec()
	require.NoError(t, err)
	activeID, err := resActive.LastInsertId()
	require.NoError(t, err)
	resArchived, err := f.ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values("disband_archived", groupNo, "archived-sub", "db_owner", 2, 1).Exec()
	require.NoError(t, err)
	archivedID, err := resArchived.LastInsertId()
	require.NoError(t, err)

	for _, tid := range []int64{activeID, archivedID} {
		_, err = f.ctx.DB().InsertInto("thread_member").
			Columns("thread_id", "uid", "role", "version").
			Values(tid, "db_member", 0, 1).Exec()
		require.NoError(t, err)
	}

	payload := config.MsgGroupDisband{
		GroupNo:      groupNo,
		Operator:     "db_owner",
		OperatorName: "owner",
	}
	var commitErr error
	committed := false
	f.handleGroupDisbandEvent([]byte(util.ToJson(payload)), func(err error) {
		committed = true
		commitErr = err
	})
	require.True(t, committed, "handler 必须调用 commit")
	require.NoError(t, commitErr, "群解散处理不应报错")

	// 核心断言：所有子区成员记录被保留（active + archived），历史可看。
	var postCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread_member").
		Where("uid=? AND thread_id IN (SELECT id FROM thread WHERE group_no=?)", "db_member", groupNo).
		Load(&postCount)
	require.NoError(t, err)
	assert.Equal(t, 2, postCount, "企业微信式解散：子区成员记录必须保留（历史可看）")

	// 子区行本身也应保留（结构不删）
	var threadCount int
	_, err = f.ctx.DB().Select("count(*)").From("thread").
		Where("group_no=?", groupNo).Load(&threadCount)
	require.NoError(t, err)
	assert.Equal(t, 2, threadCount, "企业微信式解散：子区结构必须保留")
}
