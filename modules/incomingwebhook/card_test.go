package incomingwebhook

// card-message-protocol P1：msg_type:"card" 的 payload 构造与权威校验
// （buildCardPayload —— rollout gate / 白名单 / plain 派生 / Decision 8 的
// text 种子语义）。HTTP 层的 8KB body cap / token 鉴权由既有 push 测试覆盖。

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

func cardTestModel() *incomingWebhookModel {
	return &incomingWebhookModel{WebhookID: "iwh_test1", SpaceID: "sp_01"}
}

func TestBuildCardPayload(t *testing.T) {
	// rollout gate 关闭（缺省）→ errCardDisabled（fail-closed）
	t.Setenv(cardmsg.EnvEnabled, "")
	_, err := buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card:    map[string]interface{}{"type": "AdaptiveCard", "version": "1.5"},
	}, false)
	assert.ErrorIs(t, err, errCardDisabled)

	t.Setenv(cardmsg.EnvEnabled, "true")

	// 合法卡：服务端钉 octo/v1 信封、plain 派生自卡体（Decision 8）、from/space_id
	// 为服务端白名单字段
	p, err := buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card: map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "构建 #88 通过"},
			},
		},
		Text: "ignored seed",
	}, false)
	assert.NoError(t, err)
	assert.Equal(t, cardmsg.InteractiveCard.Int(), p["type"])
	assert.Equal(t, cardmsg.ProfileV1, p["profile"])
	assert.Equal(t, "构建 #88 通过", p["plain"], "卡体产出文本时 text 种子被忽略,派生权威")
	assert.Equal(t, "sp_01", p["space_id"])

	// Decision 8 text 种子：空 body 卡 → 派生为空 → 种子生效
	p, err = buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card:    map[string]interface{}{"type": "AdaptiveCard", "version": "1.5"},
		Text:    "CI 构建通知",
	}, false)
	assert.NoError(t, err)
	assert.Equal(t, "CI 构建通知", p["plain"])

	// 空 body 无种子 → [卡片] 兜底（plain 永不为空）
	p, err = buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card:    map[string]interface{}{"type": "AdaptiveCard", "version": "1.5"},
	}, false)
	assert.NoError(t, err)
	assert.Equal(t, cardmsg.PlaceholderCard, p["plain"])

	// 脏 scheme → 白名单拒绝（外部 URL 是不可信内容，正向 http(s) allowlist）
	_, err = buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card: map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
			},
		},
	}, false)
	assert.ErrorIs(t, err, cardmsg.ErrCardBadURLScheme)

	// 超 512KiB → ErrCardPayloadTooLarge（api 层映射 413）
	_, err = buildCardPayload(cardTestModel(), &pushPayloadReq{
		MsgType: msgTypeCard,
		Card: map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": strings.Repeat("a", cardmsg.MaxPayloadBytes)},
			},
		},
	}, false)
	assert.True(t, errors.Is(err, cardmsg.ErrCardPayloadTooLarge), "err=%v", err)
}
