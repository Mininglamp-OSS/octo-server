package user

// user_profile_minimal.go —— `GET /v1/users/:uid` 在**无可见关系**时的最小资料契约。
//
// 该端点历史上只有登录鉴权：任意登录用户拿任意 UID 就能读到完整身份（短号、性别、
// 在线状态、设备指纹、来源描述、实名等），既是任意资料读取也是用户存在性枚举面。
// 可见关系判定与 `/v1/channels/:id/:type` 共用 modules/channel/service。
//
// 与 channelGet 最小集的**刻意差异**：这里保留 `follow`。
//   - channelGet 用于渲染任意消息发送者，不是关系页，给 follow:0 会被读成"明确非好友"；
//   - 本端点是资料页，客户端要靠 follow==0 渲染「加好友」入口，省略它会让陌生人
//     加好友这个正常入口消失。
//
// 加好友流程不依赖本响应的其它字段：vercode 由 search / 扫码路径铸造并校验
// （见 modules/user/api_friend.go 的 source.CheckRequestAddFriendCode），且本端点对
// 非好友本来就返回空 vercode。
type minimalUserDetailResp struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	// Follow 0=未关注（陌生人）1=已关注。见文件头：资料页据此渲染加好友入口。
	Follow int `json:"follow"`
	// Robot 供客户端区分 bot 与真人的渲染分支。注意 bot 恒可见完整资料，故走到最小集
	// 时它必为 0；保留该字段只为让契约形状稳定，客户端无需按分支解析。
	Robot int `json:"robot"`
}

// newMinimalUserDetailResp 白名单式构造最小资料集：剥离 short_no / sex / online /
// last_offline / device_flag / source_desc / vercode / remark / 实名 等全部身份与关系
// 细节。手机号 / 邮箱 / 区号本就只对本人下发（见 NewUserDetailResp 的 self 判定），
// 不在此重复。
//
// 白名单而非黑名单——将来给 UserDetailResp 新增字段时默认不泄露。
func newMinimalUserDetailResp(full *UserDetailResp) minimalUserDetailResp {
	return minimalUserDetailResp{
		UID:  full.UID,
		Name: full.Name,
		// Follow 恒为 0，**不能**从 full.Follow 复制。走到最小集意味着授权判定已认定
		// 无可达关系（非好友、无**活跃**共同 Space、无共同有效群）；而 full.Follow 是
		// 展示字段，它的同 Space 来源（GetCommonSpaceID）不校验 Space 活性，封禁 / 解散
		// Space 会让它仍为 1。照抄会让响应自相矛盾——一边剥掉身份字段说"无关系"，一边
		// 告诉客户端"已关注"，并且恰好泄露了本要隐去的那条"你们同属某个（已封禁）
		// Space"的事实。这正是本次要消除的展示/授权混淆。
		Follow: 0,
		Robot:  full.Robot,
	}
}
