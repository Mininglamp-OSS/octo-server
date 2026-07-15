package notify

// NotifyReq 通知请求
//
// Payload / Card / DocsCard / ApprovalCard 四选一:
//   - 文本通知(现状):填 Payload{type:1,content:...},另两者省略。
//   - 智能总结卡片(summary-notify pilot):填结构化 Card,另两者省略。服务端用
//     pkg/cardtmpl 生成 octo/v1 卡片,经 internal/carddispatch 派发。
//   - 文档通知卡片(docs-notify):填结构化 DocsCard,另两者省略。
//   - 通用审批卡片:填结构化 ApprovalCard；owner 由路由绑定的独立 token
//     决定，调用方只提供 action_type 和有界业务标识。
//
// 调用方永不构造 type-17 map(Decision 14 仍拒绝 payload 里的 card 形状)。
type NotifyReq struct {
	SpaceID      string                 `json:"space_id" binding:"required"`
	Service      string                 `json:"service" binding:"required"`
	Event        string                 `json:"event"`
	Targets      []string               `json:"targets" binding:"required"`
	ActorUID     string                 `json:"actor_uid"`
	Payload      map[string]interface{} `json:"payload"`
	Card         *SummaryCardFields     `json:"card"`
	DocsCard     *DocsCardFields        `json:"docs_card"`
	ApprovalCard *ApprovalCardFields    `json:"approval_card"`
}

type ApprovalCardFields struct {
	ActionType  string            `json:"action_type"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Data        map[string]string `json:"data"`
}

// SummaryCardFields 是 summary-notify 卡片通知的结构化入参(跨仓契约,见
// .octospec/tasks/card-message-internal-dispatch/summary-notify-contract.md)。
// 只承载原始字段;文案标签、布局、deep-link 由服务端 pkg/cardtmpl +
// i18n.OutboundLanguage 生成。时间字段由调用方按其时区格式化后传字符串,计数传整数。
type SummaryCardFields struct {
	TaskNo      string `json:"task_no"`      // summary_task.task_no,用于 /s/{task_no}?sp={space_id}
	Kind        string `json:"kind"`         // "completed" | "failed"
	Title       string `json:"title"`        // 总结标题(服务端转义/截断)
	TimeRange   string `json:"time_range"`   // 已格式化的时间范围;空则省略
	Members     int    `json:"members"`      // 参与人数;<=0 省略
	MsgCount    int    `json:"msg_count"`    // 消息条数;<=0 省略
	GeneratedAt string `json:"generated_at"` // 已格式化的生成时间;空则省略
	Reason      string `json:"reason"`       // failed 的脱敏原因;completed 留空
}

// Card notification kinds.
const (
	SummaryCardKindCompleted = "completed"
	SummaryCardKindFailed    = "failed"
)

// DocsCardFields is the docs-notify structured card input (cross-repo contract,
// see .octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md).
// Fields carry raw values only; attribution/label copy, layout, and the
// /d/{doc_id}?sp={space_id} deep-link are owned by the server (pkg/cardtmpl +
// modules/notify.buildDocsCard + i18n.OutboundLanguage). The docs backend
// pre-formats display strings it wants surfaced verbatim (ActorName, UpdatedAt)
// so octo-server does not resolve secondary identities on the card path.
type DocsCardFields struct {
	DocID     string `json:"doc_id"`     // maps to /d/{doc_id}?sp={space_id}
	RequestID string `json:"request_id"` // required by access_requested v2; docs domain idempotency/CAS key
	Kind      string `json:"kind"`       // "shared" | "commented" | "access_requested"
	Title     string `json:"title"`      // document title
	ActorName string `json:"actor_name"` // pre-resolved actor display name; empty allowed
	Excerpt   string `json:"excerpt"`    // optional preview / comment / access reason
	UpdatedAt string `json:"updated_at"` // pre-formatted timestamp; empty allowed
}

// Docs card notification kinds.
const (
	DocsCardKindShared          = "shared"
	DocsCardKindCommented       = "commented"
	DocsCardKindAccessRequested = "access_requested"
	DocsCardKindAccessGranted   = "access_granted"
	DocsCardKindAccessDenied    = "access_denied"
)

// BatchNotifyReq 批量通知请求
type BatchNotifyReq struct {
	Notifications []NotifyReq `json:"notifications" binding:"required"`
}

// NotifyResp 单条通知响应
type NotifyResp struct {
	Delivered []string          `json:"delivered"`
	Filtered  map[string]string `json:"filtered"`
}

// BatchNotifyResult 批量通知中单条结果
type BatchNotifyResult struct {
	NotifyResp
	Error string `json:"error,omitempty"`
}

// BatchNotifyResp 批量通知响应
type BatchNotifyResp struct {
	Results   []BatchNotifyResult `json:"results"`
	HasErrors bool                `json:"has_errors"`
}
