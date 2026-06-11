package thread

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// setupBlacklistTestData 建一个父群，含 normal 成员 + blacklist 成员（is_deleted=0），
// 用于验证 YUJ-4219 把子区写门禁切到 ExistMemberActive 后被拉黑用户 fail-closed。
func setupBlacklistTestData(t *testing.T) (*Service, string) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	userDB := user.NewDB(ctx)
	assert.NoError(t, userDB.Insert(&user.Model{UID: "normal-u", Name: "normal", ShortNo: "sn_normal"}))
	assert.NoError(t, userDB.Insert(&user.Model{UID: "black-u", Name: "black", ShortNo: "sn_black"}))

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	assert.NoError(t, groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "父群", Creator: "normal-u", Status: 1, Version: 1}))
	// 正常成员
	assert.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: "normal-u", Role: group.MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
	}))
	// 被拉黑成员：is_deleted=0，仅 status=Blacklist
	assert.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: "black-u", Role: group.MemberRoleCommon,
		Status: int(common.GroupMemberStatusBlacklist), Version: 2, Vercode: util.GenerUUID(),
	}))

	return NewService(ctx).(*Service), groupNo
}

// TestCreateThread_BlacklistDenied 被拉黑用户不应能创建子区（fail-closed）。
func TestCreateThread_BlacklistDenied(t *testing.T) {
	svc, groupNo := setupBlacklistTestData(t)

	_, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "越权建区", CreatorUID: "black-u", CreatorName: "black",
	})
	assert.Error(t, err, "被拉黑用户创建子区必须被拒")
	assert.Contains(t, err.Error(), "not a group member")
}

// TestCreateThread_NormalAllowed 正常成员可以创建子区（不被 over-block）。
func TestCreateThread_NormalAllowed(t *testing.T) {
	svc, groupNo := setupBlacklistTestData(t)

	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "正常建区", CreatorUID: "normal-u", CreatorName: "normal",
	})
	assert.NoError(t, err, "正常成员创建子区不应被拦")
	assert.NotNil(t, th)
}

// TestJoinThread_BlacklistDenied 被拉黑用户不应能加入子区（fail-closed）。
func TestJoinThread_BlacklistDenied(t *testing.T) {
	svc, groupNo := setupBlacklistTestData(t)

	// 先由正常成员建一个子区
	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "目标子区", CreatorUID: "normal-u", CreatorName: "normal",
	})
	assert.NoError(t, err)

	err = svc.JoinThread(groupNo, th.ShortID, "black-u")
	assert.Error(t, err, "被拉黑用户加入子区必须被拒")
	assert.Contains(t, err.Error(), "not a group member")
}

// TestJoinThread_NormalAllowed 正常父群成员可以加入子区。
func TestJoinThread_NormalAllowed(t *testing.T) {
	svc, groupNo := setupBlacklistTestData(t)

	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "目标子区2", CreatorUID: "normal-u", CreatorName: "normal",
	})
	assert.NoError(t, err)

	// 正常成员加入子区
	err = svc.JoinThread(groupNo, th.ShortID, "normal-u")
	assert.NoError(t, err, "正常成员加入子区不应被拦")
}

// TestUpdateSetting_BlacklistDenied 被拉黑用户不应能改子区 per-user 设置。
func TestUpdateSetting_BlacklistDenied(t *testing.T) {
	svc, groupNo := setupBlacklistTestData(t)

	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "设置子区", CreatorUID: "normal-u", CreatorName: "normal",
	})
	assert.NoError(t, err)

	err = svc.UpdateSetting(groupNo, th.ShortID, "black-u", map[string]interface{}{"mute": 1})
	assert.Error(t, err, "被拉黑用户改子区设置必须被拒")
	assert.Contains(t, err.Error(), "not a group member")
}
