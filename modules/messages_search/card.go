package messages_search

// card-message-protocol P1（spec: .octospec/tasks/card-message-protocol/
// brief.md）：InteractiveCard(=17) 的搜索命中投影。响应侧只投影 plain（镜像
// buildRichTextDetail 的「响应侧投影」定位），且必须过 Decision-2 residual-risk
// 的单一执法点 cardmsg.DisplayTextFor —— 命中文档的存储 sender 不是 bot/webhook
// 身份时，投影 [卡片] 而非存储 plain（round-3 P1-2）。
// 索引侧 searchText 物化是 wukongim-message-indexer 的跨仓 follow-up（携同一
// sender 约束），本仓不闭合。

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
)

// payloadTypeCard InteractiveCard 的 payload.type（Decision 1；≠ 名片 Card=7）。
const payloadTypeCard = 17

// cardSenderPrefixWebhook incoming webhook 合成发送者前缀。按仓库分层惯例本地
// 复制（生产代码不跨层 import modules/incomingwebhook，见其 display.go 顶注）；
// 跨包一致性由 card_test.go 的常量一致性测试兜底。
const cardSenderPrefixWebhook = "iwh_"

// newCardSenderTrusted 构造 sender 身份判定：iwh_ 前缀 → webhook 身份；否则查
// robot 表（status=1）。查询失败 fail-closed —— 宁可投影 [卡片] 也不透出不可信
// plain。Handler 持有函数而非 service，方便测试直接替换判定。
func newCardSenderTrusted(ctx *config.Context) func(string) bool {
	svc := robot.NewService(ctx)
	return func(fromUID string) bool {
		if strings.HasPrefix(fromUID, cardSenderPrefixWebhook) {
			return true
		}
		isBot, err := svc.ExistRobot(fromUID)
		if err != nil {
			return false
		}
		return isBot
	}
}
