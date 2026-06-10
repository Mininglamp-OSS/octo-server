package incomingwebhook

// 企业微信群机器人格式适配器（#297 Phase 4 的 WeCom 部分，与 Phase 3 合并实施）。
//
// 路由：POST /v1/incoming-webhooks/:webhook_id/:token/wecom
// 接受企业微信「群机器人」的出站消息格式：已配置向企微机器人推送的工具（CI、告警、
// 监控脚本），只需把 webhook URL 换成上述地址即可迁移，消息体零改动。
//
// 形态映射（高保真卡片渲染不可行，经 #297 确认接受降级并在 README 写明）：
//
//   - text / markdown / markdown_v2 → native 纯文本路径（客户端按 markdown 渲染）。
//     text 的 mentioned_list / mentioned_mobile_list 降级丢弃——与 native 把 @all
//     降级为字面量的策略一致，webhook 消息不携带 mention 语义（绕过通知策略）。
//   - news → 降级 markdown：每篇文章「标题链接 + 描述」一段。
//   - template_card → 降级 markdown：主标题 + 描述 + 跳转链接。
//   - image / file / voice 等依赖平台素材（base64 / media_id）的类型 → 400
//     invalid(reason=msg_type)：素材无法转存为 URL 引用，静默丢弃会让调用方误以为
//     已送达，显式失败 + deliveries 可见才是诚实的迁移体验。
//
// 成功响应在 native 字段基础上附带 errcode=0 / errmsg=ok（见 adapter.go
// wecomAdapter.successExtra）：多数企微 SDK 以 errcode==0 判定成功。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type wecomTextContent struct {
	Content string `json:"content"`
}

type wecomArticle struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

type wecomTemplateCard struct {
	MainTitle struct {
		Title string `json:"title"`
		Desc  string `json:"desc"`
	} `json:"main_title"`
	SubTitleText string `json:"sub_title_text"`
	CardAction   struct {
		URL string `json:"url"`
	} `json:"card_action"`
}

// wecomMsg 只声明翻译需要的字段（白名单解析），其余 payload 字段一律忽略。
type wecomMsg struct {
	MsgType    string            `json:"msgtype"`
	Text       *wecomTextContent `json:"text"`
	Markdown   *wecomTextContent `json:"markdown"`
	MarkdownV2 *wecomTextContent `json:"markdown_v2"`
	News       *struct {
		Articles []wecomArticle `json:"articles"`
	} `json:"news"`
	TemplateCard *wecomTemplateCard `json:"template_card"`
}

// parseWeComPush 把企业微信群机器人消息翻译成 native 推送请求（pushAdapter.parse）。
// 与 GitHub 适配器不同，内容长度不钳制：消息体由调用方自行编写（而非平台生成的事件），
// 超过语义上限按既有 413 拒绝，调用方有能力也应当修短。
func parseWeComPush(_ http.Header, body []byte) (*pushPayloadReq, string, string) {
	var msg wecomMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, "", "json"
	}
	var content string
	switch msg.MsgType {
	case "text":
		if msg.Text != nil {
			content = msg.Text.Content
		}
	case "markdown":
		if msg.Markdown != nil {
			content = msg.Markdown.Content
		}
	case "markdown_v2":
		if msg.MarkdownV2 != nil {
			content = msg.MarkdownV2.Content
		}
	case "news":
		if msg.News != nil {
			content = renderWeComNews(msg.News.Articles)
		}
	case "template_card":
		if msg.TemplateCard != nil {
			content = renderWeComTemplateCard(msg.TemplateCard)
		}
	default:
		// 空 msgtype 与 image / file / voice 等素材类：显式拒绝（理由见文件头注释）。
		return nil, "", "msg_type"
	}
	if strings.TrimSpace(content) == "" {
		return nil, "", "content"
	}
	return &pushPayloadReq{Content: content}, "", ""
}

// renderWeComNews 把图文消息降级为 markdown：每篇「[标题](url) + 换行 + 描述」一段。
// picurl 丢弃（封面图无法以图文混排语义复现，标题链接已承载跳转）。
func renderWeComNews(articles []wecomArticle) string {
	parts := make([]string, 0, len(articles))
	for _, a := range articles {
		title := oneLine(a.Title)
		if title == "" {
			continue
		}
		line := "**" + title + "**"
		if u := strings.TrimSpace(a.URL); u != "" {
			line = fmt.Sprintf("[%s](%s)", line, u)
		}
		if desc := strings.TrimSpace(a.Description); desc != "" {
			line += "\n" + desc
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n\n")
}

// renderWeComTemplateCard 把模板卡片降级为 markdown：主标题（粗体）+ 描述 +
// 副标题 + 跳转链接，逐行拼接；按钮 / 多列等交互元素不可复现，丢弃。
func renderWeComTemplateCard(card *wecomTemplateCard) string {
	lines := make([]string, 0, 4)
	if t := oneLine(card.MainTitle.Title); t != "" {
		lines = append(lines, "**"+t+"**")
	}
	if d := strings.TrimSpace(card.MainTitle.Desc); d != "" {
		lines = append(lines, d)
	}
	if s := strings.TrimSpace(card.SubTitleText); s != "" {
		lines = append(lines, s)
	}
	if u := strings.TrimSpace(card.CardAction.URL); u != "" {
		lines = append(lines, u)
	}
	return strings.Join(lines, "\n")
}
