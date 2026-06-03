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

	// 该模块自身需注册以触发其 SQL 迁移
	_ "github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	// 迁移依赖链：以下模块的 SQL 迁移会修改本模块依赖的 group/group_member/user 表，
	// 缺失任何一个都会导致 module.Setup 在跨模块 ALTER 时报错。
	// 详见 memory: skill_service_test.md「迁移顺序陷阱」。
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{}))
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestCreate_NonAdminForbidden(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	handler, ctx, groupNo := setupTestEnv(t)
	// 把当前用户降级为普通成员
	_, err := ctx.DB().UpdateBySql("UPDATE group_member SET role=0 WHERE group_no=? AND uid=?", groupNo, testutil.UID).Exec()
	assert.NoError(t, err)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "x",
	}))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ============================================================
// 列表 / 删除 / 重置
// ============================================================

func TestListAndDelete(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

func TestRegenerate_RotatesToken(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

	// 禁用
	_, err := ctx.DB().UpdateBySql("UPDATE incoming_webhook SET status=0 WHERE webhook_id=?", whID).Exec()
	assert.NoError(t, err)

	body, _ := json.Marshal(pushReq{Content: "hi"})
	w = do(handler, anonReq("POST",
		fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPush_RejectsTooLargeBody(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

func TestPush_RegenerateInvalidatesOldToken(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
	// 第 3 个应当拒绝
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "overflow",
	}))
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "最多")
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

	// DB 里也应当只有 cap 条记录
	var rows int
	_, err := ctx.DB().SelectBySql("SELECT count(*) FROM incoming_webhook WHERE group_no=?", groupNo).Load(&rows)
	assert.NoError(t, err)
	assert.Equal(t, cap, rows, "DB row count must equal cap; quota leaked under concurrency")
}

// TestDisbandedGroup_FailsClosed 锁定 disband 生命周期：群一旦进入非 Normal
// 状态，create / update(启用) / push 三条路径都必须拒绝，杜绝 stale 管理员
// 在 handleGroupDisband 异步窗口或之后让 webhook 复活。
func TestDisbandedGroup_FailsClosed(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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

	// 2) update 启用：先模拟"异步禁用已落库"，管理员 PUT status=1 复活必须 404
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
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
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
