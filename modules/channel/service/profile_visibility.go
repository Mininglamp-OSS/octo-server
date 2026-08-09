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
	// Followed：已关注。user.GetUserDetail 已把「好友」与「同 Space 可直接聊天」
	// 两种可达关系合并进 Follow，故这里一个布尔即覆盖两者。
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

// MinimalChannelResp 是 channelGet 在无可见关系时的最小响应契约：只序列化频道标识 /
// 名称 / 头像 / 是否机器人，供客户端渲染历史消息发送者。
//
// 不复用 model.ChannelResp——它绝大多数字段无 omitempty，即便只赋值四项，序列化仍会
// 带出 follow:0 / status:0 / notice:"" / extra:null，其中 follow:0 会被客户端读成
// 「明确非好友」。用独立 DTO 从字节层面保证「仅这四项」。
//
// 刻意省略 follow：本端点用于渲染任意发送者，不是关系页，客户端不应据此判断关系。
// （/v1/users/:uid 的最小集相反**保留** follow —— 那是资料页，要靠它渲染加好友入口。）
type MinimalChannelResp struct {
	Channel struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	} `json:"channel"`
	Name  string `json:"name"`
	Logo  string `json:"logo"`
	Robot int    `json:"robot"`
}

// NewMinimalChannelResp 从完整详情里挑出白名单四项。白名单式构造——将来给
// model.ChannelResp 新增字段时默认不泄露。
func NewMinimalChannelResp(full *model.ChannelResp) MinimalChannelResp {
	m := MinimalChannelResp{
		Name:  full.Name,
		Logo:  full.Logo,
		Robot: full.Robot,
	}
	m.Channel.ChannelID = full.Channel.ChannelID
	m.Channel.ChannelType = full.Channel.ChannelType
	return m
}
