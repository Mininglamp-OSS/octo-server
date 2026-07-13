package notify

// NotifyReq 通知请求
//
// Payload 与 Card 二选一:
//   - 文本通知(现状):填 Payload{type:1,content:...},Card 省略。
//   - 卡片通知(summary-notify pilot):填结构化 Card,Payload 省略。服务端用
//     pkg/cardtmpl 生成 octo/v1 卡片,经 internal/carddispatch 派发。调用方永不
//     构造 type-17 map(Decision 14 仍拒绝 payload 里的 card 形状)。
type NotifyReq struct {
	SpaceID  string                 `json:"space_id" binding:"required"`
	Service  string                 `json:"service" binding:"required"`
	Event    string                 `json:"event"`
	Targets  []string               `json:"targets" binding:"required"`
	ActorUID string                 `json:"actor_uid"`
	Payload  map[string]interface{} `json:"payload"`
	Card     *SummaryCardFields     `json:"card"`
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
