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
//
// 收件人有两种互斥的指定方式(见 target_role.go / validateTargeting):
//   - `targets`: 显式 uid 列表(现状,所有既有调用方都走这条)。
//   - `target_role`: 角色选择器,由服务端在 space_member 里解析出收件人。
//
// 二者**恰好只能有一个**。都给或都不给 → 400 err.shared.param.invalid。
type NotifyReq struct {
	SpaceID string `json:"space_id" binding:"required"`
	Service string `json:"service" binding:"required"`
	Event   string `json:"event"`
	// Targets is the explicit recipient list. It LOST its binding:"required"
	// tag when target_role was introduced, because binding-level `required`
	// cannot express "exactly one of two fields". The requirement did not go
	// away — it moved into validateTargeting (target_role.go), which is
	// strictly stricter than the old tag: it still rejects an absent/empty
	// `targets` when no role selector is supplied, AND it now also rejects
	// supplying both. A caller that keeps sending `targets` sees no behavioural
	// change whatsoever.
	//
	// That statement is about the single-item POST /v1/internal/notify, which
	// binds a NotifyReq directly. It never applied to /notify/batch: this
	// struct's binding tags were never evaluated for batch entries, because
	// BatchNotifyReq.Notifications carries no `dive` tag and
	// go-playground/validator does not descend into slice elements without one.
	// A batch entry with missing or empty `targets` bound cleanly before and
	// binds cleanly now; it is reported per-item inside a 207. See
	// sendNotifyBatch.
	Targets []string `json:"targets"`
	// TargetRole asks octo-server to resolve the recipients itself from
	// space_member instead of naming them. The only accepted value is
	// TargetRoleSpaceAdmin ("space_admin") = this Space's active owners and
	// admins (role>=1), robots excluded. Empty means "use Targets".
	//
	// Additive + `omitempty`: absent from every request today, and absent from
	// every serialization that does not set it, so the existing docs /
	// bot-mention / summary-card producers are byte-unaffected.
	TargetRole   string                 `json:"target_role,omitempty"`
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
	// Actions is optional. When omitted, octo-server renders the localized
	// approve/deny buttons (byte-compatible with the pre-http-actions release).
	// When present, it must contain 1..MaxApprovalCustomActions items; each
	// decision is a stable callback token and each title is display text.
	Actions []ApprovalCardAction `json:"actions,omitempty"`
}

// ApprovalCardAction lets the caller name a bounded, custom button on the
// generic approval card. Server owns the action ID and reserved metadata.
type ApprovalCardAction struct {
	Decision string `json:"decision"`
	Title    string `json:"title"`
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
	// ActorUID is the requester's uid. When ActorName is empty, octo-server (the
	// identity authority) resolves the display name from it server-side, so the
	// producer need not hold a user-lookup credential. Empty => no resolution.
	ActorUID string `json:"actor_uid"`
	// ActorAvatarURL is an optional absolute https avatar for the requester,
	// surfaced on the access-request approval card. Additive & backward
	// compatible: empty (or omitted) renders no avatar; a non-https value fails
	// the card build (same positive allowlist as any rendered image URL). The
	// docs backend owns population; octo-server does not resolve it.
	ActorAvatarURL string `json:"actor_avatar_url"`
	Excerpt        string `json:"excerpt"`    // optional preview / comment / access reason
	UpdatedAt      string `json:"updated_at"` // pre-formatted timestamp; empty allowed
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
	// DeliveredCards carries the IM coordinates of each successfully delivered
	// docs card so a producer can later locate and mutate it in place (docs
	// access-decision card sync, task docs-access-decision-card-sync). Populated
	// only on the docs-card path and only for card (not text-fallback) sends;
	// omitted on legacy/text paths. Additive + omitempty so existing consumers
	// that read only Delivered are unaffected.
	DeliveredCards []DeliveredCard `json:"delivered_cards,omitempty"`
}

// DeliveredCard is the IM locator of one delivered card. The card is located by
// (ChannelID, MessageID); ClientMsgNo is carried for idempotency/audit.
// MessageID is a decimal STRING, not a number: IM message ids are int64 and can
// exceed JS's 2^53 safe-integer range, so a JSON number would silently lose
// precision in the docs-backend (Node) consumer.
type DeliveredCard struct {
	UID         string `json:"uid"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	MessageID   string `json:"message_id"`
	ClientMsgNo string `json:"client_msg_no"`
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
