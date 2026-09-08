package bot_api

// =============================================================================
// #739 — botCreateThread CreatorUID 传参一视同仁（OBO 代建 → 触发者被自动关注）。
//
// 语义（辉哥 2026-08-11 拍板）：不管有人还是无人触发，都应让 thread 对「本次
// CreateThread 调用的实际发起者」是关注状态。落到 botCreateThread：
//   - 无 on_behalf_of（bot 主动建）           → CreatorUID = bot 自己（robotID）
//   - 带 on_behalf_of 且 OBO 授权通过（人代建）→ CreatorUID = grantor（人类触发者）
//   - OBO 授权不足（无 grant / grantor 已非父群成员）→ fail-closed，绝不回退 robotID
//
// CreatorUID 一旦落对，CreateThread 结尾的 EnsureThreadFollowForCreator 就无条件
// 为该身份补关注行。本测试断言 thread.creator_uid 落库身份，即本次改动的语义面。
// =============================================================================

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oboThreadBotID    = "bot_obo_1"
	oboThreadBotToken = "bf_obo_thread_token_1"
	// 32 位 hex，过 thread.IsValidGroupNo。
	oboThreadGroupNo = "0123456789abcdef0123456789abcd01"
	oboThreadGrantor = "human_trigger_1"
)

// setupBotCreateThreadOBO 建一个可用的建子区场景：一个 BotFather bot（bf_ token）
// 和一个人类触发者都是父群 Normal 成员，父群 status=active。OBO grant / scope 由各
// 测试按需插入，以覆盖「有授权 / 无授权」分支。
func setupBotCreateThreadOBO(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// bot（robot 表 + 群成员）。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		oboThreadBotID, "owner_obo", oboThreadBotToken,
	).Exec()
	require.NoError(t, err)

	// 父群（CreateThread 事务里会 SELECT status FROM `group`）。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, version) VALUES (?, ?, ?, 1, 1)",
		oboThreadGroupNo, "父群", "owner_obo",
	).Exec()
	require.NoError(t, err)

	// bot + 人类触发者都是父群活跃成员。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 1)",
		oboThreadGroupNo, oboThreadBotID, util.GenerUUID(), int(common.GroupMemberStatusNormal),
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 2)",
		oboThreadGroupNo, oboThreadGrantor, util.GenerUUID(), int(common.GroupMemberStatusNormal),
	).Exec()
	require.NoError(t, err)

	return s.GetRoute(), ctx
}

// insertActiveOBOGrant 给 (grantor, bot) 插一条 active=1 且 global_enabled=1 的 grant，
// 与 checkOBO 的 grant 门谓词一致。返回 grant id 供插 scope 用。
func insertActiveOBOGrant(t *testing.T, ctx *config.Context, grantor, bot string) int64 {
	t.Helper()
	res, err := ctx.DB().InsertBySql(
		"INSERT INTO obo_grants (grantor_uid, grantee_bot_uid, mode, global_enabled, active) VALUES (?, ?, 'auto', 1, 1)",
		grantor, bot,
	).Exec()
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// doBotCreateThread POST /v1/bot/groups/:group_no/threads，body 可选带 on_behalf_of。
func doBotCreateThread(t *testing.T, handler http.Handler, name, onBehalfOf string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"name": name}
	if onBehalfOf != "" {
		body["on_behalf_of"] = onBehalfOf
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest(
		"POST",
		"/v1/bot/groups/"+oboThreadGroupNo+"/threads",
		bytes.NewReader([]byte(util.ToJson(body))),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+oboThreadBotToken)
	handler.ServeHTTP(w, req)
	return w
}

// queryThreadCreator 读回刚建 thread 的 creator_uid（父群下唯一 name 命中）。
func queryThreadCreator(t *testing.T, ctx *config.Context, name string) string {
	t.Helper()
	var creator string
	err := ctx.DB().SelectBySql(
		"SELECT creator_uid FROM `thread` WHERE group_no=? AND name=? LIMIT 1",
		oboThreadGroupNo, name,
	).LoadOne(&creator)
	require.NoError(t, err)
	return creator
}

// 场景 C：bot 主动建（无 on_behalf_of）→ CreatorUID 保持 bot 自己。
func TestBotCreateThread_NoOBO_CreatorIsBot(t *testing.T) {
	handler, ctx := setupBotCreateThreadOBO(t)

	w := doBotCreateThread(t, handler, "bot自建子区", "")
	require.Equal(t, http.StatusOK, w.Code, "bot 主动建应成功, body=%s", w.Body.String())

	assert.Equal(t, oboThreadBotID, queryThreadCreator(t, ctx, "bot自建子区"),
		"无 on_behalf_of 时 CreatorUID 应保持 bot 自己")
}

// 场景 A/B：人类经 bot 代建，OBO 授权通过 → CreatorUID = 人类触发者。
// 人类是否历史上关注过父群不影响结果（EnsureThreadFollowForCreator 无条件补行），
// 故此处只需一份 grant + scope 即覆盖辉哥拍板的「一视同仁」两态。
func TestBotCreateThread_WithOBO_CreatorIsHumanTrigger(t *testing.T) {
	handler, ctx := setupBotCreateThreadOBO(t)
	gid := insertActiveOBOGrant(t, ctx, oboThreadGrantor, oboThreadBotID)
	// 显式 scope 覆盖父群频道（enabled=1）；即便不插，父群成员也走 implicit-scope。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO obo_scopes (grant_id, channel_id, channel_type, enabled) VALUES (?, ?, ?, 1)",
		gid, oboThreadGroupNo, common.ChannelTypeGroup.Uint8(),
	).Exec()
	require.NoError(t, err)

	w := doBotCreateThread(t, handler, "代建子区", oboThreadGrantor)
	require.Equal(t, http.StatusOK, w.Code, "OBO 授权通过应成功, body=%s", w.Body.String())

	assert.Equal(t, oboThreadGrantor, queryThreadCreator(t, ctx, "代建子区"),
		"带 on_behalf_of 且授权通过时 CreatorUID 应是人类触发者")
}

// fail-closed 之一：带 on_behalf_of 但无任何 OBO grant → 拒绝，绝不回退 robotID。
func TestBotCreateThread_OBO_NoGrant_Denied(t *testing.T) {
	handler, ctx := setupBotCreateThreadOBO(t)

	w := doBotCreateThread(t, handler, "无授权子区", oboThreadGrantor)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"无 grant 的 OBO 代建必须被拒（legacy D14: ResponseErrorL wire=400）, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIOBONotAuthorized.DefaultMessage,
		"deny 原因应是 obo not authorized")

	var count int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `thread` WHERE group_no=? AND name=?",
		oboThreadGroupNo, "无授权子区",
	).LoadOne(&count))
	assert.Zero(t, count, "OBO 授权不足时不得建出 thread（fail-closed，不静默回退 robotID）")
}

// fail-closed 之二：有 grant，但 grantor 已不是父群成员（TOCTOU：踢群后代建）→ 拒绝。
// 固化 checkOBO 的 grantorCanReadChannel 实时再校验：踢群不可被 OBO 代建绕过。
func TestBotCreateThread_OBO_GrantorNotGroupMember_Denied(t *testing.T) {
	handler, ctx := setupBotCreateThreadOBO(t)
	insertActiveOBOGrant(t, ctx, oboThreadGrantor, oboThreadBotID)
	// grantor 被移出父群（is_deleted=1）——grantorCanReadChannel 应判 false。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?",
		oboThreadGroupNo, oboThreadGrantor,
	).Exec()
	require.NoError(t, err)

	w := doBotCreateThread(t, handler, "踢群后代建", oboThreadGrantor)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"grantor 已非父群成员时 OBO 代建必须被拒, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPIOBONotAuthorized.DefaultMessage)
}
