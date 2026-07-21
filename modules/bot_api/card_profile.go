package bot_api

// card-message-interaction P2 D12（spec: .octospec/tasks/card-message-interaction/
// brief.md）：生产者能力发现。
//
// GET /v1/bot/card/profile —— 只读，挂在既有 botAPI 组（ba.authBot() bot-token 鉴权），
// 不加新限流、不经 SpaceMiddleware（清单是**部署级**能力常量，无租户/用户/消息数据可
// 隔离）。返回本部署的卡片能力清单，供 producer（bot SDK / OpenClaw 适配器）在启动时
// feature-detect，而非用「发一条卡看是否 400」探测 —— 400 无法区分「卡片功能关闭」与
// 「卡片非法」。
//
// 清单值全部取自 pkg/cardmsg 常量（**单一权威**，D12.2）—— 绝不在此重抄字面量，否则
// 常量变更时清单会与校验器静默漂移。profiles 取 cardmsg.AcceptedProfiles()（与校验器
// interactiveByProfile 的接受集同源）；elements/inputs/actions 取 cardmsg.DisplayElements()/
// InputElements()/DisplayActions()（与校验器白名单同源，反漂移由 pkg/cardmsg 守卫测试锁定）。
//
// enabled 取 cardmsg.BotEnabled()（部署级总开关 OCTO_CARD_MESSAGE_ENABLED AND bot 子
// 开关 OCTO_BOT_CARD_ENABLED 的有效门禁）：即便关闭也返回 200 + 全清单 —— feature
// detection 正是目的。清单与实际发卡门禁**同源**：send/edit 路径同样 gate 在
// BotEnabled()（send.go / card_revision.go），故 enabled 必然与「发卡是否会被受理」
// 一致，绝不出现「报 enabled 却发被拒」。
//
// wire contract 只增不改（additive-only，同 event_data 演进规则）：字段可新增，绝不
// 改名/删除/改类型 —— SDK 与适配器据此做能力探测与上限读取，改名等同破坏 event_data。

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// botCardProfile handles GET /v1/bot/card/profile (D12).
func (ba *BotAPI) botCardProfile(c *wkhttp.Context) {
	// authBot 已在中间件层校验 bot-token；此处再确认存在 bot 身份（与同组 GET 端点
	// 一致的防御，非资源属主校验 —— 清单无属主概念）。缺身份 → 既有 bot-auth 拒绝。
	if getRobotIDFromContext(c) == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}
	c.Response(map[string]interface{}{
		"enabled":      cardmsg.BotEnabled(),
		"card_version": cardmsg.CardVersion,
		"profiles":     cardmsg.AcceptedProfiles(),
		// elements/inputs：本部署接受的展示元素 / 交互输入白名单（源自 pkg/cardmsg 权威
		// 列表，D12.2 单一权威）。producer / J3 gate 据此按**元素粒度**做前向兼容协商——
		// 即便 card_version 停在 "1.5"，也能探测本部署是否接受 Input.Number/Date/Time 等
		// additive 新增元素，消除「版本号不变 → gate 无法分辨新旧 1.5 部署」的错配。
		"elements": cardmsg.DisplayElements(),
		"inputs":   cardmsg.InputElements(),
		// actions：本部署接受的 octo/v1 **本地动作**白名单（OpenUrl / ToggleVisibility /
		// CopyToClipboard —— 无服务端回调，源自 pkg/cardmsg 权威列表 DisplayActions()，
		// 反漂移由 TestDisplayActionsAuthority 锁定）。producer 据此按**动作粒度**前向兼容
		// 协商——即便 card_version 停在 "1.5"、profiles 不变，也能探测本部署是否接受
		// ToggleVisibility / CopyToClipboard 等 additive 新增本地动作。Action.Submit 属
		// octo/v2 交互档，经 profiles 隐式发现、不在此列。
		"actions": cardmsg.DisplayActions(),
		"limits": map[string]interface{}{
			"max_payload_bytes":    cardmsg.MaxPayloadBytes,
			"max_nodes":            cardmsg.MaxNodes,
			"max_depth":            cardmsg.MaxDepth,
			"max_input_text_bytes": cardmsg.MaxInputTextBytes,
			"max_inputs_bytes":     cardmsg.MaxInputsBytes,
			// Action.CopyToClipboard.text 的 UTF-8 字节上限（与 actions 里的 CopyToClipboard
			// 对称，供 producer 按阈值 feature-detect，免去「发一条超限卡看 400」探测）。
			"max_copy_text_bytes": cardmsg.MaxCopyTextBytes,
		},
	})
}
