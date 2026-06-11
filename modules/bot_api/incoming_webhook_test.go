package bot_api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// 触发 incomingwebhook 模块注册（SQL 迁移 + 共享实例），bot 路由挂在其上。
	_ "github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
)

// TestMain 准备 common.Setup 所需的 master key（必须 32 字节）：incomingwebhook 的
// 模块依赖把 modules/common 带进了本包的测试模块集。CI 经 ci.yml 全局环境变量注入；
// 本地直接 go test 也能跑通（与 incomingwebhook 包的 TestMain 同模式）。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		os.Setenv("OCTO_MASTER_KEY", "12345678901234567890123456789012")
	}
	os.Exit(m.Run())
}

// Bot 管理群入站 Webhook（/v1/bot/groups/:group_no/incoming-webhooks）：
//   - 群成员 bot 可创建（名称缺省自动命名；头像仅管理员可设）并管理自己创建的；
//   - 非成员 bot 403；成员 bot 不可动他人创建的（403）；
//   - 担任群管理员（group_member.role=manager）的 bot 与人类管理员同权：
//     可设头像、可管理任意 webhook。

const (
	iwhBotID         = "bot_iwh_member"
	iwhBotToken      = "bf_iwh_member_token"
	iwhBot2ID        = "bot_iwh_other"
	iwhBot2Token     = "bf_iwh_other_token"
	iwhAdminBotID    = "bot_iwh_admin"
	iwhAdminBotToken = "bf_iwh_admin_token"
	iwhOutBotID      = "bot_iwh_outsider"
	iwhOutBotToken   = "bf_iwh_outsider_token"
	iwhGroupNo       = "g_iwh_bot_1"
)

func setupBotWebhookEnv(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	for _, b := range []struct{ id, token string }{
		{iwhBotID, iwhBotToken},
		{iwhBot2ID, iwhBot2Token},
		{iwhAdminBotID, iwhAdminBotToken},
		{iwhOutBotID, iwhOutBotToken},
	} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
			b.id, "owner_iwh", b.token).Exec()
		require.NoError(t, err)
	}

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version) VALUES (?, ?, 1, 1)",
		iwhGroupNo, "iwh bot group").Exec()
	require.NoError(t, err)

	// 成员 bot ×2（role=0）、管理员 bot（role=2）；outsider 不入群。
	for _, m := range []struct {
		uid  string
		role int
	}{{iwhBotID, 0}, {iwhBot2ID, 0}, {iwhAdminBotID, 2}} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO group_member (group_no, uid, role, vercode, is_deleted, status, version) VALUES (?, ?, ?, ?, 0, 1, 1)",
			iwhGroupNo, m.uid, m.role, util.GenerUUID()).Exec()
		require.NoError(t, err)
	}

	// bot 管理路由挂了 SharedUIDRateLimiter（按 uid=robot_id 的 Redis 桶，跨测试持久），
	// 清桶保证从满额开始。
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}

	return s.GetRoute(), ctx
}

func botReq(t *testing.T, method, path, botToken string, body interface{}) *http.Request {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, path, rd)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	return req
}

func doBot(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func botWebhookBase() string {
	return fmt.Sprintf("/v1/bot/groups/%s/incoming-webhooks", iwhGroupNo)
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &m), "body: %s", w.Body.String())
	return m
}

// 成员 bot：创建（缺省自动命名 + creator=bot）→ 改自身状态 → 删除，全链路可用。
func TestBotWebhook_MemberCreateAndManageOwn(t *testing.T) {
	handler, _ := setupBotWebhookEnv(t)

	w := doBot(handler, botReq(t, "POST", botWebhookBase(), iwhBotToken, map[string]interface{}{}))
	require.Equalf(t, http.StatusOK, w.Code, "bot create body: %s", w.Body.String())
	created := decodeBody(t, w)
	assert.Equal(t, iwhBotID, created["creator_uid"])
	assert.Regexp(t, `^Webhook-[0-9a-f]{6}$`, created["name"])
	require.NotEmpty(t, created["token"])
	whID := created["webhook_id"].(string)

	// 自定义名称 → 200，强制带 "Webhook-" 前缀；自定义头像 → 400（仅管理员可设头像）。
	w = doBot(handler, botReq(t, "POST", botWebhookBase(), iwhBotToken, map[string]interface{}{"name": "ci-bot-wh"}))
	require.Equalf(t, http.StatusOK, w.Code, "named create body: %s", w.Body.String())
	assert.Equal(t, "Webhook-ci-bot-wh", decodeBody(t, w)["name"])
	w = doBot(handler, botReq(t, "POST", botWebhookBase(), iwhBotToken, map[string]interface{}{"avatar": "https://x/a.png"}))
	assert.Equalf(t, http.StatusBadRequest, w.Code, "avatar create body: %s", w.Body.String())

	// 改自身状态 OK。
	w = doBot(handler, botReq(t, "PUT", botWebhookBase()+"/"+whID, iwhBotToken, map[string]interface{}{"status": 0}))
	assert.Equalf(t, http.StatusOK, w.Code, "self disable body: %s", w.Body.String())

	// list 可见。
	w = doBot(handler, botReq(t, "GET", botWebhookBase(), iwhBotToken, nil))
	require.Equalf(t, http.StatusOK, w.Code, "list body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), whID)

	// 删除自己创建的 OK。
	w = doBot(handler, botReq(t, "DELETE", botWebhookBase()+"/"+whID, iwhBotToken, nil))
	assert.Equalf(t, http.StatusOK, w.Code, "self delete body: %s", w.Body.String())
}

// 成员 bot 不可动他人创建的（403）；非成员 bot 一切操作 403。
func TestBotWebhook_OwnershipAndMembershipGates(t *testing.T) {
	handler, _ := setupBotWebhookEnv(t)

	w := doBot(handler, botReq(t, "POST", botWebhookBase(), iwhBotToken, map[string]interface{}{}))
	require.Equalf(t, http.StatusOK, w.Code, "bot create body: %s", w.Body.String())
	whID := decodeBody(t, w)["webhook_id"].(string)

	// 另一个成员 bot 动它 → 403。
	w = doBot(handler, botReq(t, "PUT", botWebhookBase()+"/"+whID, iwhBot2Token, map[string]interface{}{"status": 0}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "other bot update body: %s", w.Body.String())
	w = doBot(handler, botReq(t, "DELETE", botWebhookBase()+"/"+whID, iwhBot2Token, nil))
	assert.Equalf(t, http.StatusForbidden, w.Code, "other bot delete body: %s", w.Body.String())

	// 非成员 bot：创建 / 列表 都 403。
	w = doBot(handler, botReq(t, "POST", botWebhookBase(), iwhOutBotToken, map[string]interface{}{}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "outsider create body: %s", w.Body.String())
	w = doBot(handler, botReq(t, "GET", botWebhookBase(), iwhOutBotToken, nil))
	assert.Equalf(t, http.StatusForbidden, w.Code, "outsider list body: %s", w.Body.String())
}

// App Bot（app_ token，authBot 的另一条鉴权分支）在本管理面的现实行为：鉴权可过，
// 但一律 403（PR #340 review，Jerry-Xin 建议钉住 app_ 分支）。
//
// 原因：权限判定只认 group_member 行，而 App Bot 当前【无法入群】——createBot 不写
// space_member（见 modules/app_bot/app_bot.go addFriend 处的注释），space 群加成员
// 对 bot 强制校验 space 成员资格（modules/group/api.go ErrGroupBotNotInSpace），
// 所以 App Bot 永远没有 group_member 行。本管理面实际仅 User Bot 可用；若未来放开
// App Bot 入群，resolveActor 按 uid 判定、不特判 token 类型，权限矩阵自动成立。
//
// 经 in-memory Registry 热路径鉴权（authAppBot 的首选分支，生产由 app_bot 模块在
// 启动时灌注）：bot_api 测试包不能 blank-import app_bot（会成 prod→test 导入环），
// 故不走 app_bot 表的 DB fallback 分支。
func TestBotWebhook_AppBotAlwaysForbidden(t *testing.T) {
	handler, _ := setupBotWebhookEnv(t)

	const (
		appBotUID   = "app_iwh_bot_uid"
		appBotToken = "app_iwh_token"
	)
	adapter := NewAppBotRegistryAdapter()
	adapter.Add(appBotToken, &AppBotRegistrySpec{UID: appBotUID, Scope: "platform"})
	prev := GetAppBotRegistry()
	SetAppBotRegistry(adapter)
	t.Cleanup(func() {
		if prev != nil {
			SetAppBotRegistry(prev)
		} else {
			SetAppBotRegistry(NewAppBotRegistryAdapter()) // atomic.Value 不可存 nil，置空注册表即可
		}
	})

	// 鉴权通过（非 401），但非群成员 → 创建/列表一律 403。
	w := doBot(handler, botReq(t, "POST", botWebhookBase(), appBotToken, map[string]interface{}{}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "app bot create body: %s", w.Body.String())
	w = doBot(handler, botReq(t, "GET", botWebhookBase(), appBotToken, nil))
	assert.Equalf(t, http.StatusForbidden, w.Code, "app bot list body: %s", w.Body.String())
}

// 管理员 bot 与人类管理员同权：可设头像、管理任意成员 bot 的 webhook。
func TestBotWebhook_AdminBotManagesAny(t *testing.T) {
	handler, _ := setupBotWebhookEnv(t)

	// 成员 bot 先建一个。
	w := doBot(handler, botReq(t, "POST", botWebhookBase(), iwhBotToken, map[string]interface{}{}))
	require.Equalf(t, http.StatusOK, w.Code, "member bot create body: %s", w.Body.String())
	memberWhID := decodeBody(t, w)["webhook_id"].(string)

	// 管理员 bot 自定义名称+头像创建 → 200（管理员可设头像）。
	w = doBot(handler, botReq(t, "POST", botWebhookBase(), iwhAdminBotToken,
		map[string]interface{}{"name": "admin-bot-wh", "avatar": "https://example.com/a.png"}))
	require.Equalf(t, http.StatusOK, w.Code, "admin bot create body: %s", w.Body.String())
	created := decodeBody(t, w)
	assert.Equal(t, "admin-bot-wh", created["name"])
	assert.Equal(t, "https://example.com/a.png", created["avatar"])

	// 管理员 bot 改名成员 bot 的 webhook → 200。
	w = doBot(handler, botReq(t, "PUT", botWebhookBase()+"/"+memberWhID, iwhAdminBotToken, map[string]interface{}{"name": "renamed-by-admin-bot"}))
	require.Equalf(t, http.StatusOK, w.Code, "admin bot rename body: %s", w.Body.String())
	assert.Equal(t, "renamed-by-admin-bot", decodeBody(t, w)["name"])

	// 管理员 bot 删除成员 bot 的 webhook → 200。
	w = doBot(handler, botReq(t, "DELETE", botWebhookBase()+"/"+memberWhID, iwhAdminBotToken, nil))
	assert.Equalf(t, http.StatusOK, w.Code, "admin bot delete body: %s", w.Body.String())
}
