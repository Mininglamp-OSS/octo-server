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

// BotRegisterReq Bot自注册请求。
//
// **死代码 —— 无调用方，不要用它。** 活的请求体是 modules/bot_api 的
// BotRegisterReq（POST /v1/bot/register 唯一入口）。本副本已与之分叉：三个字段是
// 裸 string（活的那份是 *string，用于区分「缺席」与「报了空」）、没有 agent_hosting、
// 也没有全有或全无的解码语义。它长得像个能用的请求结构、又住在 model.go 里，
// 所以尤其容易被下一个人误取 —— 要用请直接删掉它改用 bot_api 那份。
//
// 同一形状的另一处：本包 db.go 的 updateRobotAgentInfo 也是死副本。
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
	ID            int64  `json:"id"`
	UID           string `json:"uid"`
	RobotUID      string `json:"robot_uid"`
	RobotName     string `json:"robot_name"`
	ApplicantName string `json:"applicant_name"`
	OwnerUID      string `json:"owner_uid"`
	Remark        string `json:"remark"`
	Status        int    `json:"status"`
	CreatedAt     string `json:"created_at"`
}

// RobotApplyListResp 申请列表响应
type RobotApplyListResp struct {
	List  []*RobotApplyResp `json:"list"`
	Count int64             `json:"count"`
}

// userAPIKeyModel 用户API Key模型
type userAPIKeyModel struct {
	ID           int64  `json:"id"`
	UID          string `json:"uid"`
	APIKey       string `json:"api_key"`
	APIKeyHash   string `json:"api_key_hash"`
	APIKeyCipher string `json:"api_key_cipher"`
	SpaceID      string `json:"space_id"`
	ClientID     string `json:"client_id"`
	Status       int    `json:"status"`
	CreatedAt    string `json:"created_at"`
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

// UserBotResp 用户Bot列表项。
//
// 安全：列表不批量回填 `bf_` 凭证。为不破坏既有响应契约，保留 bot_token 字段
// 但**恒为空串**——老客户端读字段不会拿到 undefined，但拿不到真实 token；需要
// 某个 Bot 的 token 时单独调 GET /v1/user/bots/:bot_id/token。
type UserBotResp struct {
	RobotID     string `json:"robot_id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// BotToken 出于安全在列表里恒为空串（不批量泄露 `bf_`）；取 token 走单端点。
	BotToken  string `json:"bot_token"`
	CreatedAt string `json:"created_at"`
	// BoundAgentRef 占用方不透明标签（如 octopush:agent_xxx）；空=空闲。
	BoundAgentRef string `json:"bound_agent_ref"`
	// BoundAt 占用时间（timeFormart）；未占用时为 null（*string，与 doc 的
	// string|null 及 unbind 的显式 null 对齐——不用 omitempty 省略字段）。
	BoundAt       *string `json:"bound_at"`
	AgentPlatform string  `json:"agent_platform,omitempty"`
	AgentVersion  string  `json:"agent_version,omitempty"`
	PluginVersion string  `json:"plugin_version,omitempty"`
	// AgentHosting Agent 自报托管形态：小写 ASCII slug，本项目自用 self_hosted /
	// octo_hosted，第三方托管方按 <vendor>_hosted 自取（取值开放、服务端只校验形状，
	// 所以客户端**不要**写死映射表，直接展示 slug —— 新托管方出现时前端无需发版）。
	// 未上报时省略，与同组的 agent_platform / agent_version / plugin_version 行为一致。
	// 自报值，仅供展示与排障，调用方不得据此做授权判定。
	//
	// **字段缺席有两种含义，靠下面的 AgentReportedHostingAt 区分**：
	//   缺席 + agent_reported_hosting_at=null  → 从未上报过 hosting
	//   缺席 + agent_reported_hosting_at 有值  → 曾上报过，后被 runtime 显式清空
	// 所以 omitempty 不会丢信息：时间戳恒定下发（无 omitempty），两者合起来可判别。
	AgentHosting string `json:"agent_hosting,omitempty"`
	// AgentReportedHostingAt 最近一次收到 **agent_hosting** 上报的时间
	// （botBoundAtFormat）；从未上报为 null。只在 hosting 被上报时前进 ——
	// 只报版本号的 register 不会刷新它，所以这个时间戳回答的确实是
	// 「同一行的 agent_hosting 有多新」，而不是「这个 bot 最近有没有上报过什么」。
	// 与 BoundAt 同口径用 *string 显式下发 null（而不是 omitempty 省略字段），
	// 且与 BoundAt 一样由 SQL NOW() 写入 —— 两者在同一响应里并列，时钟必须同源。
	AgentReportedHostingAt *string `json:"agent_reported_hosting_at"`
}

// BindBotReq 占用（绑定）Bot 请求。
type BindBotReq struct {
	// AgentRef 占用方不透明标签（如 octopush:agent_xxx）。Octo 不解析其语义。
	AgentRef string `json:"agent_ref"`
}

// BindBotResp 占用（绑定）Bot 响应。
type BindBotResp struct {
	RobotID       string  `json:"robot_id"`
	BoundAgentRef string  `json:"bound_agent_ref"`
	BoundAt       *string `json:"bound_at"`
}
