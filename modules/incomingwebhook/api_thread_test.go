package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Mininglamp-OSS/octo-server/modules/thread"
)

// seedThread 直接在 `thread` 表插一行子区（不走 thread.CreateThread，避免其 WuKongIM
// 频道创建调用）——webhook 的子区绑定逻辑只经 GetThread 读 `thread` 表，与 IM 无关，故
// 直接 seed 行即可隔离测试、无需 WuKongIM。status: 1=活跃 / 2=归档（thread/const.go）。
// short_id 用真实 snowflake（ctx.UserIDGen），保证通过 thread.IsValidShortID 校验。
func seedThread(t *testing.T, ctx *config.Context, groupNo string, status int) string {
	t.Helper()
	shortID := fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO thread(short_id, group_no, name, creator_uid, status, version) VALUES(?, ?, ?, ?, ?, 1)",
		shortID, groupNo, "webhook-thread", testutil.UID, status).Exec()
	require.NoErrorf(t, err, "seed thread row")
	return shortID
}

// newSnowflakeID 生成一个格式合法但【未落库】的 short_id，用于「子区不存在」场景。
func newSnowflakeID(ctx *config.Context) string {
	return fmt.Sprintf("%d", ctx.UserIDGen.Generate().Int64())
}

func threadWebhooksPath(groupNo, shortID string) string {
	return fmt.Sprintf("/v1/groups/%s/threads/%s/incoming-webhooks", groupNo, shortID)
}

// TestThread_Create_HappyPath 子区挂载面创建 webhook：落库绑定子区、回显 thread_short_id，
// 且仍下发一个与群 webhook 同形态的推送 URL（推送方零适配）。
func TestThread_Create_HappyPath(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)
	shortID := seedThread(t, ctx, groupNo, thread.ThreadStatusActive)

	w := do(handler, authReq("POST", threadWebhooksPath(groupNo, shortID), map[string]interface{}{
		"name": "Thread CI Bot",
	}))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	res := parseJSON(t, w)
	assert.NotEmpty(t, res["webhook_id"])
	assert.Equalf(t, shortID, res["thread_short_id"], "must echo the bound thread short_id; body: %s", w.Body.String())
	url, _ := res["url"].(string)
	assert.Truef(t, strings.HasPrefix(url, "/v1/incoming-webhooks/"), "push URL unchanged (no thread in URL): %s", url)
}

// TestThread_Create_RejectMissingThread 绑定到不存在的子区（格式合法的 snowflake 但无此
// 子区）→ 404 mgmt_thread_not_found。
func TestThread_Create_RejectMissingThread(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)

	w := do(handler, authReq("POST", threadWebhooksPath(groupNo, newSnowflakeID(ctx)), map[string]interface{}{
		"name": "x",
	}))
	assert.Equalf(t, http.StatusNotFound, w.Code, "missing thread must 404; body: %s", w.Body.String())
}

// TestThread_Create_RejectInvalidShortID short_id 格式非法 → 400（reason=short_id）。
func TestThread_Create_RejectInvalidShortID(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)

	w := do(handler, authReq("POST", threadWebhooksPath(groupNo, "not-a-snowflake"), map[string]interface{}{
		"name": "x",
	}))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "invalid short_id must 400; body: %s", w.Body.String())
}

// TestThread_Create_RejectArchivedThread 绑定到【已归档】子区 → 404：绑定只接受活跃子区
// （归档后再推送会自动解档是另一回事——但创建时刻意要求活跃，避免绑定到休眠子区）。
func TestThread_Create_RejectArchivedThread(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)
	shortID := seedThread(t, ctx, groupNo, thread.ThreadStatusArchived)

	w := do(handler, authReq("POST", threadWebhooksPath(groupNo, shortID), map[string]interface{}{
		"name": "x",
	}))
	assert.Equalf(t, http.StatusNotFound, w.Code, "archived thread must reject bind; body: %s", w.Body.String())
}

// TestThread_Create_RejectThreadOfOtherGroup 子区属于另一个群 → 404（GetThread 按
// group_no+short_id 双条件查询，跨群天然查不到）。
func TestThread_Create_RejectThreadOfOtherGroup(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)
	// 子区挂在另一个群下；用当前群的路径去绑定它应当 404。
	otherGroup := "g_" + strings.ReplaceAll(newSnowflakeID(ctx), "-", "")[:12]
	shortID := seedThread(t, ctx, otherGroup, thread.ThreadStatusActive)

	w := do(handler, authReq("POST", threadWebhooksPath(groupNo, shortID), map[string]interface{}{
		"name": "x",
	}))
	assert.Equalf(t, http.StatusNotFound, w.Code, "thread under another group must 404; body: %s", w.Body.String())
}

// TestThread_ScopeIsolation 群面与子区面各管各的：群 list 只见群 webhook、子区 list 只见
// 该子区 webhook；且 webhook 不能跨作用域管理（群面删子区 webhook → 404，反之亦然）。
func TestThread_ScopeIsolation(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)
	shortID := seedThread(t, ctx, groupNo, thread.ThreadStatusActive)

	groupPath := fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo)
	threadPath := threadWebhooksPath(groupNo, shortID)

	// 群 webhook G
	gw := do(handler, authReq("POST", groupPath, map[string]interface{}{"name": "G"}))
	require.Equalf(t, http.StatusOK, gw.Code, "body: %s", gw.Body.String())
	gID := parseJSON(t, gw)["webhook_id"].(string)

	// 子区 webhook T
	tw := do(handler, authReq("POST", threadPath, map[string]interface{}{"name": "T"}))
	require.Equalf(t, http.StatusOK, tw.Code, "body: %s", tw.Body.String())
	tID := parseJSON(t, tw)["webhook_id"].(string)

	// 群 list 只见 G；子区 list 只见 T
	assert.ElementsMatch(t, []string{gID}, listWebhookIDs(t, handler, groupPath), "group list must show only the group webhook")
	assert.ElementsMatch(t, []string{tID}, listWebhookIDs(t, handler, threadPath), "thread list must show only the thread webhook")

	// 跨作用域管理一律 404
	assert.Equalf(t, http.StatusNotFound, do(handler, authReq("DELETE", groupPath+"/"+tID, nil)).Code,
		"thread webhook must NOT be deletable via the group path")
	assert.Equalf(t, http.StatusNotFound, do(handler, authReq("DELETE", threadPath+"/"+gID, nil)).Code,
		"group webhook must NOT be deletable via the thread path")
}

// listWebhookIDs GETs a management list path and returns the webhook_id set.
func listWebhookIDs(t *testing.T, handler http.Handler, path string) []string {
	t.Helper()
	w := do(handler, authReq("GET", path, nil))
	require.Equalf(t, http.StatusOK, w.Code, "list body: %s", w.Body.String())
	raw, _ := parseJSON(t, w)["list"].([]interface{})
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]interface{}); ok {
			if id, ok := m["webhook_id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// TestThread_Push_Delivers 子区 webhook 端到端推送：建一个【真实】子区（CreateThread 会
// 在 WuKongIM 建好子区频道 + 订阅者），再用与群 webhook 完全相同的 URL/body 推送 → 200。
// 投递目标频道由 targetChannel() 决定（频道映射本身另由 TestTargetChannel 纯单测钉住），
// 此处验证整条 push 流水线对子区 webhook 端到端打通、消息真正进了子区频道。
//
// 需要【支持社区话题(CommunityTopic)频道】的 WuKongIM——CI 固定的 v2.2.4 支持；本地可装
// 的 v1.2.6 不支持该频道模型（会以「父类频道不存在」400 拒绝），故按 /health 探测能力位，
// 不具备时跳过（见 requireThreadCapableIM）。群路径的 push e2e 不受影响、照常覆盖。
func TestThread_Push_Delivers(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	defer testutil.CleanAllTables(ctx)
	requireThreadCapableIM(t)

	// 建真实子区：CreateThread 会创建 WuKongIM 子区频道（订阅者=父群成员），使后续推送可达。
	tr, err := thread.NewService(ctx).CreateThread(&thread.CreateThreadReq{
		GroupNo: groupNo, Name: "push-thread", CreatorUID: testutil.UID, CreatorName: "tester",
	})
	require.NoErrorf(t, err, "create real thread (needs thread-capable WuKongIM)")

	cw := do(handler, authReq("POST", threadWebhooksPath(groupNo, tr.ShortID), map[string]interface{}{"name": "T"}))
	require.Equalf(t, http.StatusOK, cw.Code, "create body: %s", cw.Body.String())
	pushURL, _ := parseJSON(t, cw)["url"].(string)
	require.True(t, strings.HasPrefix(pushURL, "/v1/incoming-webhooks/"), "url: %s", pushURL)

	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hello thread"}`)))
	assert.Equalf(t, http.StatusOK, pw.Code, "thread push must succeed; body: %s", pw.Body.String())
	assert.Contains(t, pw.Body.String(), "message_id")
}

// requireThreadCapableIM 跳过 e2e，除非本地 WuKongIM 支持社区话题频道。判据：GET /health
// 返回 2xx——CI 固定的 v2.2.4 提供 /health（CI healthcheck 即先探 /health）；旧版 v1.2.6
// 没有 /health（404，但 /varz 会 200，故不能用 /varz 区分）。连不上或非 2xx 一律跳过。
func requireThreadCapableIM(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://127.0.0.1:5001/health")
	if err != nil {
		t.Skip("WuKongIM not reachable at 127.0.0.1:5001; thread push e2e runs in CI (v2.2.4)")
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Skipf("WuKongIM at 127.0.0.1:5001 lacks CommunityTopic support (GET /health=%d; need CI-pinned v2.2.4); thread push e2e runs in CI", resp.StatusCode)
	}
}
