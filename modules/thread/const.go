package thread

// Thread 状态
const (
	ThreadStatusActive   = 1 // 活跃
	ThreadStatusArchived = 2 // 已归档
	ThreadStatusDeleted  = 3 // 已删除
)

// 成员角色
const (
	MemberRoleNormal  = 0 // 普通成员
	MemberRoleCreator = 1 // 创建者
)

// ChannelID 分隔符
const ChannelIDSeparator = "____"

// Sequence Key
const ThreadSeqKey = "thread"

// 消息类型（与客户端约定）
const (
	ContentTypeThreadCreated = 1100 // 子区创建通知
)
