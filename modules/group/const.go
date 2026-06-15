package group

// 群状态
const (
	// GroupStatusDisabled 已禁用
	GroupStatusDisabled = 0
	// GroupStatusNormal 正常
	GroupStatusNormal = 1
	// GroupStatusDisband 解散
	GroupStatusDisband = 2
)

// MaxGroupNameLen 群名最大长度（按 rune 计）。Web / Bot / Integration 建群与改名共用，
// 超长一律静默截断到此长度（API 层可前置 reject 给出明确错误）。配套的 `group`.`name`
// 列宽由迁移加宽到 VARCHAR(50)，两者必须一致，否则 MySQL 严格模式下会报 Data too long。
const MaxGroupNameLen = 50

// 群成员角色
const (
	// MemberRoleCommon 普通成员
	MemberRoleCommon = 0
	// MemberRoleCreator 创建者
	MemberRoleCreator = 1
	// MemberRoleManager 管理者
	MemberRoleManager = 2
)

const (
	// InviteStatusWait 等待确认
	InviteStatusWait = 0
	// InviteStatusOK 已确认
	InviteStatusOK = 1
)

// 群类型
type GroupType int

const (
	GroupTypeCommon GroupType = iota // 普通群
	GroupTypeSuper                   // 超大群
)

const (
	ChannelServiceName = "channel"
)
