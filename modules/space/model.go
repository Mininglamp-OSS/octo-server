package space

import "github.com/Mininglamp-OSS/octo-server/pkg/db"

// ---------- DB Models ----------

const (
	JoinModeDirect   = 0 // 直接加入
	JoinModeApproval = 1 // 需要审批
)

// Space 状态常量（SpaceModel.Status）
const (
	SpaceStatusDisbanded = 0 // 已解散
	SpaceStatusNormal    = 1 // 正常
	SpaceStatusBanned    = 2 // 已封禁
)

// SpaceModel 空间表模型
type SpaceModel struct {
	SpaceId        string  // 空间ID
	Name           string  // 空间名称
	Description    string  // 空间描述
	Logo           string  // 空间Logo
	Creator        string  // 创建者uid
	MaxUsers       int     // 最大成员数 0.不限制
	PresetGroupIds *string // 预设群组ID列表（JSON数组）
	JoinMode       int     // 加入模式 0=直接加入 1=需要审批
	Status         int     // 状态 1.正常 0.已解散
	Version        int64   // 版本号
	db.BaseModel
}

// MemberModel 空间成员表模型
type MemberModel struct {
	SpaceId string // 空间ID
	UID     string // 成员uid
	Role    int    // 成员角色 0.普通成员 1.管理员 2.拥有者
	Status  int    // 状态 1.正常 0.已移除
	Version int64  // 版本号
	// IsBot 是否 bot 账号，冗余自 user.robot（不可变，故无需同步）。
	// 由 DB 层的 stampMemberIsBot / memberIsBotSubquery 统一填充，调用方不要自己设。
	//
	// 列名刻意不叫 robot：queryMembers / searchMembers 用 `sm.*` 加自己的
	// `... as robot` 别名，同名列会在结果集里撞车、改变这两个已废弃接口的取值。
	IsBot int
	db.BaseModel
}

type memberSearchModel struct {
	MemberModel
	Name     string
	Username string
	Email    string
	Phone    string
	Robot    int
}

// InvitationModel 邀请表模型
type InvitationModel struct {
	SpaceId    string   // 空间ID
	InviteCode string   // 邀请码
	Creator    string   // 创建者uid
	MaxUses    int      // 最大使用次数
	UsedCount  int      // 已使用次数
	ExpiresAt  *db.Time // 过期时间
	Status     int      // 状态 1.有效 0.无效
	db.BaseModel
}

// spaceJoinApplyModel 加入申请表模型
type spaceJoinApplyModel struct {
	Id          int64  // 申请ID
	SpaceId     string // 空间ID
	UID         string // 申请人UID
	InviteCode  string // 使用的邀请码
	Remark      string // 申请备注
	Status      int    // 0=待处理 1=通过 2=拒绝
	ReviewerUID string // 审批人UID
	db.BaseModel
}

// spaceJoinApplyDetailModel 带申请人名称的申请详情
type spaceJoinApplyDetailModel struct {
	spaceJoinApplyModel
	ApplicantName string // 申请人名称
}

// EmailInvite 类型常量
const (
	EmailInviteTypeOwner  = 1 // 邀请成为新空间的 owner（lazy-create）
	EmailInviteTypeMember = 2 // 邀请加入已有空间作为成员/管理员
)

// EmailInvite 状态常量
const (
	EmailInviteStatusPending  = 0
	EmailInviteStatusConsumed = 1
	EmailInviteStatusExpired  = 2
	EmailInviteStatusRevoked  = 3
)

// EmailInvite 角色常量（仅 member 类型）
const (
	EmailInviteRoleMember = 0
	EmailInviteRoleAdmin  = 1
)

// spaceEmailInviteModel 邮件邀请表模型。
// Id / CreatedAt / UpdatedAt 由嵌入的 db.BaseModel 提供，与本模块其他 model
// （SpaceModel / MemberModel / InvitationModel）保持一致。
type spaceEmailInviteModel struct {
	TokenHash          string   // SHA-256 hex
	InviteType         int      // 1=owner 2=member
	Email              string   // 收件邮箱
	SpaceId            string   // member 类型关联空间ID
	Role               int      // member 角色
	PlannedName        string   // owner 类型计划空间名
	PlannedDescription string   // owner 类型计划描述
	PlannedLogo        string   // owner 类型计划 logo
	PlannedMaxUsers    int      // owner 类型计划最大成员数
	PlannedJoinMode    int      // owner 类型计划加入模式
	Status             int      // 0=pending 1=consumed 2=expired 3=revoked
	ExpiresAt          *db.Time // 过期时间
	CreatedBy          string   // 发起人UID
	ConsumedBy         string   // 接受人UID
	ConsumedAt         *db.Time // 接受时间
	db.BaseModel
}

// ---------- Request Models ----------

type createSpaceReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	JoinMode    int    `json:"join_mode"` // 0=直接加入(默认) 1=需要审批
}

// updateSpaceReq 用户侧 PUT /v1/space/:space_id 请求体。
// 所有字段均为可选指针；nil 表示不变更。handler 拒空 body / all-nil。
// 注意：max_users 仍是管理端专属，本结构不暴露。
type updateSpaceReq struct {
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	Logo           *string `json:"logo"`
	JoinMode       *int    `json:"join_mode"`
	PresetGroupIds *string `json:"preset_group_ids"`
}

type addMemberReq struct {
	UIDs []string `json:"uids"`
}

type removeMemberReq struct {
	UIDs []string `json:"uids"`
}

type updateMemberRoleReq struct {
	Role int `json:"role"`
}

type joinSpaceReq struct {
	InviteCode string `json:"invite_code"`
}

// ---------- Response Models ----------

type spaceResp struct {
	SpaceId     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	Creator     string `json:"creator"`
	Status      int    `json:"status"`
	Role        int    `json:"role"`
	MaxUsers    int    `json:"max_users"`
	MemberCount int    `json:"member_count"`
	JoinMode    int    `json:"join_mode"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type memberResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Role      int    `json:"role"`
	Robot     int    `json:"robot"`
	CreatedAt string `json:"created_at"`
}

// ---------- Directory Response Models ----------

// 目录行的身份取值。kind 只有这两种，没有第三态：停用 bot 不会退化成 human，
// 它在查询阶段就整行排除（见 directoryWhereClause）。
const (
	directoryKindHuman = "human"
	directoryKindBot   = "bot"
)

// bot 与当前登录用户的好友关系态，取值与 /v1/robot/space_bots 的 status 一致。
const (
	directoryRelationAdded    = "added"
	directoryRelationPending  = "pending"
	directoryRelationNotAdded = "not_added"
)

// directoryBotResp 目录行的 bot 附加信息。
//
// 刻意只带 relation。space_bots 还会返回 description / creator_uid /
// creator_name / bot_commands / auto_approve，本接口一个都不给：目录行渲染的是
// 头像（由 uid 推导）+ 名字 + 在线态 + AI 的已添加状态，其余字段列表侧无人读，
// 而 bot_commands 是 bot 的完整命令列表——按线上最大 Space 的 ~4400 个 bot 估算，
// 胖行会把一次性响应推到 MB 级，精简行约 740KB（gzip 后 ~120KB）。bot 详情自有
// 接口，列表不兼任详情投影。将来确有调用方需要，应显式加 ?detail=full，而不是
// 放宽默认返回。
type directoryBotResp struct {
	Relation string `json:"relation"`
}

// directoryItemResp 目录单行。Bot 仅在 Kind == bot 时存在。
type directoryItemResp struct {
	UID       string            `json:"uid"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Role      int               `json:"role"`
	CreatedAt string            `json:"created_at"`
	Bot       *directoryBotResp `json:"bot,omitempty"`
}

// directoryResp 目录分页响应。Total 是过滤后、分页前的总数，客户端据此渲染
// tab 计数，不必再把整份名单拉回本地做 filter 计算。
type directoryResp struct {
	Total int                 `json:"total"`
	Page  int                 `json:"page"`
	Limit int                 `json:"limit"`
	Items []directoryItemResp `json:"items"`
}

// directoryRowModel 目录查询的行模型。
//
// 复用 MemberDetailModel 以继承 #344 的 DisplayName 兜底链（user.name →
// user_verification.real_name → 稳定占位符）。「是不是 bot 账号」读的是
// MemberModel.IsBot（space_member 自己的冗余列），ActiveBot 独立记录「robot 表里
// 是否存在 status=1 的行」，两者分开才能把停用 bot 与真人区分开。
type directoryRowModel struct {
	MemberDetailModel
	ActiveBot int
}

// isBot 判定该行是否应以 bot 身份呈现：既是 bot 账号，又处于启用状态。
// 查询侧已排除「是 bot 账号但未启用」的行，这里的双条件是冗余防御——若将来
// WHERE 被改动，分类逻辑不会跟着悄悄把停用 bot 归到 human。
func (m *directoryRowModel) isBot() bool {
	return m.IsBot == 1 && m.ActiveBot == 1
}

type memberSearchResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Role      int    `json:"role"`
	Robot     int    `json:"robot"`
	CreatedAt string `json:"created_at"`
}

type inviteResp struct {
	InviteCode  string `json:"invite_code"`
	SpaceId     string `json:"space_id"`
	SpaceName   string `json:"space_name"`
	Creator     string `json:"creator"`
	MaxUses     int    `json:"max_uses"`
	UsedCount   int    `json:"used_count"`
	ExpiresAt   string `json:"expires_at"`
	MemberCount int    `json:"member_count"`
	JoinMode    int    `json:"join_mode"`
}

// botResp Bot 信息响应
type botResp struct {
	RobotID string `json:"robot_id"`
	Name    string `json:"name"`
	Avatar  string `json:"avatar"`
}

// invitePreviewResp 邀请预览响应（含 Bot 列表）
type invitePreviewResp struct {
	InviteCode  string    `json:"invite_code"`
	SpaceId     string    `json:"space_id"`
	SpaceName   string    `json:"space_name"`
	Description string    `json:"description"`
	Logo        string    `json:"logo"`
	Creator     string    `json:"creator"`
	MaxUses     int       `json:"max_uses"`
	UsedCount   int       `json:"used_count"`
	ExpiresAt   string    `json:"expires_at"`
	MemberCount int       `json:"member_count"`
	JoinMode    int       `json:"join_mode"`
	Bots        []botResp `json:"bots"`
}

// updateInviteReq 更新邀请码请求。Status 为可选字段：
//   - nil：不改状态
//   - 0：禁用（等价 DELETE）
//   - 1：启用（可用于对误禁邀请码恢复）
type updateInviteReq struct {
	MaxUses   *int    `json:"max_uses"`
	ExpiresAt *string `json:"expires_at"`
	Status    *int    `json:"status"`
}

// spaceInviteListResp 用户端邀请码列表单条响应
type spaceInviteListResp struct {
	InviteCode string `json:"invite_code"`
	SpaceId    string `json:"space_id"`
	Creator    string `json:"creator"`
	MaxUses    int    `json:"max_uses"`
	UsedCount  int    `json:"used_count"`
	ExpiresAt  string `json:"expires_at"`
	Status     int    `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// MemberDetailModel 带用户名的成员详情
type MemberDetailModel struct {
	MemberModel
	Name     string // 用户名称（user.name）
	RealName string // 实名兜底（user_verification.real_name），仅作 name 为空时的回退源
	Robot    int    // 是否机器人 1=是 0=否
}

// memberDisplayNamePlaceholderPrefix 是成员展示名的稳定占位符前缀。
// 当 user.name 与 user_verification.real_name 均为空时，用 "<前缀><uid>" 作为
// 可读且稳定的占位符，避免前端渲染空白行。uid 已经在 memberResp 里返回，故
// 用它兜底不泄露新信息——但禁止用 short_no / username 兜底（privacy-gated）。
const memberDisplayNamePlaceholderPrefix = "User "

// DisplayName 返回成员的展示名，按 issue #344 的兜底链解析：
//  1. user.name 非空 → 原样返回；
//  2. 否则 user_verification.real_name 非空 → 返回实名；
//  3. 都空 → 返回稳定占位符 "User <uid>"。
//
// 任何分支都不会返回空串，也不会暴露 short_no / username。
func (m *MemberDetailModel) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	if m.RealName != "" {
		return m.RealName
	}
	return memberDisplayNamePlaceholderPrefix + m.UID
}

// SpaceDetailModel 带成员数和角色的空间详情（MaxUsers 从 SpaceModel 继承）
type SpaceDetailModel struct {
	SpaceModel
	Role        int // 当前用户角色
	MemberCount int // 成员数
}

// ---------- Join Apply Response Models ----------

// spaceJoinApplyResp 加入申请响应
type spaceJoinApplyResp struct {
	ID            int64  `json:"id"`
	SpaceId       string `json:"space_id"`
	UID           string `json:"uid"`
	ApplicantName string `json:"applicant_name"`
	Remark        string `json:"remark"`
	Status        int    `json:"status"`
	ReviewerUID   string `json:"reviewer_uid,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// spaceJoinApplyListResp 加入申请列表响应
type spaceJoinApplyListResp struct {
	List  []*spaceJoinApplyResp `json:"list"`
	Count int64                 `json:"count"`
}
