package incomingwebhook

// card-message-protocol P1（spec: .octospec/tasks/card-message-protocol/
// brief.md）：incoming webhook 的 msg_type:"card" 原生推送路径 —— 三个卡片
// 生产者入口之一（另两个：bot_api / robot）。调用方直接给标准 Adaptive Cards
// 1.5 JSON（octo/v1 白名单子集），服务端组 InteractiveCard(=17) 信封并做与
// bot ingress 完全对称的 write-strict 校验。

import (
	"errors"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

const msgTypeCard = "card"

// errCardDisabled Decision 2 rollout gate：OCTO_CARD_MESSAGE_ENABLED 未开启。
// 独立于结构性 invalid（deliveries 里 reason=card_disabled 便于运维定位），
// 对外同样是 400。
var errCardDisabled = errors.New("incomingwebhook: card messages disabled")

// buildCardPayload 把调用方的标准 AC JSON 组装为 type-17 信封并权威校验：
//
//   - 信封只含白名单字段：card（调用方 JSON 原样，Validate 是唯一权威闸）、
//     card_version/profile（服务端钉 octo/v1，不接受调用方覆盖 —— 与丢弃
//     req.Extra 的安全基线一致）、from.kind=webhook、服务端派生 space_id；
//   - cardmsg.Validate：白名单/大小/URL/树上限 write-strict（与 bot ingress
//     同一权威）；cardmsg.Finalize：重算权威 plain + 完整出站 payload 复检；
//   - Decision 8 `text` 种子语义：仅当派生 plain 为空（= [卡片] 兜底）且调用方
//     给了 text 时，用 text 作 plain（rune 上限与纯文本路径一致）；卡片 body
//     产出文本时 text 被忽略 —— 派生始终权威。种子覆盖发生在 Finalize 复检之后、
//     会改变 plain 字节，故覆盖后再复检一次：卡片体逼近 512KiB 时，用种子替换极短
//     的 [卡片] 占位可能把 payload 顶过上限，不能只靠 8KB body cap 的间接边界
//     （PR#543 review 🟡）。
//
// 错误映射（push 路径）：ErrCardPayloadTooLarge → 413；errCardDisabled →
// 400 reason=card_disabled；其余校验错误 → 400 reason=card。
func buildCardPayload(m *incomingWebhookModel, req *pushPayloadReq, allowOverride bool) (map[string]interface{}, error) {
	if !cardmsg.Enabled() {
		return nil, errCardDisabled
	}
	name, avatar := resolveFromIdentity(m, req, allowOverride)
	payload := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card":         req.Card,
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"from": map[string]interface{}{
			"kind":       extraKindValue,
			"webhook_id": m.WebhookID,
			"name":       name,
			"avatar":     avatar,
		},
		// space_id 由服务端从 group 派生，不接受调用方覆盖（与 buildPayload 一致）。
		"space_id": m.SpaceID,
	}
	if err := cardmsg.Validate(payload); err != nil {
		return nil, err
	}
	if err := cardmsg.Finalize(payload); err != nil {
		return nil, err
	}
	if payload["plain"] == cardmsg.PlaceholderCard {
		seed := req.Text
		if seed != "" && utf8.RuneCountInString(seed) <= maxContentRunes() {
			payload["plain"] = seed
			// 种子改变了权威 plain（在 Finalize 复检之后）——对最终 payload 再复检，
			// 保证 buildCardPayload 返回的 payload 恒不超 512KiB，不依赖下游远处复检。
			if err := cardmsg.RecheckPayloadSize(payload); err != nil {
				return nil, err
			}
		}
	}
	return payload, nil
}
