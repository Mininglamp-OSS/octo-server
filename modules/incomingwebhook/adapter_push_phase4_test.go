package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 4（GitLab / 飞书适配器）push 端到端用例。与 Phase 3 用例同风格走
// testutil.NewTestServer（需 MySQL/Redis/WuKongIM，CI 执行）；成功路径只断言「通过
// 鉴权/校验」（非 4xx），下游 SendMessage 可能 200 或 502。

// create/regenerate 响应的 urls 现含 gitlab/feishu。
func TestCreate_ReturnsPhase4AdapterURLs(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	w := do(handler, authReq("POST", fmt.Sprintf("/v1/groups/%s/incoming-webhooks", groupNo), map[string]interface{}{
		"name": "p4-wh",
	}))
	require.Equalf(t, http.StatusOK, w.Code, "create body: %s", w.Body.String())
	urls, ok := parseJSON(t, w)["urls"].(map[string]interface{})
	require.True(t, ok, "create response must carry urls; body=%s", w.Body.String())
	native, _ := urls["native"].(string)
	assert.Equal(t, native+"/gitlab", urls["gitlab"])
	assert.Equal(t, native+"/feishu", urls["feishu"])
}

// GitLab push（带正确 X-Gitlab-Token）通过鉴权/翻译（非 4xx）。
func TestPush_GitLabPushEvent_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	body := []byte(`{
		"ref": "refs/heads/main",
		"total_commits_count": 1,
		"commits": [{"id": "aaaabbbbcccc", "message": "feat: hello", "url": "https://gitlab.com/o/r/-/commit/aaaabbbb"}],
		"project": {"path_with_namespace": "o/r", "web_url": "https://gitlab.com/o/r"},
		"user_username": "alice"
	}`)
	w := pushAdapterRaw(handler, whID, token, "gitlab", body, map[string]string{
		"X-Gitlab-Event": "Push Hook",
		"X-Gitlab-Token": token,
	})
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid push event must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), "message_id")
	}
}

// GitLab X-Gitlab-Token 与 URL token 不匹配 → 401（即便 URL token 正确）。
func TestPush_GitLabBadGitlabToken_Unauthorized(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "gitlab",
		[]byte(`{"ref":"refs/heads/main","total_commits_count":1,"commits":[{"id":"a","message":"m","url":"u"}],"user_username":"a"}`),
		map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "not-the-url-token"})
	assert.Equalf(t, http.StatusUnauthorized, w.Code, "mismatched X-Gitlab-Token must 401; body=%s", w.Body.String())
}

// GitLab 缺事件头 → 400 invalid（误配置要立刻可见）。X-Gitlab-Token 正确以越过鉴权闸。
func TestPush_GitLabMissingEventHeader_Invalid(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "gitlab", []byte(`{}`),
		map[string]string{"X-Gitlab-Token": token})
	assert.Equalf(t, http.StatusBadRequest, w.Code, "missing X-Gitlab-Event must 400; body=%s", w.Body.String())
}

// GitLab 渲染子集之外的事件：200 + skipped。
func TestPush_GitLabUnsupportedEvent_Skipped(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "gitlab", []byte(`{}`),
		map[string]string{"X-Gitlab-Event": "Wiki Page Hook", "X-Gitlab-Token": token})
	require.Equalf(t, http.StatusOK, w.Code, "unsupported event must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`)
}

// GitLab token 不匹配落审计（reason=token），管理员可在 deliveries 里定位配置错误。
func TestPush_GitLabBadGitlabToken_Audited(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "gitlab",
		[]byte(`{"ref":"refs/heads/main"}`),
		map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong"})
	require.Equal(t, http.StatusUnauthorized, w.Code)

	require.Eventually(t, func() bool {
		dw := do(handler, authReq("GET", fmt.Sprintf("/v1/groups/%s/incoming-webhooks/%s/deliveries", groupNo, whID), nil))
		if dw.Code != http.StatusOK {
			return false
		}
		list, _ := parseJSON(t, dw)["list"].([]interface{})
		for _, item := range list {
			row, _ := item.(map[string]interface{})
			if row["adapter"] == "gitlab" && row["reason"] == "token" && int(row["http_status"].(float64)) == http.StatusUnauthorized {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "token mismatch must be recorded as a failed delivery")
}

// 飞书 text 通过鉴权/翻译；成功响应附带 code=0（平台 SDK 兼容）。
func TestPush_FeishuText_Delivers(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "feishu",
		[]byte(`{"msg_type":"text","content":{"text":"hello from feishu"}}`), nil)
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid feishu text must translate; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize; body=%s", w.Body.String())
	if w.Code == http.StatusOK {
		assert.Contains(t, w.Body.String(), `"code":0`)
	}
}

// 飞书素材类消息（image）→ 400 invalid（显式失败优于静默丢弃）。
func TestPush_FeishuImage_Rejected(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	w := pushAdapterRaw(handler, whID, token, "feishu",
		[]byte(`{"msg_type":"image","content":{"image_key":"img_v2_xxx"}}`), nil)
	assert.Equalf(t, http.StatusBadRequest, w.Code, "feishu image must 400; body=%s", w.Body.String())
}

// 适配器路由沿用同一鉴权：错 URL token 统一 401（gitlab/feishu 与 native 同口径）。
func TestPush_Phase4Route_AuthEnforced(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	w1 := pushAdapterRaw(handler, whID, "wrong-token", "gitlab",
		[]byte(`{"ref":"refs/heads/main"}`),
		map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong-token"})
	assert.Equalf(t, http.StatusUnauthorized, w1.Code, "gitlab: bad url token must 401; body=%s", w1.Body.String())

	w2 := pushAdapterRaw(handler, whID, "wrong-token", "feishu",
		[]byte(`{"msg_type":"text","content":{"text":"hi"}}`), nil)
	assert.Equalf(t, http.StatusUnauthorized, w2.Code, "feishu: bad url token must 401; body=%s", w2.Body.String())
}
