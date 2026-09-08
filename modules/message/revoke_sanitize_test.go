package message

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

// TestSanitizeRevokedPayloadBytes 校验 bot 拉历史路径复用的 []byte 版占位构造器：
// 只保留数字 type、剥离一切正文，并在 payload 不可解析时 fail-closed。
func TestSanitizeRevokedPayloadBytes(t *testing.T) {
	parse := func(t *testing.T, b []byte) map[string]interface{} {
		t.Helper()
		var m map[string]interface{}
		assert.NoError(t, json.Unmarshal(b, &m))
		return m
	}

	t.Run("strips content, keeps only numeric type", func(t *testing.T) {
		in := []byte(`{"type":1,"content":"secret original text","url":"https://x/private.png"}`)
		out := parse(t, SanitizeRevokedPayloadBytes(in))
		assert.Equal(t, float64(common.Text.Int()), out["type"])
		_, hasContent := out["content"]
		_, hasURL := out["url"]
		assert.False(t, hasContent, "content must be stripped")
		assert.False(t, hasURL, "url must be stripped")
		assert.Len(t, out, 1, "only type should survive")
	})

	t.Run("non-scalar type carrying a body falls back to ContentError", func(t *testing.T) {
		// 把正文藏进 type（对象/字符串/数组）不能绕过脱敏。
		for _, in := range [][]byte{
			[]byte(`{"type":{"nested":"secret"}}`),
			[]byte(`{"type":"secret as string"}`),
			[]byte(`{"type":["secret"]}`),
		} {
			out := parse(t, SanitizeRevokedPayloadBytes(in))
			assert.Equal(t, float64(common.ContentError.Int()), out["type"])
			assert.Len(t, out, 1)
		}
	})

	t.Run("fail-closed on unparseable / empty payload", func(t *testing.T) {
		for _, in := range [][]byte{
			[]byte(`not json at all`),
			[]byte(`{"type":1,`), // truncated
			[]byte(``),
			nil,
		} {
			out := parse(t, SanitizeRevokedPayloadBytes(in))
			// 绝不透传原文；缺失 type 归一为 ContentError。
			assert.Equal(t, float64(common.ContentError.Int()), out["type"])
			assert.Len(t, out, 1)
		}
	})
}

// TestRevokeSanitizeConsistency 保证 bot 路径（[]byte）与 from() 路径（MsgSyncResp）
// 的撤回占位口径一致——两者都经 revokedPayload 单一来源，所以对同一原始 payload
// 产出等价的占位 type。这是 WS-168「回归保护：channel/sync 与 bot sync 行为一致」的
// 结构性单测锚点。
func TestRevokeSanitizeConsistency(t *testing.T) {
	original := map[string]interface{}{"type": common.Text.Int(), "content": "hi"}

	// from() 路径
	m := &MsgSyncResp{Payload: map[string]interface{}{"type": common.Text.Int(), "content": "hi"}, SignalPayload: "cipher"}
	sanitizeRevokedMsgSyncResp(m)
	assert.Equal(t, revokedPayload(original)["type"], m.Payload["type"])
	assert.Empty(t, m.SignalPayload)
	assert.Nil(t, m.Streams)

	// bot 路径
	raw, err := json.Marshal(original)
	assert.NoError(t, err)
	var botOut map[string]interface{}
	assert.NoError(t, json.Unmarshal(SanitizeRevokedPayloadBytes(raw), &botOut))
	assert.Equal(t, float64(common.Text.Int()), botOut["type"])
	_, hasContent := botOut["content"]
	assert.False(t, hasContent)
}
