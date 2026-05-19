package botfather

// BotRegisterResp Bot自注册响应
type BotRegisterResp struct {
	RobotID        string `json:"robot_id"`
	Name           string `json:"name"`
	IMToken        string `json:"im_token"`
	WSURL          string `json:"ws_url"`
	APIURL         string `json:"api_url"`
	OwnerUID       string `json:"owner_uid"`
	OwnerChannelID string `json:"owner_channel_id"`
}

// BotRegisterReq Bot自注册请求（可选字段，兼容旧客户端空 body）
type BotRegisterReq struct {
	AgentPlatform string `json:"agent_platform"` // AI Agent 平台名称
	AgentVersion  string `json:"agent_version"`  // Agent 平台版本号
	PluginVersion string `json:"plugin_version"` // Octo 插件版本号
}

// BotSendMessageReq Bot发送消息请求
type BotSendMessageReq struct {
	ChannelID   string                 `json:"channel_id"`
	ChannelType uint8                  `json:"channel_type"`
	StreamNo    string                 `json:"stream_no"`
	Payload     map[string]interface{} `json:"payload"`
}

// BotTypingReq Bot输入状态请求
type BotTypingReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// BotEventsReq Bot获取事件请求
type BotEventsReq struct {
	EventID int64 `json:"event_id"`
	Limit   int64 `json:"limit"`
}

// BotEventAckReq Bot确认事件请求
type BotEventAckReq struct {
	EventID int64 `json:"event_id"`
}

// BotReadReceiptReq Bot阅读回执请求
type BotReadReceiptReq struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	MessageIDs  []string `json:"message_ids"`
}

// BotSyncMessagesReq Bot同步历史消息请求
type BotSyncMessagesReq struct {
	ChannelID       string `json:"channel_id"`
	ChannelType     uint8  `json:"channel_type"`
	StartMessageSeq uint32 `json:"start_message_seq"`
	EndMessageSeq   uint32 `json:"end_message_seq"`
	Limit           int    `json:"limit"`
	PullMode        int    `json:"pull_mode"`
}

// BotHeartbeatReq Bot心跳请求（REST模式）
type BotHeartbeatReq struct{}

// BotInfo BotFather中的机器人信息
type BotInfo struct {
	RobotID     string `json:"robot_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BotToken    string `json:"bot_token"`
	CreatorUID  string `json:"creator_uid"`
	Status      int    `json:"status"`
}

// RobotApplyReq 申请使用AI的请求
type RobotApplyReq struct {
	RobotUID string `json:"robot_uid"`
	Remark   string `json:"remark"`
	SpaceID  string `json:"space_id"` // Space ID（可选，客户端从 body 传递）
}

// RobotApplySureReq Owner通过申请的请求
type RobotApplySureReq struct {
	ApplyID int64 `json:"apply_id"`
}

// RobotApplyResp 申请记录响应
type RobotApplyResp struct {
	ID           int64  `json:"id"`
	UID          string `json:"uid"`
	RobotUID     string `json:"robot_uid"`
	RobotName    string `json:"robot_name"`
	ApplicantName string `json:"applicant_name"`
	OwnerUID     string `json:"owner_uid"`
	Remark       string `json:"remark"`
	Status       int    `json:"status"`
	CreatedAt    string `json:"created_at"`
}

// RobotApplyListResp 申请列表响应
type RobotApplyListResp struct {
	List  []*RobotApplyResp `json:"list"`
	Count int64             `json:"count"`
}

// userAPIKeyModel 用户API Key模型
type userAPIKeyModel struct {
	ID        int64  `json:"id"`
	UID       string `json:"uid"`
	APIKey    string `json:"api_key"`
	SpaceID   string `json:"space_id"`
	CreatedAt string `json:"created_at"`
}

// CreateBotReq 通过API Key创建Bot请求
type CreateBotReq struct {
	Name        string  `json:"name"`
	Username    string  `json:"username"`
	Description *string `json:"description"`
	SpaceID     string  `json:"space_id"` // 可选，指定 Bot 加入的 Space
}

// CreateBotResp 创建Bot响应
type CreateBotResp struct {
	RobotID     string `json:"robot_id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BotToken    string `json:"bot_token"`
	CreatedAt   string `json:"created_at"`
}

// UpdateBotReq 更新Bot请求
type UpdateBotReq struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// UserBotResp 用户Bot列表项
type UserBotResp struct {
	RobotID       string `json:"robot_id"`
	Username      string `json:"username"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	BotToken      string `json:"bot_token"`
	CreatedAt     string `json:"created_at"`
	AgentPlatform string `json:"agent_platform,omitempty"`
	AgentVersion  string `json:"agent_version,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
}
