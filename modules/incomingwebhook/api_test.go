package incomingwebhook_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// 该模块自身需注册以触发其 SQL 迁移
	_ "github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	// 迁移依赖链：以下模块的 SQL 迁移会修改本模块依赖的 group/group_member/user 表，
	// 缺失任何一个都会导致 module.Setup 在跨模块 ALTER 时报错。
	// 详见 memory: skill_service_test.md「迁移顺序陷阱」。
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	modulescommon "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

// TestMain 准备 common.Setup 所需的 master key（必须 32 字节）。CI 通过 ci.yml
// 全局环境变量已经注入；本地直接 go test 也能跑通，无需额外设置。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		os.Setenv("OCTO_MASTER_KEY", "12345678901234567890123456789012")
	}
	os.Exit(m.Run())
}

// 在 _test 包下没法直接引用未导出类型，因此推送请求体在测试里独立定义。
type pushReq struct {
	Content  string `json:"content"`
	Username string `json:"username,omitempty"`
}

// setupTestEnv 启动测试服务并准备好群 + 群主成员。
func setupTestEnv(t *testing.T) (http.Handler, *config.Context, string) {
	s, ctx := testutil.NewTestServer()
	groupNo := "g_" + util.GenerUUID()[:12]

	// 群记录（最简：name + space_id）
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, ?, 1, '')",
		groupNo, "test").Exec()
	assert.NoError(t, err)

	// 群主成员
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 1, 1, 0, 1)",
		groupNo, testutil.UID).Exec()
	assert.NoError(t, err)

	// 管理类路由挂了 SharedUIDRateLimiter（per-login-user 桶 ratelimit:uid:{uid}），
	// 该桶在 Redis 持久、CleanAllTables 不清，跨测试 / -count=N 累积会撞 burst 触发
	// 429。每次 setup 清桶，保证每个测试从满桶开始（参考 category 测试同名 helper）。
	resetUIDRateLimit(t, ctx)

	return s.GetRoute(), ctx, groupNo
}

// resetUIDRateLimit 清空 per-uid 令牌桶键（ratelimit:uid:{uid}），让后续 HTTP
// 调用从满桶开始。SharedUIDRateLimiter 的桶不随 CleanAllTables 清理。
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rdsClient := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rdsClient.Close()
	keys, err := rdsClient.Keys("ratelimit:uid:*").Result()
	if err == nil && len(keys) > 0 {
		_ = rdsClient.Del(keys...).Err()
	}
}

func authReq(method, path string, body interface{}) *http.Request {
	var r *bytes.Reader
	if body != nil {
		r = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", testutil.Token)
	return req
}

func anonReq(method, path string, body []byte) *http.Request {
	req, _ := http.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// anonReqIP is anonReq with a fixed client IP, so tests can exercise the per-IP
// auth-failure budget. Sets X-Real-Ip (the trusted, top-priority source that
// clientIP() reads), matching how a real edge proxy attributes the caller.
func anonReqIP(method, path string, body []byte, ip string) *http.Request {
	req := anonReq(method, path, body)
	req.RemoteAddr = ip + ":12345"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

// resetIPFailBucket clears the per-IP auth-failure token bucket so a test starts
// from a full budget; the bucket persists in Redis across runs (TTL) and is not
// cleared by CleanAllTables (mirrors resetUIDRateLimit).
func resetIPFailBucket(t *testing.T, ctx *config.Context, ip string) {
	t.Helper()
	delRedisKey(t, ctx, "ratelimit:incoming_webhook_ipfail:"+ip)
}

// resetStrictIPBucket clears the per-IP request limiter bucket
// (StrictIPRateLimitMiddleware, tag "incoming_webhook") for the same reason.
func resetStrictIPBucket(t *testing.T, ctx *config.Context, ip string) {
	t.Helper()
	delRedisKey(t, ctx, "ratelimit:strict:incoming_webhook:"+ip)
}

func delRedisKey(t *testing.T, ctx *config.Context, key string) {
	t.Helper()
	rdsClient := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rdsClient.Close()
	_ = rdsClient.Del(key).Err()
}

func do(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func parseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	var m map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &m)
	assert.NoErrorf(t, err, "body: %s", w.Body.String())
	return m
}

// ============================================================
// 创建
// ============================================================

func TestCreate_HappyPath(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "GitHub Bot",
	}))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	res := parseJSON(t, w)
	assert.NotEmpty(t, res["webhook_id"])
	assert.NotEmpty(t, res["token"])
	assert.NotEmpty(t, res["url"])
	url, _ := res["url"].(string)
	assert.True(t, strings.HasPrefix(url, "/v1/incoming-webhooks/"))
	// created_at 必须由 insertWithQuota 回填，否则会以 epoch(0) 返回给客户端
	createdAt, _ := res["created_at"].(float64)
	assert.Greater(t, int64(createdAt), int64(0), "created_at must be populated, not zero/epoch")
}

func TestCreate_RejectsEmptyName(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{}))
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestCreate_NonAdminForbidden(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	// 把当前用户降级为普通成员
	_, err := ctx.DB().UpdateBySql("UPDATE group_member SET role=0 WHERE group_no=? AND uid=?", groupNo, testutil.UID).Exec()
	assert.NoError(t, err)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestFeatureToggle_DisabledStopsPushAndMgmtWrites 验证总开关
// (system_setting incomingwebhook.enabled=0) 关闭后：push 返回 404、管理写操作
// (create) 返回 403，而 list 只读仍可用。直接 DB 写 + Reload 共享快照，模拟
// manager 写路径但避开 admin token（语义同 space.TestCreateSpace_DisabledBySystemSetting）。
func TestFeatureToggle_DisabledStopsPushAndMgmtWrites(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	t.Setenv("DM_INCOMINGWEBHOOK_ENABLED", "") // 证明开关纯由 DB 驱动

	// 1) 功能开启时先建一个 webhook，拿到推送 URL。
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "toggle-wh",
	}))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	pushURL, _ := parseJSON(t, w)["url"].(string)
	assert.True(t, strings.HasPrefix(pushURL, "/v1/incoming-webhooks/"), "url: %s", pushURL)

	// 2) 关闭总开关：DB 写入 incomingwebhook.enabled=0 + Reload 共享快照。
	_, err := ctx.DB().InsertInto("system_setting").
		Pair("category", "incomingwebhook").
		Pair("key_name", "enabled").
		Pair("value", "0").
		Pair("value_type", "bool").
		Pair("description", "").
		Exec()
	assert.NoError(t, err)
	settings := modulescommon.EnsureSystemSettings(ctx)
	assert.NoError(t, settings.Reload())
	defer func() {
		_, _ = ctx.DB().DeleteFrom("system_setting").
			Where("category=?", "incomingwebhook").Exec()
		_ = settings.Reload()
	}()

	// 3) push → 404（功能全局停用，gate 在限流链最前短路）。
	pw := do(handler, anonReq("POST", pushURL, []byte(`{"content":"hi"}`)))
	assert.Equalf(t, http.StatusNotFound, pw.Code, "push body: %s", pw.Body.String())

	// 4) 管理写操作 → 403。
	cw := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "blocked",
	}))
	assert.Equalf(t, http.StatusForbidden, cw.Code, "create body: %s", cw.Body.String())

	// 5) list 只读仍可用 → 200，且必须仍只有最初那 1 个 webhook —— 证明被拒的 create
	//    没有真正落库（requireMgmtEnabled 必须 c.Abort()，否则 403 之后 create handler
	//    仍会执行、把 "blocked" 写进去，列表会变成 2 条）。
	lw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	assert.Equalf(t, http.StatusOK, lw.Code, "list body: %s", lw.Body.String())
	list, _ := parseJSON(t, lw)["list"].([]interface{})
	require.Lenf(t, list, 1, "disabled create must not insert a row; list=%s", lw.Body.String())
	first, _ := list[0].(map[string]interface{})
	assert.Equal(t, "toggle-wh", first["name"], "the only webhook must be the original, not the blocked create")
}

// ============================================================
// 列表 / 删除 / 重置
// ============================================================

func TestListAndDelete(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)

	// 创建 2 个
	for i := 0; i < 2; i++ {
		w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
			"name": fmt.Sprintf("wh-%d", i),
		}))
		assert.Equal(t, http.StatusOK, w.Code)
	}

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	assert.Equal(t, http.StatusOK, w.Code)
	res := parseJSON(t, w)
	list, _ := res["list"].([]interface{})
	assert.Equal(t, 2, len(list))

	// 删一个
	first := list[0].(map[string]interface{})
	whID := first["webhook_id"].(string)
	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	res = parseJSON(t, w)
	list, _ = res["list"].([]interface{})
	assert.Equal(t, 1, len(list))
}

// TestDelete_FreesQuota 软删除（#254）后释放每群配额：填满→删一个→可再建一个。
func TestDelete_FreesQuota(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_MAX_PER_GROUP", "2")
	handler, _, groupNo := setupTestEnv(t)

	ids := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
			"name": fmt.Sprintf("wh-%d", i),
		}))
		assert.Equalf(t, http.StatusOK, w.Code, "i=%d body=%s", i, w.Body.String())
		ids = append(ids, parseJSON(t, w)["webhook_id"].(string))
	}

	// 配额满：第三个 409。
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "overflow",
	}))
	assert.Equalf(t, http.StatusConflict, w.Code, "quota must be full: %s", w.Body.String())

	// 删一个释放配额。
	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, ids[0]), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	// 现在可再建一个。
	w = do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "after-delete",
	}))
	assert.Equalf(t, http.StatusOK, w.Code, "soft-delete must free quota: %s", w.Body.String())
}

// TestDelete_CannotRevive 软删除（#254）后该 webhook 不可被复活/再操作：
// update / 再次 delete / regenerate 都必须 404。
func TestDelete_CannotRevive(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	whID := parseJSON(t, w)["webhook_id"].(string)

	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	// update 改名必须 404（不可对已删除资源操作）。
	name := "revived"
	w = do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"name": name}))
	assert.Equalf(t, http.StatusNotFound, w.Code, "update on soft-deleted webhook must 404: %s", w.Body.String())

	// update 重新启用必须 404（不可复活）。
	statusOne := 1
	w = do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"status": statusOne}))
	assert.Equalf(t, http.StatusNotFound, w.Code, "re-enable soft-deleted webhook must 404: %s", w.Body.String())

	// 再次 delete 必须 404（已删除视为不存在）。
	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equalf(t, http.StatusNotFound, w.Code, "re-delete soft-deleted webhook must 404: %s", w.Body.String())

	// regenerate 必须 404（不可给已删除 webhook 颁发新 token）。
	w = do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
	assert.Equalf(t, http.StatusNotFound, w.Code, "regenerate soft-deleted webhook must 404: %s", w.Body.String())
}

// TestDelete_PushFails 软删除（#254）后用原 token 推送必须 401（status 闸自动失效）。
func TestDelete_PushFails(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equalf(t, http.StatusUnauthorized, w.Code, "push with deleted webhook token must 401: %s", w.Body.String())
}

// TestPush_SoftDeletedWebhookSpendsIPBudget locks the P2 refinement: pushing to
// a soft-deleted webhook_id is a scan/abuse signal (no legit caller pushes to a
// deleted URL), so it spends the IP failure budget and gets gated — closing the
// "leaked deleted URL hammered forever, never gated" gap.
func TestPush_SoftDeletedWebhookSpendsIPBudget(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_RPS", "0.01")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "2")

	handler, ctx, groupNo := setupTestEnv(t)
	const ip = "203.0.113.77"
	resetIPFailBucket(t, ctx, ip)
	resetStrictIPBucket(t, ctx, ip)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)
	w = do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)
	// Two pushes to the deleted webhook spend the budget (401 each).
	for i := 0; i < 2; i++ {
		w = do(handler, anonReqIP("POST", url, body, ip))
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "attempt %d should be 401; body=%s", i, w.Body.String())
	}
	// Budget exhausted → the gate now rejects with 429.
	w = do(handler, anonReqIP("POST", url, body, ip))
	assert.Equalf(t, http.StatusTooManyRequests, w.Code,
		"a hammered soft-deleted webhook must spend the IP budget and get gated; body=%s", w.Body.String())
}

// TestPush_DisabledWebhookKeepsIPGrace is the other half of the asymmetry: a
// merely DISABLED webhook (a legit caller may still hold a valid token in the
// window right after an admin disables it) does NOT spend the budget, so it is
// never gated — only ever a uniform 401.
func TestPush_DisabledWebhookKeepsIPGrace(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_RPS", "0.01")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "2")

	handler, ctx, groupNo := setupTestEnv(t)
	const ip = "203.0.113.88"
	resetIPFailBucket(t, ctx, ip)
	resetStrictIPBucket(t, ctx, ip)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)
	// Disable (status=0), not delete.
	_, err := ctx.DB().UpdateBySql("UPDATE incoming_webhook SET status=0 WHERE webhook_id=?", whID).Exec()
	assert.NoError(t, err)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)
	// Even past the failure burst of 2, a disabled webhook never gates — always 401.
	for i := 0; i < 4; i++ {
		w = do(handler, anonReqIP("POST", url, body, ip))
		assert.Equalf(t, http.StatusUnauthorized, w.Code,
			"disabled webhook must keep its grace (never 429); attempt %d body=%s", i, w.Body.String())
	}
}

// TestDelete_ConcurrentUpdate_NoRevive 端到端回归 TOCTOU 复活漏洞（#254 follow-up）：
// DELETE 与多个 PUT status=1 并发时，删除必须最终生效——webhook 不得复活进列表，原
// token 不得再推送。修复后此性质与调度无关恒成立（条件 updateFields 永不写已删除行），
// 故为稳定断言；修复前命中竞态窗口时会失败。配合 -race 一并探测数据竞争。
func TestDelete_ConcurrentUpdate_NoRevive(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	}()
	statusOne := 1
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
				map[string]interface{}{"status": statusOne}))
		}()
	}
	wg.Wait()

	// 删除必须最终生效：列表不含该 webhook。
	w = do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	res := parseJSON(t, w)
	list, _ := res["list"].([]interface{})
	assert.Emptyf(t, list, "deleted webhook must not be revived into the management list; list=%v", list)

	// 原 token 必须保持失效。
	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equalf(t, http.StatusUnauthorized, w.Code, "deleted webhook token must stay revoked, body=%s", w.Body.String())
}

// TestDelete_ConcurrentRegenerate_NoRevive 端到端回归 DELETE 与 regenerate 并发：
// regenerate 有自己的"回读 + 返回新 token"路径，必须同样不能复活已删除 webhook。无论
// regenerate 是否赢得竞态轮换 token，删除最终生效——webhook 不在列表，且最初的 token
// 始终失效（要么被某次 regenerate 换掉，要么因 status=2 失效）。
func TestDelete_ConcurrentRegenerate_NoRevive(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
		}()
	}
	wg.Wait()

	// 删除必须最终生效：列表不含该 webhook。
	w = do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), nil))
	res := parseJSON(t, w)
	list, _ := res["list"].([]interface{})
	assert.Emptyf(t, list, "deleted webhook must not be revived by concurrent regenerate; list=%v", list)

	// 最初的 token 必须保持失效。
	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equalf(t, http.StatusUnauthorized, w.Code, "original token must stay revoked, body=%s", w.Body.String())
}

// TestUpdate_DBError_Returns5xx / TestRegenerate_DBError_Returns5xx 守护本 PR 的语义
// 变更：update / regenerate 在底层表不可用时按 5xx 失败，绝不退回旧的 stale-200 兜底
// （那会谎报成功）。
//
// 实现说明：通过 RENAME 掉 incoming_webhook 表注入查询故障。黑盒下故障会先命中
// queryManageable 的前置读（同样返回 5xx），无法单独触发"写后回读"那一支；但本测试仍
// 锁定了总契约——这些管理写端点在 DB 不可用时一律 5xx，不会返回 stale-200。
func TestUpdate_DBError_Returns5xx(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	whID := parseJSON(t, w)["webhook_id"].(string)

	_, err := ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook TO incoming_webhook_bak").Exec()
	assert.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook_bak TO incoming_webhook").Exec()
	}()

	name := "x2"
	w = do(handler, authReq("PUT", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"name": name}))
	assert.GreaterOrEqualf(t, w.Code, 500, "update must fail as 5xx when the table is unavailable, never stale-200; code=%d body=%s", w.Code, w.Body.String())
}

func TestRegenerate_DBError_Returns5xx(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	whID := parseJSON(t, w)["webhook_id"].(string)

	_, err := ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook TO incoming_webhook_bak").Exec()
	assert.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook_bak TO incoming_webhook").Exec()
	}()

	w = do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
	assert.GreaterOrEqualf(t, w.Code, 500, "regenerate must fail as 5xx when the table is unavailable, never stale-200; code=%d body=%s", w.Code, w.Body.String())
}

func TestRegenerate_RotatesToken(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	oldToken := created["token"].(string)

	w = do(handler, authReq("POST",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)
	res := parseJSON(t, w)
	newToken := res["token"].(string)
	assert.NotEqual(t, oldToken, newToken)
}

// ============================================================
// 推送端点鉴权
// ============================================================

func TestPush_RejectsBadToken(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)

	// 错 token
	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/wrong-token", whID), body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPush_RejectsDisabledWebhook(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	// 禁用（status=0 即 statusDisabled）
	_, err := ctx.DB().UpdateBySql("UPDATE incoming_webhook SET status=0 WHERE webhook_id=?", whID).Exec()
	assert.NoError(t, err)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPush_RejectsTooLargeBody(t *testing.T) {
	// 用 env 把上限收紧到 1KB，让测试与运行时配置共用同一函数（maxBytes()），
	// 避免在测试里硬编码 8KB 与生产默认值漂移。
	t.Setenv("DM_INCOMINGWEBHOOK_MAX_BYTES", "1024")

	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	big := strings.Repeat("A", 1024+100)
	body, _ := json.Marshal(pushReq{Content: big})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestPush_RejectsEmptyContent(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	body, _ := json.Marshal(pushReq{Content: "   "})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPush_PerWebhookRateLimitTriggers429 验证 per-webhook 令牌桶真的连通 Redis：
// 把 burst 收紧到 2、rps 收紧到 0.01（10s 补 1 个），连发 3 次，第 3 次必拒。
// 前两次允许的请求可能因 WuKongIM 投递失败返回 502；这里只断言限流分支。
func TestPush_PerWebhookRateLimitTriggers429(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_BURST", "2")
	t.Setenv("DM_INCOMINGWEBHOOK_RPS", "0.01")

	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)

	// 前两次消耗 burst（结果不强求 200，可能 200/502，关键是没被限流）
	for i := 0; i < 2; i++ {
		w = do(handler, anonReq("POST", url, body))
		assert.NotEqualf(t, http.StatusTooManyRequests, w.Code, "i=%d body=%s", i, w.Body.String())
	}
	// 第三次必被限流
	w = do(handler, anonReq("POST", url, body))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// TestPush_PerWebhookRateLimitIsPerWebhook 验证一个 webhook 被打满不影响另一个 webhook
// （限流键空间按 webhook_id 隔离）。
func TestPush_PerWebhookRateLimitIsPerWebhook(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_BURST", "1")
	t.Setenv("DM_INCOMINGWEBHOOK_RPS", "0.01")

	handler, _, groupNo := setupTestEnv(t)
	create := func(name string) (string, string) {
		w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
			"name": name,
		}))
		c := parseJSON(t, w)
		return c["webhook_id"].(string), c["token"].(string)
	}
	wh1, tk1 := create("a")
	wh2, tk2 := create("b")
	body, _ := json.Marshal(pushReq{Content: "hi"})

	// 把 wh1 的桶耗尽
	do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", wh1, tk1), body))
	w := do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", wh1, tk1), body))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "wh1 second call must hit 429")

	// wh2 不应受影响（burst=1 第一次请求应当通过限流分支，可能 200/502）
	w = do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", wh2, tk2), body))
	assert.NotEqualf(t, http.StatusTooManyRequests, w.Code,
		"wh2 must not be limited by wh1's bucket; got %d body=%s", w.Code, w.Body.String())
}

// TestPush_LocalFloorRateLimits_RedisIndependent verifies the process-local
// push floor caps the public endpoint on its own. The Redis-backed per-webhook
// and per-IP limiters are left at their generous defaults (5/10 and 30/60), so
// they would NOT reject these few calls — the 429 therefore comes from the
// in-memory floor, which is exactly the protection that survives a Redis outage
// (where both Redis limiters fail open). Floor burst is tightened to 2 via env.
func TestPush_LocalFloorRateLimits_RedisIndependent(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_LOCAL_BURST", "2")
	t.Setenv("DM_INCOMINGWEBHOOK_LOCAL_RPS", "0.01") // ~never refills within the test

	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)

	// First two consume the floor burst (not rate-limited; result may be 200/502
	// depending on WuKongIM, the point is only that they pass the floor).
	for i := 0; i < 2; i++ {
		w = do(handler, anonReq("POST", url, body))
		assert.NotEqualf(t, http.StatusTooManyRequests, w.Code, "i=%d body=%s", i, w.Body.String())
	}
	// Third exceeds the floor → 429, despite the Redis per-webhook limiter (10
	// burst) still having capacity. Proves the floor is an independent ceiling.
	w = do(handler, anonReq("POST", url, body))
	assert.Equalf(t, http.StatusTooManyRequests, w.Code, "local floor must 429 after its burst; body=%s", w.Body.String())
}

// TestPush_ValidPushesNotIPLimited is the core of "only failures count toward
// IP": with a tiny IP failure budget but a generous per-webhook limit, a stream
// of VALID pushes from one fixed IP is never IP-throttled, because valid pushes
// don't spend the IP budget. This is the fixed/shared server-IP case.
func TestPush_ValidPushesNotIPLimited(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_RPS", "0.01")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "1")
	// generous per-webhook bucket so the per-Key limiter doesn't 429 either
	t.Setenv("DM_INCOMINGWEBHOOK_RPS", "1000")
	t.Setenv("DM_INCOMINGWEBHOOK_BURST", "1000")

	handler, ctx, groupNo := setupTestEnv(t)
	const ip = "203.0.113.7"
	resetIPFailBucket(t, ctx, ip)
	resetStrictIPBucket(t, ctx, ip)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)
	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)

	// Many valid pushes from one IP, far past the failure burst of 1 — none
	// failure-gated (the per-IP REQUEST limiter stays at its generous default).
	for i := 0; i < 5; i++ {
		w = do(handler, anonReqIP("POST", url, body, ip))
		assert.NotEqualf(t, http.StatusTooManyRequests, w.Code,
			"valid push %d from a fixed IP must not be failure-limited; body=%s", i, w.Body.String())
	}
}

// TestPush_ValidPushesCappedByPerIPRequestLimit locks the layer the reviewers
// asked to keep: the per-IP REQUEST limiter (StrictIPRateLimitMiddleware) bounds
// ALL requests from one IP — including valid pushes — so a single IP holding many
// valid tokens cannot drive the full process floor. Only the per-IP request
// budget is tightened here; every other layer stays generous, so a 429 can only
// come from that limiter.
func TestPush_ValidPushesCappedByPerIPRequestLimit(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_RPS", "0.01")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_BURST", "2")
	t.Setenv("DM_INCOMINGWEBHOOK_RPS", "1000")
	t.Setenv("DM_INCOMINGWEBHOOK_BURST", "1000")
	t.Setenv("DM_INCOMINGWEBHOOK_LOCAL_BURST", "1000")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "1000")

	handler, ctx, groupNo := setupTestEnv(t)
	const ip = "203.0.113.50"
	resetStrictIPBucket(t, ctx, ip)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)
	body, _ := json.Marshal(pushReq{Content: "hi"})
	url := fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token)

	// First two valid pushes consume the per-IP request burst (not 429).
	for i := 0; i < 2; i++ {
		w = do(handler, anonReqIP("POST", url, body, ip))
		assert.NotEqualf(t, http.StatusTooManyRequests, w.Code,
			"valid push %d should pass the per-IP request burst; body=%s", i, w.Body.String())
	}
	// Third valid push exceeds the per-IP request budget → 429, even though the
	// token is valid (the multi-valid-token-from-one-IP cap).
	w = do(handler, anonReqIP("POST", url, body, ip))
	assert.Equalf(t, http.StatusTooManyRequests, w.Code,
		"per-IP request limiter must cap valid pushes from one IP; body=%s", w.Body.String())
}

// TestPush_FailedAuthGetsIPLimited verifies the failure path DOES throttle by
// IP: with a failure budget of 2, the first two bad-token attempts return 401
// (each spends a token), and once the budget is spent the gate rejects further
// attempts with 429 before the handler/DB even run.
func TestPush_FailedAuthGetsIPLimited(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_RPS", "0.01") // ~never refills within the test
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "2")

	handler, ctx, groupNo := setupTestEnv(t)
	const ip = "203.0.113.9"
	resetIPFailBucket(t, ctx, ip)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	whID := parseJSON(t, w)["webhook_id"].(string)
	body, _ := json.Marshal(pushReq{Content: "hi"})
	badURL := fmt.Sprintf("/v1/incoming-webhooks/%s/wrong-token", whID)

	// First two bad-token attempts spend the budget and return 401.
	for i := 0; i < 2; i++ {
		w = do(handler, anonReqIP("POST", badURL, body, ip))
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "attempt %d should be 401; body=%s", i, w.Body.String())
	}
	// Budget exhausted: the gate now rejects with 429 before the handler runs.
	w = do(handler, anonReqIP("POST", badURL, body, ip))
	assert.Equalf(t, http.StatusTooManyRequests, w.Code,
		"after burning the IP failure budget, further attempts must be 429; body=%s", w.Body.String())
}

// TestPush_IPBudgetNotSpoofableViaXFF pins the trusted-IP contract: a scanner
// varying the LEFTMOST X-Forwarded-For entry every request cannot evade the
// failure budget, because clientIP() keys off the RIGHTMOST (trusted-proxy-
// appended) entry — not gin's spoofable c.ClientIP().
func TestPush_IPBudgetNotSpoofableViaXFF(t *testing.T) {
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_RPS", "0.01")
	t.Setenv("DM_INCOMINGWEBHOOK_IP_FAIL_BURST", "2")

	handler, ctx, groupNo := setupTestEnv(t)
	const realIP = "198.51.100.20"
	resetIPFailBucket(t, ctx, realIP)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	whID := parseJSON(t, w)["webhook_id"].(string)
	body, _ := json.Marshal(pushReq{Content: "hi"})
	badURL := fmt.Sprintf("/v1/incoming-webhooks/%s/wrong-token", whID)

	// The trusted proxy appends realIP last; the attacker forges a different
	// leftmost hop on every request. No X-Real-Ip, so clientIP() reads the
	// rightmost XFF entry.
	mk := func(spoof string) *http.Request {
		req := anonReq("POST", badURL, body)
		req.Header.Set("X-Forwarded-For", spoof+", "+realIP)
		return req
	}
	assert.Equal(t, http.StatusUnauthorized, do(handler, mk("10.0.0.1")).Code)
	assert.Equal(t, http.StatusUnauthorized, do(handler, mk("10.0.0.2")).Code)
	// Despite a fresh spoofed leftmost IP each time, the budget (keyed on realIP)
	// is exhausted → 429. A spoofable leftmost-XFF read would never reach this.
	w = do(handler, mk("10.0.0.3"))
	assert.Equalf(t, http.StatusTooManyRequests, w.Code,
		"varying the leftmost XFF must not grant fresh budget; body=%s", w.Body.String())
}

func TestPush_RegenerateInvalidatesOldToken(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	oldToken := created["token"].(string)

	w = do(handler, authReq("POST",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	// 旧 token 应当 401
	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, oldToken), body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreate_QuotaEnforced(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	// 把每群配额降到 2，避免循环 10 次拖慢测试
	t.Setenv("DM_INCOMINGWEBHOOK_MAX_PER_GROUP", "2")

	for i := 0; i < 2; i++ {
		w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
			"name": fmt.Sprintf("wh-%d", i),
		}))
		assert.Equalf(t, http.StatusOK, w.Code, "i=%d body=%s", i, w.Body.String())
	}
	// 第 3 个应当被配额拒绝：迁移到 typed code 后返回语义化 409 Conflict
	// （ErrIncomingWebhookQuotaExceeded），不再依赖响应里的中文文案。
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "overflow",
	}))
	assert.Equalf(t, http.StatusConflict, w.Code, "quota exceeded must be 409: %s", w.Body.String())
}

// TestCreate_QuotaConcurrent 验证 insertWithQuota 在并发下守住上限。
// 之前的 countByGroupNo + insert 两步式写法在并发下会让多个请求同时通过配额校验，
// 实际写入超过 maxPerGroup（PR #31 lml2468 / Jerry-Xin 反馈）。
func TestCreate_QuotaConcurrent(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	const cap = 3
	const fanout = 10
	t.Setenv("DM_INCOMINGWEBHOOK_MAX_PER_GROUP", strconv.Itoa(cap))

	var wg sync.WaitGroup
	codes := make([]int, fanout)
	for i := 0; i < fanout; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
				"name": fmt.Sprintf("wh-%d", idx),
			}))
			codes[idx] = w.Code
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	assert.Equalf(t, cap, ok, "exactly %d concurrent creates should succeed, got %d (codes=%v)", cap, ok, codes)

	// DB 里也应当只有 cap 条有效记录。按 status != statusDeleted(2) 计——与
	// insertWithQuota 的有效计数同语义；本测试无软删除行，过滤与否结果相同，但保持
	// 一致可避免将来扩展（先删再并发建）时断言被软删除行误导。
	var rows int
	_, err := ctx.DB().SelectBySql("SELECT count(*) FROM incoming_webhook WHERE group_no=? AND status != 2", groupNo).Load(&rows)
	assert.NoError(t, err)
	assert.Equal(t, cap, rows, "DB row count must equal cap; quota leaked under concurrency")
}

// TestDisbandedGroup_FailsClosed 锁定 disband 生命周期：群一旦进入非 Normal
// 状态，create / update(启用) / push 三条路径都必须拒绝，杜绝 stale 管理员
// 在 handleGroupDisband 异步窗口或之后让 webhook 复活。
func TestDisbandedGroup_FailsClosed(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)

	// 在群 Normal 状态下先建一个 webhook，方便后续测 update / push
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusOK, w.Code)
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)
	token := created["token"].(string)

	// 把群标记为已解散（GroupStatusDisband=2）
	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET status=2 WHERE group_no=?", groupNo).Exec()
	assert.NoError(t, err)

	// 1) push：即便 webhook.status=1 且 token 正确也必须 401
	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equalf(t, http.StatusUnauthorized, w.Code,
		"push must fail closed on disbanded group, body=%s", w.Body.String())

	// 2) update 启用：先模拟"异步禁用已落库"（status=0 即 statusDisabled），管理员 PUT status=1 复活必须 404
	_, err = ctx.DB().UpdateBySql("UPDATE incoming_webhook SET status=0 WHERE webhook_id=?", whID).Exec()
	assert.NoError(t, err)
	statusOne := 1
	w = do(handler, authReq("PUT",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID),
		map[string]interface{}{"status": statusOne}))
	assert.Equalf(t, http.StatusNotFound, w.Code,
		"re-enable on disbanded group must fail closed, body=%s", w.Body.String())

	// 3) create：群已解散，重新创建 webhook 必须 404
	w = do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "revive",
	}))
	assert.Equalf(t, http.StatusNotFound, w.Code,
		"create on disbanded group must fail closed, body=%s", w.Body.String())

	// 4) regenerate：群已解散，重置 token（即便走管理员路径）必须 404
	w = do(handler, authReq("POST",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/regenerate", groupNo, whID), nil))
	assert.Equalf(t, http.StatusNotFound, w.Code,
		"regenerate on disbanded group must fail closed, body=%s", w.Body.String())
}

func TestUpdate_RejectsCrossGroupAccess(t *testing.T) {
	handler, ctx, groupNoA := setupTestEnv(t)

	// 在群 A 创建一个 webhook
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNoA), map[string]interface{}{
		"name": "x",
	}))
	created := parseJSON(t, w)
	whID := created["webhook_id"].(string)

	// 创建群 B 并把 testutil.UID 设为群主
	groupNoB := "g_" + util.GenerUUID()[:12]
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'b', 1, '')", groupNoB).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no, uid, role, status, is_deleted, version) VALUES(?, ?, 1, 1, 0, 1)",
		groupNoB, testutil.UID).Exec()
	assert.NoError(t, err)

	// 拿 A 的 webhook_id 去群 B 路径下尝试更新，应 404
	name := "hijack"
	w = do(handler, authReq("PUT",
		fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNoB, whID),
		map[string]interface{}{"name": name}))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
