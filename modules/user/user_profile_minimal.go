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
	// Follow 0=未关注（陌生人）。走到最小集意味着授权判定已认定无可达关系，故恒为 0，
	// **不能**从 full.Follow 复制（那是展示字段，其同 Space 来源不校验 Space 活性）。
	// 资料页据此渲染加好友入口。
	Follow int `json:"follow"`
	// Robot 供客户端区分 bot 与真人的渲染分支。
	Robot int `json:"robot"`

	// ── 以下全部是**调用方自己**的状态，必须原样透传 ──
	// 规则见 chservice.MinimalChannelResp 的文档：客户端整行写回缓存且不做字段存在性
	// 检查，省略"调用方自己的设置"买不到任何隐私，只会把用户自己开启的功能悄悄关掉。
	Status       int    `json:"status"`        // 调用方是否拉黑对方（1/2，永不为 0）
	Mute         int    `json:"mute"`          // 免打扰
	Top          int    `json:"top"`           // 置顶
	ChatPwdOn    int    `json:"chat_pwd_on"`   // 聊天密码锁——缺失会让锁静默失效
	Screenshot   int    `json:"screenshot"`    // 截屏通知
	RevokeRemind int    `json:"revoke_remind"` // 撤回提醒
	Receipt      int    `json:"receipt"`       // 消息回执
	Flame        int    `json:"flame"`         // 阅后即焚
	FlameSecond  int    `json:"flame_second"`  // 阅后即焚秒数
	Remark       string `json:"remark"`        // 调用方给对方设置的备注
}

// newMinimalUserDetailResp 按「调用方自身状态全留、对方身份全剥」构造最小资料集。
//
// 剥掉的是对方身份/在线态：username / short_no / sex / category / source_desc /
// vercode / online / last_offline / device_flag / 实名字段 / bot_* / be_deleted /
// be_blacklist（后两者是**对方**对调用方的动作，属对方信息）。
// 手机号 / 邮箱 / 区号本就只对本人下发（见 NewUserDetailResp 的 self 判定）。
//
// 白名单而非黑名单——将来给 UserDetailResp 新增字段时默认不泄露；新增的若属"调用方
// 自己的状态"，须显式加进来，否则客户端写回时会把它清零。
func newMinimalUserDetailResp(full *UserDetailResp) minimalUserDetailResp {
	return minimalUserDetailResp{
		UID:          full.UID,
		Name:         full.Name,
		Follow:       0,
		Robot:        full.Robot,
		Status:       full.Status,
		Mute:         full.Mute,
		Top:          full.Top,
		ChatPwdOn:    full.ChatPwdOn,
		Screenshot:   full.Screenshot,
		RevokeRemind: full.RevokeRemind,
		Receipt:      full.Receipt,
		Flame:        full.Flame,
		FlameSecond:  full.FlameSecond,
		Remark:       full.Remark,
	}
}
