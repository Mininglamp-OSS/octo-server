package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	// 接口① /v1/channels/:id/:type 由 channel 模块提供，须注册其路由（及 SQL 迁移）。
	_ "github.com/Mininglamp-OSS/octo-server/modules/channel"
)

// 这些测试覆盖 #249 合并后的客户端兼容问题：客户端把 webhook 合成发送者（iwh_ 前缀）
// 当普通用户去查 /v1/channels、/v1/users、/v1/users/:uid/avatar，服务端需识别前缀并
// 兜底返回发送者名/头像，且 webhook 删除后优雅降级（不报 500、不裂图）。

// createWebhook 通过管理端点创建一个 webhook，返回其 webhook_id（iwh_xxx）。
func createWebhook(t *testing.T, handler http.Handler, groupNo, name, avatar string) string {
	t.Helper()
	body := map[string]interface{}{"name": name}
	if avatar != "" {
		body["avatar"] = avatar
	}
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), body))
	assert.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	id, _ := parseJSON(t, w)["webhook_id"].(string)
	assert.NotEmpty(t, id)
	return id
}

// 接口①：GET /v1/channels/:id/:type 解析 webhook 为单聊频道详情。
func TestWebhookRender_ChannelGet(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "GitHub Bot", "")

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/channels/%s/1", whID), nil))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	res := parseJSON(t, w)

	ch, _ := res["channel"].(map[string]interface{})
	assert.Equal(t, whID, ch["channel_id"])
	assert.Equal(t, float64(1), ch["channel_type"])
	assert.Equal(t, "GitHub Bot", res["name"])
	assert.Equal(t, fmt.Sprintf("users/%s/avatar", whID), res["logo"])
	extra, _ := res["extra"].(map[string]interface{})
	assert.Equal(t, "webhook", extra["kind"])
	// 绝不泄漏 token/token_hash。
	assert.NotContains(t, w.Body.String(), "token")
}

// 接口②：GET /v1/users/:uid 解析 webhook 为最小化用户详情。
func TestWebhookRender_UserGet(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "GitHub Bot", "")

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/users/%s?group_no=%s", whID, groupNo), nil))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	res := parseJSON(t, w)
	assert.Equal(t, whID, res["uid"])
	assert.Equal(t, "GitHub Bot", res["name"])
	assert.Equal(t, "webhook", res["category"])
	assert.NotContains(t, w.Body.String(), "token")
}

// 接口③：GET /v1/users/:uid/avatar — 有自定义 http(s) 头像 URL 时 302 重定向。
func TestWebhookRender_Avatar_Redirect(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	const avatarURL = "https://example.com/avatar.png"
	whID := createWebhook(t, handler, groupNo, "WH", avatarURL)

	w := do(handler, anonReq("GET", fmt.Sprintf("/v1/users/%s/avatar", whID), nil))
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, avatarURL, w.Header().Get("Location"))
}

// 接口③：未设置头像时回退到默认头像（不裂图）。
func TestWebhookRender_Avatar_Default(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "WH", "")

	w := do(handler, anonReq("GET", fmt.Sprintf("/v1/users/%s/avatar", whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.NotEmpty(t, w.Body.Bytes())
}

// 软删除（#254）后：接口②仍返回 200 + 真实发送者名，让该 webhook 的历史消息继续
// 可渲染（软删除保留行，display datasource 不按 status 过滤）。
func TestWebhookRender_AfterSoftDelete_UserGet(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "GitHub Bot", "")

	w := do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(handler, authReq("GET", fmt.Sprintf("/v1/users/%s?group_no=%s", whID, groupNo), nil))
	assert.Equalf(t, http.StatusOK, w.Code, "soft-deleted webhook must still resolve; body: %s", w.Body.String())
	res := parseJSON(t, w)
	assert.Equal(t, whID, res["uid"])
	assert.Equal(t, "GitHub Bot", res["name"])
	assert.Equal(t, "webhook", res["category"])
	assert.NotContains(t, w.Body.String(), "token")
}

// 软删除（#254）后：接口①仍把 webhook 解析为单聊频道并返回真实名。
func TestWebhookRender_AfterSoftDelete_ChannelGet(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "GitHub Bot", "")

	w := do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(handler, authReq("GET", fmt.Sprintf("/v1/channels/%s/1", whID), nil))
	assert.Equalf(t, http.StatusOK, w.Code, "soft-deleted webhook channel must still resolve; body: %s", w.Body.String())
	res := parseJSON(t, w)
	assert.Equal(t, "GitHub Bot", res["name"])
}

// 真实查询故障必须返回 5xx，不能被降级成 404 / 默认头像（reviewer #250 🔴）。
// 通过临时重命名 incoming_webhook 表注入 query error，测完 defer 恢复。
func TestWebhookRender_UserGet_QueryError_Returns5xx(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "WH", "")

	_, err := ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook TO incoming_webhook_bak").Exec()
	assert.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook_bak TO incoming_webhook").Exec()
	}()

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/users/%s?group_no=%s", whID, groupNo), nil))
	assert.GreaterOrEqualf(t, w.Code, 500, "query failure must surface as 5xx, not be masked as not-found; body: %s", w.Body.String())
}

func TestWebhookRender_Avatar_QueryError_Returns5xx(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID := createWebhook(t, handler, groupNo, "WH", "https://example.com/a.png")

	_, err := ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook TO incoming_webhook_bak").Exec()
	assert.NoError(t, err)
	defer func() {
		_, _ = ctx.DB().UpdateBySql("RENAME TABLE incoming_webhook_bak TO incoming_webhook").Exec()
	}()

	w := do(handler, anonReq("GET", fmt.Sprintf("/v1/users/%s/avatar", whID), nil))
	assert.GreaterOrEqualf(t, w.Code, 500, "query failure must surface as 5xx, not a default avatar; code=%d", w.Code)
}

// 软删除（#254）后：接口③仍 302 重定向到真实头像 URL（头像行保留，不退化为默认图）。
func TestWebhookRender_AfterSoftDelete_Avatar(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	const avatarURL = "https://example.com/a.png"
	whID := createWebhook(t, handler, groupNo, "WH", avatarURL)

	w := do(handler, authReq("DELETE", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s", groupNo, whID), nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(handler, anonReq("GET", fmt.Sprintf("/v1/users/%s/avatar", whID), nil))
	assert.Equalf(t, http.StatusFound, w.Code, "soft-deleted webhook avatar must still redirect; code=%d", w.Code)
	assert.Equal(t, avatarURL, w.Header().Get("Location"))
}
