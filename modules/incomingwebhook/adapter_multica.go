package incomingwebhook

// Multica 事件适配器（#426）。
//
// 路由：POST /v1/incoming-webhooks/:webhook_id/:token/multica
// 在 Multica 工作区 Settings → Webhooks 把 Webhook URL 配成上述地址即可，
// 无需任何中间转换层。
//
// 鉴权沿用 URL 内的 128-bit token（与 native / github / wecom 一致）。Multica
// 出站请求会带 X-Multica-Signature-256（与 GitHub 的 X-Hub-Signature-256 同
// 算法），与 github 适配器对称——目前不校验，留作后续可选项。
//
// 渲染策略：按 envelope.event 把已支持事件翻译成 markdown 文本（走 native 纯
// 文本路径，客户端按 markdown 渲染）。v1 只识别 issue.status_changed；未来
// issue.created / issue.assigned / comment.created 等是【加性】的——新增 case 即可，
// 不动既有契约。
//
// 子集之外的事件返回 200 + auditSkipped(reason=event)：Multica 侧投递成功
// （不会把订阅标红），管理端 deliveries 里 reason=event 可见，与 github
// 适配器的语义完全一致。
//
// mu* 结构体只声明渲染需要的字段（白名单解析），其余 payload 字段一律忽略。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// muIssue 是 multica IssueResponse 渲染所需的最小字段子集（白名单）。
// Identifier 形如 "MUL-123"；Status 是状态枚举（todo/in_progress/...）。
type muIssue struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Status     string `json:"status"`
}

// muActor 是事件触发者标识（{type,id}）。type ∈ member / agent 等；id 是
// 触发者主键（uuid 或 agent slug）。当前 envelope 不带显示名，渲染只用 type。
type muActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// muEnvelope 是 multica 出站 webhook 的固定信封（见 multica
// server/internal/integrations/outwebhook/dispatcher.go outboundPayload）。
//
// issue_url / assignee_type / assignee_name 是 multica 侧富集字段（后端用
// PublicURL+slug+identifier 拼好 url、join 出 assignee 显示名后发来）——octo
// 侧无法从 UUID/workspace_id 自行推断,所以直接消费。三者都是【加性】的:
// 旧版 multica 不发 → 留空,渲染降级（无链接 / 无指派行）。
type muEnvelope struct {
	Event          string  `json:"event"`
	WorkspaceID    string  `json:"workspace_id"`
	Actor          muActor `json:"actor"`
	Issue          muIssue `json:"issue"`
	IssueURL       string  `json:"issue_url"`
	AssigneeType   string  `json:"assignee_type"`
	AssigneeName   string  `json:"assignee_name"`
	PreviousStatus string  `json:"previous_status"`
}

// 已知事件类型。v1 只有 issue.status_changed；新增事件加 case 即可。
const (
	muEventIssueStatusChanged = "issue.status_changed"
)

// parseMulticaPush 把 multica envelope 翻译成 native 推送请求（pushAdapter.parse）。
//
// 三个返回值恰有一个生效（见 pushAdapter.parse 注释）：
//   - req     非 nil：照常走纯文本路径投递；
//   - skip    非空：合法但刻意不投递（事件类型不在渲染子集内 → "event"），
//     返回 200 + auditSkipped，管理端 deliveries 里 reason=event 可见；
//   - invalid 非空：解析失败 → 400 + auditFailed，reason 同 native：
//     "json" / "no_event" / "content"（事件已识别但渲染结果为空）。
//
// header 当前未使用（X-Multica-Event 仅作辅助；事件分发以 body.event 为准，
// 防止 header/body 不一致导致渲染走偏）。保留参数以匹配 pushAdapter.parse 签名，
// 并预留未来按 header 做版本协商的余地。
func parseMulticaPush(_ http.Header, body []byte) (*pushPayloadReq, string, string) {
	var ev muEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, "", "json"
	}
	// lower+trim 与 api.go 里的 msg_type 处理保持一致——避免发送方误用大小写
	// 变体把合法事件（"Issue.Status_Changed"）静默降级成 200 skip(event)
	// (yujiawei review P2-2)。事件名按 multica 契约本就是 lowercase-dotted 枚举，
	// 这里做规范化只是 defense-in-depth。
	event := strings.ToLower(strings.TrimSpace(ev.Event))
	if event == "" {
		// 缺 event 字段是「配置错误」（不像合法 multica 出站流量），与 github
		// 适配器缺 X-GitHub-Event 头同语义——复用同一个 no_event 原因码，让
		// deliveries 排障口径跨适配器一致（yujiawei review P2-2）：跟「事件已识别但
		// 不在渲染子集内」的 200 skip(event) 区分开。reason 字典在 #330 时已经为
		// no_event 注册了 i18n / DB 注释，无须新增。
		return nil, "", "no_event"
	}

	var content string
	switch event {
	case muEventIssueStatusChanged:
		content = renderMulticaIssueStatusChanged(ev)
	default:
		// 渲染子集之外（issue.created / comment.created / 未来未知事件）：
		// multica 侧无需修复任何东西，200 + skip。
		return nil, "event", ""
	}
	if content == "" {
		// 事件类型已识别、但 payload 关键字段缺失渲染不出可读内容：按 400
		// 拒绝而非静默跳过，让发送方能在 deliveries 里看到 reason=content
		// 排查 payload 是否构造错（与 native content 为空的语义一致）。
		return nil, "", "content"
	}
	// 8 KiB body cap 已经在 handlePush 第 4 步前置拦掉超长 body（足够大的 title
	// 会先 413，而不是到这里），所以此处的 clipRunes 不是「保 title 不被 413」的
	// 兜底，而是渲染层对 maxContentRunes(4000 rune) 上限的本地约束——避免拼装后
	// 的最终 content 越过 push 路径下游对 RichText 字数的语义钳位。
	return &pushPayloadReq{Content: clipRunes(content, maxContentRunes())}, "", ""
}

// renderMulticaIssueStatusChanged 渲染 issue.status_changed 事件。
//
// 输出形如（markdown，客户端会渲染），多行结构化：
//
//	**[MUL-123 Fix login redirect](https://app.multica.ai/acme/issues/MUL-123)**
//	状态: `todo` → `in_progress`
//	指派: 张三 · 触发: agent
//
// 三行分别是「标题（带链接）/ 状态变更 / 指派·触发」；缺字段的行自动省略。
// issue_url 与 assignee_name 是 multica 侧富集字段（octo 无法从 UUID/
// workspace_id 自行推断域名、slug、人名），渲染层直接消费、并对 url 做
// http(s) 白名单。各字段的转义/钳长策略见函数内逐行注释。
func renderMulticaIssueStatusChanged(ev muEnvelope) string {
	id := strings.TrimSpace(ev.Issue.Identifier)
	title := strings.TrimSpace(ev.Issue.Title)
	status := strings.TrimSpace(ev.Issue.Status)
	prev := strings.TrimSpace(ev.PreviousStatus)
	issueURL := strings.TrimSpace(ev.IssueURL)
	assignee := strings.TrimSpace(ev.AssigneeName)
	assigneeType := strings.TrimSpace(ev.AssigneeType)
	actorType := strings.TrimSpace(ev.Actor.Type)
	// id 与 status 是渲染所必需的最小集；缺失任一无法生成可读消息，回退
	// 让上层走 reason=content 而非凭空生造。
	if id == "" || status == "" {
		return ""
	}

	const shortFieldMax = 64

	var b strings.Builder

	// --- 标题行 ---
	// 链接文本是 "{identifier} {title}"（title 可空）。有合法 url 时整体包成
	// markdown 链接；否则降级纯文本。
	//   - 链接文本用 mdInertText（不是 mdLinkText）：CommonMark 会在链接文本
	//     内部解析强调/代码 span,所以文本里的 `*`/反引号/`[]` 必须按 inert 处理,
	//     否则一个含 `**` 的 title 注入真粗体、一个含反引号的 title 把后面 status
	//     code-span 切坏（#496 yujiawei/OctoBoooot review）。mdInertText 转义
	//     `*_[]<>|` 并剥反引号——转义后的 `\]` 是字面右括号,不会闭合链接,对链接
	//     文本同样安全。
	//   - 链接目标用 safeMarkdownURL:仅 isHTTPURL 不够,目标是独立注入上下文,
	//     `https://ok/) [phish](https://evil` 会闭合链接再注入第二个链接
	//     （#496 Jerry-Xin/OctoBoooot review）。校验失败则降级纯文本标题。
	if safeURL, ok := safeMarkdownURL(issueURL); ok {
		linkText := id
		if title != "" {
			linkText = id + " " + title
		}
		fmt.Fprintf(&b, "**[%s](%s)**", mdInertText(linkText, 200), safeURL)
	} else {
		fmt.Fprintf(&b, "**%s**", mdInertText(id, shortFieldMax))
		if title != "" {
			b.WriteString(" ")
			b.WriteString(mdInertText(title, 200))
		}
	}

	// --- 状态行 ---
	// 反引号包起来避免下划线（in_progress）被当斜体；prev 缺失或与 status 相同
	// 时只显示当前状态，不画 "→ X" 暗示变化的尾巴。
	b.WriteString("\n状态: ")
	if prev != "" && prev != status {
		fmt.Fprintf(&b, "`%s` → `%s`",
			mdCodeSpanText(prev, shortFieldMax),
			mdCodeSpanText(status, shortFieldMax))
	} else {
		fmt.Fprintf(&b, "`%s`", mdCodeSpanText(status, shortFieldMax))
	}

	// --- 指派 / 触发行（任一非空才渲染整行）---
	// assignee_name 由 multica 富集发来（octo 只收到 UUID，无法自解析人名）。
	// assignee_type（member/agent/squad）附在名字后,让群里能区分指派给「人」
	// 还是「Agent/Squad」(#496 yujiawei review:type 之前解析了却没用)。
	var meta []string
	if assignee != "" {
		label := "指派: " + mdInertText(assignee, shortFieldMax)
		if assigneeType != "" {
			label += " (" + mdInertText(assigneeType, shortFieldMax) + ")"
		}
		meta = append(meta, label)
	}
	if actorType != "" {
		meta = append(meta, "触发: "+mdInertText(actorType, shortFieldMax))
	}
	if len(meta) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(meta, " · "))
	}

	return b.String()
}
