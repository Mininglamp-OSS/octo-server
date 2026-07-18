package thread

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

func TestBuildThreadCreatedPayload_WithSourceMessageID(t *testing.T) {
	sourceMessageID := int64(2042876115227152384)
	sourcePayload := json.RawMessage(`{"type":1,"content":"hello"}`)

	payload := buildThreadCreatedPayload(
		"shortID123",
		"测试子区",
		"groupNo____shortID123",
		"uid_creator",
		"创建者",
		&sourceMessageID,
		sourcePayload,
	)

	assert.Equal(t, ContentTypeThreadCreated, payload["type"])
	assert.Equal(t, "shortID123", payload["short_id"])
	assert.Equal(t, sourceMessageID, payload["source_message_id"],
		"IM 推送的 payload 应包含 source_message_id")
	assert.Equal(t, int64(1), payload["message_count"])
	assert.NotNil(t, payload["last_message"])
}

func TestBuildThreadCreatedPayload_WithoutSourceMessageID(t *testing.T) {
	payload := buildThreadCreatedPayload(
		"shortID456",
		"无源消息子区",
		"groupNo____shortID456",
		"uid_creator",
		"创建者",
		nil,
		nil,
	)

	assert.Equal(t, ContentTypeThreadCreated, payload["type"])
	assert.Equal(t, "shortID456", payload["short_id"])
	_, hasSourceMsgID := payload["source_message_id"]
	assert.False(t, hasSourceMsgID,
		"source_message_id 为 nil 时不应出现在 payload 中")
	assert.Equal(t, int64(0), payload["message_count"])
}

// TestBuildThreadRenamedPayload 校验子区改名 Tip 消息 payload 与群改名对称：
// content 以 "{0}" 占位符开头（客户端替换成 operator 名），type=Tip，extra 携带 operator。
func TestBuildThreadRenamedPayload(t *testing.T) {
	payload := buildThreadRenamedPayload("uid_op", "操作者", "新子区名")

	assert.Equal(t, common.Tip, payload["type"], "子区改名必须是 Tip 类型（不可回复/不可@）")
	assert.Equal(t, `{0}修改子区名为"新子区名"`, payload["content"],
		"content 需以 {0} 占位符开头，与群改名 tip 一致")
	assert.Equal(t, "uid_op", payload["from_uid"])

	extra, ok := payload["extra"].([]config.UserBaseVo)
	assert.True(t, ok, "extra 应为 []config.UserBaseVo，供客户端解析 {0}")
	assert.Len(t, extra, 1)
	assert.Equal(t, "uid_op", extra[0].UID)
	assert.Equal(t, "操作者", extra[0].Name)
}
