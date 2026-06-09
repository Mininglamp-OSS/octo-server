package incomingwebhook_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 1（图文混排）push 端到端用例。与现有 push 测试同风格走 testutil.NewTestServer
// （需 MySQL/Redis/WuKongIM，CI 执行）。成功路径断言「通过校验」而非强求 200：测试
// 桩下游 SendMessage 可能返回 200 或 502，关键是请求没被 4xx（鉴权/校验）挡下——与
// 既有 TestPush_* 的鲁棒断言口径一致。
//
// 这里只断言 HTTP 状态码、不断言 details.reason：testutil.NewTestServer 未挂 i18n
// ErrorRenderer，错误体只是 legacy 的 {msg,status}（不含 error.details）。reason 契约
// 改在 api_i18n_test.go 的 i18n 渲染器 harness 里锁（生产经 main.go 挂了该 renderer）。

// createWebhookWithToken 创建一个 webhook 并返回 (webhook_id, token)。
func createWebhookWithToken(t *testing.T, handler http.Handler, groupNo string) (string, string) {
	t.Helper()
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "richtext-wh",
	}))
	require.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	created := parseJSON(t, w)
	return created["webhook_id"].(string), created["token"].(string)
}

func pushRichText(handler http.Handler, whID, token string, body map[string]interface{}) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	return do(handler, anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s", whID, token), raw))
}

// 成功：text + image 图文混排通过校验（非 4xx）。
func TestPush_RichText_Success(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	body := map[string]interface{}{
		"msg_type": "richtext",
		"blocks": []map[string]interface{}{
			{"type": "text", "text": "Build #123 passed ✅"},
			{"type": "image", "url": "https://example.com/chart.png", "width": 800, "height": 400},
		},
	}
	w := pushRichText(handler, whID, token, body)
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid richtext must pass validation; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusRequestEntityTooLarge, w.Code, "valid richtext must not be 413; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	// 下游投递成功时返回 200 + message_id；测试桩可能 502，故仅在 200 时校验 message_id。
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), "message_id")
	}
}

// 空 blocks → 400（reason=blocks）。
func TestPush_RichText_RejectsEmptyBlocks(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushRichText(handler, whID, token, map[string]interface{}{
		"msg_type": "richtext",
		"blocks":   []map[string]interface{}{},
	})
	assert.Equalf(t, http.StatusBadRequest, w.Code, "empty blocks must 400; body=%s", w.Body.String())
}

// 图片块缺 width/height → 400。
func TestPush_RichText_RejectsBadImage(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushRichText(handler, whID, token, map[string]interface{}{
		"msg_type": "richtext",
		"blocks": []map[string]interface{}{
			{"type": "image", "url": "https://example.com/a.png"},
		},
	})
	assert.Equalf(t, http.StatusBadRequest, w.Code, "image without size must 400; body=%s", w.Body.String())
}

// 非法 msg_type → 400（reason=msg_type）。
func TestPush_RejectsUnknownMsgType(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushRichText(handler, whID, token, map[string]interface{}{
		"msg_type": "card",
		"content":  "hi",
	})
	assert.Equalf(t, http.StatusBadRequest, w.Code, "unknown msg_type must 400; body=%s", w.Body.String())
}

// 向后兼容：显式 msg_type="text" 与缺省一致，纯文本仍走老路径通过校验。
func TestPush_TextMsgTypeBackCompat(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushRichText(handler, whID, token, map[string]interface{}{
		"msg_type": "text",
		"content":  "hello **world**",
	})
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "text msg_type must pass; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
}
