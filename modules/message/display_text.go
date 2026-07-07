package message

// card-message-protocol P1（round-2 finding #3）：「按内容类型描述消息」的本地
// display-text helper。octo-lib 的 common.GetDisplayText 不认识 InteractiveCard
// (=17)——没有这层包装，置顶 tip 等服务端文案面会渲染「未知消息类型」。
// card 分支随 octo-lib companion PR 上游化后本 helper 退役。
//
// 注意区分两类文案面：
//   - 内容类型占位（本 helper / 置顶 tip）：恒 [卡片]，无 plain 信任问题；
//   - 按 plain 描述内容（推送/搜索/摘要/引用）：走 cardmsg.DisplayTextFor，
//     带 Decision-2 residual-risk 的 sender 身份门。

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
