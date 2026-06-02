// Package webhook · 图文混排 RichText(=14) 推送 alert 单测。
//
// 任务背景：getMessageAlert 的 switch 原本无 type=14 case，14 消息推送正文为空
// 字符串。这里锁定新增的 RichText case 用 common.GetRichTextDisplayText 从 server
// 权威 plain 生成推送正文，并验证 14 已进入 getSupportTypes（否则 pushTo 在
// containSupportType 处就把 14 丢弃，永远到不了 getMessageAlert）。
package webhook

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// makeRichTextOfflineMsg builds a PERSONAL msgOfflineNotify carrying a RichText
// payload, with both PayloadMap (for the gate) and Payload bytes (the alert
// builder reads the raw bytes via GetRichTextDisplayText).
func makeRichTextOfflineMsg(t *testing.T, payloadJSON string) msgOfflineNotify {
	t.Helper()
	var m msgOfflineNotify
	m.ChannelType = common.ChannelTypePerson.Uint8()
	m.Payload = []byte(payloadJSON)
	dec := json.NewDecoder(bytes.NewBufferString(payloadJSON))
	dec.UseNumber() // 复刻 ingress 解码（type 为 json.Number），命中 alert switch 的 json.Number 分支
	var pm map[string]interface{}
	if err := dec.Decode(&pm); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	m.PayloadMap = pm
	return m
}

func TestGetSupportTypes_IncludesRichText(t *testing.T) {
	found := false
	for _, ct := range getSupportTypes() {
		if ct == common.RichText {
			found = true
			break
		}
	}
	assert.True(t, found, "RichText=14 必须在 getSupportTypes，否则 pushTo 丢弃 14 消息推不出")
}

func TestGetMessageAlert_RichText_UsesAuthoritativePlain(t *testing.T) {
	ctx := config.NewContext(config.New()) // ContentDetailOn defaults true
	toUser := &user.Resp{MsgShowDetail: 1}

	msg := makeRichTextOfflineMsg(t, `{"type":14,"plain":"看这张图 [图片] 谢谢","content":[
		{"type":"text","text":"看这张图 "},
		{"type":"image","url":"https://x/y.png","width":10,"height":10},
		{"type":"text","text":" 谢谢"}
	]}`)

	alert, err := getMessageAlert(msg, toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "看这张图 [图片] 谢谢", alert)
}

func TestGetMessageAlert_RichText_FallbackBuildsFromContent(t *testing.T) {
	// plain 缺失时 GetRichTextDisplayText 现场遍历 content 生成。
	ctx := config.NewContext(config.New())
	toUser := &user.Resp{MsgShowDetail: 1}

	msg := makeRichTextOfflineMsg(t, `{"type":14,"content":[
		{"type":"text","text":"hi"},
		{"type":"image","url":"https://x/y.png","width":1,"height":1}
	]}`)

	alert, err := getMessageAlert(msg, toUser, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "hi"+common.RichTextImagePlaceholder, alert)
}
