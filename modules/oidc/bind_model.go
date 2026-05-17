package oidc

// BindMethod 自助绑定流程允许的二次验证手段(需求 FR-3.1)。
//
// 类型化字符串便于:
//   - 配置文件解析时做 allowlist 过滤(SR-3 禁用 email_otp)
//   - 审计日志按手段维度聚合
//   - 前端 /info 接口直接展示可用方法列表
type BindMethod string

const (
	// BindMethodPassword 账号密码二次验证。
	BindMethodPassword BindMethod = "password"
	// BindMethodSMSOTP 短信 OTP 二次验证。OTP 必须发到 OIDC claims 中
	// phone_number_verified=true 的手机号,不接受用户输入(FR-3.3)。
	BindMethodSMSOTP BindMethod = "sms_otp"
	// BindMethodEmailOTP **明确禁用**(SR-3)。若 IdP 返回的 email 与 dmwork
	// user.email 同值,邮箱 OTP 等价于"用 OIDC claim 自证身份",退化为单
	// 因子。常量保留是为了让"非法手段过滤"的代码路径可读 —— 见
	// loadBindMethods 中的显式 drop。
	BindMethodEmailOTP BindMethod = "email_otp"
)

// validBindMethods 自助绑定可用的二次验证手段白名单。BindMethodEmailOTP
// 故意不在内(SR-3)。
var validBindMethods = map[BindMethod]struct{}{
	BindMethodPassword: {},
	BindMethodSMSOTP:   {},
}

// BindStatus 自助绑定 bind_token 状态机。Redis 持久化,TTL 由 BindConfig.TokenTTL 控制。
//
// 合法迁移路径:
//
//	issued ─── verify ok ───▶ verified ─── confirm ok ───▶ confirmed
//	  │                          │
//	  │                          └── confirm fail ──▶ verified (允许重试,直到 ConfirmMax)
//	  │
//	  └── 超限/手动放弃 ──▶ refused
//
// CAS 由 BindStore.UpdateStatus 保证。confirmed/refused 是终态,任何指向终态
// 之外的 UpdateStatus 都应当返 ErrBindStatusConflict。
type BindStatus string

const (
	BindStatusIssued    BindStatus = "issued"
	BindStatusVerified  BindStatus = "verified"
	BindStatusConfirmed BindStatus = "confirmed"
	BindStatusRefused   BindStatus = "refused"
)

// BindSession bind_token 在 Redis 里的完整快照。
//
// 字段说明:
//   - JTI:            bind_token 自身,32 字节 base64,Redis key 后缀(SR-7)
//   - Issuer/Subject: OIDC claims 原值,confirm 时直接写 user_oidc_identity
//                     —— 用户在 5min TTL 内可能换 IdP session,这里固化避免漂移
//   - CandidateUID:   email/phone 多匹配场景下用户选定的 uid(M1 暂不支持选择,
//                     字段预留)。当前实现下用户走密码路径需先输入 username/uid 定位
//   - ClaimsSnapshot: 完整 IDTokenClaims JSON,confirm 时透传给 IssueSession
//   - SDSnapshot:     原 StateData JSON 关键字段(authcode/return_to/device_flag),
//                     confirm 后回填到原发起设备的 ThirdAuthcode(FR-6.3)
//   - Status:         状态机字段(见 BindStatus)
//   - VerifiedMethod: 记录哪种手段通过的(audit 维度)
//   - OriginIP/UA:    审计 + FR-6.2 设备差异提示
//   - CreatedAt:      签发时 Unix 秒,前端展示 + TTL 兜底计算
//
// JSON 序列化:tag 显式写小写,与 oidc 模块其他 *_redis 实现一致。
type BindSession struct {
	JTI            string     `json:"jti"`
	Issuer         string     `json:"issuer"`
	Subject        string     `json:"sub"`
	CandidateUID   string     `json:"candidate_uid,omitempty"`
	ClaimsSnapshot []byte     `json:"claims"`
	SDSnapshot     []byte     `json:"sd"`
	Status         BindStatus `json:"status"`
	VerifiedMethod BindMethod `json:"verified_method,omitempty"`
	OriginIP       string     `json:"origin_ip,omitempty"`
	OriginUA       string     `json:"origin_ua,omitempty"`
	CreatedAt      int64      `json:"created_at"`
}
