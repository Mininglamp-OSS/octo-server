package oidc

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
)

// IdentityModel 第三方 OIDC 身份与本地 UID 的绑定关系
type IdentityModel struct {
	UID           string
	Issuer        string
	Subject       string
	Email         string
	EmailVerified int
	Phone         string
	PhoneVerified int
	LinkedAt      time.Time
	LastLoginAt   *time.Time
	db.BaseModel
}

// RefreshModel OIDC refresh_token 的加密存储,用于后台状态同步轮询
type RefreshModel struct {
	IdentityID      int64
	TokenHash       string
	TokenCiphertext []byte
	ExpiresAt       time.Time
	LastRefreshedAt *time.Time
	RevokedAt       *time.Time
	db.BaseModel
}

// AuditEvent 审计事件类型
type AuditEvent string

const (
	EventAuthorize    AuditEvent = "authorize"
	EventCallbackOK   AuditEvent = "callback_ok"
	EventCallbackFail AuditEvent = "callback_fail"
	EventRefreshOK    AuditEvent = "refresh_ok"
	EventRefreshFail  AuditEvent = "refresh_fail"
	EventLogout       AuditEvent = "logout"

	// 自助绑定流程事件(P0,SR-6 审计完整性)。
	// 不改 oidc_audit_log schema —— 仍写同一张表,通过 event 列区分场景。
	EventBindIssued      AuditEvent = "bind_issued"
	EventBindVerifyOK    AuditEvent = "bind_verify_ok"
	EventBindVerifyFail  AuditEvent = "bind_verify_fail"
	EventBindOTPSend     AuditEvent = "bind_otp_send"
	EventBindOTPSendFail AuditEvent = "bind_otp_send_fail"
	EventBindConfirmOK   AuditEvent = "bind_confirm_ok"
	EventBindConfirmFail AuditEvent = "bind_confirm_fail"
	EventBindRefused     AuditEvent = "bind_refused"
	EventBindCreated     AuditEvent = "bind_created"
	EventBindCreateFail  AuditEvent = "bind_create_fail"
)

// AuditModel 登录与状态同步审计
//
// Event 用 AuditEvent 字符串别名,提供编译期类型安全防止误用任意字符串;
// dbr 通过反射读取底层 string,落库行为与裸 string 字段完全一致。
type AuditModel struct {
	UID       string
	Event     AuditEvent
	IP        string
	UserAgent string
	Reason    string
	TraceID   string
	db.BaseModel
}
