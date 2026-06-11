package thread

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupReadPathBlacklistData 建一个父群（登录用户 testutil.UID 为正常成员、且是
// 子区创建者），返回 server/ctx/groupNo/子区 shortID。读路径门禁（list/get/
// listMembers/getThreadSimple）都查 testutil.UID 的父群 active 成员身份；测试通过
// 把它的 group_member.status 切到 Blacklist 来验证「normal → later blacklisted」
// 转换后读路径直接 deny（#345 复审 + ReviewBot P2-test 点名补强）。
func setupReadPathBlacklistData(t *testing.T) (*server.Server, *config.Context, string, string) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	userDB := user.NewDB(ctx)
	require.NoError(t, userDB.Insert(&user.Model{UID: testutil.UID, Name: "登录用户", ShortNo: "sn_login"}))

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "父群", Creator: testutil.UID, Status: 1, Version: 1}))
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: group.MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
	}))

	// 由登录用户建一个子区作为读目标。
	svc := NewService(ctx).(*Service)
	th, err := svc.CreateThread(&CreateThreadReq{
		GroupNo: groupNo, Name: "读目标子区", CreatorUID: testutil.UID, CreatorName: "登录用户",
	})
	require.NoError(t, err)
	require.NotNil(t, th)

	return s, ctx, groupNo, th.ShortID
}

func blacklistLoginUser(t *testing.T, ctx *config.Context, groupNo string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), groupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)
}

func doGet(t *testing.T, s *server.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", path, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// TestReadPath_ListThreads_BlacklistTransition 列表读路径：normal 放行，
// 被拉黑后直接 deny。
func TestReadPath_ListThreads_BlacklistTransition(t *testing.T) {
	s, ctx, groupNo, _ := setupReadPathBlacklistData(t)
	path := "/v1/groups/" + groupNo + "/threads"

	w := doGet(t, s, path)
	assert.Equal(t, http.StatusOK, w.Code, "正常成员应能列出子区")

	blacklistLoginUser(t, ctx, groupNo)
	w = doGet(t, s, path)
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑后列子区必须被拒")
}

// TestReadPath_GetThread_BlacklistTransition 详情读路径转换。
func TestReadPath_GetThread_BlacklistTransition(t *testing.T) {
	s, ctx, groupNo, shortID := setupReadPathBlacklistData(t)
	path := "/v1/groups/" + groupNo + "/threads/" + shortID

	w := doGet(t, s, path)
	assert.Equal(t, http.StatusOK, w.Code, "正常成员应能读子区详情")

	blacklistLoginUser(t, ctx, groupNo)
	w = doGet(t, s, path)
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑后读详情必须被拒")
}

// TestReadPath_ListMembers_BlacklistTransition 成员列表读路径转换。
func TestReadPath_ListMembers_BlacklistTransition(t *testing.T) {
	s, ctx, groupNo, shortID := setupReadPathBlacklistData(t)
	path := "/v1/groups/" + groupNo + "/threads/" + shortID + "/members"

	w := doGet(t, s, path)
	assert.Equal(t, http.StatusOK, w.Code, "正常成员应能列子区成员")

	blacklistLoginUser(t, ctx, groupNo)
	w = doGet(t, s, path)
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑后列成员必须被拒")
}

// TestReadPath_GetThreadSimple_BlacklistTransition 简化路由读路径转换。
func TestReadPath_GetThreadSimple_BlacklistTransition(t *testing.T) {
	s, ctx, groupNo, shortID := setupReadPathBlacklistData(t)
	path := "/v1/threads/" + shortID

	w := doGet(t, s, path)
	assert.Equal(t, http.StatusOK, w.Code, "正常成员应能读子区(简化路由)")

	blacklistLoginUser(t, ctx, groupNo)
	w = doGet(t, s, path)
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑后读子区(简化路由)必须被拒")
}
