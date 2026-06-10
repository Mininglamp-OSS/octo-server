package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 2（失败可观测性 + 管理 UX）的 HTTP e2e 用例。需 MySQL/Redis/WuKongIM（CI）。
// 审计写入是异步（submitDelivery → goroutine），故依赖异步落库的断言用 require.Eventually
// 轮询，避免 flaky；不依赖异步的（直接 DB 注入 + 读端点、鉴权/404）则确定断言。

// --- deliveries 端点 ---

// 直接 DB 注入成功+失败各一条，GET deliveries 必须确定返回两条且字段正确、无 token。
func TestDeliveries_Endpoint(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO incoming_webhook_audit(webhook_id, group_no, ip, byte_size, message_id, status, reason, http_status, adapter) VALUES(?,?,?,?,?,?,?,?,?)",
		whID, groupNo, "1.2.3.4", 12, 99, 1, "", 200, "native").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO incoming_webhook_audit(webhook_id, group_no, ip, byte_size, message_id, status, reason, http_status, adapter) VALUES(?,?,?,?,?,?,?,?,?)",
		whID, groupNo, "1.2.3.4", 0, 0, 2, "delivery_failed", 502, "native").Exec()
	require.NoError(t, err)

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "token")
	// 隐私取舍：deliveries 不下发调用方 IP（review 决定）。
	assert.NotContains(t, w.Body.String(), "1.2.3.4", "deliveries must not expose caller ip")

	list, _ := parseJSON(t, w)["list"].([]interface{})
	require.Len(t, list, 2)

	var success, failed int
	for _, item := range list {
		row, _ := item.(map[string]interface{})
		switch int(row["status"].(float64)) {
		case 1:
			success++
			assert.Equal(t, float64(200), row["http_status"])
		case 2:
			failed++
			assert.Equal(t, "delivery_failed", row["reason"])
			assert.Equal(t, float64(502), row["http_status"])
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, failed)
}

// limit 查询参数钳制返回条数。
func TestDeliveries_LimitParam(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)
	for i := 0; i < 3; i++ {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO incoming_webhook_audit(webhook_id, group_no, ip, status, http_status, adapter) VALUES(?,?,?,?,?,?)",
			whID, groupNo, "1.2.3.4", 1, 200, "native").Exec()
		require.NoError(t, err)
	}
	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries?limit=2", groupNo, whID), nil))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list, _ := parseJSON(t, w)["list"].([]interface{})
	assert.Len(t, list, 2)
}

// deliveries 是创建者或管理员可见：创建者被降级为普通成员后仍可见（创建者权限），
// 非创建者普通成员的 403 见 api_member_test.go TestMemberManage_OwnOnly。
func TestDeliveries_DemotedCreatorStillAllowed(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)
	_, err := ctx.DB().UpdateBySql("UPDATE group_member SET role=0 WHERE group_no=? AND uid=?", groupNo, testutil.UID).Exec()
	require.NoError(t, err)

	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
	assert.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// 不存在 / 跨群 webhook → 404。
func TestDeliveries_NotFound(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/iwh_nonexistent/deliveries", groupNo), nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 端到端：推送非法 blocks → 400，且失败被异步记入审计（reason=blocks, http_status=400）。
func TestDeliveries_RecordsPushFailure(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	// 缺图片宽高 → 400（确定）。
	pw := pushRichText(handler, whID, token, map[string]interface{}{
		"msg_type": "richtext",
		"blocks":   []map[string]interface{}{{"type": "image", "url": "https://example.com/a.png"}},
	})
	require.Equalf(t, http.StatusBadRequest, pw.Code, "bad blocks must 400; body=%s", pw.Body.String())

	// 异步审计：轮询 deliveries 直到出现一条 reason=blocks 的失败记录。
	require.Eventually(t, func() bool {
		w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
		if w.Code != http.StatusOK {
			return false
		}
		list, _ := parseJSON(t, w)["list"].([]interface{})
		for _, item := range list {
			row, _ := item.(map[string]interface{})
			if row["reason"] == "blocks" && int(row["status"].(float64)) == 2 && int(row["http_status"].(float64)) == 400 {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "push failure must be recorded as a delivery")
}

// 鉴权失败（错 token）绝不落 deliveries（反枚举不变量）：失败仅进 IP 失败预算。
func TestDeliveries_AuthFailureNotRecorded(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	bad := pushRichText(handler, whID, "wrong-token", map[string]interface{}{"content": "hi"})
	require.Equal(t, http.StatusUnauthorized, bad.Code)

	// 给异步留点时间，随后 deliveries 必须仍为空（鉴权失败不记审计）。
	time.Sleep(300 * time.Millisecond)
	w := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list, _ := parseJSON(t, w)["list"].([]interface{})
	assert.Empty(t, list, "auth failures must not be recorded per-webhook (anti-enumeration)")
}

// --- 测试推送端点 ---

// 测试推送是创建者或管理员可用：创建者被降级为普通成员后仍可用（创建者权限，
// 下游投递在测试桩下可能 200/500，只断言不是 403）。非创建者普通成员的 403 见
// api_member_test.go TestMemberManage_OwnOnly。
func TestTestPush_DemotedCreatorStillAllowed(t *testing.T) {
	handler, ctx, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)
	_, err := ctx.DB().UpdateBySql("UPDATE group_member SET role=0 WHERE group_no=? AND uid=?", groupNo, testutil.UID).Exec()
	require.NoError(t, err)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/test", groupNo, whID), nil))
	assert.NotEqualf(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// 不存在的 webhook → 404。
func TestTestPush_NotFound(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/iwh_nope/test", groupNo), nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 管理员触发测试推送通过鉴权/校验（下游投递在测试桩下可能 200 或 500，故只断言非
// 403/404；成功时记一条 adapter=test 的投递）。
func TestTestPush_PassesAuth(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/test", groupNo, whID), nil))
	assert.NotEqualf(t, http.StatusForbidden, w.Code, "admin must pass auth; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusNotFound, w.Code, "existing webhook must be found; body=%s", w.Body.String())

	if w.Code == http.StatusOK {
		require.Eventually(t, func() bool {
			dw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
			if dw.Code != http.StatusOK {
				return false
			}
			list, _ := parseJSON(t, dw)["list"].([]interface{})
			for _, item := range list {
				row, _ := item.(map[string]interface{})
				if row["adapter"] == "test" {
					return true
				}
			}
			return false
		}, 3*time.Second, 50*time.Millisecond, "successful test push must be recorded with adapter=test")
	}
}

// --- text 别名 ---

// 缺省 msg_type 下 {"text":"..."} 作为 content 别名通过校验（非 400）。
func TestPush_TextAlias(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushRichText(handler, whID, token, map[string]interface{}{"text": "hello via alias"})
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "text alias must satisfy content; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
}
