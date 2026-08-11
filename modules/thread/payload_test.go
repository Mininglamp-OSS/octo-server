package thread

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
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
		"content 需以 {0} 占位符开头，客户端用 extra[0] 渲染 operator 名")
	assert.Equal(t, "uid_op", payload["from_uid"])

	extra, ok := payload["extra"].([]config.UserBaseVo)
	assert.True(t, ok, "extra 应为 []config.UserBaseVo，供客户端解析 {0}")
	assert.Len(t, extra, 1)
	assert.Equal(t, "uid_op", extra[0].UID)
	assert.Equal(t, "操作者", extra[0].Name)
}

// TestIsSystemContentType 锁定 onMessages 系统消息过滤边界：普通用户消息（<1000）进解档/统计
// 路径，系统通知（Tip / 群通知 / 客服 / 通话结果，均 >=1000）被跳过。这是 P1 修复的核心：
// 防止子区改名 / 置顶 tip 静默解档归档子区、抬高 message_count、覆盖 preview、自动加入 operator。
func TestIsSystemContentType(t *testing.T) {
	// 普通用户消息（sendSourceMessage 拷贝到子区的源消息也在此区间）→ 不是系统消息
	for _, ct := range []common.ContentType{common.Text, common.Image, common.Voice, common.Video, common.File, common.RichText} {
		assert.False(t, isSystemContentType(int(ct)), "content type %d 应视为普通用户消息", ct)
	}
	// 系统通知类 → 是系统消息，onMessages 应跳过
	for _, ct := range []common.ContentType{common.Tip, common.FriendApply, common.GroupCreate, common.GroupUpdate, common.RevokeMessage} {
		assert.True(t, isSystemContentType(int(ct)), "content type %d 应视为系统消息", ct)
	}
	// 缺省 / 无法解析的 type 视为普通消息（不会误跳过真实消息）
	assert.False(t, isSystemContentType(0))
}

// TestParsePayloadType 校验从 payload 提取 content type：改名 tip 的 payload 必须被识别为
// 系统消息，从而在 onMessages 里被过滤掉——这是 P1 副作用（解档 / message_count / preview /
// 自动加入）的闭环断言，无需 DB / IM 即可锁定。
func TestParsePayloadType(t *testing.T) {
	renamePayload := []byte(util.ToJson(buildThreadRenamedPayload("uid_op", "操作者", "新子区名")))
	assert.Equal(t, int(common.Tip), parsePayloadType(renamePayload), "改名 tip 应解析出 Tip 类型")
	assert.True(t, isSystemContentType(parsePayloadType(renamePayload)),
		"改名 tip 必须被 onMessages 判为系统消息并跳过，否则会解档/抬 count/覆盖 preview")

	// 普通文本消息
	assert.Equal(t, 1, parsePayloadType([]byte(`{"type":1,"content":"hello"}`)))
	// 缺省 / 空 / 非法 payload → 0
	assert.Equal(t, 0, parsePayloadType([]byte(`{"content":"no type"}`)))
	assert.Equal(t, 0, parsePayloadType(nil))
	assert.Equal(t, 0, parsePayloadType([]byte(`not json`)))
}

// TestShouldProcessThreadMessage 在 listener 决策层（onMessages 用到的同一判定）锁定不变式：
// 只有子区频道里的用户聊天正文才触发「解档 / message_count / preview / 自动加入」；子区改名 tip
// 等系统消息必须被跳过。相比只喂 parsePayloadType 的纯单测，这里用真实 config.MessageResp
// 覆盖了 channel-type 门 + content-type 门的组合，未来 listener 重接线若漏掉过滤会在此失败。
func TestShouldProcessThreadMessage(t *testing.T) {
	topic := common.ChannelTypeCommunityTopic.Uint8()
	renameTip := []byte(util.ToJson(buildThreadRenamedPayload("uid_op", "操作者", "新子区名")))

	tests := []struct {
		name string
		msg  *config.MessageResp
		want bool
	}{
		{"nil message", nil, false},
		{"non-topic channel (group) skipped", &config.MessageResp{ChannelType: common.ChannelTypeGroup.Uint8(), Payload: []byte(`{"type":1,"content":"hi"}`)}, false},
		{"topic + rename Tip skipped", &config.MessageResp{ChannelType: topic, ChannelID: "g____s", FromUID: "uid_op", Payload: renameTip}, false},
		{"topic + generic system type skipped", &config.MessageResp{ChannelType: topic, Payload: []byte(`{"type":1005}`)}, false},
		{"topic + user text processed", &config.MessageResp{ChannelType: topic, ChannelID: "g____s", FromUID: "uid_u", Payload: []byte(`{"type":1,"content":"hello"}`)}, true},
		{"topic + rich text processed", &config.MessageResp{ChannelType: topic, Payload: []byte(`{"type":14,"content":"rich"}`)}, true},
		{"topic + unparseable payload processed (fail-safe)", &config.MessageResp{ChannelType: topic, Payload: []byte(`not json`)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldProcessThreadMessage(tt.msg))
		})
	}
}
