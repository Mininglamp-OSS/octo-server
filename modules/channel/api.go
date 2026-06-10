package channel

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
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
		auth.GET("/channels/:channel_id/:channel_type", ch.channelGet)                          // 获取频道信息
		auth.POST("/channels/:channel_id/:channel_type/message/clear", ch.clearChannelMessages) // 清空频道消息
		auth.GET("/channels/:channel_id/:channel_type/storyline", ch.getStoryline)              // 获取群聊个人故事线
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

	// BotFather 的命令菜单是服务端自有文案：库里只存部署默认语言的兜底，这里按
	// 请求协商语言重渲染（#335）。其余 bot 的 commands 是创建者内容，原样透传。
	if channelType == common.ChannelTypePerson.Uint8() && channelID == cmdmenu.BotFatherUID && channelResp.Extra != nil {
		if _, ok := channelResp.Extra["bot_commands"]; ok {
			channelResp.Extra["bot_commands"] = cmdmenu.JSON(octoi18n.OutboundLanguage(c.Request.Context()))
		}
	}

	c.JSON(http.StatusOK, channelResp)

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
