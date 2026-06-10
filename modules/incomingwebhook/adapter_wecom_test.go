package incomingwebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 企业微信适配器纯翻译单测（无 DB/Redis/IM 依赖）。fixture 即企微群机器人文档的
// 出站消息格式——迁移调用方发什么，这里就收什么。

func TestParseWeComPush_TextAndMarkdown(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		body := `{"msgtype":"text","text":{"content":"hello","mentioned_list":["@all"]}}`
		req, skip, invalid := parseWeComPush(nil, []byte(body))
		require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
		assert.Equal(t, "hello", req.Content)
		assert.Empty(t, req.MsgType, "adapters emit the plain-text path")
	})
	t.Run("markdown", func(t *testing.T) {
		body := `{"msgtype":"markdown","markdown":{"content":"**bold** msg"}}`
		req, _, _ := parseWeComPush(nil, []byte(body))
		require.NotNil(t, req)
		assert.Equal(t, "**bold** msg", req.Content)
	})
	t.Run("markdown_v2", func(t *testing.T) {
		body := `{"msgtype":"markdown_v2","markdown_v2":{"content":"# title"}}`
		req, _, _ := parseWeComPush(nil, []byte(body))
		require.NotNil(t, req)
		assert.Equal(t, "# title", req.Content)
	})
}

func TestParseWeComPush_News(t *testing.T) {
	body := `{"msgtype":"news","news":{"articles":[
		{"title":"Release v1", "description":"changelog", "url":"https://example.com/v1", "picurl":"https://example.com/p.png"},
		{"title":"", "description":"skipped: empty title"},
		{"title":"No link article"}
	]}}`
	req, _, _ := parseWeComPush(nil, []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "[**Release v1**](https://example.com/v1)")
	assert.Contains(t, req.Content, "changelog")
	assert.Contains(t, req.Content, "**No link article**")
	assert.NotContains(t, req.Content, "empty title", "articles without a title are dropped")
	assert.NotContains(t, req.Content, "picurl", "cover images are dropped")
}

func TestParseWeComPush_TemplateCard(t *testing.T) {
	body := `{"msgtype":"template_card","template_card":{
		"card_type":"text_notice",
		"main_title":{"title":"Deploy done","desc":"prod cluster"},
		"sub_title_text":"all pods healthy",
		"card_action":{"type":1,"url":"https://example.com/deploy/1"}
	}}`
	req, _, _ := parseWeComPush(nil, []byte(body))
	require.NotNil(t, req)
	assert.Contains(t, req.Content, "**Deploy done**")
	assert.Contains(t, req.Content, "prod cluster")
	assert.Contains(t, req.Content, "all pods healthy")
	assert.Contains(t, req.Content, "https://example.com/deploy/1")
}

func TestParseWeComPush_Rejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // expected invalid reason
	}{
		{"malformed json", `{not json`, "json"},
		{"image relies on platform media", `{"msgtype":"image","image":{"base64":"...","md5":"..."}}`, "msg_type"},
		{"file relies on platform media", `{"msgtype":"file","file":{"media_id":"x"}}`, "msg_type"},
		{"missing msgtype", `{"text":{"content":"hi"}}`, "msg_type"},
		{"text without payload", `{"msgtype":"text"}`, "content"},
		{"text with blank content", `{"msgtype":"text","text":{"content":"  "}}`, "content"},
		{"news with no usable article", `{"msgtype":"news","news":{"articles":[{"description":"no title"}]}}`, "content"},
		{"template_card with nothing to render", `{"msgtype":"template_card","template_card":{}}`, "content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, skip, invalid := parseWeComPush(nil, []byte(tc.body))
			assert.Nil(t, req)
			assert.Empty(t, skip, "wecom adapter never skips: the sender controls the body")
			assert.Equal(t, tc.want, invalid)
		})
	}
}
