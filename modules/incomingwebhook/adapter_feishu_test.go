package incomingwebhook

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 飞书适配器纯翻译单测（无 DB/Redis/IM 依赖）。feishu* 结构体是白名单解析，多余字段
// （含 timestamp/sign）都被忽略。

func TestParseFeishuPush_Text(t *testing.T) {
	body := `{"msg_type":"text","content":{"text":"**Build #123** passed ✅"},"timestamp":"1700000000","sign":"xxx"}`
	req, skip, invalid := parseFeishuPush(http.Header{}, []byte(body))
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Equal(t, "**Build #123** passed ✅", req.Content)
	assert.Empty(t, req.MsgType, "feishu emits the plain-text path")
}

func TestParseFeishuPush_Post(t *testing.T) {
	body := `{
		"msg_type": "post",
		"content": {"post": {"zh_cn": {
			"title": "项目更新",
			"content": [
				[{"tag":"text","text":"第一行 "},{"tag":"a","text":"链接","href":"https://example.com"}],
				[{"tag":"at","user_name":"someone"},{"tag":"text","text":" 请查收"}],
				[{"tag":"img","image_key":"img_v2_xxx"}]
			]
		}}}
	}`
	req, skip, invalid := parseFeishuPush(http.Header{}, []byte(body))
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, "**项目更新**")
	assert.Contains(t, req.Content, "第一行 [链接](https://example.com)")
	assert.Contains(t, req.Content, "@someone 请查收")
	assert.NotContains(t, req.Content, "img_v2_xxx", "image_key tags are dropped (cannot be re-hosted)")
}

// a-tag 的 href 必须是 http(s)：危险 scheme（javascript:/data:）降级为纯文本，不渲染
// 成投递给群内其它成员的可点击链接（#423 review，Jerry-Xin/mochashanyao）。
func TestParseFeishuPush_PostRejectsUnsafeHref(t *testing.T) {
	for _, scheme := range []string{"javascript:alert(1)", "data:text/html,evil", "  JavaScript:alert(1)"} {
		body := fmt.Sprintf(`{"msg_type":"post","content":{"post":{"zh_cn":{"content":[[{"tag":"a","text":"click","href":%q}]]}}}}`, scheme)
		req, _, _ := parseFeishuPush(http.Header{}, []byte(body))
		require.NotNil(t, req, "scheme=%q", scheme)
		assert.Equal(t, "click", req.Content, "unsafe-scheme href must degrade to plain text; scheme=%q", scheme)
		assert.NotContains(t, req.Content, "](", "no markdown link emitted for unsafe scheme; scheme=%q", scheme)
	}
}

// post 的 at user_name 是自由文本，进 `@X` 必须经 mdInertText 转义。
func TestParseFeishuPush_PostAtNameEscaped(t *testing.T) {
	body := `{"msg_type":"post","content":{"post":{"zh_cn":{"content":[[{"tag":"at","user_name":"**ev** [x](http://a)"}]]}}}}`
	req, _, _ := parseFeishuPush(http.Header{}, []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, `@\*\*ev\*\*`, "at name bold markers must be escaped")
	assert.Contains(t, req.Content, `\[x\]`, "at name brackets must be escaped")
}

func TestParseFeishuPush_PostLocaleFallback(t *testing.T) {
	t.Run("en_us when zh_cn absent", func(t *testing.T) {
		body := `{"msg_type":"post","content":{"post":{"en_us":{"title":"Update","content":[[{"tag":"text","text":"hello"}]]}}}}`
		req, _, _ := parseFeishuPush(http.Header{}, []byte(body))
		require.NotNil(t, req)
		assert.Contains(t, req.Content, "**Update**")
		assert.Contains(t, req.Content, "hello")
	})
	t.Run("a tag without href degrades to plain text", func(t *testing.T) {
		body := `{"msg_type":"post","content":{"post":{"zh_cn":{"content":[[{"tag":"a","text":"裸链接"}]]}}}}`
		req, _, _ := parseFeishuPush(http.Header{}, []byte(body))
		require.NotNil(t, req)
		assert.Equal(t, "裸链接", req.Content)
	})
}

func TestParseFeishuPush_Interactive(t *testing.T) {
	body := `{
		"msg_type": "interactive",
		"card": {
			"header": {"title": {"content": "告警"}},
			"elements": [
				{"tag": "div", "text": {"content": "CPU 超过阈值"}},
				{"tag": "markdown", "content": "**节点**: node-1"},
				{"tag": "action", "actions": [{"tag":"button","text":{"content":"忽略"}}]},
				{"tag": "img", "img_key": "img_xxx"}
			]
		}
	}`
	req, skip, invalid := parseFeishuPush(http.Header{}, []byte(body))
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	assert.Contains(t, req.Content, "**告警**")
	assert.Contains(t, req.Content, "CPU 超过阈值")
	assert.Contains(t, req.Content, "**节点**: node-1")
	assert.NotContains(t, req.Content, "忽略", "button/action elements are dropped")
	assert.NotContains(t, req.Content, "img_xxx", "image elements are dropped")
}

func TestParseFeishuPush_Rejected(t *testing.T) {
	t.Run("image type rejected", func(t *testing.T) {
		body := `{"msg_type":"image","content":{"image_key":"img_v2_xxx"}}`
		req, skip, invalid := parseFeishuPush(http.Header{}, []byte(body))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		assert.Equal(t, "msg_type", invalid)
	})
	t.Run("share_chat type rejected", func(t *testing.T) {
		body := `{"msg_type":"share_chat","content":{"share_chat_id":"oc_xxx"}}`
		req, _, invalid := parseFeishuPush(http.Header{}, []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "msg_type", invalid)
	})
	t.Run("empty msg_type rejected", func(t *testing.T) {
		body := `{"content":{"text":"hi"}}`
		req, _, invalid := parseFeishuPush(http.Header{}, []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "msg_type", invalid)
	})
	t.Run("empty rendered content rejected", func(t *testing.T) {
		body := `{"msg_type":"text","content":{"text":"   "}}`
		req, _, invalid := parseFeishuPush(http.Header{}, []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "content", invalid)
	})
	t.Run("malformed body is invalid json", func(t *testing.T) {
		req, _, invalid := parseFeishuPush(http.Header{}, []byte(`{not json`))
		assert.Nil(t, req)
		assert.Equal(t, "json", invalid)
	})
	t.Run("post with only dropped img is empty content", func(t *testing.T) {
		body := `{"msg_type":"post","content":{"post":{"zh_cn":{"content":[[{"tag":"img","image_key":"k"}]]}}}}`
		req, _, invalid := parseFeishuPush(http.Header{}, []byte(body))
		assert.Nil(t, req)
		assert.Equal(t, "content", invalid)
	})
}
