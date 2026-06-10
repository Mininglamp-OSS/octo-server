package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3（平台适配器）push 端到端用例。与现有 push 测试同风格走 testutil.NewTestServer
// （需 MySQL/Redis/WuKongIM，CI 执行）。成功路径只断言「通过鉴权/校验」（非 4xx）——
// 测试桩下游 SendMessage 可能 200 或 502，口径与 richtext_push_test.go 一致。

// pushAdapterRaw 向适配器后缀路由发原始 body（可带平台事件头）。
func pushAdapterRaw(handler http.Handler, whID, token, suffix string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	r := anonReq("POST", fmt.Sprintf("/v1/incoming-webhooks/%s/%s/%s", whID, token, suffix), body)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return do(handler, r)
}

// create/regenerate 响应携带各推送形态的 URL（#297 顺延的 onboarding 项）。
func TestCreate_ReturnsAdapterURLs(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "adapter-wh",
	}))
	require.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	created := parseJSON(t, w)

	urls, ok := created["urls"].(map[string]interface{})
	require.True(t, ok, "create response must carry urls; body=%s", w.Body.String())
	native, _ := urls["native"].(string)
	assert.Equal(t, created["url"], native, "urls.native must equal the legacy url field")
	assert.Equal(t, native+"/github", urls["github"])
	assert.Equal(t, native+"/wecom", urls["wecom"])
}

// GitHub ping：200 + skipped，不投递消息，且异步记一条 status=3(skipped) 的投递。
func TestPush_GitHubPing_SkippedAndAudited(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github",
		[]byte(`{"zen":"Keep it logically awesome.","hook_id":1}`),
		map[string]string{"X-GitHub-Event": "ping"})
	require.Equalf(t, http.StatusOK, w.Code, "ping must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`, "ping response must mark the delivery as skipped")

	require.Eventually(t, func() bool {
		dw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
		if dw.Code != http.StatusOK {
			return false
		}
		list, _ := parseJSON(t, dw)["list"].([]interface{})
		for _, item := range list {
			row, _ := item.(map[string]interface{})
			if row["adapter"] == "github" && int(row["status"].(float64)) == 3 && row["reason"] == "ping" {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "ping must be recorded as a skipped delivery")
}

// GitHub push 事件通过鉴权/翻译（非 4xx）。
func TestPush_GitHubPushEvent_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	body := []byte(`{
		"ref": "refs/heads/main",
		"commits": [{"id": "aaaabbbbcccc", "message": "feat: hello", "url": "https://github.com/o/r/commit/aaaabbbb"}],
		"repository": {"full_name": "o/r", "html_url": "https://github.com/o/r"},
		"sender": {"login": "alice"}
	}`)
	w := pushAdapterRaw(handler, whID, token, "github", body, map[string]string{"X-GitHub-Event": "push"})
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid push event must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), "message_id")
	}
}

// 渲染子集之外的事件：200 + skipped（GitHub 侧不标红，群内不刷屏）。
func TestPush_GitHubUnsupportedEvent_Skipped(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github",
		[]byte(`{"action":"started"}`), map[string]string{"X-GitHub-Event": "watch"})
	require.Equalf(t, http.StatusOK, w.Code, "unsupported event must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`)
}

// 缺事件头 → 400 invalid（误配置要立刻可见，而非静默跳过）。
func TestPush_GitHubMissingEventHeader_Invalid(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "github", []byte(`{}`), nil)
	assert.Equalf(t, http.StatusBadRequest, w.Code, "missing X-GitHub-Event must 400; body=%s", w.Body.String())
}

// 适配器路由沿用同一鉴权：错 token 统一 401（反枚举口径不变）。
func TestPush_AdapterRoute_AuthEnforced(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	for _, suffix := range []string{"github", "wecom"} {
		w := pushAdapterRaw(handler, whID, "wrong-token", suffix,
			[]byte(`{"msgtype":"text","text":{"content":"hi"}}`),
			map[string]string{"X-GitHub-Event": "push"})
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "%s: bad token must 401; body=%s", suffix, w.Body.String())
	}
}

// 企微 text 格式通过鉴权/翻译；成功响应附带 errcode=0（平台 SDK 兼容）。
func TestPush_WeComText_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "wecom",
		[]byte(`{"msgtype":"text","text":{"content":"hello from wecom"}}`), nil)
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid wecom text must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), `"errcode":0`)
	}
}

// 企微素材类消息（base64 图片）→ 400 invalid（显式失败优于静默丢弃）。
func TestPush_WeComImage_Rejected(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "wecom",
		[]byte(`{"msgtype":"image","image":{"base64":"...","md5":"..."}}`), nil)
	assert.Equalf(t, http.StatusBadRequest, w.Code, "wecom image must 400; body=%s", w.Body.String())
}
