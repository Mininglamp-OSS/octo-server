package messages_search

// card-message-protocol P1：type-17 命中的响应侧投影 —— bot/webhook sender
// 投影权威 plain；非可信 sender 投影 [卡片]（Decision 2 residual-risk，
// round-3 P1-2）。索引侧 searchText 是 indexer 跨仓 follow-up，本仓不闭合。

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

func TestSingleMessageHitCardProjection(t *testing.T) {
	cardType := payloadTypeCard
	raw := json.RawMessage(`{"type":17,"card":{"body":[{"type":"TextBlock","text":"内部字段"}]},"plain":"审批单 #42:待审批","card_version":"1.5","profile":"octo/v1"}`)
	doc := Doc{
		MessageID:  9001,
		From:       "bot_x",
		Payload:    &Payload{Type: &cardType},
		PayloadRaw: raw,
	}

	trustedHandler := &Handler{cardSenderTrusted: func(string) bool { return true }}
	mh := trustedHandler.singleMessageHit(doc, "g_1", nil)
	assert.Equal(t, "审批单 #42:待审批", mh.Snippet, "bot sender 命中投影 = 权威 plain")
	assert.Equal(t, "text", mh.MessageKind, "message_kind 枚举已锁,卡片折入 text")

	untrustedHandler := &Handler{cardSenderTrusted: func(string) bool { return false }}
	mh = untrustedHandler.singleMessageHit(doc, "g_1", nil)
	assert.Equal(t, cardmsg.PlaceholderCard, mh.Snippet, "非可信 sender 必须遮蔽为 [卡片]")
}

// 跨包常量一致性：本模块本地复制的 iwh_ 前缀必须与 incomingwebhook 契约常量
// 一致（分层方向不允许生产代码跨层 import,漂移由测试兜底 —— 与 webhook 模块
// 同一模式;incomingwebhook.WebhookIDPrefix 的导出注释即为此用途）。
func TestCardSenderPrefixMatchesIncomingWebhook(t *testing.T) {
	// 直接字面断言,避免本(下层)模块的测试二进制引入上层模块依赖。
	assert.Equal(t, "iwh_", cardSenderPrefixWebhook)
}
