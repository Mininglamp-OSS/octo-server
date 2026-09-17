package bot_api

// =============================================================================
// bot-thread-archive — POST /v1/bot/groups/:group_no/threads/:short_id/archive
//
// 覆盖：
//   - 成功 + 幂等：creator bot 归档自己创建的子区；GET 复查 status=2；
//     重复调用仍成功（service 已归档短路 + DB CAS 双保险）。
//   - 失败映射：非 creator / 非管理员 → store_failed；子区不存在 → 同一
//     store_failed（与 botDeleteThread 同口径，不向调用方区分原因）。
//   - 路由注册：无 token → 401（authBot 在组上；缺路由会是 gin 404）；
//     AI 容器群 → ErrAITeamContainerProtected（protectAIContainerMutation
//     挂在路由上，且先于 handler）。
//
// 依赖 MySQL + Redis（authBot 查 robot.bot_token；归档版本号走 ctx.GenSeq）。
// 不依赖 WuKongIM：ArchiveThread 只切 thread.status，GET thread 只读 DB，
// 两者都没有 IM 调用。
// =============================================================================

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// btaGroupNo 必须过 thread.IsValidGroupNo（32 位 hex）；三个 short_id 都是
	// 15-20 位纯数字，满足 thread.IsValidShortID 的 snowflake 形状。
	btaRobotID  = "bot_bta_1"
	btaBotToken = "bf_bta_token_1"
	btaGroupNo  = "fedcba9876543210fedcba9876543210"

	btaShortID        = "1489104291682713601" // 成功 + 幂等用例
	btaForeignShortID = "1489104291682713602" // 他人创建 → 无权限
	btaMissingShortID = "1489104291682713999" // 不存在 → not found 映射
)

// btaStoreFailedMsg 是 ErrBotAPIStoreFailed 的 DefaultMessage。testutil 的默认
// renderer 只透出 msg/status，故失败口径断言落在文案上（与
// threads_blacklist_test 的 tblNotGroupMemberMsg 同法）。
var btaStoreFailedMsg = errcode.ErrBotAPIStoreFailed.DefaultMessage

// setupBotArchiveThread wires a real BotAPI on a clean DB with one active bot
// (bot_token auth) that is a NORMAL member of one active group.
func setupBotArchiveThread(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		btaRobotID, "owner_bta", btaBotToken,
	).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status) VALUES (?, ?, ?, 1)",
		btaGroupNo, "g-bta", "owner_bta",
	).Exec()
	require.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, ?, 1)",
		btaGroupNo, btaRobotID, util.GenerUUID(), int(common.GroupMemberStatusNormal),
	).Exec()
	require.NoError(t, err)

	return s.GetRoute(), ctx
}

// insertBotArchiveThread inserts one active thread whose creator is creatorUID.
func insertBotArchiveThread(t *testing.T, ctx *config.Context, shortID, creatorUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO thread (short_id, group_no, name, creator_uid, status, version) VALUES (?, ?, ?, ?, ?, 1)",
		shortID, btaGroupNo, "t-"+shortID, creatorUID, thread.ThreadStatusActive,
	).Exec()
	require.NoError(t, err)
}

func doBotArchivePost(t *testing.T, handler http.Handler, shortID string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost,
		"/v1/bot/groups/"+btaGroupNo+"/threads/"+shortID+"/archive", nil)
	require.NoError(t, err)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+btaBotToken)
	}
	handler.ServeHTTP(w, req)
	return w
}

func botArchiveGetThread(t *testing.T, handler http.Handler, shortID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet,
		"/v1/bot/groups/"+btaGroupNo+"/threads/"+shortID, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+btaBotToken)
	handler.ServeHTTP(w, req)
	return w
}

func botArchiveThreadStatus(t *testing.T, ctx *config.Context, shortID string) int {
	t.Helper()
	var status int
	err := ctx.DB().SelectBySql("SELECT status FROM thread WHERE short_id=?", shortID).LoadOne(&status)
	require.NoError(t, err)
	return status
}

// TestBotArchiveThread_SuccessAndIdempotent：creator bot 归档自己的子区成功；
// GET 复查 status=2（与上线后真机验收同一路径）；重复调用仍成功（幂等）。
func TestBotArchiveThread_SuccessAndIdempotent(t *testing.T) {
	handler, ctx := setupBotArchiveThread(t)
	insertBotArchiveThread(t, ctx, btaShortID, btaRobotID)

	w := doBotArchivePost(t, handler, btaShortID, true)
	require.Equal(t, http.StatusOK, w.Code, "creator bot 归档自己的子区应成功, body=%s", w.Body.String())
	assert.Equal(t, thread.ThreadStatusArchived, botArchiveThreadStatus(t, ctx, btaShortID))

	got := botArchiveGetThread(t, handler, btaShortID)
	require.Equal(t, http.StatusOK, got.Code, "body=%s", got.Body.String())
	var resp struct {
		Status int `json:"status"`
	}
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &resp))
	assert.Equal(t, thread.ThreadStatusArchived, resp.Status, "GET 复查 status=2")

	again := doBotArchivePost(t, handler, btaShortID, true)
	assert.Equal(t, http.StatusOK, again.Code, "重复归档必须幂等成功, body=%s", again.Body.String())
	assert.Equal(t, thread.ThreadStatusArchived, botArchiveThreadStatus(t, ctx, btaShortID))
}

// TestBotArchiveThread_NoPermission：成员 bot 非 creator / 非管理员时归档他人
// 子区被 service 的 canOperate 拒绝；handler 按 botDeleteThread 同口径映射为
// store_failed（不向调用方区分无权限 / 不存在），且子区保持 active。
func TestBotArchiveThread_NoPermission(t *testing.T) {
	handler, ctx := setupBotArchiveThread(t)
	insertBotArchiveThread(t, ctx, btaForeignShortID, "owner_bta_other")

	w := doBotArchivePost(t, handler, btaForeignShortID, true)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"legacy D14: ResponseErrorL wire=400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), btaStoreFailedMsg)
	assert.NotContains(t, w.Body.String(), "permission", "拒绝原因不得回显给 bot")
	assert.Equal(t, thread.ThreadStatusActive, botArchiveThreadStatus(t, ctx, btaForeignShortID),
		"被拒后子区必须保持 active")
}

// TestBotArchiveThread_UnknownThread：子区不存在（service 返回 "thread not
// found"）同样映射为 store_failed —— 与删除一致，调用方按「内部 500 包成 400」
// 的瞬时失败语义处理，不区分不存在与存储失败。
func TestBotArchiveThread_UnknownThread(t *testing.T) {
	handler, _ := setupBotArchiveThread(t)

	w := doBotArchivePost(t, handler, btaMissingShortID, true)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"legacy D14: ResponseErrorL wire=400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), btaStoreFailedMsg)
}

// TestBotArchiveThread_RouteRegistered：路由挂在 botAPI 组内。缺 token 必须先被
// authBot 以 401 拒绝而不是 gin 的 404 —— 后者正是路由没注册时的形状。
func TestBotArchiveThread_RouteRegistered(t *testing.T) {
	handler, _ := setupBotArchiveThread(t)

	w := doBotArchivePost(t, handler, btaShortID, false)
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"无 token 应先被 authBot 拒绝（401）；404 说明路由没挂上, body=%s", w.Body.String())
}

// TestBotArchiveThread_AIContainerMutationRejected：路由带
// protectAIContainerMutation —— AI 会话容器群的归档在 handler 之前被拒，与其它
// 变更类子区路由一致。
func TestBotArchiveThread_AIContainerMutationRejected(t *testing.T) {
	handler, ctx := setupBotArchiveThread(t)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `group` SET purpose=? WHERE group_no=?", aiteam.GroupPurpose, btaGroupNo,
	).Exec()
	require.NoError(t, err)

	w := doBotArchivePost(t, handler, btaShortID, true)
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"legacy D14: ResponseErrorL wire=400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), errcode.ErrAITeamContainerProtected.DefaultMessage,
		"AI 容器必须在 handler 之前被 protectAIContainerMutation 拦截")
}
