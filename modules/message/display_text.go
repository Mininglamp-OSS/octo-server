package message

// card-message-protocol P1（round-2 finding #3）：「按内容类型描述消息」的本地
// display-text helper。octo-lib 的 common.GetDisplayText 不认识 InteractiveCard
// (=17)——没有这层包装，置顶 tip 等服务端文案面会渲染「未知消息类型」。
// card 分支随 octo-lib companion PR 上游化后本 helper 退役。
//
// 注意区分两类文案面：
//   - 内容类型占位（本 helper / 置顶 tip）：恒 [卡片]，无 plain 信任问题；
//   - 按 plain 描述内容（当前为离线推送、搜索命中）：走 cardmsg.DisplayTextFor，
//     带 Decision-2 residual-risk 的 sender 身份门。
//
// 说明（PR#543 review）：P1 目前只有推送 / 搜索两处「按 plain 描述」的服务端文案面，
// 均已接入 DisplayTextFor。会话摘要 / 引用预览尚无服务端生成 type-17 文本的面
// （会话 last_message 返回的是原始 payload —— 属消息 transport，由客户端渲染门禁
// (Decision 2 layer b) 治理，不是服务端文案面，故不经本遮蔽）。将来若新增此类
// 服务端摘要/引用文案面，必须同样经 DisplayTextFor。

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

func displayContentTypeText(contentType int) string {
	if contentType == cardmsg.InteractiveCard.Int() {
		return cardmsg.DisplayText()
	}
	return common.GetDisplayText(contentType)
}
