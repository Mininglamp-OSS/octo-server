package message

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// 置顶或取消置顶消息
func (m *Message) pinnedMessage(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	loginName := c.GetLoginName()
	type reqVO struct {
		MessageID   string `json:"message_id"`   // 消息唯一ID
		MessageSeq  uint32 `json:"message_seq"`  // 消息序列号
		ChannelID   string `json:"channel_id"`   // 频道唯一ID
		ChannelType uint8  `json:"channel_type"` // 频道类型
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if req.ChannelID == "" {
		respondMessageRequestInvalid(c, "channel_id")
		return
	}
	if req.MessageID == "" {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	if req.MessageSeq <= 0 {
		respondMessageRequestInvalid(c, "message_seq")
		return
	}

	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if loginUID == req.ChannelID {
			respondMessageRequestInvalid(c, "channel_id")
			return
		}
		isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
		if err != nil {
			m.Error("查询好友关系错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isFriend {
			httperr.ResponseErrorL(c, errcode.ErrMessageConversationForbidden, nil, nil)
			return
		}
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	} else if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		groupInfo, err := m.groupService.GetGroupDetail(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询群组信息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if groupInfo == nil || groupInfo.Status != 1 {
			httperr.ResponseErrorL(c, errcode.ErrMessageGroupNotFound, nil, nil)
			return
		}
		isCreatorOrManager, err := m.groupService.IsCreatorOrManager(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询用户在群内权限错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isCreatorOrManager && groupInfo.AllowMemberPinnedMessage == 0 {
			httperr.ResponseErrorL(c, errcode.ErrMessagePinnedForbidden, nil, nil)
			return
		}
	}
	messageIds := make([]int64, 0)
	id, _ := strconv.ParseInt(req.MessageID, 10, 64)
	messageIds = append(messageIds, id)
	syncMsg, err := m.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		LoginUID:    loginUID,
		MessageIds:  messageIds,
	})
	if err != nil {
		m.Error("查询消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if syncMsg == nil || len(syncMsg.Messages) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}
	message := syncMsg.Messages[0]
	messageExtra, err := m.messageExtraDB.queryWithMessageID(req.MessageID)
	if err != nil {
		m.Error("查询消息扩展信息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if messageExtra != nil && messageExtra.IsDeleted == 1 {
		httperr.ResponseErrorL(c, errcode.ErrMessageNotFound, nil, nil)
		return
	}
	appConfig, err := m.commonService.GetAppConfig()
	if err != nil {
		m.Error("查询配置错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	var maxCount = 10
	if appConfig != nil {
		maxCount = appConfig.ChannelPinnedMessageMaxCount
	}
	currentCount, err := m.pinnedDB.queryCountWithChannel(fakeChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询当前置顶消息数量错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	pinnedMessage, err := m.pinnedDB.queryWithMessageId(fakeChannelID, req.ChannelType, req.MessageID)
	if err != nil {
		m.Error("查询置顶消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if currentCount >= int64(maxCount) && (pinnedMessage == nil || pinnedMessage.IsDeleted == 1) {
		respondMessagePinnedLimitExceeded(c, maxCount)
		return
	}

	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("开启事务错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	isPinned := 0
	isSendSystemMsg := false
	if pinnedMessage == nil {
		err = m.pinnedDB.insertTx(&pinnedMessageModel{
			MessageId:   req.MessageID,
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			IsDeleted:   0,
			MessageSeq:  req.MessageSeq,
			Version:     time.Now().UnixMilli(),
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("新增置顶消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
		isSendSystemMsg = true
		isPinned = 1
	} else {
		if pinnedMessage.IsDeleted == 1 {
			pinnedMessage.IsDeleted = 0
			isPinned = 1
		} else {
			pinnedMessage.IsDeleted = 1
			isPinned = 0
		}
		pinnedMessage.Version = time.Now().UnixMilli()
		err = m.pinnedDB.updateTx(pinnedMessage, tx)
		if err != nil {
			tx.Rollback()
			m.Error("取消置顶消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}
	version, err := m.genMessageExtraSeq(fakeChannelID)
	if err != nil {
		tx.Rollback()
		m.Error("生成消息扩展序列号失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	err = m.messageExtraDB.insertOrUpdatePinnedTx(&messageExtraModel{
		MessageID:   req.MessageID,
		MessageSeq:  req.MessageSeq,
		ChannelID:   fakeChannelID,
		ChannelType: req.ChannelType,
		IsPinned:    isPinned,
		Version:     version,
	}, tx)
	if err != nil {
		tx.Rollback()
		m.Error("更新消息置顶状态失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		m.Error("事务提交失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     c.GetLoginUID(),
		CMD:         common.CMDSyncPinnedMessage,
	})

	if err != nil {
		m.Error("发送cmd失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
		return
	}
	if isSendSystemMsg {
		var payloadMap map[string]interface{}
		if len(message.Payload) > LargePayloadThreshold {
			// 置顶系统消息拼接只需 type / content 等字段，超大 payload 走截断避免完整反序列化
			payloadMap = TruncatedPayload(message.Payload)
		} else {
			err := util.ReadJsonByByte(message.Payload, &payloadMap)
			if err != nil {
				m.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(message.Payload)))
				c.ResponseOK()
				return
			}
		}
		var contentType int = 0
		var content string = ""
		if payloadMap["type"] != nil {
			switch v := payloadMap["type"].(type) {
			case json.Number:
				contentTypeI, _ := v.Int64()
				contentType = int(contentTypeI)
			case float64:
				contentType = int(v)
			}
		}
		if contentType == common.Text.Int() {
			if contentStr, ok := payloadMap["content"].(string); ok {
				content = fmt.Sprintf("`%s`", contentStr)
			} else {
				content = common.GetDisplayText(contentType)
			}
		} else {
			content = common.GetDisplayText(contentType)
		}
		mesageContent := fmt.Sprintf("{0} 置顶了%s", content)
		// YUJ-660 Medium-1 partial: 此 Tip 在 PERSONAL DM 上同样需要服务端权威
		// payload.space_id（GROUP / COMMUNITY_TOPIC 也走同一条路径，由 enrich
		// 函数按 channelType 分派到群表 / 父群权威源 / sender SpaceID）。
		// 不直接调 m.ctx.SendMessage 绕过 enrichment。
		tipPayload := map[string]interface{}{
			"from_uid":  loginUID,
			"from_name": loginName,
			"content":   mesageContent,
			"extra": []config.UserBaseVo{
				{
					UID:  loginUID,
					Name: loginName,
				},
			},
			"type": common.Tip,
		}
		tipPayload = m.enrichPayloadWithSpaceID(req.ChannelID, req.ChannelType, tipPayload, spacepkg.GetSpaceID(c))
		err = m.ctx.SendMessage(&config.MsgSendReq{
			Header: config.MsgHeader{
				NoPersist: 0,
				RedDot:    1,
				SyncOnce:  0, // 只同步一次
			},
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			FromUID:     loginUID,
			Payload:     []byte(util.ToJson(tipPayload)),
		})
		if err != nil {
			m.Warn("发送解散群消息错误", zap.Error(err))
		}
	}
	c.ResponseOK()
}

func (m *Message) clearPinnedMessage(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if req.ChannelID == "" {
		respondMessageRequestInvalid(c, "channel_id")
		return
	}
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if loginUID == req.ChannelID {
			respondMessageRequestInvalid(c, "channel_id")
			return
		}
		isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
		if err != nil {
			m.Error("查询好友关系错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isFriend {
			httperr.ResponseErrorL(c, errcode.ErrMessageConversationForbidden, nil, nil)
			return
		}
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	} else {
		// 查询权限
		isCreatorOrManager, err := m.groupService.IsCreatorOrManager(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询用户在群内权限错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isCreatorOrManager {
			httperr.ResponseErrorL(c, errcode.ErrMessagePinnedForbidden, nil, nil)
			return
		}
	}
	pinnedMsgs, err := m.pinnedDB.queryWithUnDeletedMessage(fakeChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询置顶消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	messageIds := make([]string, 0)
	if len(pinnedMsgs) <= 0 {
		c.ResponseOK()
		return
	}

	for _, msg := range pinnedMsgs {
		messageIds = append(messageIds, msg.MessageId)
	}
	messageUserExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID(messageIds, loginUID)
	if err != nil {
		m.Error("查询用户消息扩展字段失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	channelOffsetM, err := m.channelOffsetDB.queryWithUIDAndChannel(loginUID, fakeChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询频道偏移量失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	updateModel := make([]*pinnedMessageModel, 0)
	for _, msg := range pinnedMsgs {
		isAdd := true
		if len(messageUserExtras) > 0 {
			for _, extra := range messageUserExtras {
				if extra.MessageID == msg.MessageId && extra.MessageIsDeleted == 1 {
					isAdd = false
					break
				}
			}
		}
		if channelOffsetM != nil && msg.MessageSeq <= channelOffsetM.MessageSeq {
			isAdd = false
		}
		if isAdd {
			msg.IsDeleted = 1
			msg.Version = time.Now().UnixMilli()
			updateModel = append(updateModel, msg)
		}
	}
	if len(updateModel) == 0 {
		c.ResponseOK()
		return
	}
	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("开启事务错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	for _, msg := range updateModel {
		err = m.pinnedDB.updateTx(msg, tx)
		if err != nil {
			tx.Rollback()
			m.Error("删除置顶消息错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}

		version, err := m.genMessageExtraSeq(fakeChannelID)
		if err != nil {
			tx.Rollback()
			m.Error("生成消息扩展序列号失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
		err = m.messageExtraDB.insertOrUpdatePinnedTx(&messageExtraModel{
			MessageID:   msg.MessageId,
			MessageSeq:  msg.MessageSeq,
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			IsPinned:    0,
			Version:     version,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("修改消息扩展置顶状态错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		m.Error("事务提交失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     c.GetLoginUID(),
		CMD:         common.CMDSyncPinnedMessage,
	})

	if err != nil {
		m.Error("发送cmd失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (m *Message) syncPinnedMessage(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	var req struct {
		Version     int64  `json:"version"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if req.ChannelID == "" {
		respondMessageRequestInvalid(c, "channel_id")
		return
	}
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		if loginUID == req.ChannelID {
			respondMessageRequestInvalid(c, "channel_id")
			return
		}
		isFriend, err := m.userService.IsFriend(loginUID, req.ChannelID)
		if err != nil {
			m.Error("查询好友关系错误", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isFriend {
			httperr.ResponseErrorL(c, errcode.ErrMessageConversationForbidden, nil, nil)
			return
		}
		fakeChannelID = common.GetFakeChannelIDWith(loginUID, req.ChannelID)
	} else if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		isMember, err := m.groupService.ExistMember(req.ChannelID, loginUID)
		if err != nil {
			m.Error("查询群成员失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if !isMember {
			httperr.ResponseErrorL(c, errcode.ErrMessageNotGroupMember, nil, nil)
			return
		}
	}
	pinnedMsgs, err := m.pinnedDB.queryWithChannelIDAndVersion(fakeChannelID, req.ChannelType, req.Version)
	if err != nil {
		m.Error("查询置顶消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	messageSeqs := make([]uint32, 0)
	messageIds := make([]string, 0)
	list := make([]*MsgSyncResp, 0)
	pinnedMessageList := make([]*pinnedMessageResp, 0)
	if len(pinnedMsgs) <= 0 {
		c.Response(map[string]interface{}{
			"pinned_messages": pinnedMessageList,
			"messages":        list,
		})
		return
	}

	for _, msg := range pinnedMsgs {
		messageSeqs = append(messageSeqs, msg.MessageSeq)
		messageIds = append(messageIds, msg.MessageId)
	}

	resp, err := m.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, loginUID, messageSeqs)
	if err != nil {
		m.Error("查询频道内的消息失败！", zap.Error(err), zap.String("req", util.ToJson(req)))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	if resp == nil || len(resp.Messages) == 0 {
		c.Response(map[string]interface{}{
			"pinned_messages": pinnedMessageList,
			"messages":        list,
		})
		return
	}
	// 消息全局扩张
	messageExtras, err := m.messageExtraDB.queryWithMessageIDsAndUID(messageIds, loginUID)
	if err != nil {
		m.Error("查询消息扩展字段失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	messageExtraMap := map[string]*messageExtraDetailModel{}
	if len(messageExtras) > 0 {
		for _, messageExtra := range messageExtras {
			messageExtraMap[messageExtra.MessageID] = messageExtra
		}
	}
	// 消息用户扩张
	messageUserExtras, err := m.messageUserExtraDB.queryWithMessageIDsAndUID(messageIds, loginUID)
	if err != nil {
		m.Error("查询用户消息扩展字段失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	messageUserExtraMap := map[string]*messageUserExtraModel{}
	if len(messageUserExtras) > 0 {
		for _, messageUserExtraM := range messageUserExtras {
			messageUserExtraMap[messageUserExtraM.MessageID] = messageUserExtraM
		}
	}
	// 查询消息回应
	messageReaction, err := m.messageReactionDB.queryWithMessageIDs(messageIds)
	if err != nil {
		m.Error("查询消息回应数据错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	messageReactionMap := map[string][]*reactionModel{}
	if len(messageReaction) > 0 {
		for _, reaction := range messageReaction {
			msgReactionList := messageReactionMap[reaction.MessageID]
			if msgReactionList == nil {
				msgReactionList = make([]*reactionModel, 0)
			}
			msgReactionList = append(msgReactionList, reaction)
			messageReactionMap[reaction.MessageID] = msgReactionList
		}
	}
	channelOffsetM, err := m.channelOffsetDB.queryWithUIDAndChannel(loginUID, fakeChannelID, req.ChannelType)
	if err != nil {
		m.Error("查询频道偏移量失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	// 频道偏移
	channelIds := make([]string, 0)
	channelIds = append(channelIds, fakeChannelID)
	channelSettings, err := m.channelService.GetChannelSettings(channelIds)
	if err != nil {
		m.Error("查询频道设置错误", zap.Error(err), zap.String("req", util.ToJson(req)))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	var channelOffsetMessageSeq uint32 = 0
	if len(channelSettings) > 0 && channelSettings[0].OffsetMessageSeq > 0 {
		channelOffsetMessageSeq = channelSettings[0].OffsetMessageSeq
	}
	for _, message := range resp.Messages {
		if channelOffsetM != nil && message.MessageSeq <= channelOffsetM.MessageSeq {
			continue
		}
		msgResp := &MsgSyncResp{}
		messageIDStr := strconv.FormatInt(message.MessageID, 10)
		messageExtra := messageExtraMap[messageIDStr]
		messageUserExtra := messageUserExtraMap[messageIDStr]
		msgResp.from(message, loginUID, messageExtra, messageUserExtra, messageReactionMap[messageIDStr], channelOffsetMessageSeq)
		list = append(list, msgResp)
	}

	// YUJ-98 / YUJ-101: 置顶消息同步路径同样回填 msg-level 外部来源字段，
	// 保持与 /message/channel/sync 的外部标识口径一致。
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		m.enrichExternalMarkers(req.ChannelID, list)
	}

	for _, msg := range pinnedMsgs {
		messageUserExtra := messageUserExtraMap[msg.MessageId]
		if messageUserExtra != nil && messageUserExtra.MessageIsDeleted == 1 {
			msg.IsDeleted = 1
		}
		if channelOffsetM != nil && msg.MessageSeq <= channelOffsetM.MessageSeq {
			msg.IsDeleted = 1
		}
		toChannelID := common.GetToChannelIDWithFakeChannelID(msg.ChannelID, loginUID)
		pinnedMessageList = append(pinnedMessageList, &pinnedMessageResp{
			MessageID:   msg.MessageId,
			MessageSeq:  msg.MessageSeq,
			ChannelID:   toChannelID,
			ChannelType: msg.ChannelType,
			IsDeleted:   msg.IsDeleted,
			Version:     msg.Version,
			CreatedAt:   msg.CreatedAt.String(),
			UpdatedAt:   msg.UpdatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"pinned_messages": pinnedMessageList,
		"messages":        list,
	})
}

func (m *Message) deletePinnedMessage(channelID string, channelType uint8, messageIds []string, loginUID string, tx *dbr.Tx) error {
	fakeChannelID := channelID
	if channelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(channelID, loginUID)
	}
	pinnedMessages, err := m.pinnedDB.queryWithMessageIds(fakeChannelID, channelType, messageIds)
	if err != nil {
		m.Error("查询置顶消息错误", zap.Error(err))
		return errors.New("查询置顶消息错误")
	}
	if len(pinnedMessages) == 0 {
		return nil
	}
	for _, pinnedMessage := range pinnedMessages {
		pinnedMessage.IsDeleted = 1
		pinnedMessage.Version = time.Now().UnixMilli()
		err = m.pinnedDB.updateTx(pinnedMessage, tx)
		if err != nil {
			tx.Rollback()
			m.Error("取消置顶消息错误", zap.Error(err))
			return errors.New("取消置顶消息错误")
		}
	}

	err = m.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   channelID,
		ChannelType: channelType,
		FromUID:     loginUID,
		CMD:         common.CMDSyncPinnedMessage,
	})

	if err != nil {
		m.Warn("发送cmd失败！", zap.Error(err))
	}
	return nil
}

type pinnedMessageResp struct {
	MessageID   string `json:"message_id"`
	MessageSeq  uint32 `json:"message_seq"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	IsDeleted   int8   `json:"is_deleted"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}
