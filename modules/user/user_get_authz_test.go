package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
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
	// 显式注册两个 group 侧 hook，并在用例结束后复原。
	//
	// 不能依赖模块 bootstrap 注册的全局值：同包的 pinned_test.go 会把
	// groupMemberChecker 置为 nil，整包跑时会污染这些用例（单独跑通过、整包跑失败）。
	// 这里直连 group 的 db 语义显然不可能（modules/user 不能 import modules/group，
	// 反向依赖），所以用等价的最小实现——判定口径与 group.ExistMember /
	// ExistCommonGroup 一致：group_member 行未删且（共同群还要求）双方 status 正常、
	// 群未解散。
	prevMember, prevCommon := getGroupMemberChecker(), getCommonGroupChecker()
	t.Cleanup(func() {
		setGroupMemberChecker(prevMember)
		RegisterCommonGroupChecker(prevCommon)
	})
	setGroupMemberChecker(func(groupNo, uid string) (bool, error) {
		var cnt int
		_, err := ctx.DB().SelectBySql(
			"SELECT 1 FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 LIMIT 1", groupNo, uid,
		).Load(&cnt)
		return cnt > 0, err
	})
	RegisterCommonGroupChecker(func(a, b string) (bool, error) {
		var cnt int
		_, err := ctx.DB().SelectBySql(
			"SELECT 1 FROM group_member m1 "+
				"INNER JOIN group_member m2 ON m1.group_no=m2.group_no "+
				"INNER JOIN `group` g ON g.group_no=m1.group_no "+
				"WHERE m1.uid=? AND m2.uid=? AND m1.is_deleted=0 AND m2.is_deleted=0 "+
				"AND m1.status=1 AND m2.status=1 AND g.status<>2 LIMIT 1", a, b,
		).Load(&cnt)
		return cnt > 0, err
	})
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

// seedUserGroupMemberInvited 带 invite_uid 建成员行。u.get 只有在成员行 InviteUID
// 非空时才把 group_member 塞进响应，所以验证"群维度富化是否被抑制"必须用这个 seeder
// ——否则负向断言恒成立（空断言），证明不了门禁起作用。
// seedSpace 建 Space 父行并指定 status（1=正常, 2=封禁, 0=已解散）。
// 授权判定要 JOIN space 校验活性，所以测试必须建父行——只插 space_member 会让
// "同 Space" 判定为假（这正是 P2-1 修复后的预期行为）。
func seedSpace(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, version) VALUES (?,?,?,?,1)",
		spaceID, "sp-"+spaceID, testutil.UID, status,
	).Exec()
	assert.NoError(t, err)
}

func seedSpaceMemberRow(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, version) VALUES (?,?,0,1,1)", spaceID, uid,
	).Exec()
	assert.NoError(t, err)
}

func seedUserGroupMemberInvited(t *testing.T, ctx *config.Context, groupNo, uid, inviteUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted, invite_uid) VALUES (?,?,0,1,1,?,0,?)",
		groupNo, uid, fmt.Sprintf("vc-%s-%s", groupNo, uid), inviteUID,
	).Exec()
	assert.NoError(t, err)
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
	assert.Contains(t, body, "\"follow\":0",
		"资料页需要 follow 渲染加好友入口，且走到最小集必然是 0（不能从展示字段复制）, body=%s", body)
	assert.NotContains(t, body, "\"follow\":1", "最小集不得声称已关注")
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
	seedSpace(t, ctx, "sp_a", 1) // 1 = SpaceStatusNormal
	for _, uid := range []string{testutil.UID, "ut_same"} {
		seedSpaceMemberRow(t, ctx, "sp_a", uid)
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

// newMinimalUserDetailResp 是白名单构造：**序列化后**只有 uid/name/follow/robot 四个
// 顶层字段。断言 key 集合而非逐字段取值——后者在给 DTO 新增第五个字段时仍会通过。
func TestNewMinimalUserDetailResp_WhitelistOnly(t *testing.T) {
	full := &UserDetailResp{
		// Follow 刻意给 1：最小集必须按授权决策输出 0，而不是复制这个展示字段。
		UID: "u1", Name: "N", Follow: 1, Robot: 0,
		ShortNo: "SN", Sex: 1, Online: 1, LastOffline: 123, Vercode: "vc",
		SourceDesc: "src", RealName: "张三", RealnameVerified: true, Phone: "13000000000",
	}

	b, err := json.Marshal(newMinimalUserDetailResp(full))
	assert.NoError(t, err)

	var m map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"follow", "name", "robot", "uid"}, keys,
		"最小集只能有这四个顶层字段, got=%s", string(b))
	assert.Contains(t, string(b), "\"follow\":0", "入参 Follow=1 时输出仍须为 0, got=%s", string(b))
	for _, leaked := range []string{"SN", "short_no", "sex", "online", "last_offline", "vercode", "source_desc", "real_name", "张三", "13000000000"} {
		assert.NotContains(t, string(b), leaked)
	}
}

// group_no 是调用方传入的：调用方**不在**该群时，群维度富化必须整体不下发
// （group_member / 外部来源 Space 字段 / 该群的 vercode）。否则任何能看到目标的人
// 都能拿一个自己不在的群号反查该群维度元数据。
func TestUserGet_GroupNoEnrichment_CallerNotMember_Suppressed(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	// 目标可见（同 Space），但调用方不在 ug_outsider 群里。
	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_target", Name: "TargetU", ShortNo: "SNTGT",
	}))
	// 必须建 space 父行：授权判定 JOIN space 校验活性，只插 space_member 会让目标
	// 判为不可见 → 请求直接走最小集、到不了群维度富化块，本用例的负向断言就恒成立
	// （空断言）。这里踩过一次，见 PR #722 review。
	seedSpace(t, ctx, "sp_x", 1)
	for _, uid := range []string{testutil.UID, "ut_target"} {
		seedSpaceMemberRow(t, ctx, "sp_x", uid)
	}
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('ug_outsider','outsider-grp','ut_target',1,1)",
	).Exec()
	assert.NoError(t, err)
	seedUserGroupMemberInvited(t, ctx, "ug_outsider", "ut_target", "ut_inviter") // 只有目标在群里

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/ut_target?group_no=ug_outsider", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", body)
	assert.Contains(t, body, "TargetU", "目标本身可见，主响应不受影响")
	assert.NotContains(t, body, "\"group_member\":{", "调用方非群成员不得拿到群成员元数据, body=%s", body)
	assert.NotContains(t, body, "vc-ug_outsider", "不得泄露该群维度的 vercode")
}

// 调用方确实在该群时，群维度富化正常工作（确保上面的门禁不是把功能整体关掉）。
func TestUserGet_GroupNoEnrichment_CallerIsMember_Works(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_mate", Name: "MateU", ShortNo: "SNMATE",
	}))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('ug_shared','shared-grp',?,1,1)", testutil.UID,
	).Exec()
	assert.NoError(t, err)
	seedUserGroupMember(t, ctx, "ug_shared", testutil.UID)
	seedUserGroupMemberInvited(t, ctx, "ug_shared", "ut_mate", testutil.UID)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/users/ut_mate?group_no=ug_shared", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", body)
	assert.Contains(t, body, "group_member", "同群调用方应能拿到群成员信息, body=%s", body)
}

// 被该群拉黑（status=Blacklist，is_deleted 仍为 0）后，该群不再构成"有共同群"关系
// → 降级为最小集。
//
// 注意本用例的**有效范围**：newUserAuthzServer 为消除全局状态污染注册了自写的 checker
// 替身（modules/user 不能 import modules/group），所以这里验证的是「checker 判否时
// handler 正确降级」这条接线，**不是** ExistCommonGroup 谓词本身。谓词语义（黑名单 /
// 已解散 / 禁用群仍算 / 已退群）由 modules/group/db_common_group_test.go 直接钉在生产
// 实现上，并逐条做过变异验证——曾经在这里误以为覆盖了谓词，见 PR #722 review。
func TestUserGet_CommonGroupButBlacklisted_Minimal(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_blk", Name: "BlacklistedPeer", ShortNo: "SNBLK",
	}))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES ('ug_blk','blk-grp','ut_blk',1,1)",
	).Exec()
	assert.NoError(t, err)
	// 调用方在群里但被拉黑（status=2），目标正常。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES ('ug_blk',?,0,2,1,'vc-blk-self',0)", testutil.UID,
	).Exec()
	assert.NoError(t, err)
	seedUserGroupMember(t, ctx, "ug_blk", "ut_blk")

	w := getUser(s, "ut_blk")
	body := w.Body.String()

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", body)
	assert.Contains(t, body, "BlacklistedPeer", "名字仍可渲染，不裂图")
	assert.NotContains(t, body, "short_no", "被该群拉黑后不应再凭该群拿到完整资料, body=%s", body)
}

// 同属一个**已封禁** Space：不构成授权口径的可达关系 → 降级最小集（P2-1）。
// 封禁只翻 space.status、不清 space_member 行，若判定只查关联表，封禁冻结会失效。
func TestUserGet_BannedCommonSpace_Minimal(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_banned", Name: "BannedSpacePeer", ShortNo: "SNBANNED",
	}))
	seedSpace(t, ctx, "sp_banned", 2) // 2 = SpaceStatusBanned
	seedSpaceMemberRow(t, ctx, "sp_banned", testutil.UID)
	seedSpaceMemberRow(t, ctx, "sp_banned", "ut_banned")

	w := getUser(s, "ut_banned")
	body := w.Body.String()

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", body)
	assert.Contains(t, body, "BannedSpacePeer", "名字仍可渲染")
	assert.Contains(t, body, "\"follow\":0", "授权判定认定无关系，最小集必须回 follow:0, body=%s", body)
	assert.NotContains(t, body, "\"follow\":1",
		"不得复制展示字段：封禁 Space 会让 full.Follow 仍为 1，照抄即泄露该共属事实")
	assert.NotContains(t, body, "short_no", "封禁 Space 不应再构成可达关系, body=%s", body)
	assert.NotContains(t, body, "SNBANNED")
}

// 同属一个**已解散** Space（status=0）同样不构成可达关系。
func TestUserGet_DisbandedCommonSpace_Minimal(t *testing.T) {
	s, ctx := newUserAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, NewService(ctx).AddUser(&AddUserReq{
		UID: "ut_dis", Name: "DisbandedSpacePeer", ShortNo: "SNDIS",
	}))
	seedSpace(t, ctx, "sp_dis", 0) // 0 = SpaceStatusDisbanded
	seedSpaceMemberRow(t, ctx, "sp_dis", testutil.UID)
	seedSpaceMemberRow(t, ctx, "sp_dis", "ut_dis")

	w := getUser(s, "ut_dis")
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "short_no", "已解散 Space 不应构成可达关系")
	assert.Contains(t, w.Body.String(), "\"follow\":0", "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "\"follow\":1")
}
