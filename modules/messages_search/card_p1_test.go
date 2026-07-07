package messages_search

// card-message-protocol P1：type-17 命中的响应侧投影 —— bot/webhook sender
// 投影权威 plain；非可信 sender 投影 [卡片]（Decision 2 residual-risk，
// round-3 P1-2）。索引侧 searchText 是 indexer 跨仓 follow-up，本仓不闭合。

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
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
// 一致（分层方向不允许生产代码跨层 import,漂移由【测试】兜底 —— 断言对着导出
// 契约常量 incomingwebhook.WebhookIDPrefix,而非字面量,否则前缀漂移测试照过、
// 遮蔽逻辑静默失配。webhook 模块的 sibling 测试同一模式）。
func TestCardSenderPrefixMatchesIncomingWebhook(t *testing.T) {
	assert.Equal(t, incomingwebhook.WebhookIDPrefix, cardSenderPrefixWebhook)
}
