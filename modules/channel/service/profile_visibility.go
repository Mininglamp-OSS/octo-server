package service

import "github.com/Mininglamp-OSS/octo-lib/model"

// profile_visibility.go —— 单聊/用户资料的**对象级可见性判定**，两个端点共用：
//
//	GET /v1/channels/:channel_id/:channel_type  （channel_type=PERSON）
//	GET /v1/users/:uid
//
// 两者历史上都只有登录鉴权、没有对象级关系校验，且共用 user.GetUserDetail 作为数据源。
// 判定收口在本包而不是各自的 handler，是为了让两端口径不可能漂移——一边放宽另一边就
// 静默重开同一个越权面。
//
// 本包是**零依赖叶子包**（modules/user 反向引用它，见 modules/user/api_friend.go），
// 因此这里不 import modules/user / modules/incomingwebhook：合成身份前缀（iwh_）与
// 系统/客服分类常量归那些上层模块所有，由调用方判定后以布尔传入。

// CommonGroupChecker 判定两个用户是否至少同属一个未解散的群。
// 由 group 模块提供实现（channel 直接注入 groupService，user 走注册钩子）。
type CommonGroupChecker func(uidA string, uidB string) (bool, error)

// PersonProfileInput 描述判定所需的调用方与目标属性。
type PersonProfileInput struct {
	LoginUID string
	PeerUID  string
	// SyntheticIdentity：目标是展示专用合成身份（如 incoming webhook 的 iwh_ 前缀）。
	// 这类身份没有隐私字段，保持完整以免展示层裂图。
	SyntheticIdentity bool
	// SystemAccount：系统 / 客服账号。
	SystemAccount bool
	// Robot：目标是 bot。资料公开可查——用户要先看到 bot 才能决定是否添加；
	// "查看资料" != "已可交互"，未加好友仍不能与 bot 对话。
	Robot bool
	// Followed：调用方与目标存在可达关系（好友，或同属一个**正常状态**的 Space）。
	//
	// 必须由调用方用**授权口径**计算（user.IService.HasAuthzRelation），
	// **不要**直接传 UserDetailResp.Follow / ChannelResp.Follow：那是展示字段，
	// 其"同 Space"来源不校验 Space 活性（封禁 Space 只翻父行、成员行仍在），
	// 用它做授权会让封禁冻结失效。
	Followed bool
}

// PersonProfileVisible 判定调用方是否可见目标的完整资料。可见关系：
// 本人 / 合成身份 / bot / 系统账号 / 已关注（好友或同 Space） / 共同群。
//
// 共同群是最后一道判定，因为它是唯一需要查库的一项——外部群里跨 Space 的非好友成员
// 既非好友也无共同 Space，仅靠共同群可达；不放行会让群内成员名/头像裂图。
//
// hasCommonGroup 为 nil 时 fail closed（返回 false），调用方降级为最小集：宁可少给
// 字段，也不能因为依赖未注入就放开完整资料。
func PersonProfileVisible(in PersonProfileInput, hasCommonGroup CommonGroupChecker) (bool, error) {
	if in.PeerUID == "" {
		return false, nil
	}
	if in.LoginUID != "" && in.LoginUID == in.PeerUID {
		return true, nil
	}
	if in.SyntheticIdentity || in.Robot || in.SystemAccount || in.Followed {
		return true, nil
	}
	if hasCommonGroup == nil || in.LoginUID == "" {
		return false, nil
	}
	return hasCommonGroup(in.LoginUID, in.PeerUID)
}

// MinimalChannelResp 是 channelGet 在无可见关系时的最小响应契约。
//
// 取舍规则（本次的核心政策，逐字段判断而非一刀切白名单）：
//
//	下发所有「调用方自己的状态」，只省略「对方的身份」。
//
// 理由是客户端**整行写回缓存**且不做字段存在性检查：三端都用一个新分配的对象接收本
// 响应，缺失键取零值，再无条件覆盖 SDK 缓存里正确的值。所以省略一个"调用方自己的
// 设置"不会买到任何隐私，只会把用户自己开启的功能悄悄关掉——已实测的后果包括：
//   - 缺 status → 零值 0 撞上"已禁用/封禁"哨兵，Android 隐藏输入框、显示封禁视图；
//   - 缺 extra.chat_pwd_on → 聊天密码锁失效，加锁会话直接打开且不提示、列表预览
//     不再打码（Android 内存态、iOS 落盘）；
//   - 缺 extra.msg_auto_delete → iOS 新发消息不再按期自动删除（该 key 全端只由本
//     接口注入）；
//   - 缺 mute/stick/save/remark → 免打扰、置顶、备注被重置（iOS 历史上已为 mute
//     单独硬编码过兜底，说明这是已知且已复发过的 bug class）。
//
// 真正买到隐私的是对方身份/在线态的省略：username、short_no、sex、category、
// source_desc、vercode、online、last_offline、device_flag、实名字段、bot_* 等。
// follow 与 be_deleted / be_blacklist 也不下发：前者是关系判决（发送者渲染不需要，
// 给 0 会被读成"明确非好友"），后两者是**对方**对调用方的动作，属对方信息。
type MinimalChannelResp struct {
	Channel struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	} `json:"channel"`
	ParentChannel *struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	} `json:"parent_channel,omitempty"`

	// 渲染必需（对方的可公开展示信息）
	Name  string `json:"name"`
	Logo  string `json:"logo"`
	Robot int    `json:"robot"`

	// 以下全部是**调用方自己**的状态，必须原样透传
	Status      int `json:"status"`       // 调用方是否拉黑对方（1/2，永不为 0）
	Receipt     int `json:"receipt"`      // 消息回执设置
	Stick       int `json:"stick"`        // 置顶
	Mute        int `json:"mute"`         // 免打扰
	ShowNick    int `json:"show_nick"`    // 显示昵称
	Flame       int `json:"flame"`        // 阅后即焚
	FlameSecond int `json:"flame_second"` // 阅后即焚秒数

	Remark string `json:"remark"` // 调用方给对方设置的备注

	// Extra 只保留调用方自己的会话设置键，不含对方身份（实名、bot_* 等一律不进）。
	Extra map[string]interface{} `json:"extra"`
}

// callerOwnedExtraKeys 是 Extra 中属于**调用方自己**的键；其余（realname_verified /
// real_name / short_no / sex / source_desc / vercode / bot_* 等）是对方身份，不下发。
var callerOwnedExtraKeys = []string{
	"chat_pwd_on",     // 聊天密码锁——缺失会让锁静默失效
	"screenshot",      // 截屏通知
	"revoke_remind",   // 撤回提醒
	"msg_auto_delete", // 消息定时删除——缺失会让 iOS 新消息不再自动删除
}

// NewMinimalChannelResp 按「调用方自身状态全留、对方身份全剥」构造最小集。
//
// 必须在 channelSettingDB 富化**之后**调用：msg_auto_delete 与 parent_channel 是那一
// 步才附到 full 上的，提前返回会让它们永远缺失。
func NewMinimalChannelResp(full *model.ChannelResp) MinimalChannelResp {
	m := MinimalChannelResp{
		ParentChannel: full.ParentChannel,
		Name:          full.Name,
		Logo:          full.Logo,
		Robot:         full.Robot,
		Status:        full.Status,
		Receipt:       full.Receipt,
		Stick:         full.Stick,
		Mute:          full.Mute,
		ShowNick:      full.ShowNick,
		Flame:         full.Flame,
		FlameSecond:   full.FlameSecond,
		Remark:        full.Remark,
		Extra:         map[string]interface{}{},
	}
	m.Channel.ChannelID = full.Channel.ChannelID
	m.Channel.ChannelType = full.Channel.ChannelType
	for _, k := range callerOwnedExtraKeys {
		if v, ok := full.Extra[k]; ok {
			m.Extra[k] = v
		}
	}
	return m
}
