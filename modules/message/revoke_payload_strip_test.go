package message

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

// TestRevokedPayload 校验 revokedPayload 只保留 type、剥离一切内容承载字段。
func TestRevokedPayload(t *testing.T) {
	t.Run("strips content and every non-type field", func(t *testing.T) {
		out := revokedPayload(map[string]interface{}{
			"type":    common.Text.Int(),
			"content": "secret original text",
			"url":     "https://example.com/private.png",
			"name":    "私密文件.pdf",
			"reply":   map[string]interface{}{"content": "quoted secret"},
		})
		assert.Equal(t, common.Text.Int(), out["type"])
		assert.Len(t, out, 1, "only type should remain")
		for _, k := range []string{"content", "url", "name", "reply"} {
			_, ok := out[k]
			assert.Falsef(t, ok, "%q must be stripped", k)
		}
	})

	t.Run("missing type falls back to ContentError", func(t *testing.T) {
		out := revokedPayload(map[string]interface{}{"content": "x"})
		assert.Equal(t, common.ContentError.Int(), out["type"])
		assert.Len(t, out, 1)
	})

	// 安全回归：type 是不可信调用方数据，send 路径不约束其为数字标量。若把正文藏进
	// 非标量 type（字符串 / 对象 / 数组），必须被规范化为 ContentError 而非原样透传，
	// 否则撤回脱敏可被绕过（D23 整改 / PR #628 review by Jerry-Xin + yujiawei）。
	t.Run("non-scalar type carrying content is normalized, never leaked", func(t *testing.T) {
		const secret = "secret hidden in type field"
		cases := map[string]interface{}{
			"string type": secret,
			"object type": map[string]interface{}{"nested": secret},
			"array type":  []interface{}{secret},
			"bool type":   true,
		}
		for name, badType := range cases {
			t.Run(name, func(t *testing.T) {
				out := revokedPayload(map[string]interface{}{"type": badType, "content": secret})
				assert.Equal(t, common.ContentError.Int(), out["type"], "non-scalar type must fall back to ContentError")
				assert.Len(t, out, 1)
				body, err := json.Marshal(out)
				assert.NoError(t, err)
				assert.NotContains(t, string(body), secret, "no content may survive via the type field")
			})
		}
	})

	// 合法数字 type 的三种反序列化结果都必须原样保留（不会被误判为 ContentError）。
	t.Run("numeric type is preserved across float64/int/json.Number", func(t *testing.T) {
		assert.Equal(t, common.Text.Int(), revokedPayload(map[string]interface{}{"type": float64(common.Text.Int())})["type"])
		assert.Equal(t, common.Text.Int(), revokedPayload(map[string]interface{}{"type": common.Text.Int()})["type"])
		assert.Equal(t, common.Text.Int(), revokedPayload(map[string]interface{}{"type": json.Number("1")})["type"])
	})
}

// TestMsgSyncRespFrom_RevokedStripsContent 是核心回归：撤回消息经 from()
// 组装后，原始正文不得出现在下发的任何字段里，而 revoke/revoker 元数据保留。
// 覆盖 /v1/message/channel/sync 与 /v1/conversation/sync（两者共用 from()）。
func TestMsgSyncRespFrom_RevokedStripsContent(t *testing.T) {
	const secret = "top secret revoked message body"
	payload, err := json.Marshal(map[string]interface{}{
		"type":    common.Text.Int(),
		"content": secret,
	})
	assert.NoError(t, err)

	msgResp := &config.MessageResp{
		MessageID:   12345,
		MessageSeq:  10,
		FromUID:     "sender1",
		ChannelID:   "chan1",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
	}
	extra := &messageExtraDetailModel{}
	extra.MessageID = "12345"
	extra.Revoke = 1
	extra.Revoker = "admin1"
	extra.Version = 7

	m := &MsgSyncResp{}
	m.from(msgResp, "viewer1", extra, nil, nil, 0)

	assert.Equal(t, 1, m.Revoke, "revoke flag must be preserved for the tip")
	assert.Equal(t, "admin1", m.Revoker, "revoker must be preserved for the tip")

	_, hasContent := m.Payload["content"]
	assert.False(t, hasContent, "revoked message must not ship original content")
	assert.Len(t, m.Payload, 1, "only type should remain in the payload")

	// Belt-and-suspenders: the secret must not appear anywhere in the wire body.
	body, err := json.Marshal(m)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), secret,
		"revoked original content leaked into the sync response")
}

// TestMsgSyncRespFrom_RevokedStripsContentEdit 校验「编辑后又撤回」的消息不再经
// message_extra.content_edit 下发编辑后的原文。
func TestMsgSyncRespFrom_RevokedStripsContentEdit(t *testing.T) {
	const editedSecret = "edited secret body that was later revoked"
	payload, err := json.Marshal(map[string]interface{}{"type": common.Text.Int(), "content": "orig"})
	assert.NoError(t, err)

	msgResp := &config.MessageResp{
		MessageID:   555,
		MessageSeq:  6,
		FromUID:     "sender1",
		ChannelID:   "chan1",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
	}
	extra := &messageExtraDetailModel{}
	extra.MessageID = "555"
	extra.Revoke = 1
	extra.Revoker = "admin1"
	extra.EditedAt = 1700000000
	contentEdit, err := json.Marshal(map[string]interface{}{"type": common.Text.Int(), "content": editedSecret})
	assert.NoError(t, err)
	extra.ContentEdit.Valid = true
	extra.ContentEdit.String = string(contentEdit)

	m := &MsgSyncResp{}
	m.from(msgResp, "viewer1", extra, nil, nil, 0)

	assert.Equal(t, 1, m.Revoke)
	if assert.NotNil(t, m.MessageExtra) {
		assert.Nil(t, m.MessageExtra.ContentEdit, "revoked message must not ship content_edit")
		assert.Equal(t, 0, m.MessageExtra.EditedAt)
	}
	body, err := json.Marshal(m)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), editedSecret,
		"revoked edited content leaked via content_edit")
}

// TestMsgSyncRespFrom_RevokedClearsSignalPayload 校验撤回的 signal 加密消息
// 不再下发密文 blob（signal_payload）。
func TestMsgSyncRespFrom_RevokedClearsSignalPayload(t *testing.T) {
	cipher := []byte("encrypted-cipher-bytes-of-revoked-msg")
	msgResp := &config.MessageResp{
		MessageID:   333,
		MessageSeq:  4,
		FromUID:     "sender1",
		ChannelID:   "chan1",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Setting:     config.Setting{Signal: true}.ToUint8(),
		Payload:     cipher,
	}
	extra := &messageExtraDetailModel{}
	extra.MessageID = "333"
	extra.Revoke = 1
	extra.Revoker = "admin1"

	m := &MsgSyncResp{}
	m.from(msgResp, "viewer1", extra, nil, nil, 0)

	assert.Equal(t, 1, m.Revoke)
	assert.Empty(t, m.SignalPayload, "revoked signal ciphertext must be cleared")

	body, err := json.Marshal(m)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), base64.StdEncoding.EncodeToString(cipher),
		"revoked signal ciphertext leaked into the sync response")
}

// TestMsgSyncRespFrom_RevokedClearsStreams 校验撤回的流式消息不再下发 stream blob。
func TestMsgSyncRespFrom_RevokedClearsStreams(t *testing.T) {
	const streamSecret = "streamed secret token"
	payload, err := json.Marshal(map[string]interface{}{"type": common.Text.Int(), "content": "x"})
	assert.NoError(t, err)

	msgResp := &config.MessageResp{
		MessageID:   444,
		MessageSeq:  5,
		FromUID:     "sender1",
		ChannelID:   "chan1",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
		Streams: []*config.StreamItemResp{
			{StreamSeq: 1, ClientMsgNo: "c1", Blob: []byte(`{"content":"` + streamSecret + `"}`)},
		},
	}
	extra := &messageExtraDetailModel{}
	extra.MessageID = "444"
	extra.Revoke = 1
	extra.Revoker = "admin1"

	m := &MsgSyncResp{}
	m.from(msgResp, "viewer1", extra, nil, nil, 0)

	assert.Nil(t, m.Streams, "revoked stream blobs must be cleared")
	body, err := json.Marshal(m)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), streamSecret)
}

// TestMsgSyncRespFrom_NotRevokedKeepsContent 反向回归：未撤回消息内容原样保留，
// 确保脱敏逻辑不误伤正常消息。
func TestMsgSyncRespFrom_NotRevokedKeepsContent(t *testing.T) {
	const visible = "normal visible message content"
	payload, err := json.Marshal(map[string]interface{}{
		"type":    common.Text.Int(),
		"content": visible,
	})
	assert.NoError(t, err)

	msgResp := &config.MessageResp{
		MessageID:   222,
		MessageSeq:  3,
		FromUID:     "sender1",
		ChannelID:   "chan1",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
	}

	// no extra -> not revoked
	m := &MsgSyncResp{}
	m.from(msgResp, "viewer1", nil, nil, nil, 0)

	assert.Equal(t, 0, m.Revoke)
	assert.Equal(t, visible, m.Payload["content"], "non-revoked content must be preserved")
}

// TestSelectSpaceLastMessage_SkipsRevoked 校验 space_last_message 兜底选取会跳过
// 已撤回消息，避免把撤回原文当作「最后一条消息」预览下发（/conversation/sync
// SpaceLastMessage 字段的残余泄漏点）。
func TestSelectSpaceLastMessage_SkipsRevoked(t *testing.T) {
	mk := func(id int64, spaceID string) *config.MessageResp {
		payload, _ := json.Marshal(map[string]interface{}{
			"type":     common.Text.Int(),
			"content":  "msg content",
			"space_id": spaceID,
		})
		return &config.MessageResp{MessageID: id, Payload: payload}
	}
	// 顺序（旧 -> 新）：#1 spaceA(正常), #2 spaceA(撤回)。倒序遍历先命中 #2。
	messages := []*config.MessageResp{mk(1, "spaceA"), mk(2, "spaceA")}

	t.Run("revoked newest is skipped, older non-revoked chosen", func(t *testing.T) {
		revoked := map[string]bool{"2": true}
		got := selectSpaceLastMessage(messages, revoked, "spaceA", "", false)
		if assert.NotNil(t, got) {
			assert.Equal(t, int64(1), got.MessageID, "must skip revoked #2 and return #1")
		}
	})

	t.Run("no revoked -> newest chosen", func(t *testing.T) {
		got := selectSpaceLastMessage(messages, map[string]bool{}, "spaceA", "", false)
		if assert.NotNil(t, got) {
			assert.Equal(t, int64(2), got.MessageID)
		}
	})

	t.Run("all revoked -> nil (nothing previewed)", func(t *testing.T) {
		revoked := map[string]bool{"1": true, "2": true}
		got := selectSpaceLastMessage(messages, revoked, "spaceA", "", false)
		assert.Nil(t, got, "no non-revoked space message must yield no preview")
	})
}

// TestRevokedMessageIDSet_NilDB 校验注入的 messageExtraDB 为空时返回空集合、nil error
// （不跳过），保证纯逻辑单测路径不依赖 DB。查询出错的 fail-closed 语义（返回 error →
// findSpaceLastMessageFallback 跳过预览兜底）依赖真实 DB，由 infra-gated 集成测试覆盖。
func TestRevokedMessageIDSet_NilDB(t *testing.T) {
	msgs := []*config.MessageResp{{MessageID: 1}, {MessageID: 2}}

	set, err := revokedMessageIDSet(msgs, nil)
	assert.NoError(t, err)
	assert.Empty(t, set)

	set, err = revokedMessageIDSet(nil, nil)
	assert.NoError(t, err)
	assert.Empty(t, set)
}
