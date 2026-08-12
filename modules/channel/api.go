package channel

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	chservice "github.com/Mininglamp-OSS/octo-server/modules/channel/service"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

type Channel struct {
	ctx *config.Context
	log.Log
	userService      user.IService
	groupService     group.IService
	channelSettingDB *channelSettingDB
}

func New(ctx *config.Context) *Channel {
	return &Channel{
		ctx:              ctx,
		Log:              log.NewTLog("Channel"),
		userService:      user.NewService(ctx),
		groupService:     group.NewService(ctx),
		channelSettingDB: newChannelSettingDB(ctx),
	}
}

// Route 路由配置
func (ch *Channel) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1", ch.ctx.AuthMiddleware(r))
	{
		auth.GET("/channel/state", ch.state)
		// channelGet 是跨用户读频道详情的入口，单独挂 SharedUIDRateLimiter（须在
		// AuthMiddleware 之后才能读到 uid），封顶批量枚举频道/用户的速率。
		auth.GET("/channels/:channel_id/:channel_type", appwkhttp.SharedUIDRateLimiter(r, ch.ctx), ch.channelGet) // 获取频道信息
		auth.POST("/channels/:channel_id/:channel_type/message/clear", ch.clearChannelMessages)                   // 清空频道消息
		auth.GET("/channels/:channel_id/:channel_type/storyline", ch.getStoryline)                                // 获取群聊个人故事线
	}

	// Routes that build PERSONAL MsgSendReq need SpaceMiddleware so the
	// PERSONAL branch can read a SpaceMiddleware-validated space_id from the
	// gin context (spacepkg.GetSpaceID); without this, payload.space_id would
	// be fail-closed stripped by the NewPersonalMsgSendReq builder.
	spaceAuth := r.Group("/v1", ch.ctx.AuthMiddleware(r), spacepkg.SpaceMiddleware(ch.ctx))
	{
		spaceAuth.POST("/channels/:channel_id/:channel_type/message/autodelete", ch.setAutoDeleteForMessage) // 设置消息定时删除时间
	}
}

func (ch *Channel) clearChannelMessages(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	channelID := c.Param("channel_id")
	channelTypeI64 := util.ParseInt64OrDefault(c.Param("channel_type"), 0)
	channelType := uint8(channelTypeI64)
	if channelID == "" {
		c.ResponseError(errors.New("频道Id不能为空"))
		return
	}
	modules := register.GetModules(ch.ctx)
	var err error
	var channelResp *model.ChannelResp
	for _, m := range modules {
		if m.BussDataSource.ChannelGet != nil {
			channelResp, err = m.BussDataSource.ChannelGet(channelID, channelType, loginUID)
			if err != nil {
				if errors.Is(err, register.ErrDatasourceNotProcess) {
					continue
				}
				ch.Error("查询频道失败！", zap.Error(err))
				c.ResponseError(err)
				return
			}
			break
		}
	}
	if channelResp == nil {
		ch.Error("频道不存在！", zap.String("channel_id", channelID), zap.Uint8("channelType", channelType))
		c.ResponseError(errors.New("频道不存在！"))
		return
	}
	fakeChannelID := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		// 验证当前用户是私聊的参与者
		if loginUID == channelID {
			c.ResponseError(errors.New("频道ID不合法"))
			return
		}
		isFriend, err := ch.userService.IsFriend(loginUID, channelID)
		if err != nil {
			ch.Error("查询好友关系错误", zap.Error(err))
			c.ResponseError(errors.New("查询好友关系错误"))
			return
		}
		if !isFriend {
			c.ResponseError(errors.New("没有权限操作此频道"))
			return
		}
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, channelID)
	} else {
		isCreatorOrManager, err := ch.groupService.IsCreatorOrManager(channelID, loginUID)
		if err != nil {
			c.ResponseError(errors.New("查询群的创建者或管理员错误"))
			ch.Error("查询群的创建者或管理员错误", zap.Error(err))
			return
		}
		if !isCreatorOrManager {
			c.ResponseError(errors.New("没有权限设置"))
			return
		}
	}
	channelMaxSeqResp, err := ch.ctx.IMGetChannelMaxSeq(channelID, channelType)
	if err != nil {
		ch.Error("查询频道最大序列号失败！", zap.Error(err))
		c.ResponseError(errors.New("查询频道最大序列号失败！"))
		return
	}
	var maxSeq uint32 = 0
	if channelMaxSeqResp != nil {
		maxSeq = channelMaxSeqResp.MessageSeq
	}
	if err := ch.channelSettingDB.insertOrAddOffsetMessageSeq(fakeChannelID, channelType, maxSeq); err != nil {
		ch.Error("设置频道最大偏移序列号失败", zap.Error(err))
		c.ResponseError(errors.New("设置频道最大偏移序列号失败"))
		return
	}
	err = ch.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   false,
		ChannelID:   channelID,
		ChannelType: channelType,
		FromUID:     loginUID,
		CMD:         common.CMDMessageErase,
		Param: map[string]interface{}{
			"erase_type":   "all", // "all" | "from"
			"channel_id":   channelID,
			"channel_type": channelType,
			"from_uid":     loginUID,
		},
	})
	if err != nil {
		ch.Error("发送清空频道聊天记录命令失败！", zap.String("channel_id", channelID), zap.Error(err))
		c.ResponseError(errors.New("发送清空频道聊天记录命令失败！"))
		return
	}
	c.ResponseOK()
}

func (ch *Channel) channelGet(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	channelID := c.Param("channel_id")
	channelTypeI64 := util.ParseInt64OrDefault(c.Param("channel_type"), 0)
	channelType := uint8(channelTypeI64)

	// 如果是单聊且 channel_id 带 Space 前缀，提取真实 UID
	if channelType == common.ChannelTypePerson.Uint8() {
		_, peerID := spacepkg.ParseChannelID(channelID)
		if peerID != "" {
			channelID = peerID
		}
	}

	// 对象级授权（GROUP 前置）：非成员——含群不存在（ExistMember 对不存在群同样返回
	// false）——统一 forbidden。既堵越权（历史上任意登录用户替换 channel_id 即可读
	// 群名/公告/成员数/space_id），也不泄露群是否存在，口径与 GET /v1/groups/:group_no
	// （groupGet）一致。
	if channelType == common.ChannelTypeGroup.Uint8() {
		isMember, err := ch.groupService.ExistMember(channelID, loginUID)
		if err != nil {
			ch.Error("查询群成员关系失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
			return
		}
	}

	modules := register.GetModules(ch.ctx)
	var err error
	var channelResp *model.ChannelResp
	for _, m := range modules {
		if m.BussDataSource.ChannelGet != nil {
			channelResp, err = m.BussDataSource.ChannelGet(channelID, channelType, loginUID)
			if err != nil {
				if errors.Is(err, register.ErrDatasourceNotProcess) {
					continue
				}
				ch.Error("查询频道失败！", zap.Error(err))
				c.ResponseError(err)
				return
			}
			break
		}
	}
	if channelResp == nil {
		// 目标不存在：按类型分流，避免存在性枚举 oracle。
		//   - COMMUNITY_TOPIC：与"非父群成员"统一 forbidden，不泄露子区是否存在。
		//   - PERSON：统一 not_found（"无关系但存在"的用户仍需返回名/头像供历史发送者
		//     渲染，故无法与 not_found 合并，见 task brief）。
		//   - 其它类型：保持原有"频道不存在"。
		switch channelType {
		case common.ChannelTypeCommunityTopic.Uint8():
			httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
		case common.ChannelTypePerson.Uint8():
			httperr.ResponseErrorL(c, errcode.ErrUserNotFound, nil, nil)
		default:
			ch.Error("频道不存在！", zap.String("channel_id", channelID), zap.Uint8("channelType", channelType))
			c.ResponseError(errors.New("频道不存在！"))
		}
		return
	}

	// 对象级授权（COMMUNITY_TOPIC 后置）：子区详情继承父群 ACL。父群号从展示数据
	// 的 extra 取（channel 模块不直接依赖 thread 的 channel_id 解析），用活跃成员
	// 校验（排除黑名单，语义同 ExistMemberActive 关于 CommunityTopic 的说明）。
	if channelType == common.ChannelTypeCommunityTopic.Uint8() {
		parentGroupNo, _ := channelResp.Extra["group_no"].(string)
		if parentGroupNo == "" {
			httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
			return
		}
		isMember, err := ch.groupService.ExistMemberActive(parentGroupNo, loginUID)
		if err != nil {
			ch.Error("查询子区父群成员关系失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
			return
		}
	}

	// 对象级授权（PERSON 分级降级）：无可见关系时只返回渲染历史发送者所需的最小集，
	// 不下发短号/在线状态/设备指纹/实名等身份细节。不做二元 403——渲染任意消息发送者
	// 名/头像是本端点的既有职责（YUJ-411），硬拒会让外部群跨 Space 成员裂图。
	// 只在这里**判定**，降级响应推迟到 channelSettingDB 富化之后再构造：
	// msg_auto_delete / parent_channel 是那一步才附到 channelResp 上的，而它们属于
	// 调用方自己的会话设置，必须进最小集（缺失会让 iOS 新消息不再自动删除）。
	personMinimal := false
	if channelType == common.ChannelTypePerson.Uint8() {
		visible, err := ch.personProfileVisible(loginUID, channelID, channelResp)
		if err != nil {
			// 用 user 域错误码：这是单聊（人）资料读取失败，客户端不该看到群错误。
			ch.Error("查询单聊资料可见关系失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrUserQueryFailed, nil, nil)
			return
		}
		personMinimal = !visible
	}

	fakeChannelID := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, channelID)
	}

	channelSettingM, err := ch.channelSettingDB.queryWithChannel(fakeChannelID, channelType) // TODO: 这里虽然暂时看着没啥用，后面可以统一频道的设置信息
	if err != nil {
		ch.Error("查询频道设置失败！", zap.Error(err))
		c.ResponseError(errors.New("查询频道设置失败！"))
		return
	}
	if channelSettingM != nil {
		if channelSettingM.ParentChannelID != "" {
			channelResp.ParentChannel = &struct {
				ChannelID   string `json:"channel_id"`
				ChannelType uint8  `json:"channel_type"`
			}{
				ChannelID:   channelSettingM.ParentChannelID,
				ChannelType: channelSettingM.ParentChannelType,
			}
		}
		if channelSettingM.MsgAutoDelete > 0 {
			channelResp.Extra["msg_auto_delete"] = channelSettingM.MsgAutoDelete
		}
	}

	// 无可见关系：剥掉对方身份、保留调用方自身会话设置后返回（见
	// chservice.NewMinimalChannelResp 的取舍规则）。放在设置富化之后、BotFather 文案
	// 重渲染之前——后者只对 bot 生效，而 bot 恒可见，走不到这里。
	if personMinimal {
		c.JSON(http.StatusOK, chservice.NewMinimalChannelResp(channelResp))
		return
	}

	// BotFather 的命令菜单是服务端自有文案：库里只存部署默认语言的兜底，这里按
	// 请求协商语言重渲染（#335）。其余 bot 的 commands 是创建者内容，原样透传。
	if channelType == common.ChannelTypePerson.Uint8() && channelID == cmdmenu.BotFatherUID && channelResp.Extra != nil {
		if _, ok := channelResp.Extra["bot_commands"]; ok {
			channelResp.Extra["bot_commands"] = cmdmenu.JSON(octoi18n.OutboundLanguage(c.Request.Context()))
		}
	}

	c.JSON(http.StatusOK, channelResp)

}

// personProfileVisible 判定 loginUID 是否可见 peerID 的完整单聊资料。判定本身收口在
// modules/channel/service（与 /v1/users/:uid 共用，避免两端口径漂移）；这里只负责把
// 本模块能看到的目标属性翻译成该函数的输入。
func (ch *Channel) personProfileVisible(loginUID string, peerID string, resp *model.ChannelResp) (bool, error) {
	// 身份类放行（本人 / webhook / bot / 系统 Bot）不需要查关系，先判定；只有都不命中
	// 才付出 HasAuthzRelation 的查询代价。系统 Bot 走 pkg/space.SystemBots 白名单，不看
	// category 字段——category=system 的非白名单账号必须回落到关系检查，避免整份身份泄露。
	fastPath := chservice.PersonProfileInput{
		LoginUID:          loginUID,
		PeerUID:           peerID,
		SyntheticIdentity: strings.HasPrefix(peerID, user.WebhookUIDPrefix),
		SystemBot:         spacepkg.IsSystemBot(peerID),
		Robot:             resp.Robot == 1,
	}
	if visible, err := chservice.PersonProfileVisible(fastPath, nil); err != nil || visible {
		return visible, err
	}
	// 关系腿走授权口径：不能用 resp.Follow（展示字段，同 Space 来源不校验 Space 活性）。
	related, err := ch.userService.HasAuthzRelation(loginUID, peerID)
	if err != nil {
		return false, err
	}
	in := fastPath
	in.Followed = related
	return chservice.PersonProfileVisible(in, ch.groupService.ExistCommonGroup)
}

func (ch *Channel) state(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	channelID := c.Query("channel_id")
	channelTypeI64 := util.ParseInt64OrDefault(c.Query("channel_type"), 0)

	channelType := uint8(channelTypeI64)

	var signalOn uint8 = 0
	var onlineCount int = 0
	if channelType != common.ChannelTypePerson.Uint8() {

		// 验证当前用户是否是群组成员
		isMember, err := ch.groupService.ExistMember(channelID, loginUID)
		if err != nil {
			c.ResponseError(errors.New("查询群成员信息错误"))
			ch.Error("查询群成员信息错误", zap.Error(err))
			return
		}
		if !isMember {
			c.ResponseError(errors.New("非群成员无法查询群状态"))
			return
		}

		members, err := ch.groupService.GetMembers(channelID)
		if err != nil {
			c.ResponseError(errors.New("查询群成员错误"))
			ch.Error("查询群成员错误", zap.Error(err))
			return
		}
		uids := make([]string, 0)
		if len(members) > 0 {
			for _, member := range members {
				uids = append(uids, member.UID)
			}
		}
		onlineUsers, err := ch.userService.GetUserOnlineStatus(uids)
		if err != nil {
			c.ResponseError(errors.New("查询群成员在线数量错误"))
			ch.Error("查询群成员在线数量错误", zap.Error(err))
			return
		}
		if len(onlineUsers) > 0 {
			for _, user := range onlineUsers {
				if user.Online == 1 {
					onlineCount += 1
				}
			}
		}
	}
	// 查询该频道是否通话中
	callChannelIDs := make([]string, 0)
	fakeChannelId := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		fakeChannelId = common.GetFakeChannelIDWith(loginUID, channelID)
	}
	callChannelIDs = append(callChannelIDs, fakeChannelId)
	var callingChannels []*model.CallingChannelResp
	modules := register.GetModules(ch.ctx)
	for _, m := range modules {
		if m.BussDataSource.GetCallingChannel != nil {
			callingChannels, _ = m.BussDataSource.GetCallingChannel(loginUID, callChannelIDs)
			break
		}
	}
	var callingParticipantResp []*CallingParticipantResp
	roomName := ""
	if len(callingChannels) > 0 && callingChannels[0] != nil && len(callingChannels[0].Participants) > 0 {
		roomName = callingChannels[0].RoomName
		for _, p := range callingChannels[0].Participants {
			callingParticipantResp = append(callingParticipantResp, &CallingParticipantResp{
				UID:  p.UID,
				Name: p.Name,
			})
		}
	}
	c.Response(stateResp{
		SignalOn:    signalOn,
		OnlineCount: onlineCount,
		CallInfo: &rtcResp{
			RoomName:            roomName,
			CallingParticipants: callingParticipantResp,
		},
	})

}

func (ch *Channel) setAutoDeleteForMessage(c *wkhttp.Context) {
	channelID := c.Param("channel_id")
	channelTypeI64 := util.ParseInt64OrDefault(c.Param("channel_type"), 0)
	channelType := uint8(channelTypeI64)

	var req struct {
		MsgAutoDelete int64 `json:"msg_auto_delete"` // 单位秒
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.ResponseError(errors.New("参数错误"))
		ch.Error("参数错误", zap.Error(err))
		return
	}

	loginUID := c.GetLoginUID()
	fakeChannelID := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, channelID)
	} else {
		isCreatorOrManager, err := ch.groupService.IsCreatorOrManager(channelID, loginUID)
		if err != nil {
			c.ResponseError(errors.New("查询群的创建者或管理员错误"))
			ch.Error("查询群的创建者或管理员错误", zap.Error(err))
			return
		}
		if !isCreatorOrManager {
			c.ResponseError(errors.New("没有权限设置"))
			ch.Error("没有权限设置")
			return
		}
	}

	if err := ch.channelSettingDB.insertOrAddMsgAutoDelete(fakeChannelID, channelType, req.MsgAutoDelete); err != nil {
		c.ResponseError(errors.New("设置失败"))
		ch.Error("设置失败", zap.Error(err))
		return
	}
	if req.MsgAutoDelete > 0 {
		// YUJ-674 / Mininglamp-OSS#37: PERSONAL 走 NewPersonalMsgSendReq builder
		// (sender SpaceID 取自 SpaceMiddleware-validated context)。
		// GROUP / COMMUNITY_TOPIC / 其它 channel_type 保留旧路径 — payload.space_id
		// 的服务端权威注入依赖上游 enrichPayloadWithSpaceID（不在本 issue 范围）。
		autoDeletePayloadMap := map[string]interface{}{
			"content": fmt.Sprintf("{0}设置消息在 %s 后自动删除", formatSecondToDisplayTime(req.MsgAutoDelete)),
			"type":    common.Tip,
			"data": map[string]interface{}{
				"msg_auto_delete": req.MsgAutoDelete,
				"data_type":       "autoDeleteForMessage",
			},
			"extra": []config.UserBaseVo{
				{
					UID:  loginUID,
					Name: c.GetLoginName(),
				},
			},
		}
		var err error
		if channelType == common.ChannelTypePerson.Uint8() {
			err = ch.ctx.SendMessage(config.NewPersonalMsgSendReq(
				channelID,
				loginUID,
				autoDeletePayloadMap,
				spacepkg.GetSpaceID(c),
				config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
			))
		} else {
			err = ch.ctx.SendMessage(&config.MsgSendReq{
				FromUID:     loginUID,
				ChannelID:   channelID,
				ChannelType: channelType,
				Payload:     []byte(util.ToJson(autoDeletePayloadMap)),
				Header: config.MsgHeader{
					RedDot: 1,
				},
			})
		}
		if err != nil {
			ch.Error("发送消息失败！", zap.Error(err))
			c.ResponseError(errors.New("发送消息失败！"))
			return
		}
	}
	channelReq := config.ChannelReq{
		ChannelID:   channelID,
		ChannelType: channelType,
	}
	err := ch.ctx.SendChannelUpdateWithFromUID(channelReq, channelReq, loginUID)
	if err != nil {
		ch.Warn("发送频道更新命令失败！", zap.Error(err))
	}
	c.ResponseOK()
}

func formatSecondToDisplayTime(second int64) string {
	if second < 60 {
		return fmt.Sprintf("%d秒", second)
	}
	if second < 60*60 {
		return fmt.Sprintf("%d分钟", second/60)
	}
	if second < 60*60*24 {
		return fmt.Sprintf("%d小时", second/60/60)
	}
	if second < 60*60*24*30 {
		return fmt.Sprintf("%d天", second/60/60/24)
	}
	if second < 60*60*24*30*12 {
		return fmt.Sprintf("%d月", second/60/60/24/30)
	}
	return fmt.Sprintf("%d年", second/60/60/24/30/12)
}

type stateResp struct {
	SignalOn    uint8    `json:"signal_on"`    // 是否可以signal加密聊天
	OnlineCount int      `json:"online_count"` // 成员在线数量
	CallInfo    *rtcResp `json:"call_info"`    // 通话信息
}
type rtcResp struct {
	RoomName            string                    `json:"room_name"`
	CallingParticipants []*CallingParticipantResp `json:"calling_participants"` // 通话中的成员
}
type CallingParticipantResp struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}
