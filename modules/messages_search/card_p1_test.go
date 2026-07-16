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

	trustedHandler := &Handler{cardTrust: stubTrust(true)}
	mh := trustedHandler.singleMessageHit(doc, "g_1", 0, nil)
	assert.Equal(t, "审批单 #42:待审批", mh.Snippet, "bot sender 命中投影 = 权威 plain")
	assert.Equal(t, "text", mh.MessageKind, "message_kind 枚举已锁,卡片折入 text")

	untrustedHandler := &Handler{cardTrust: stubTrust(false)}
	mh = untrustedHandler.singleMessageHit(doc, "g_1", 0, nil)
	assert.Equal(t, cardmsg.PlaceholderCard, mh.Snippet, "非可信 sender 必须遮蔽为 [卡片]")
}

// stubTrust 是 cardSenderTruster 的测试替身。
type stubTrust bool

func (s stubTrust) Trusted(string) bool { return bool(s) }
