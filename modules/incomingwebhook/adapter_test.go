package incomingwebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 适配层公共件的纯单测（无 DB/Redis/IM 依赖）：native parse、成功响应体、文本工具。

func TestParseNativePush(t *testing.T) {
	t.Run("valid json", func(t *testing.T) {
		req, skip, invalid := parseNativePush(nil, []byte(`{"content":"hi","msg_type":"text"}`))
		require.NotNil(t, req)
		assert.Empty(t, skip)
		assert.Empty(t, invalid)
		assert.Equal(t, "hi", req.Content)
		assert.Equal(t, "text", req.MsgType)
	})
	t.Run("malformed json", func(t *testing.T) {
		req, skip, invalid := parseNativePush(nil, []byte(`{not json`))
		assert.Nil(t, req)
		assert.Empty(t, skip)
		assert.Equal(t, "json", invalid)
	})
}

func TestSuccessBody(t *testing.T) {
	t.Run("native delivered", func(t *testing.T) {
		body := successBody(nativeAdapter, 42, "")
		assert.Equal(t, 0, body["status"])
		assert.Equal(t, int64(42), body["message_id"])
		assert.NotContains(t, body, "skipped")
		assert.NotContains(t, body, "errcode")
	})
	t.Run("skip carries reason", func(t *testing.T) {
		body := successBody(githubAdapter, 0, "ping")
		assert.Equal(t, "ping", body["skipped"])
		assert.Equal(t, int64(0), body["message_id"])
	})
	t.Run("wecom carries platform compat fields", func(t *testing.T) {
		body := successBody(wecomAdapter, 7, "")
		assert.Equal(t, 0, body["errcode"])
		assert.Equal(t, "ok", body["errmsg"])
		assert.Equal(t, int64(7), body["message_id"])
	})
}

func TestClipRunes(t *testing.T) {
	assert.Equal(t, "abc", clipRunes("abc", 5), "short string passes through")
	assert.Equal(t, "ab…", clipRunes("abcdef", 3), "clip ends with ellipsis")
	assert.Equal(t, "", clipRunes("abc", 0), "non-positive max yields empty")
	// 多字节字符按 rune 截断，不会切出半个字符。
	assert.Equal(t, "中文…", clipRunes("中文字符串", 3))
}

func TestFirstLineAndOneLine(t *testing.T) {
	assert.Equal(t, "fix: bug", firstLine("fix: bug\n\nlong body"))
	assert.Equal(t, "no newline", firstLine("no newline"))
	assert.Equal(t, "a b c", oneLine("a\r\nb\nc"))
}

func TestSafeMarkdownURL(t *testing.T) {
	ok := []string{
		"https://app.multica.ai/acme/issues/MUL-1",
		"http://localhost:3000/x/issues/Y-2",
		"https://app.multica.ai/acme%20corp/issues/MUL%207", // pre-escaped segments
	}
	for _, u := range ok {
		got, valid := safeMarkdownURL(u)
		assert.Truef(t, valid, "%q should be accepted", u)
		assert.Equal(t, u, got)
	}

	bad := []string{
		"",
		"javascript:alert(1)",                       // non-http scheme
		"data:text/html,x",                          // non-http scheme
		"https://ok.com/) [phish](https://evil.com", // link-destination injection
		"https://ok.com/issue)",                     // trailing paren ends destination
		"https://ok.com/a b",                        // embedded space
		"https://ok.com/a\nb",                       // newline
		"https://ok.com/<script>",                   // angle brackets
		"https://ok.com/]x",                         // bracket
		"https://",                                  // no host
		"https://例子.com/x",                          // non-ASCII (IDN) — reject rather than mis-render
	}
	for _, u := range bad {
		got, valid := safeMarkdownURL(u)
		assert.Falsef(t, valid, "%q should be rejected", u)
		assert.Empty(t, got)
	}
}
