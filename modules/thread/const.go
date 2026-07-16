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

// ReminderTypeMentionMe 镜像 modules/message.ReminderTypeMentionMe (=1, 有人@我)。
// 在此本地定义避免 thread 包反向依赖 message 包。message 侧的值由
// modules/message/validation_test.go (assert ReminderTypeMentionMe == 1) 固定；
// 跨包等值断言见本包 TestReminderTypeMentionMeMatchesMessagePackage。
const ReminderTypeMentionMe = 1

// 消息类型（与客户端约定）
const (
	ContentTypeThreadCreated = 1100 // 子区创建通知
)

// 源消息 payload 最大大小 (64KB)
const maxSourcePayloadBytes = 64 * 1024

// 子区列表分页默认值
const (
	DefaultThreadPageSize int64 = 15
	MaxThreadPageSize     int64 = 100
)

// listThreads ?status= 入参合法值。
const (
	ListStatusActive   = "active"
	ListStatusArchived = "archived"
	ListStatusAll      = "all"
)
