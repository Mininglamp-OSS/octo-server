package opanalytics

// ===== ETL 持久化模型 (扁平结构，字段↔列名一一对应，不嵌入 BaseModel) =====

// factMemberChannelDailyModel 对应 octo_fact_member_channel_daily 的一行(③)。
type factMemberChannelDailyModel struct {
	StatDate    string // YYYY-MM-DD (报告时区自然日)
	ChannelID   string
	ChannelType uint8
	SpaceID     string
	ConvType    uint8
	ContentType uint8
	SenderUID   string
	SenderType  uint8
	MsgCount    int
	LastMsgAt   int64
}

// factChannelDailyModel 对应 octo_fact_channel_daily 的一行(④)。
type factChannelDailyModel struct {
	StatDate           string
	ChannelID          string
	ChannelType        uint8
	SpaceID            string
	ConvType           uint8
	HumanMsgCount      int
	AgentMsgCount      int
	ActiveHumanMembers int
	ActiveAgentMembers int
	LastMsgAt          int64
}

// ===== ETL 源读取/中间模型 =====

// userDimRow 从 user 表读出的成员维表来源行。
type userDimRow struct {
	UID      string
	Name     string
	Email    string
	Phone    string
	Zone     string
	Robot    int    // 0=human 1=agent
	Category string // 'system' 等，用于排除判定
	Status   int    // user.status: 1=正常 其它=禁用/注销(从成员总数剔除)
}

// groupDimRow 从 group 表读出的会话维表来源行。
type groupDimRow struct {
	GroupNo      string
	Name         string
	SpaceID      string
	Status       int   // group.status: 0=异常 1=正常
	CreatedAtSec int64 // UNIX_TIMESTAMP(group.created_at)
}

// groupMemberCountRow group_member(status=1) 按群聚合的人/agent 计数。
type groupMemberCountRow struct {
	GroupNo  string
	AgentCnt int
	TotalCnt int
}

// srcMessageRow 分片 message 表读出的单条消息(ETL 只取聚合所需列)。
// ID 是分片表自增主键，用作增量抽取的 keyset 水位(message 表无 timestamp 索引)。
// CreatedUnix 是落库时间(纪元秒)，用于稳定性滞后闸门(见 etlLagSeconds)。
type srcMessageRow struct {
	ID          int64
	FromUID     string
	ChannelID   string
	ChannelType uint8
	Timestamp   int64 // 发送时间(纪元秒)
	CreatedUnix int64 // 落库时间(纪元秒, = UNIX_TIMESTAMP(created_at))
}

// ===== 读侧(接口)结果 / 响应 VO =====

// overviewResp 模块A 概览卡片(私聊只出活跃数，无总数/占比)。
type overviewResp struct {
	SpaceTotal         int64                     `json:"space_total"`
	GroupTotal         int64                     `json:"group_total"`
	HumanMemberTotal   int64                     `json:"human_member_total"`
	AgentTotal         int64                     `json:"agent_total"`
	ActiveGroups       int64                     `json:"active_groups"`
	ActiveHumanMembers int64                     `json:"active_human_members"`
	ActiveAgentMembers int64                     `json:"active_agent_members"`
	HumanMsgCount      int64                     `json:"human_msg_count"`
	AgentMsgCount      int64                     `json:"agent_msg_count"`
	PrivateActiveCount int64                     `json:"private_active_count"` // 私聊活跃数(口径1：不出总数)
	MessageComposition []*messageCompositionItem `json:"message_composition"`
}

// messageCompositionItem 模块B 会话类型分布(固定 conv_type=1..4，缺数据补 0)。
type messageCompositionItem struct {
	ConvType           uint8 `json:"conv_type"`
	HumanMsgCount      int64 `json:"human_msg_count"`
	AgentMsgCount      int64 `json:"agent_msg_count"`
	TotalMsgCount      int64 `json:"total_msg_count"`
	ActiveChannelCount int64 `json:"active_channel_count"`
}

// trendResp 模块C 趋势响应。
type trendResp struct {
	Granularity string       `json:"granularity"`
	List        []*trendItem `json:"list"`
}

type trendItem struct {
	Bucket             string                  `json:"bucket"`
	StartDate          string                  `json:"start_date"`
	EndDate            string                  `json:"end_date"`
	HumanMsgCount      int64                   `json:"human_msg_count"`
	AgentMsgCount      int64                   `json:"agent_msg_count"`
	TotalMsgCount      int64                   `json:"total_msg_count"`
	ActiveHumanMembers int64                   `json:"active_human_members"`
	ActiveAgentMembers int64                   `json:"active_agent_members"`
	ActiveGroups       int64                   `json:"active_groups"`
	PrivateActiveCount int64                   `json:"private_active_count"`
	ConvTypeMsgCounts  []*trendConvTypeMsgItem `json:"conv_type_msg_counts"`
}

type trendConvTypeMsgItem struct {
	ConvType      uint8 `json:"conv_type"`
	HumanMsgCount int64 `json:"human_msg_count"`
	AgentMsgCount int64 `json:"agent_msg_count"`
	TotalMsgCount int64 `json:"total_msg_count"`
}

// spaceListItem 表一 Space 列表行。
type spaceListItem struct {
	SpaceID          string `json:"space_id"`
	Name             string `json:"name"`
	GroupTotal       int64  `json:"group_total"`
	HumanMemberTotal int64  `json:"human_member_total"`
	AgentTotal       int64  `json:"agent_total"`
	HumanMsgCount    int64  `json:"human_msg_count"`
	AgentMsgCount    int64  `json:"agent_msg_count"`
	LastActive       int64  `json:"last_active"` // 最后活跃时间戳(纪元秒)，0=范围内无消息
	IsActive         bool   `json:"is_active"`
}

// channelListItem 表二 群组列表行(仅群组，私聊不进表二)。
type channelListItem struct {
	ChannelID        string `json:"channel_id"`
	Name             string `json:"name"`
	ConvType         uint8  `json:"conv_type"`
	MemberCount      int    `json:"member_count"`
	HumanMemberCount int    `json:"human_member_count"`
	AgentMemberCount int    `json:"agent_member_count"`
	HumanMsgCount    int64  `json:"human_msg_count"`
	AgentMsgCount    int64  `json:"agent_msg_count"`
	LastActiveAt     int64  `json:"last_active_at"`
	Status           uint8  `json:"status"`
	IsActive         bool   `json:"is_active"`
}

// directChatItem 全局私聊活跃列表行(口径2：私聊走独立全局列表)。
type directChatItem struct {
	ChannelID   string `json:"channel_id"`
	MemberAUID  string `json:"member_a_uid"`
	MemberAName string `json:"member_a_name"`
	MemberBUID  string `json:"member_b_uid"`
	MemberBName string `json:"member_b_name"`
	ConvType    uint8  `json:"conv_type"`
	MsgCount    int64  `json:"msg_count"`
	LastActive  int64  `json:"last_active"`
}
