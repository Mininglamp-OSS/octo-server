package bot_api

// =============================================================================
// Issue #352（PR #345 mandatory follow-up）— validateBotGroupAccess 黑名单门禁。
//
// 所有 bot 子区端点（botCreateThread/botListThreads/botGetThread/botDeleteThread/
// botListThreadMembers/botJoinThread/botLeaveThread/botGetThreadMd/
// botUpdateThreadMd）共享 validateBotGroupAccess。该门禁必须用 ExistMemberActive
// （is_deleted=0 AND status=Normal）：被拉黑（status=Blacklist）的 bot 不得再通过
// bot API 读写子区。GROUP 级端点（groups.go）保持 permissive ExistMember 不动。
// =============================================================================

import (
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
	tblRobotID  = "bot_tbl_1"
	tblBotToken = "bf_tbl_token_1"
	// tblGroupNo 必须过 thread.IsValidGroupNo（32 位 hex），否则 400 invalid 短路在
	// 成员门禁之前。
	tblGroupNo = "0123456789abcdef0123456789abcdef"
)

// tblNotGroupMemberMsg 是 ErrBotAPINotGroupMember 的 DefaultMessage。testutil
// 测试服务器使用默认 renderer（只透出 msg/status，不带 i18n code id），所以对
// deny 原因断言落在 message 文案上，区分于参数校验的 "Invalid request."。
var tblNotGroupMemberMsg = errcode.ErrBotAPINotGroupMember.DefaultMessage

// setupBotThreadBlacklist wires a real BotAPI on a clean DB with one active
// BotFather bot (bot_token auth) that is a NORMAL member of one group.
// Per-test the member status is flipped to Blacklist to assert the gate.
func setupBotThreadBlacklist(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		tblRobotID, "owner_tbl", tblBotToken,
	).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 1)",
		tblGroupNo, tblRobotID, util.GenerUUID(), int(common.GroupMemberStatusNormal),
	).Exec()
	require.NoError(t, err)

	return s.GetRoute(), ctx
}

func setBotMemberStatus(t *testing.T, ctx *config.Context, status common.GroupMemberStatus) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(status), tblGroupNo, tblRobotID,
	).Exec()
	require.NoError(t, err)
}

func doBotGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tblBotToken)
	handler.ServeHTTP(w, req)
	return w
}

// TestBotThreadAccess_BlacklistTransition：正常成员 bot 可列子区；被拉黑后
// 同一端点必须被 validateBotGroupAccess 以 not_group_member 拒绝（不再 200）。
func TestBotThreadAccess_BlacklistTransition(t *testing.T) {
	handler, ctx := setupBotThreadBlacklist(t)
	listPath := "/v1/bot/groups/" + tblGroupNo + "/threads"

	w := doBotGet(t, handler, listPath)
	assert.Equal(t, http.StatusOK, w.Code,
		"正常成员 bot 应能列子区, body=%s", w.Body.String())

	setBotMemberStatus(t, ctx, common.GroupMemberStatusBlacklist)
	w = doBotGet(t, handler, listPath)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"被拉黑 bot 必须被拒（legacy D14: ResponseErrorL wire=400）, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), tblNotGroupMemberMsg,
		"deny 原因应是 not_group_member")
}

// TestBotThreadAccess_BlacklistCoversThreadScopedRoute：带 :short_id 的子区端点
// 走 validateBotThreadAccess → validateBotGroupAccess，同样被黑名单拦截
// （成员门禁先于 thread 存在性校验，无需真实子区数据）。
func TestBotThreadAccess_BlacklistCoversThreadScopedRoute(t *testing.T) {
	handler, ctx := setupBotThreadBlacklist(t)
	setBotMemberStatus(t, ctx, common.GroupMemberStatusBlacklist)

	w := doBotGet(t, handler, "/v1/bot/groups/"+tblGroupNo+"/threads/1489104291682713601/md")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"被拉黑 bot 读子区 GROUP.md 必须被拒, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), tblNotGroupMemberMsg)
}

// TestBotThreadAccess_RemovedMemberStillDenied：is_deleted=1（被移出）保持原有
// 拒绝语义不被本次收紧破坏（纵深防御回归）。
func TestBotThreadAccess_RemovedMemberStillDenied(t *testing.T) {
	handler, ctx := setupBotThreadBlacklist(t)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET is_deleted=1 WHERE group_no=? AND uid=?",
		tblGroupNo, tblRobotID,
	).Exec()
	require.NoError(t, err)

	w := doBotGet(t, handler, "/v1/bot/groups/"+tblGroupNo+"/threads")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"被移出的 bot 必须被拒, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), tblNotGroupMemberMsg)
}
