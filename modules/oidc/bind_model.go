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
//	issued ─── verify ok ───▶ verified ─── confirm ok ───▶ Consume(token 消失)
//	  │                          │
//	  │                          └── confirm fail ──▶ verified (允许重试,直到 ConfirmMax)
//	  │
//	  ├── create CAS lock ───▶ creating ─── IssueSession+Insert+Consume ──▶ (token 消失)
//	  │                          │
//	  │                          └── 中途失败 ──▶ creating (stuck,TTL 自然清理)
//	  │
//	  └── 超限/手动放弃 ──▶ refused
//
// CAS 由 BindStore.CASSave 保证。creating/refused 是中间或终止状态,任何对它们
// 的进一步推进都返 ErrBindStatusConflict。confirmed/created 不入 Redis ——
// 成功路径由 Consume 把 session 整条删除,后续 Get 拿到 ErrBindNotFound。
//
// creating 引入的目的:把"建号副作用"(IssueSession 落库 + identity.Insert)
// 包在 CAS 锁后面,防止并发 verify/create 把已经在写真实用户的 token 推到其他
// 状态后再 saveCreated 撞 conflict —— 那种顺序会留下"ghost user"。
type BindStatus string

const (
	BindStatusIssued    BindStatus = "issued"
	BindStatusVerified  BindStatus = "verified"
	BindStatusConfirmed BindStatus = "confirmed"
	BindStatusRefused   BindStatus = "refused"
	// BindStatusCreating 介于 issued 与 Consume 之间的中间锁:CAS issued→creating
	// 成功后才允许进入 IssueSession / identity.Insert 等副作用阶段。失败留 creating,
	// 由 5min TTL 自然清理(用户需重走 OIDC 登录获取新 bind_token)。
	BindStatusCreating BindStatus = "creating"
)

// IssueReason 描述 bind_token 是从哪种 callback 失败分支签发出来的。
// Create 路径依赖此字段决定是否允许走"自助建号":多账号脏数据(manual_conflict)
// 来源的 token 不应当被允许凭 claims 建号 —— 用户在 dmwork 已经有(多个)账号,
// 走 /bind/create 会再造出新账号加剧数据混乱;只能走 P1 Admin 人工合并。
//
// 字段在 BindSession 中持久化,Info 路径用来回填 create_blocked,Create 路径
// 用来拒绝建号。两个 Issue 入口(callback 的 ResolveOrLink 失败分支)按
// err 类型显式置位,避免 oidc 模块的状态隐式扩散。
type IssueReason string

const (
	// BindReasonUnknownUser claims 未命中已有 dmwork 账号(AllowNewUser=false
	// 或 autolink 未命中)。这是"可以走 /bind/create 自助建号"的合法来源。
	BindReasonUnknownUser IssueReason = "unknown_user"
	// BindReasonManualConflict claims email/phone 命中**多条** dmwork 账号(脏数据)。
	// 不允许走 /bind/create:重复建号会加剧数据混乱;只能走 Admin 人工合并(P1)。
	BindReasonManualConflict IssueReason = "manual_conflict"
)

// BindSession bind_token 在 Redis 里的完整快照。
//
// 字段说明:
//   - JTI:            bind_token 自身,32 字节 base64,Redis key 后缀(SR-7)
//   - Issuer/Subject: OIDC claims 原值,confirm 时直接写 user_oidc_identity
//     —— 用户在 5min TTL 内可能换 IdP session,这里固化避免漂移
//   - CandidateUID:   email/phone 多匹配场景下用户选定的 uid(M1 暂不支持选择,
//     字段预留)。当前实现下用户走密码路径需先输入 username/uid 定位
//   - ClaimsSnapshot: 完整 IDTokenClaims JSON,confirm 时透传给 IssueSession
//   - SDSnapshot:     原 StateData JSON 关键字段(authcode/return_to/device_flag),
//     confirm 后回填到原发起设备的 ThirdAuthcode(FR-6.3)
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
	// IssueReason 见 IssueReason godoc。Create 路径在 manual_conflict 时拒绝;
	// Info 路径用它回填 create_blocked 让前端展示对应的引导文案。
	// 旧 token(本字段引入前签发的)会落空字符串,Create 视同 unknown_user 放行,
	// 保持 5min TTL 窗口内的灰度兼容。
	IssueReason IssueReason `json:"issue_reason,omitempty"`
}
