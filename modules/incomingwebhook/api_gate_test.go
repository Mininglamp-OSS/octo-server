package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// featuregate 灰度闸门集成测试（webhook-feature-flag-rollout）
//
// 固定 feature_key 的规则缓存（ft:rule:* / ft:scope:*）在 Redis 中持久，且
// CleanAllTables 不清 Redis —— 所有改规则的 helper 都必须随手清缓存，否则上个
// 测试的规则会泄漏到下一个。
// ============================================================

const createPathTmpl = "/v1/groups/%s/incoming-webhooks"

func resetFeatureGateCache(t *testing.T, ctx *config.Context) {
	t.Helper()
	cli := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer cli.Close()
	var all []string
	for _, pat := range []string{"ft:rule:*", "ft:scope:*"} {
		if ks, err := cli.Keys(pat).Result(); err == nil {
			all = append(all, ks...)
		}
	}
	if len(all) > 0 {
		_ = cli.Del(all...).Err()
	}
}

func setFeatureGate(t *testing.T, ctx *config.Context, key, mode string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO feature_gate(feature_key, mode, percent) VALUES(?, ?, 0) "+
			"ON DUPLICATE KEY UPDATE mode=VALUES(mode)", key, mode).Exec()
	assert.NoError(t, err)
	resetFeatureGateCache(t, ctx)
}

func clearFeatureGate(t *testing.T, ctx *config.Context, key string) {
	t.Helper()
	_, err := ctx.DB().DeleteFrom("feature_gate").Where("feature_key=?", key).Exec()
	assert.NoError(t, err)
	resetFeatureGateCache(t, ctx)
}

func addFeatureScope(t *testing.T, ctx *config.Context, key, scopeID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO feature_gate_scope(feature_key, scope_type, scope_id) VALUES(?, 'group', ?) "+
			"ON DUPLICATE KEY UPDATE feature_key=feature_key", key, scopeID).Exec()
	assert.NoError(t, err)
	resetFeatureGateCache(t, ctx)
}

func createWebhook(t *testing.T, handler http.Handler, groupNo string) (webhookID, token string) {
	t.Helper()
	w := do(handler, authReq("POST", fmt.Sprintf(createPathTmpl, groupNo),
		map[string]interface{}{"name": "gate-test"}))
	require.Equalf(t, http.StatusOK, w.Code, "建 webhook 失败 body: %s", w.Body.String())
	res := parseJSON(t, w)
	return res["webhook_id"].(string), res["token"].(string)
}

func pushURL(webhookID, token string) string {
	return fmt.Sprintf("/v1/incoming-webhooks/%s/%s", webhookID, token)
}

// ---- create 闸门 ----

func TestGate_CreateDenied_NoRule(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	clearFeatureGate(t, ctx, "incoming_webhook_create") // 删默认 on → 无规则 fail-closed
	w := do(handler, authReq("POST", fmt.Sprintf(createPathTmpl, groupNo),
		map[string]interface{}{"name": "x"}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "无规则应 fail-closed 403, body: %s", w.Body.String())
}

func TestGate_CreateDenied_ModeOff(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	setFeatureGate(t, ctx, "incoming_webhook_create", "off")
	w := do(handler, authReq("POST", fmt.Sprintf(createPathTmpl, groupNo),
		map[string]interface{}{"name": "x"}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "off 应 403, body: %s", w.Body.String())
}

func TestGate_CreateWhitelist_Hit(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	setFeatureGate(t, ctx, "incoming_webhook_create", "whitelist")
	addFeatureScope(t, ctx, "incoming_webhook_create", groupNo) // 命中本群
	w := do(handler, authReq("POST", fmt.Sprintf(createPathTmpl, groupNo),
		map[string]interface{}{"name": "x"}))
	assert.Equalf(t, http.StatusOK, w.Code, "白名单命中应 200, body: %s", w.Body.String())
}

func TestGate_CreateWhitelist_Miss(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	setFeatureGate(t, ctx, "incoming_webhook_create", "whitelist") // 不加本群 scope
	w := do(handler, authReq("POST", fmt.Sprintf(createPathTmpl, groupNo),
		map[string]interface{}{"name": "x"}))
	assert.Equalf(t, http.StatusForbidden, w.Code, "白名单未命中应 403, body: %s", w.Body.String())
}

// ---- push 闸门 ----

func TestGate_PushDisabled(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	wid, token := createWebhook(t, handler, groupNo) // create=on 默认放行
	setFeatureGate(t, ctx, "incoming_webhook_push", "off")
	w := do(handler, anonReq("POST", pushURL(wid, token), []byte(`{"content":"hi"}`)))
	assert.Equalf(t, http.StatusServiceUnavailable, w.Code, "push off 应 503, body: %s", w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"), "503 应带 Retry-After")
}

func TestGate_PushKillSwitch(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	wid, token := createWebhook(t, handler, groupNo)
	t.Setenv("DM_FT_INCOMING_WEBHOOK_PUSH_KILL", "1")
	w := do(handler, anonReq("POST", pushURL(wid, token), []byte(`{"content":"hi"}`)))
	assert.Equalf(t, http.StatusServiceUnavailable, w.Code, "push kill 应 503, body: %s", w.Body.String())
}

// TestGate_PushDisabled_NoTokenLeak 验证关停期防探测：连不存在的 webhook/token
// 也返回统一 503（gate 在 handler 最前、查 DB 之前），不泄露 token 正确性。
func TestGate_PushDisabled_NoTokenLeak(t *testing.T) {
	handler, ctx, _ := setupTestEnv(t)
	setFeatureGate(t, ctx, "incoming_webhook_push", "off")
	w := do(handler, anonReq("POST", pushURL("iwh_nonexistent", "badtoken"), []byte(`{"content":"hi"}`)))
	assert.Equalf(t, http.StatusServiceUnavailable, w.Code, "关停期任意请求统一 503, body: %s", w.Body.String())
}
