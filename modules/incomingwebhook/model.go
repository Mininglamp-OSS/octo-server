package incomingwebhook

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
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
type pushPayloadReq struct {
	Content   string                 `json:"content"`
	Username  string                 `json:"username,omitempty"`
	AvatarURL string                 `json:"avatar_url,omitempty"`
	Extra     map[string]interface{} `json:"extra,omitempty"`
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
