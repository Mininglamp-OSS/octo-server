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
// interactiveByProfile 的接受集同源）。
//
// enabled 直取 cardmsg.Enabled()（部署级 rollout 开关）：即便关闭也返回 200 + 全清单
// —— feature detection 正是目的；只有 send/edit 路径在关闭时拒绝（send.go:97）。
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
		"enabled":      cardmsg.Enabled(),
		"card_version": cardmsg.CardVersion,
		"profiles":     cardmsg.AcceptedProfiles(),
		"limits": map[string]interface{}{
			"max_payload_bytes":    cardmsg.MaxPayloadBytes,
			"max_nodes":            cardmsg.MaxNodes,
			"max_depth":            cardmsg.MaxDepth,
			"max_input_text_bytes": cardmsg.MaxInputTextBytes,
			"max_inputs_bytes":     cardmsg.MaxInputsBytes,
		},
	})
}
