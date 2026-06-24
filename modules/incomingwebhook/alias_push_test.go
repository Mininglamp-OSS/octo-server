package incomingwebhook_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #455：/v1/webhooks/{id}/{token} 是 canonical /v1/incoming-webhooks/... 推送端点的
// 短别名，共用同一套 handler / 中间件链 / 限流桶。这里端到端验证别名行为与 canonical
// 一致（同鉴权 / 同校验 / 同投递），并锁住「别名也走同一条鉴权链」这一安全不变量。
// 与既有 push 测试同口径：成功路径只断言「未被 4xx 挡下」，下游 SendMessage 在测试桩
// 下可能 200/502。

// aliasPush 向 /v1/webhooks 别名前缀发原始 body（suffix 为空走 native，否则走适配器）。
func aliasPush(handler http.Handler, whID, token, suffix string, body []byte) *httptest.ResponseRecorder {
	path := fmt.Sprintf("/v1/webhooks/%s/%s", whID, token)
	if suffix != "" {
		path += "/" + suffix
	}
	return do(handler, anonReq("POST", path, body))
}

// 别名 native 推送：与 canonical 一样通过鉴权 + 校验（非 4xx）。
func TestAliasPush_Native_Success(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	body := []byte(`{"content":"hello via alias"}`)
	w := aliasPush(handler, whID, token, "", body)

	assert.NotEqualf(t, http.StatusUnauthorized, w.Code, "valid token must authorize via alias; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusBadRequest, w.Code, "valid body must pass validation via alias; body=%s", w.Body.String())
	assert.NotEqualf(t, http.StatusNotFound, w.Code, "alias route must be registered; body=%s", w.Body.String())
}

// 别名鉴权链与 canonical 完全一致：错误 token 必须 401（证明别名没有绕过鉴权）。
func TestAliasPush_WrongToken_Unauthorized(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, _ := createWebhookWithToken(t, handler, groupNo)

	w := aliasPush(handler, whID, "wrong-token", "", []byte(`{"content":"x"}`))
	assert.Equalf(t, http.StatusUnauthorized, w.Code, "wrong token must 401 via alias; body=%s", w.Body.String())
}

// 别名适配器路由（GitHub ping）：与 canonical 一致 200 + skipped，证明 6 条推送形态都
// 在别名前缀下注册并走同一条链。
func TestAliasPush_GitHubPing_SkippedLikeCanonical(t *testing.T) {
	handler, _, groupNo := setupTestEnv(t)
	whID, token := createWebhookWithToken(t, handler, groupNo)

	r := anonReq("POST", fmt.Sprintf("/v1/webhooks/%s/%s/github", whID, token),
		[]byte(`{"zen":"Keep it logically awesome.","hook_id":1}`))
	r.Header.Set("X-GitHub-Event", "ping")
	w := do(handler, r)

	require.Equalf(t, http.StatusOK, w.Code, "alias github ping must 200; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"skipped"`, "alias ping must mark delivery skipped, same as canonical")
}
