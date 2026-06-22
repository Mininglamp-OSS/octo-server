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
type muEnvelope struct {
	Event          string  `json:"event"`
	WorkspaceID    string  `json:"workspace_id"`
	Actor          muActor `json:"actor"`
	Issue          muIssue `json:"issue"`
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
//     "json" / "content"（事件已识别但渲染结果为空）。
//
// header 当前未使用（X-Multica-Event 仅作辅助；事件分发以 body.event 为准，
// 防止 header/body 不一致导致渲染走偏）。保留参数以匹配 pushAdapter.parse 签名，
// 并预留未来按 header 做版本协商的余地。
func parseMulticaPush(_ http.Header, body []byte) (*pushPayloadReq, string, string) {
	var ev muEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, "", "json"
	}
	event := strings.TrimSpace(ev.Event)
	if event == "" {
		// 缺 event 字段是配置错误（不像合法 multica 出站流量），按 json 拒绝。
		// 与 github 的 no_event 不同：github 那边事件名在 header，缺 header 与
		// 缺 body 字段语义有别；multica 全在 body 里，统一走 json 即可。
		return nil, "", "json"
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
	// envelope 里的 title 长度由 multica 端控制（理论上可达 MySQL TEXT
	// 上限）；钳到 maxContentRunes 内，绝不让 multica 流量 413。
	return &pushPayloadReq{Content: clipRunes(content, maxContentRunes())}, "", ""
}

// renderMulticaIssueStatusChanged 渲染 issue.status_changed 事件。
//
// 输出形如（markdown，客户端会渲染）：
//
//	**MUL-123** Fix login redirect: `todo` → `in_progress` (by agent)
//
// 设计取舍：
//   - 不拼 issue URL：envelope 当前不带 URL；服务端无法可靠推断 multica
//     部署域名（self-hosted 路径多种）。需要可点链接时由 multica 后续在
//     envelope 加 issue.url 字段，渲染层加链接即可，不破坏既有契约。
//   - 标题与 identifier 之间用空格分隔，与 GitHub 适配器的 issue 渲染风格
//     一致；状态用反引号包起来，避免下划线（in_progress）被 markdown 误
//     当作斜体。
//   - actor 名当前不在 envelope 里，只渲染 type（"by agent" / "by member"）；
//     未显示触发者类型的情况下省略尾注。
//   - 标题里的 `[` / `]` 经 mdLinkText 转义（即便当前没拼成链接也防御性
//     处理，未来加链接时不必再回头改）。
func renderMulticaIssueStatusChanged(ev muEnvelope) string {
	id := strings.TrimSpace(ev.Issue.Identifier)
	title := strings.TrimSpace(ev.Issue.Title)
	status := strings.TrimSpace(ev.Issue.Status)
	prev := strings.TrimSpace(ev.PreviousStatus)
	// id 与 status 是渲染所必需的最小集；缺失任一无法生成可读消息，回退
	// 让上层走 reason=content 而非凭空生造。
	if id == "" || status == "" {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", id)
	if title != "" {
		// 钳到 200 rune 与 GitHub PR/issue 标题渲染保持一致。
		b.WriteString(" ")
		b.WriteString(mdLinkText(title, 200))
	}
	b.WriteString(": ")
	if prev != "" && prev != status {
		fmt.Fprintf(&b, "`%s` → `%s`", prev, status)
	} else {
		// 新建 issue 或事件 payload 缺 previous_status 时只显示当前状态，
		// 不画"→ X"那种暗示状态变化的尾巴。
		fmt.Fprintf(&b, "`%s`", status)
	}
	if actorType := strings.TrimSpace(ev.Actor.Type); actorType != "" {
		fmt.Fprintf(&b, " (by %s)", actorType)
	}
	return b.String()
}
