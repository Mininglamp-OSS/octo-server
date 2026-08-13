package channel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

// channel_get_authz_test.go —— GET /v1/channels/:channel_id/:channel_type 的对象级
// 授权矩阵（task channel-get-object-authz）。该端点历史上无任何鉴权：任意登录用户
// 替换 channel_id 即可读取无权访问的频道详情。

// resetChannelUIDRateLimit 清掉 SharedUIDRateLimiter 为本测试用户建的 per-uid 桶。
// channelGet 现挂了 SharedUIDRateLimiter，桶持久且不被 CleanAllTables 清除，
// 连续用例会累积把 loginUID 限流掉——须在每个用例 setup 时显式重置。
//
// 只删 testutil.UID 这一个 key：限流器是进程级单例、共享同一 Redis keyspace，而
// testutil.UID 跨包通用；用 KEYS ratelimit:uid:* + DEL 全量会删掉并发运行的其它包的
// 桶（KEYS 本身也是 O(keyspace) 阻塞命令）。
func resetChannelUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	_ = rds.Del("ratelimit:uid:" + testutil.UID).Err()
}

func newChannelAuthzServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))
	// 不注入 renderer 时错误信封拿不到 error.code；forbidden/not_found 断言依赖它。
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	resetChannelUIDRateLimit(t, ctx)
	// 登录用户
	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{UID: testutil.UID, Name: "login-user", ShortNo: "SNLOGIN"}))
	return s, ctx
}

func seedGroup(t *testing.T, ctx *config.Context, groupNo, name, notice, creator string) {
	seedGroupStatus(t, ctx, groupNo, name, notice, creator, 1) // 1 = GroupStatusNormal
}

// seedGroupStatus 建群并显式指定 group.status（1=正常, 2=已解散）。
func seedGroupStatus(t *testing.T, ctx *config.Context, groupNo, name, notice, creator string, status int) {
	t.Helper()
	// 反引号：group 是 MySQL 保留字（见 dbr 表名反引号约定）。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, notice, creator, status, version) VALUES (?,?,?,?,?,1)",
		groupNo, name, notice, creator, status,
	).Exec()
	assert.NoError(t, err)
}

func seedMember(t *testing.T, ctx *config.Context, groupNo, uid string, role int) {
	seedMemberDeleted(t, ctx, groupNo, uid, role, 0)
}

// seedMemberDeleted 建群成员并显式指定 is_deleted（1 = 已退群/已移除，行仍保留）。
func seedMemberDeleted(t *testing.T, ctx *config.Context, groupNo, uid string, role, isDeleted int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, version, vercode, is_deleted) VALUES (?,?,?,1,1,?,?)",
		groupNo, uid, role, fmt.Sprintf("vc-%s-%s", groupNo, uid), isDeleted,
	).Exec()
	assert.NoError(t, err)
}

// seedSpace 建 Space 父行（status: 1=正常 / 2=封禁 / 0=已解散）。
// 授权口径的"同 Space"判定会 JOIN space 校验活性（space.HasActiveCommonSpace），
// 所以只插 space_member 不建父行 = 不可达——这正是封禁冻结生效的机制。
func seedSpace(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, version) VALUES (?,?,?,?,1)",
		spaceID, "sp-"+spaceID, testutil.UID, status,
	).Exec()
	assert.NoError(t, err)
}

// seedSpaceMember 把 uid 加入某 Space（成员行 status=1 在籍）。
func seedSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, version) VALUES (?,?,0,1,1)",
		spaceID, uid,
	).Exec()
	assert.NoError(t, err)
}

// setUserCategory 把用户标记为系统/客服账号（AddUserReq 没有 Category 字段）。
func setUserCategory(t *testing.T, ctx *config.Context, uid, category string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql("UPDATE `user` SET category=? WHERE uid=?", category, uid).Exec()
	assert.NoError(t, err)
}

func getChannel(s *server.Server, channelID string, channelType uint8) *httptest.ResponseRecorder {
	return getChannelWithSpace(s, channelID, channelType, "")
}

// getChannelWithSpace 允许指定（或省略）X-Space-Id，用于验证 Space 头不是授权输入。
func getChannelWithSpace(s *server.Server, channelID string, channelType uint8, spaceID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/v1/channels/%s/%d", channelID, channelType), nil)
	req.Header.Set("token", testutil.Token)
	if spaceID != "" {
		req.Header.Set("X-Space-Id", spaceID)
	}
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// GROUP 非成员：不再回群详情，返回 view_forbidden，且不泄露群名/公告。
func TestChannelGet_Group_NonMember_Forbidden(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_authz_nm", "SECRET_GROUP_NAME", "SECRET_NOTICE", "creator_x")
	seedMember(t, ctx, "g_authz_nm", "creator_x", 1) // 仅 creator_x，loginUID 非成员

	w := getChannel(s, "g_authz_nm", common.ChannelTypeGroup.Uint8())

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String()) // D14 兼容期固定 400
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden", "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "SECRET_GROUP_NAME", "非成员不得读到群名")
	assert.NotContains(t, w.Body.String(), "SECRET_NOTICE", "非成员不得读到群公告")
}

// GROUP 成员：正常返回群详情。
func TestChannelGet_Group_Member_OK(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_authz_m", "MEMBER_GROUP_NAME", "", "creator_x")
	seedMember(t, ctx, "g_authz_m", "creator_x", 1)
	seedMember(t, ctx, "g_authz_m", testutil.UID, 0) // loginUID 是成员

	w := getChannel(s, "g_authz_m", common.ChannelTypeGroup.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "MEMBER_GROUP_NAME")
}

// GROUP 不存在：不再 500（nil-panic 修复），且与非成员统一为 forbidden（不泄露存在性）。
func TestChannelGet_Group_NotFound_NoPanicUnified(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	w := getChannel(s, "g_does_not_exist", common.ChannelTypeGroup.Uint8())

	assert.NotEqual(t, http.StatusInternalServerError, w.Code, "群不存在不应再 panic 成 500, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden",
		"不存在与非成员应返回同一 forbidden 响应, body=%s", w.Body.String())
}

// PERSON 无关系：只回最小集（name/logo/robot），不下发 short_no 等身份细节。
func TestChannelGet_Person_NoRelation_Minimal(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_stranger", Name: "StrangerName", ShortNo: "SNSTRANGER",
	}))

	w := getChannel(s, "t_stranger", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "StrangerName", "最小集仍需返回 name 供发送者渲染")
	assert.NotContains(t, w.Body.String(), "short_no", "无关系不得下发 short_no")
	assert.NotContains(t, w.Body.String(), "SNSTRANGER", "无关系不得下发短号值")
	// 最小集是专用 DTO：零值字段也不得出现（follow:0 会被客户端误判为"明确非好友"）。
	assert.NotContains(t, w.Body.String(), "\"follow\"", "最小集不得含 follow 字段, body=%s", w.Body.String())
	// status 与 follow 取舍相反：必须下发。缺失会被三端当成 0="已禁用/封禁"哨兵并写回
	// 本地缓存（Android 隐藏输入框）。值来自调用方自己的拉黑状态，永不为 0。
	assert.Contains(t, w.Body.String(), "\"status\":1",
		"最小集必须下发 status 且为正常值, body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "\"status\":0", "status 绝不能是 0（客户端封禁哨兵）")
	assert.NotContains(t, w.Body.String(), "\"device_flag\"", "最小集不得含 device_flag 字段")
	// 调用方自己的会话设置必须保留：客户端整行写回缓存，省略会把用户自己开的功能关掉。
	for _, kept := range []string{"\"mute\"", "\"stick\"", "\"remark\"", "\"extra\"", "\"chat_pwd_on\""} {
		assert.Contains(t, w.Body.String(), kept,
			"最小集必须保留调用方自身设置 %s, body=%s", kept, w.Body.String())
	}
}

// PERSON 仅共同群（非好友、无共同 Space）：视为可见，返回完整资料（含 short_no）。
// 覆盖 Acceptance「外部群跨 Space 非好友成员 name/logo 仍可渲染」。
func TestChannelGet_Person_CommonGroupOnly_Full(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_cofellow", Name: "CoGroupMate", ShortNo: "SNCOFELLOW",
	}))
	// 两人同属一群，但不是好友、无共同 Space。
	seedGroup(t, ctx, "g_common", "co-group", "", testutil.UID)
	seedMember(t, ctx, "g_common", testutil.UID, 1)
	seedMember(t, ctx, "g_common", "t_cofellow", 0)

	w := getChannel(s, "t_cofellow", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "CoGroupMate")
	assert.Contains(t, w.Body.String(), "short_no", "共同群可达应返回完整资料（含 extra）")
}

// PERSON Bot：即使调用方未加好友也可查看 bot 资料（不降级）。
func TestChannelGet_Person_Bot_Viewable(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_bot", Name: "SomeBot", ShortNo: "SNBOT", Robot: 1,
	}))

	w := getChannel(s, "t_bot", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "SomeBot")
	assert.Contains(t, w.Body.String(), "short_no", "bot 资料应完整可查，不被降级为最小集")
}

// PERSON 真实不存在用户：走统一 i18n not_found，不再是旧 raw error，也不 500。
func TestChannelGet_Person_NotExist_NotFound(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	// 不建该用户。
	w := getChannel(s, "no_such_user_zzz", common.ChannelTypePerson.Uint8())

	assert.NotEqual(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.user.not_found",
		"不存在用户应走统一 not_found i18n 信封, body=%s", w.Body.String())
}

// PERSON 仅共同「已解散」群：不构成有效共同群关系，降级最小集（P1-1 修复）。
func TestChannelGet_Person_CommonGroupDisbanded_Minimal(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_disband", Name: "DisbandMate", ShortNo: "SNDISBAND",
	}))
	// 唯一共同群已解散（status=2）；成员行仍在（disband 只改群状态、成员异步清理）。
	seedGroupStatus(t, ctx, "g_disband", "dg", "", testutil.UID, 2)
	seedMember(t, ctx, "g_disband", testutil.UID, 1)
	seedMember(t, ctx, "g_disband", "t_disband", 0)

	w := getChannel(s, "t_disband", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "DisbandMate")
	assert.NotContains(t, w.Body.String(), "short_no", "已解散群不构成共同群关系，应降级为最小集")
	assert.NotContains(t, w.Body.String(), "SNDISBAND")
}

// GROUP 已退群成员（group_member 行仍在，is_deleted=1）：不得再读群详情。
// 这是线上复现用的退群闭环场景——修复前 groups/{gno} 返回 403 而 channels/{gno}/2
// 仍返回 200 + 完整详情（响应里 quit:1 已表明服务端知道调用方退群了）。
func TestChannelGet_Group_LeftMember_Forbidden(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_left", "LEFT_GROUP_NAME", "LEFT_NOTICE", "creator_x")
	seedMember(t, ctx, "g_left", "creator_x", 1)
	seedMemberDeleted(t, ctx, "g_left", testutil.UID, 0, 1) // 已退群

	w := getChannel(s, "g_left", common.ChannelTypeGroup.Uint8())

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden", "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "LEFT_GROUP_NAME", "退群后不得再读到群名")
	assert.NotContains(t, w.Body.String(), "LEFT_NOTICE", "退群后不得再读到群公告")
	// quit 标记也不该出现——整个详情都不下发，而不是"带 quit:1 的详情"。
	assert.NotContains(t, w.Body.String(), "\"quit\"", "退群后不得下发任何群详情字段")
}

// PERSON 跨 Space（双方各在不同 Space、非好友、无共同群）：只回最小集；
// 且 X-Space-Id 缺失 / 伪造 / 指向自己的 Space 结果完全一致——证明 Space 头不是授权输入。
func TestChannelGet_Person_CrossSpace_Minimal_SpaceHeaderIrrelevant(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_otherspace", Name: "OtherSpaceUser", ShortNo: "SNOTHERSP",
	}))
	seedSpace(t, ctx, "space_a", 1)
	seedSpace(t, ctx, "space_b", 1)
	seedSpaceMember(t, ctx, "space_a", testutil.UID)   // 调用方在 A
	seedSpaceMember(t, ctx, "space_b", "t_otherspace") // 目标在 B

	for _, spaceHeader := range []string{"", "space_a", "space_b", "bogus_space_xyz"} {
		name := spaceHeader
		if name == "" {
			name = "(无头)"
		}
		t.Run(name, func(t *testing.T) {
			resetChannelUIDRateLimit(t, ctx)
			w := getChannelWithSpace(s, "t_otherspace", common.ChannelTypePerson.Uint8(), spaceHeader)

			assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
			assert.Contains(t, w.Body.String(), "OtherSpaceUser", "最小集仍需回 name")
			assert.NotContains(t, w.Body.String(), "short_no", "跨 Space 不得下发 short_no（Space 头无论如何都不能提权）")
			assert.NotContains(t, w.Body.String(), "SNOTHERSP")
			// extra 存在但只含调用方自己的会话设置，不含对方身份键。
			assert.Contains(t, w.Body.String(), "\"chat_pwd_on\"")
			assert.NotContains(t, w.Body.String(), "real_name")
		})
	}
}

// PERSON 同 Space（无好友关系）：可直接聊天，故资料完整可见——锁住"同 Space 即可达"，
// 防止把跨 Space 的收紧误伤成同 Space 也降级。
func TestChannelGet_Person_SameSpace_Full(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_samespace", Name: "SameSpaceUser", ShortNo: "SNSAMESP",
	}))
	seedSpace(t, ctx, "space_a", 1) // 正常 Space
	seedSpaceMember(t, ctx, "space_a", testutil.UID)
	seedSpaceMember(t, ctx, "space_a", "t_samespace")

	w := getChannel(s, "t_samespace", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "SameSpaceUser")
	assert.Contains(t, w.Body.String(), "short_no", "同 Space 可直接聊天，资料应完整下发")
}

// PERSON 系统 Bot：pkg/space.SystemBots 白名单里的账号无需任何关系即可查看完整资料。
// 用 robot=0 的 fileHelper（而非 robot=1 的 u_10000），确保命中 SystemBot 分支而非 Robot
// 分支——CleanAllTables 已清掉迁移预置的 fileHelper，这里重新建号无冲突。
func TestChannelGet_Person_SystemBot_Viewable(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "fileHelper", Name: "SystemBot", ShortNo: "SNSYSBOT",
	}))

	w := getChannel(s, "fileHelper", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "SystemBot")
	assert.Contains(t, w.Body.String(), `"short_no":`, "白名单系统 Bot 资料应完整可查，不被降级为最小集")
}

// 回归（本次收紧的核心）：category=system 但**不在** SystemBots 白名单的账号（如 admin
// 超管号）不再享受系统放行，无可见关系时必须降级为最小集，不得整份下发身份字段。
func TestChannelGet_Person_SystemCategoryNotWhitelisted_Minimal(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_sysacct", Name: "SystemAccount", ShortNo: "SNSYSACCT",
	}))
	setUserCategory(t, ctx, "t_sysacct", user.CategorySystem)

	w := getChannel(s, "t_sysacct", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), `"short_no":`,
		"非白名单的 category=system 账号无关系时必须降级为最小集，不得泄露身份字段")
}

// 同上，锁住被移除判定的另一半：`customerService` 也不再享受身份放行（两端对称，
// 一边漏掉就会静默重开同一越权面）。
func TestChannelGet_Person_CustomerServiceCategoryNotWhitelisted_Minimal(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	assert.NoError(t, user.NewService(ctx).AddUser(&user.AddUserReq{
		UID: "t_csacct", Name: "CustomerServiceAcct", ShortNo: "SNCSACCT",
	}))
	setUserCategory(t, ctx, "t_csacct", user.CategoryCustomerService)

	w := getChannel(s, "t_csacct", common.ChannelTypePerson.Uint8())

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), `"short_no":`,
		"customerService 账号无关系时同样必须降级为最小集")
}

// 最小集的 JSON 线上形状（判定与构造已收口到 modules/channel/service，纯逻辑分支在
// 那里的 profile_visibility_test.go 里锁；这里只钉序列化后的字段集合）。
func TestMinimalChannelResp_JSONShape(t *testing.T) {
	full := &model.ChannelResp{}
	full.Channel.ChannelID = "u1"
	full.Channel.ChannelType = common.ChannelTypePerson.Uint8()
	full.Name = "N"
	full.Logo = "users/u1/avatar"
	full.Follow = 1
	full.Status = 2
	full.Notice = "secret-notice"
	full.Mute = 1
	full.Stick = 1
	full.ShowNick = 1
	full.Receipt = 1
	full.Remark = "my-remark"
	full.Flame = 1
	full.FlameSecond = 30
	full.Extra = map[string]interface{}{
		"short_no": "SNX", "real_name": "张三", "sex": 1,
		"chat_pwd_on": 1, "screenshot": 1, "revoke_remind": 1, "msg_auto_delete": 60,
	}

	b, err := json.Marshal(chservice.NewMinimalChannelResp(full))
	assert.NoError(t, err)

	var m map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// 规则：调用方自身状态全留、对方身份全剥。
	assert.Equal(t, []string{
		"channel", "extra", "flame", "flame_second", "logo", "mute", "name",
		"receipt", "remark", "robot", "show_nick", "status", "stick",
	}, keys, "最小集字段集合已固定, got=%s", string(b))
	assert.Contains(t, string(b), "\"status\":2", "status 必须原样透传（此处入参为 2）")
	assert.Contains(t, string(b), "\"mute\":1", "调用方的免打扰设置必须透传")
	assert.Contains(t, string(b), "\"chat_pwd_on\":1",
		"聊天密码锁必须透传：缺失会让加锁会话直接打开且不提示, got=%s", string(b))
	assert.Contains(t, string(b), "\"msg_auto_delete\":60",
		"消息定时删除必须透传：缺失会让 iOS 新消息不再自动删除, got=%s", string(b))
	// 对方身份/在线态一律不下发；follow 是关系判决，同样不下发。
	for _, leaked := range []string{"short_no", "SNX", "\"follow\"", "notice",
		"real_name", "device_flag", "last_offline", "\"sex\"", "vercode", "\"online\""} {
		assert.NotContains(t, string(b), leaked)
	}
}
