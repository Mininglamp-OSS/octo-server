package incomingwebhook

// 飞书自定义机器人格式适配器（#297 Phase 4）。
//
// 路由：POST /v1/incoming-webhooks/:webhook_id/:token/feishu
// 接受飞书「自定义机器人」的出站消息格式：已配置向飞书机器人推送的工具，只需把
// webhook URL 换成上述地址即可迁移，消息体零改动。
//
// 形态映射（高保真卡片渲染不可行，经 #297 确认接受降级并在 README 写明，与 WeCom
// 适配器同一契约）：
//
//   - text        → native 纯文本路径（客户端按 markdown 渲染）。
//   - post（富文本）→ 降级 markdown：标题加粗，每行的 text/a/at 标签内联拼接（a 渲染为
//     markdown 链接，at 渲染为 @用户名）；img 标签丢弃——飞书图文用 image_key 引用平台
//     素材，无法转存为 URL（与 WeCom 图片同理）。走文本路径而非 RichText：text 块不渲染
//     markdown，链接会失去可点击性，文本路径反而更保真（#297 确认）。
//   - interactive（卡片）→ 降级 markdown：标题 + div/markdown 元素文本逐行拼接；按钮 /
//     图片 / 分隔等交互或素材元素丢弃。
//   - image / share_chat 等依赖平台素材的类型 → 400 invalid(reason=msg_type)：素材无法
//     转存，静默丢弃会让调用方误以为已送达，显式失败 + deliveries 可见才诚实。
//
// 成功响应在 native 字段基础上附带 code=0 / msg=success（见 adapter.go
// feishuAdapter.successExtra）：多数飞书 SDK 以 code==0 判定成功。
//
// 飞书带 secret 时消息体里会有 timestamp / sign 字段——白名单解析直接忽略，鉴权一律
// 走 URL token，不另校验飞书 sign。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// feishuMsg 只声明翻译需要的字段（白名单解析），其余 payload 字段一律忽略。
type feishuMsg struct {
	MsgType string         `json:"msg_type"`
	Content *feishuContent `json:"content"`
	Card    *feishuCard    `json:"card"`
}

type feishuContent struct {
	Text string                      `json:"text"`
	Post map[string]feishuPostLocale `json:"post"`
}

type feishuPostLocale struct {
	Title   string            `json:"title"`
	Content [][]feishuPostTag `json:"content"`
}

type feishuPostTag struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Href     string `json:"href"`
	UserName string `json:"user_name"`
}

type feishuCard struct {
	Header *struct {
		Title struct {
			Content string `json:"content"`
		} `json:"title"`
	} `json:"header"`
	Elements []feishuCardElement `json:"elements"`
}

// feishuCardElement：div 元素文本在 text.content；markdown 元素文本直接在 content。
type feishuCardElement struct {
	Tag  string `json:"tag"`
	Text *struct {
		Content string `json:"content"`
	} `json:"text"`
	Content string `json:"content"`
}

// parseFeishuPush 把飞书自定义机器人消息翻译成 native 推送请求（pushAdapter.parse）。
// 与 GitHub/GitLab 不同，内容长度不钳制：消息体由调用方编写（非平台生成的事件），
// 超过语义上限按既有 413 拒绝，调用方有能力也应当修短。
func parseFeishuPush(_ http.Header, body []byte) (*pushPayloadReq, string, string) {
	var msg feishuMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, "", "json"
	}
	var content string
	switch msg.MsgType {
	case "text":
		if msg.Content != nil {
			content = msg.Content.Text
		}
	case "post":
		if msg.Content != nil {
			content = renderFeishuPost(msg.Content.Post)
		}
	case "interactive":
		content = renderFeishuCard(msg.Card)
	default:
		// 空 msg_type 与 image / share_chat 等素材类：显式拒绝（理由见文件头注释）。
		return nil, "", "msg_type"
	}
	if strings.TrimSpace(content) == "" {
		return nil, "", "content"
	}
	return &pushPayloadReq{Content: content}, "", ""
}

// renderFeishuPost 把富文本降级为 markdown：标题加粗，每行内联拼接 text/a/at 标签，
// 行间换行。优先 zh_cn 语言块，回退 en_us，再回退任意一个。
func renderFeishuPost(post map[string]feishuPostLocale) string {
	if len(post) == 0 {
		return ""
	}
	loc := pickFeishuLocale(post)
	var lines []string
	if title := oneLine(loc.Title); title != "" {
		lines = append(lines, "**"+title+"**")
	}
	for _, row := range loc.Content {
		var sb strings.Builder
		for _, tag := range row {
			switch tag.Tag {
			case "text":
				sb.WriteString(tag.Text)
			case "a":
				text := oneLine(tag.Text)
				// href 必须是 http(s)：飞书 a-tag 的 href 来自入站 payload，裸传会让
				// `javascript:` / `data:` 等危险 scheme 渲染成投递给群内其它成员的可点击
				// 链接（scheme 注入，#423 review，Jerry-Xin/mochashanyao）。非 http(s) 降级
				// 为纯文本。链接文本里的 `]`/`[` 仍转义，避免破坏 markdown 链接结构。
				if href := strings.TrimSpace(tag.Href); isHTTPURL(href) {
					fmt.Fprintf(&sb, "[%s](%s)", mdLinkTextEscaper.Replace(text), href)
				} else {
					sb.WriteString(text)
				}
			case "at":
				// user_name 是自由文本，进 `@X` 纯文本上下文须经 mdInertText 转义，
				// 防止 `]`/`[`/`*` 等注入（同 glActor 的处理，#423 review）。
				if tag.UserName != "" {
					sb.WriteString("@" + mdInertText(tag.UserName, 64))
				}
			// img 等依赖 image_key 的标签：无法转存为 URL，丢弃（见文件头注释）。
			default:
			}
		}
		if line := strings.TrimRight(sb.String(), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// pickFeishuLocale 选语言块：zh_cn 优先，en_us 次之，最后兜底任意一个（map 遍历序
// 不稳定，仅作为「至少别丢消息」的最末兜底）。
func pickFeishuLocale(post map[string]feishuPostLocale) feishuPostLocale {
	if v, ok := post["zh_cn"]; ok {
		return v
	}
	if v, ok := post["en_us"]; ok {
		return v
	}
	for _, v := range post {
		return v
	}
	return feishuPostLocale{}
}

// renderFeishuCard 把交互卡片降级为 markdown：标题加粗 + div/markdown 元素文本逐行
// 拼接。按钮 / 图片 / 分隔线等交互或素材元素无法复现，丢弃。
func renderFeishuCard(card *feishuCard) string {
	if card == nil {
		return ""
	}
	var lines []string
	if card.Header != nil {
		if title := oneLine(card.Header.Title.Content); title != "" {
			lines = append(lines, "**"+title+"**")
		}
	}
	for _, el := range card.Elements {
		switch el.Tag {
		case "div":
			if el.Text != nil {
				if t := strings.TrimSpace(el.Text.Content); t != "" {
					lines = append(lines, t)
				}
			}
		case "markdown":
			if t := strings.TrimSpace(el.Content); t != "" {
				lines = append(lines, t)
			}
		// action / img / hr / note 等：交互或素材元素，丢弃。
		default:
		}
	}
	return strings.Join(lines, "\n")
}
