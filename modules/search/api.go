package search

import (
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

type Search struct {
	ctx *config.Context
	log.Log
	userService    user.IService
	groupService   group.IService
	messageService message.IService
}

func New(ctx *config.Context) *Search {
	s := &Search{
		ctx:            ctx,
		Log:            log.NewTLog("search"),
		userService:    user.NewService(ctx),
		groupService:   group.NewService(ctx),
		messageService: message.NewService(ctx),
	}
	return s
}

func (s *Search) Route(r *wkhttp.WKHttp) {
	searchs := r.Group("/v1/search", s.ctx.AuthMiddleware(r))
	{
		searchs.POST("/global", s.global) // 全局搜索
	}
}

func (s *Search) global(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		OnlyMessage int    `json:"only_message"` // 只加载消息
		ContentType []int  `json:"content_type"` // 消息类型
		Keyword     string `json:"keyword"`      // 搜索关键字
		FromUID     string `json:"from_uid"`     // 发送者uid
		ChannelID   string `json:"channel_id"`   // 频道ID
		ChannelType uint8  `json:"channel_type"` // 频道类型
		Topic       string `json:"topic"`        // 根据topic搜索
		Limit       int    `json:"limit"`        // 查询限制数量
		Page        int    `json:"page"`         // 页码，分页使用，默认为1
		StartTime   int64  `json:"start_time"`   //  消息时间（开始）
		EndTime     int64  `json:"end_time"`     // 消息时间（结束，结果不包含end_time）
	}
	if err := c.BindJSON(&req); err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if req.Limit > 100 {
		req.Limit = 100
	}
	payload := map[string]interface{}{
		"content": req.Keyword,
		"name":    req.Keyword,
	}
	highlights := []string{"payload.content", "payload.name"}

	// 查询消息
	msgResp, err := s.ctx.IMSearchUserMessages(&config.SearchUserMessageReq{
		UID:          loginUID,
		Payload:      payload,
		PayloadTypes: req.ContentType,
		Limit:        req.Limit,
		Page:         req.Page,
		FromUID:      req.FromUID,
		ChannelID:    req.ChannelID,
		ChannelType:  req.ChannelType,
		Topic:        req.Topic,
		StartTime:    req.StartTime,
		EndTime:      req.EndTime,
		Highlights:   highlights,
	})
	if err != nil {
		s.Warn("查询悟空IM消息错误（不影响好友和群搜索）", zap.Error(err))
		msgResp = nil
	}
	channelIds := make([]string, 0)
	messageIds := make([]string, 0)
	if msgResp != nil && len(msgResp.Messages) > 0 {
		for _, m := range msgResp.Messages {
			messageIds = append(messageIds, m.MessageIDStr)
			channelIds = append(channelIds, m.ChannelID)
		}
	}
	// 查询撤回标记
	revokedMsgExtras, err := s.messageService.GetRevokedMessages(messageIds)
	if err != nil {
		s.Error("查询消息撤回消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息撤回消息错误"))
		return
	}
	// 查询后台管理删除标记
	deletedMsgExtras, err := s.messageService.GetDeletedMessages(messageIds)
	if err != nil {
		s.Error("查询消息删除消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息删除消息错误"))
		return
	}
	// 查询登录用户的删除标记
	deletedMsgUserExtras, err := s.messageService.GetDeletedMessagesWithUID(loginUID, messageIds)
	if err != nil {
		s.Error("查询消息删除消息错误", zap.Error(err))
		c.ResponseError(errors.New("查询消息删除消息错误"))
		return
	}

	// 查询登录用户清空channel消息标记
	channelOffsetResps, err := s.messageService.GetChannelOffsetWithUID(loginUID, channelIds)
	if err != nil {
		s.Error("查询用户清空channel消息标记错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户清空channel消息标记错误"))
		return
	}

	// 1. 预处理：构建 Map（O(n) 一次性处理）
	revokedMap := make(map[string]bool, len(revokedMsgExtras))
	for _, extra := range revokedMsgExtras {
		revokedMap[extra.MessageIDStr] = true
	}

	deletedMap := make(map[string]bool, len(deletedMsgExtras))
	for _, extra := range deletedMsgExtras {
		if extra.IsMutualDeleted == 1 {
			deletedMap[extra.MessageIDStr] = true
		}
	}

	deletedUserMap := make(map[string]bool, len(deletedMsgUserExtras))
	for _, extra := range deletedMsgUserExtras {
		if extra.MessageIsDeleted == 1 {
			deletedUserMap[extra.MessageIDStr] = true
		}
	}

	// channelID -> 清空到的 messageSeq
	channelOffsetMap := make(map[string]uint32, len(channelOffsetResps))
	for _, offset := range channelOffsetResps {
		channelOffsetMap[offset.ChannelID] = offset.MessageSeq
	}

	realMessages := make([]*config.MessageResp, 0)
	if msgResp != nil && len(msgResp.Messages) > 0 {
		for _, m := range msgResp.Messages {
			// O(1) 检查是否撤回
			if revokedMap[m.MessageIDStr] {
				continue
			}

			// O(1) 检查是否后台删除
			if deletedMap[m.MessageIDStr] {
				continue
			}

			// O(1) 检查是否用户删除
			if deletedUserMap[m.MessageIDStr] {
				continue
			}

			// O(1) 检查是否清空channel消息
			if offsetSeq, ok := channelOffsetMap[m.ChannelID]; ok && offsetSeq >= m.MessageSeq {
				continue
			}

			realMessages = append(realMessages, m)
		}
	}
	groupIds := make([]string, 0)
	uids := make([]string, 0)
	msgFromUids := make([]string, 0)

	if len(realMessages) > 0 {
		for _, m := range realMessages {
			if m.ChannelType == common.ChannelTypeGroup.Uint8() {
				groupIds = append(groupIds, m.ChannelID)
			} else if m.ChannelType == common.ChannelTypePerson.Uint8() {
				uids = append(uids, m.ChannelID)
			}
			if m.FromUID != "" {
				msgFromUids = append(msgFromUids, m.FromUID)
			}
		}
	}

	var joinedGroups []*group.InfoResp
	if req.OnlyMessage == 0 {
		joinedGroups, err = s.groupService.GetGroupsWithMemberUID(loginUID)
		if err != nil {
			s.Error("查询加入的群列表错误", zap.Error(err))
			c.ResponseError(errors.New("查询加入的群列表错误"))
			return
		}
		if len(joinedGroups) > 0 {
			for _, group := range joinedGroups {
				groupIds = append(groupIds, group.GroupNo)
			}
		}
	}

	var groups []*group.GroupResp
	var users []*user.UserDetailResp
	if len(groupIds) > 0 {
		groups, err = s.groupService.GetGroupDetails(groupIds, loginUID)
		if err != nil {
			s.Error("查询群列表错误", zap.Error(err))
			c.ResponseError(errors.New("查询群列表错误"))
			return
		}
	}
	if len(msgFromUids) > 0 {
		uids = append(uids, msgFromUids...)
	}
	if len(uids) > 0 {
		realUids := util.RemoveRepeatedElement(uids)
		users, err = s.userService.GetUserDetails(realUids, loginUID)
		if err != nil {
			s.Error("查询用户列表错误", zap.Error(err))
			c.ResponseError(errors.New("查询用户列表错误"))
			return
		}
	}

	// 加入的群（按 Space 过滤）
	groupResps := make([]*channelResp, 0)
	searchSpaceID := c.Query("space_id")
	if req.OnlyMessage == 0 && len(joinedGroups) > 0 {
		for _, g := range joinedGroups {
			// Space 过滤：如果指定了 space_id，只显示该 Space 的群
			if searchSpaceID != "" && g.SpaceID != searchSpaceID {
				continue
			}
			isAdd := false
			remark := ""
			if strings.Contains(g.Name, req.Keyword) {
				isAdd = true
			}
			if len(groups) > 0 {
				for _, group := range groups {
					if group.GroupNo == g.GroupNo {
						remark = group.Remark
						if strings.Contains(group.Remark, req.Keyword) {
							isAdd = true
						}
						break
					}
				}
			}
			if isAdd {
				escapedKeyword := html.EscapeString(req.Keyword)
				name := strings.ReplaceAll(g.Name, req.Keyword, fmt.Sprintf("<mark>%s</mark>", escapedKeyword))
				groupResps = append(groupResps, &channelResp{
					ChannelID:     g.GroupNo,
					ChannelType:   common.ChannelTypeGroup.Uint8(),
					ChannelName:   name,
					ChannelRemark: remark,
				})
			}
		}
	}

	// 查询联系人（Space 模式：搜索 Space 成员；否则搜索好友）
	friendResps := make([]*channelResp, 0)
	if req.OnlyMessage == 0 {
		if searchSpaceID != "" {
			var members []struct {
				UID  string `db:"uid"`
				Name string `db:"name"`
			}
			_, err := s.ctx.DB().SelectBySql(
				"SELECT sm.uid, IFNULL(u.name,'') as name FROM space_member sm LEFT JOIN user u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1 AND (u.name LIKE ? OR sm.uid LIKE ?)",
				searchSpaceID, "%"+req.Keyword+"%", "%"+req.Keyword+"%",
			).Load(&members)
			if err != nil {
				s.Error("搜索Space成员错误", zap.Error(err))
			}
			for _, m := range members {
				if m.UID == loginUID {
					continue
				}
				escapedKeyword := html.EscapeString(req.Keyword)
				name := strings.ReplaceAll(m.Name, req.Keyword, fmt.Sprintf("<mark>%s</mark>", escapedKeyword))
				friendResps = append(friendResps, &channelResp{
					ChannelID:   m.UID,
					ChannelName: name,
					ChannelType: common.ChannelTypePerson.Uint8(),
				})
			}
		} else {
			friends, err := s.userService.SearchFriendsWithKeyword(loginUID, req.Keyword)
			if err != nil {
				s.Error("查询好友错误", zap.Error(err))
				c.ResponseError(err)
				return
			}
			if len(friends) > 0 {
				for _, friend := range friends {
					escapedKeyword := html.EscapeString(req.Keyword)
					name := strings.ReplaceAll(friend.Name, req.Keyword, fmt.Sprintf("<mark>%s</mark>", escapedKeyword))
					friendResps = append(friendResps, &channelResp{
						ChannelID:     friend.UID,
						ChannelName:   name,
						ChannelType:   common.ChannelTypePerson.Uint8(),
						ChannelRemark: friend.Remark,
					})
				}
			}
		}
	}

	messagesResp := make([]*messageResp, 0)
	if len(realMessages) > 0 {
		for _, msg := range realMessages {
			var isDeleted int8 = 0
			setting := config.SettingFromUint8(msg.Setting)
			var payloadMap map[string]interface{}
			if setting.Signal {
				payloadMap = map[string]interface{}{
					"type": common.SignalError.Int(),
				}
			} else {
				err := util.ReadJsonByByte(msg.Payload, &payloadMap)
				if err != nil {
					log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(msg.Payload)))
				}
				if len(payloadMap) > 0 {
					visibles := payloadMap["visibles"]
					if visibles != nil {
						visiblesArray, ok := visibles.([]interface{})
						if !ok {
							visiblesArray = nil
						}
						if len(visiblesArray) > 0 {
							isDeleted = 1
							for _, limitUID := range visiblesArray {
								if limitUID == loginUID {
									isDeleted = 0
								}
							}
						}
					}
				} else {
					payloadMap = map[string]interface{}{
						"type": common.ContentError.Int(),
					}
				}
			}
			if isDeleted == 1 {
				continue
			}
			var tempChannel *channelResp
			if msg.ChannelType == common.ChannelTypePerson.Uint8() {
				for _, user := range users {
					if user.UID == msg.ChannelID {
						tempChannel = &channelResp{
							ChannelID:     user.UID,
							ChannelType:   common.ChannelTypePerson.Uint8(),
							ChannelRemark: user.Remark,
							ChannelName:   user.Name,
						}
						break
					}
				}
			}
			var fromChannel *channelResp
			if len(users) > 0 && msg.FromUID != "" {
				for _, user := range users {
					if msg.FromUID == user.UID {
						fromChannel = &channelResp{
							ChannelID:     user.UID,
							ChannelType:   common.ChannelTypePerson.Uint8(),
							ChannelRemark: user.Remark,
							ChannelName:   user.Name,
						}
						break
					}
				}
			}
			if msg.ChannelType == common.ChannelTypeGroup.Uint8() {
				for _, group := range groups {
					if group.GroupNo == msg.ChannelID {
						tempChannel = &channelResp{
							ChannelID:     group.GroupNo,
							ChannelType:   common.ChannelTypeGroup.Uint8(),
							ChannelName:   group.Name,
							ChannelRemark: group.Remark,
						}
						break
					}
				}
			}
			messagesResp = append(messagesResp, &messageResp{
				MessageIDStr: msg.MessageIDStr,
				MessageID:    msg.MessageID,
				MessageSeq:   msg.MessageSeq,
				FromUID:      msg.FromUID,
				Timestamp:    msg.Timestamp,
				Payload:      payloadMap,
				ClientMsgNo:  msg.ClientMsgNo,
				Channel:      tempChannel,
				IsDeleted:    isDeleted,
				FromChannel:  fromChannel,
			})
		}
	}
	c.Response(map[string]interface{}{
		"friends":  friendResps,
		"groups":   groupResps,
		"messages": messagesResp,
	})
}

type channelResp struct {
	ChannelID     string `json:"channel_id"`
	ChannelType   uint8  `json:"channel_type"`
	ChannelRemark string `json:"channel_remark"`
	ChannelName   string `json:"channel_name"`
}

type messageResp struct {
	Setting      uint8                  `json:"setting"`           // 设置
	MessageID    int64                  `json:"message_id"`        // 服务端的消息ID(全局唯一)
	MessageIDStr string                 `json:"message_idstr"`     // 服务端的消息ID(全局唯一)字符串形式
	MessageSeq   uint32                 `json:"message_seq"`       // 消息序列号 （用户唯一，有序递增）
	ClientMsgNo  string                 `json:"client_msg_no"`     // 客户端消息唯一编号
	FromUID      string                 `json:"from_uid"`          // 发送者UID
	Expire       uint32                 `json:"expire,omitempty"`  // expire
	Timestamp    int32                  `json:"timestamp"`         // 服务器消息时间戳(10位，到秒)
	Payload      map[string]interface{} `json:"payload"`           // 消息内容
	IsDeleted    int8                   `json:"is_deleted"`        // 是否已删除
	Channel      *channelResp           `json:"channel,omitempty"` // 消息所属channel
	FromChannel  *channelResp           `json:"from_channel"`      // 消息发送者channel
}
