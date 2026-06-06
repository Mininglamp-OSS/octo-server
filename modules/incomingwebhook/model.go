package incomingwebhook

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

// incoming_webhook.status 取值。SQL 列为 SMALLINT，仅 0/1 由 init 迁移注释覆盖；
// 2 是 #254 引入的软删除标记，复用同一列、无需迁移。
//
//   - statusEnabled  正常可推送（push 闸要求 status == statusEnabled）。
//   - statusDisabled 管理员主动关停，或群解散级联禁用（handleGroupDisband）。
//   - statusDeleted  软删除：保留行供历史消息渲染（display datasource 不按 status
//     过滤），同时 push 自动失效、从管理列表隐藏、释放每群配额，且不可经 update 复活。
const (
	statusDisabled = 0
	statusEnabled  = 1
	statusDeleted  = 2
)

// incomingWebhookModel 对应 incoming_webhook 表。
type incomingWebhookModel struct {
	WebhookID  string
	TokenHash  string
	GroupNo    string
	SpaceID    string
	Name       string
	Avatar     string
	CreatorUID string
	Status     int
	LastUsedAt dbr.NullTime
	CallCount  int64
	db.BaseModel
}

// auditModel 对应 incoming_webhook_audit 表，记录成功推送的最小审计信息。
type auditModel struct {
	WebhookID string
	GroupNo   string
	IP        string
	ByteSize  int
	MessageID int64
	db.BaseModel
}

// pushPayloadReq 推送端点的请求体。
//
// 兼容两种消息形态，由 MsgType 选择（缺省/"text" 走纯文本，与历史行为一致）：
//   - 纯文本：Content 必填，客户端按 markdown 渲染（历史契约不变）。
//   - 富文本/图文混排（MsgType=="richtext"）：Blocks 承载有序图文块，服务端翻译为
//     octo 原生 RichText(=14) 后走 richtext.Validate/Finalize。对外刻意不暴露内部
//     ContentType 数字与 plain 等服务端字段——调用方只需描述「文本块 / 图片块」。
type pushPayloadReq struct {
	MsgType   string                 `json:"msg_type,omitempty"`
	Content   string                 `json:"content"`
	Blocks    []webhookBlock         `json:"blocks,omitempty"`
	Username  string                 `json:"username,omitempty"`
	AvatarURL string                 `json:"avatar_url,omitempty"`
	Extra     map[string]interface{} `json:"extra,omitempty"`
}

// webhookBlock 是富文本消息的单个有序块（对外契约）。字段刻意与 octo-lib
// common.RichTextBlock 对齐但独立声明：对外只暴露 text/image 两类块所需的字段，
// 不让调用方感知内部 ContentType 编号或 plain/size 等服务端派生字段。
//   - type=="text"  ：Text 必填且非空。
//   - type=="image" ：URL（仅 http/https）+ Width/Height（>0）必填，供端上占位排版。
//
// ⚠️ 新增块类型时务必与 pkg/richtext（及其底层 common.RichTextBlock 支持的类型）保持
// 同步：buildRichTextPayload 按白名单逐类翻译，未在此显式支持的块类型会被 Validate 拒绝。
type webhookBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// createReq 管理端创建 webhook 的请求体。
type createReq struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// updateReq 修改 webhook 的请求体。零值字段不更新。
type updateReq struct {
	Name   *string `json:"name,omitempty"`
	Avatar *string `json:"avatar,omitempty"`
	Status *int    `json:"status,omitempty"`
}

// webhookResp 对外暴露的 webhook 元信息（不含 token / token_hash）。
type webhookResp struct {
	WebhookID  string `json:"webhook_id"`
	GroupNo    string `json:"group_no"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	CreatorUID string `json:"creator_uid"`
	Status     int    `json:"status"`
	LastUsedAt int64  `json:"last_used_at"`
	CallCount  int64  `json:"call_count"`
	CreatedAt  int64  `json:"created_at"`
}

// createResp 创建/重置返回；token 仅此一次出现。
type createResp struct {
	webhookResp
	Token string `json:"token"`
	URL   string `json:"url"`
}
