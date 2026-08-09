package user

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

// user_get_authz_test.go —— GET /v1/users/:uid 的对象级授权矩阵。
//
// 该端点与 /v1/channels/:id/:type 同根因：都只有登录鉴权、共用 GetUserDetail，任意
// 登录用户拿任意 UID 就能读到完整身份（短号 / 性别 / 在线状态 / 设备指纹 / 实名）。
// 可见性判定收口在 modules/channel/service，两端共用，故这里主要钉本端点特有的部分：
// 最小集**保留 follow**（资料页要靠它渲染加好友入口），以及不存在目标的响应码。

func newUserAuthzServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))
	// 不注入 renderer 拿不到 error.code
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{UID: testutil.UID, Name: "login-user", ShortNo: "SNLOGINU"}))
	return s, ctx
}

func getUser(s *server.Server, uid string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/"+uid, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func seedUserGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES (?,?,0,1,1,?,0)",
		groupNo, uid, fmt.Sprintf("vc-%s-%s", groupNo, uid),
	).Exec()
	assert.NoError(t, err)
}

// 无关系：降级为最小集——剥掉身份细节，但**保留 follow**（加好友入口靠它）。
func TestUserGet_NoRelation_MinimalKeepsFollow(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_stranger", Name: "StrangerU", ShortNo: "SNSTRU",
	}))

	w := getUser(s, "ut_stranger")
	body := w.Body.String()

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", body)
	assert.Contains(t, body, "StrangerU", "最小集仍需返回 name")
	assert.Contains(t, body, "\"follow\"", "资料页需要 follow 渲染加好友入口, body=%s", body)
	// 身份细节必须剥离
	for _, leaked := range []string{"SNSTRU", "short_no", "device_flag", "last_offline", "source_desc", "sex", "vercode"} {
		assert.NotContains(t, body, leaked, "无关系不得下发 %s", leaked)
	}
}

// 同 Space（无好友关系，可直接聊天）：完整资料。
func TestUserGet_SameSpace_Full(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_same", Name: "SameSpaceU", ShortNo: "SNSAMEU",
	}))
	for _, uid := range []string{testutil.UID, "ut_same"} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status, version) VALUES ('sp_a',?,0,1,1)", uid,
		).Exec()
		assert.NoError(t, err)
	}

	w := getUser(s, "ut_same")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "short_no", "同 Space 可直接聊天，资料应完整")
}

// 仅共同群（跨 Space、非好友）：完整资料。走的是 group 模块注册进来的共同群 checker。
func TestUserGet_CommonGroupOnly_Full(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_cofellow", Name: "CoGroupU", ShortNo: "SNCOU",
	}))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('ug_common','co',?,1,1)", testutil.UID,
	).Exec()
	assert.NoError(t, err)
	seedUserGroupMember(t, ctx, "ug_common", testutil.UID)
	seedUserGroupMember(t, ctx, "ug_common", "ut_cofellow")

	w := getUser(s, "ut_cofellow")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "short_no", "共同群可达应返回完整资料")
}

// bot：无需任何关系即可查看完整资料（用户要先看到 bot 才能决定是否添加）。
func TestUserGet_Bot_Viewable(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_bot", Name: "SomeBotU", ShortNo: "SNBOTU", Robot: 1,
	}))

	w := getUser(s, "ut_bot")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "short_no", "bot 资料应完整可查")
}

// 系统账号：同上。
func TestUserGet_SystemAccount_Viewable(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_sys", Name: "SystemU", ShortNo: "SNSYSU",
	}))
	_, err := ctx.DB().UpdateBySql("UPDATE `user` SET category=? WHERE uid=?", CategorySystem, "ut_sys").Exec()
	assert.NoError(t, err)

	w := getUser(s, "ut_sys")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "short_no", "系统账号资料应完整可查")
}

// 本人：完整资料（含仅自己可见的手机号字段路径）。
func TestUserGet_Self_Full(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	w := getUser(s, testutil.UID)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "SNLOGINU", "本人应看到自己的短号")
}

// 不存在的用户：走 not_found，而不是此前的"查询失败"（后者会误导客户端重试）。
func TestUserGet_NotExist_NotFound(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	w := getUser(s, "ut_no_such_user")
	body := w.Body.String()

	assert.NotEqual(t, http.StatusInternalServerError, w.Code, "body=%s", body)
	assert.Contains(t, body, "err.server.user.not_found", "不存在用户应走 not_found, body=%s", body)
	assert.NotContains(t, body, "err.server.user.query_failed", "不得再报查询失败")
}

// newMinimalUserDetailResp 是白名单构造：只有 uid/name/follow/robot 四个字段。
func TestNewMinimalUserDetailResp_WhitelistOnly(t *testing.T) {
	full := &UserDetailResp{
		UID: "u1", Name: "N", Follow: 0, Robot: 0,
		ShortNo: "SN", Sex: 1, Online: 1, LastOffline: 123, Vercode: "vc",
		SourceDesc: "src", RealName: "张三", RealnameVerified: true, Phone: "13000000000",
	}
	m := newMinimalUserDetailResp(full)
	assert.Equal(t, "u1", m.UID)
	assert.Equal(t, "N", m.Name)
	assert.Equal(t, 0, m.Follow)
	assert.Equal(t, 0, m.Robot)
}
