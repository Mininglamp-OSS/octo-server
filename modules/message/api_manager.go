package message

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Manager 消息管理
type Manager struct {
	ctx *config.Context
	log.Log
	userService  user.IService
	groupService group.IService
	managerDB    *managerDB
	pinnedDB     *pinnedDB
}

// NewManager NewManager
func NewManager(ctx *config.Context) *Manager {
	return &Manager{
		ctx:          ctx,
		Log:          log.NewTLog("MessageManager"),
		userService:  user.NewService(ctx),
		groupService: group.NewService(ctx),
		managerDB:    newManagerDB(ctx),
		pinnedDB:     newPinnedDB(ctx),
	}
}

// Route 路由配置
func (m *Manager) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
		auth.POST("/message/send", m.sendMsg)                         // 发送消息
		auth.POST("message/sendfriends", m.sendMsgToFriends)          // 给某个用户代发消息
		auth.GET("/message", m.list)                                  // 代发消息记录
		auth.POST("/message/sendall", m.sendMsgToAllUsers)            // 给所有用户发送一条消息
		auth.GET("/message/record", m.record)                         // 消息记录
		auth.GET("/message/recordpersonal", m.recordpersonal)         // 单聊聊天记录
		auth.POST("/message/prohibit_words", m.addProhibitWords)      // 添加违禁词
		auth.GET("/message/prohibit_words", m.prohibitWords)          // 查询违禁词
		auth.DELETE("/message/prohibit_words", m.deleteProhibitWords) // 删除违禁词
		auth.DELETE("/message", m.delete)                             // 删除消息
	}
}
func (m *Manager) sendMsgToFriends(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	type ReqVO struct {
		UID     string   `json:"uid"`
		ToUIDs  []string `json:"to_uids"`
		Content string   `json:"content"`
	}
	var req ReqVO
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if req.UID == "" {
		respondMessageRequestInvalid(c, "from_uid")
		return
	}
	if req.Content == "" {
		respondMessageRequestInvalid(c, "content")
		return
	}
	if len(req.ToUIDs) == 0 {
		respondMessageRequestInvalid(c, "subscribers")
		return
	}
	go m.sendMessageToFriends(req.ToUIDs, req.UID, req.Content)
	c.ResponseOK()
}

func (m *Manager) sendMessageToFriends(toUids []string, fromUID string, content string) error {
	err := m.ctx.SendMessageBatch(&config.MsgSendBatch{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		FromUID: fromUID,
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": content,
			"type":    1,
		})),
		Subscribers: toUids,
	})
	if err != nil {
		m.Error("发送消息错误", zap.Error(err))
		return errors.New("发送消息错误")
	}
	return nil
}
func (m *Manager) delete(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	type msgVO struct {
		MessageID  string `json:"message_id"`
		MessageSeq uint32 `json:"message_seq"`
	}
	type reqVO struct {
		List        []*msgVO `json:"list"`
		ChannelID   string   `json:"channel_id"`
		FromUID     string   `json:"from_uid"`
		ChannelType uint8    `json:"channel_type"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		m.Error("数据格式有误！", zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if len(req.List) == 0 {
		respondMessageRequestInvalid(c, "msg_ids")
		return
	}
	if req.ChannelType == uint8(common.ChannelTypePerson) && (req.FromUID == "" || req.ChannelID == req.FromUID) {
		respondMessageRequestInvalid(c, "from_uid")
		return
	}
	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(req.ChannelID, req.FromUID)
	}
	tx, err := m.ctx.DB().Begin()
	if err != nil {
		m.Error("开启事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	msgIds := make([]string, 0)
	for _, msg := range req.List {
		version, err := m.genMessageExtraSeq(fakeChannelID)
		if err != nil {
			m.Error("生成消息扩展序列号失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
		msgIds = append(msgIds, msg.MessageID)
		err = m.managerDB.updateMsgExtraVersionAndDeletedTx(&messageExtraModel{
			ChannelID:   fakeChannelID,
			ChannelType: req.ChannelType,
			MessageID:   msg.MessageID,
			MessageSeq:  msg.MessageSeq,
			IsDeleted:   1,
			Version:     version,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}
	pinnedMsgs, err := m.pinnedDB.queryWithMessageIds(fakeChannelID, req.ChannelType, msgIds)
	if err != nil {
		tx.Rollback()
		m.Error("查询置顶消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	isSendSyncPinnedMsgCMD := false
	if len(pinnedMsgs) > 0 {
		for _, pinnedMsg := range pinnedMsgs {
			if pinnedMsg.IsDeleted == 0 {
				pinnedMsg.IsDeleted = 1
				pinnedMsg.Version = time.Now().UnixMilli()
				isSendSyncPinnedMsgCMD = true
				err = m.pinnedDB.updateTx(pinnedMsg, tx)
				if err != nil {
					tx.Rollback()
					m.Error("删除置顶消息错误", zap.Error(err))
					httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
					return
				}
			}
		}
	}
	if isSendSyncPinnedMsgCMD {
		err = m.ctx.SendCMD(config.MsgCMDReq{
			NoPersist:   true,
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			FromUID:     loginUID,
			CMD:         common.CMDSyncPinnedMessage,
		})

		if err != nil {
			m.Warn("发送cmd失败！", zap.Error(err))
		}
	}
	var eventID int64 = 0
	if m.ctx.GetConfig().ZincSearch.SearchOn {
		eventID, err = m.ctx.EventBegin(&wkevent.Data{
			Event: event.EventUpdateSearchMessage,
			Data: &config.UpdateSearchMessageReq{
				MessageIDs: msgIds,
				ChannelID:  req.ChannelID,
			},
			Type: wkevent.None,
		}, tx)
		if err != nil {
			tx.Rollback()
			m.Error("开启事件失败！", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		m.Error("提交事务失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if eventID > 0 {
		m.ctx.EventCommit(eventID)
	}
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		err = m.ctx.SendCMD(config.MsgCMDReq{
			NoPersist:   false,
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			CMD:         common.CMDSyncMessageExtra,
			FromUID:     req.FromUID,
			Param: map[string]interface{}{
				"channel_id":   req.ChannelID,
				"channel_type": req.ChannelType,
			},
		})
	} else {
		err = m.ctx.SendCMD(config.MsgCMDReq{
			NoPersist:   false,
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			CMD:         common.CMDSyncMessageExtra,
			Param: map[string]interface{}{
				"channel_id":   req.ChannelID,
				"channel_type": req.ChannelType,
			},
		})
	}

	if err != nil {
		m.Error("发送cmd失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
		return
	}
	c.ResponseOK()
}
func (m *Manager) deleteProhibitWords(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	is_deleted := c.Query("is_deleted")
	isDeleted, _ := strconv.Atoi(is_deleted)
	id := c.Query("id")
	if id == "" || (isDeleted != 0 && isDeleted != 1) {
		respondMessageRequestInvalid(c, "")
		return
	}
	tempID, _ := strconv.Atoi(id)
	words, err := m.managerDB.queryProhibitWordsWithID(tempID)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	if words == nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageBanwordNotFound, nil, nil)
		return
	}
	words.IsDeleted = isDeleted
	genSeqVal, err := m.ctx.GenSeq(common.ProhibitWordKey)
	if err != nil {
		m.Error("生成违禁词序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	words.Version = genSeqVal
	err = m.managerDB.updateProhibitWord(words)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

func (m *Manager) prohibitWords(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	pageIndex, pageSize := c.GetPage()
	searchKey := c.Query("search_key")
	var result []*prohibitWordsModel
	var count int64 = 0
	if searchKey == "" {
		result, err = m.managerDB.queryProhibitWords(uint64(pageIndex), uint64(pageSize))
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		count, err = m.managerDB.queryProhibitWordsCount()
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
	} else {
		result, err = m.managerDB.queryProhibitWordsWithContentAndPage(searchKey, uint64(pageIndex), uint64(pageSize))
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}

		count, err = m.managerDB.queryProhibitWordsCountWithContent(searchKey)
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
	}

	list := make([]*prohibitWordsVO, 0)

	if len(result) > 0 {
		for _, word := range result {
			list = append(list, &prohibitWordsVO{
				Content:   word.Content,
				CreatedAt: word.CreatedAt.String(),
				IsDeleted: word.IsDeleted,
				Version:   word.Version,
				Id:        word.Id,
			})
		}
	}
	c.Response(map[string]interface{}{
		"list":  list,
		"count": count,
	})
}
func (m *Manager) addProhibitWords(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	content := c.Query("content")
	if content == "" {
		respondMessageRequestInvalid(c, "word")
		return
	}
	model, err := m.managerDB.queryProhibitWordsWithContent(content)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	version, err := m.ctx.GenSeq(common.ProhibitWordKey)
	if err != nil {
		m.Error("生成违禁词序列号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	if model != nil {
		model.IsDeleted = 0
		model.Version = version
		err = m.managerDB.updateProhibitWord(model)
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	} else {
		err = m.managerDB.insertProhibitWord(&prohibitWordsModel{
			IsDeleted: 0,
			Content:   content,
			Version:   version,
		})
		if err != nil {
			m.Error(common.ErrData.Error(), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
			return
		}
	}
	c.ResponseOK()
}
func (m *Manager) recordpersonal(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	uid := c.Query("uid")
	touid := c.Query("touid")
	pageIndex, pageSize := c.GetPage()
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(touid) == "" {
		respondMessageRequestInvalid(c, "uid")
		return
	}
	channelID := common.GetFakeChannelIDWith(uid, touid)
	msgs, err := m.managerDB.queryWithChannelID(channelID, uint64(pageIndex), uint64(pageSize))
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	count, err := m.managerDB.queryRecordCount(channelID)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	list := make([]*recordVO, 0)
	if len(msgs) == 0 {
		c.Response(list)
		return
	}
	uids := make([]string, 0)
	msgIds := make([]string, 0)
	for _, msg := range msgs {
		uids = append(uids, msg.FromUID)
		msgIds = append(msgIds, strconv.FormatInt(msg.MessageID, 10))
	}
	msgExtrs, err := m.managerDB.queryMsgExtrWithMsgIds(msgIds)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	userList, err := m.userService.GetUsers(uids)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	ids := make([]int64, 0)
	for _, msg := range msgs {
		sendName := ""
		for _, user := range userList {
			if user.UID == msg.FromUID {
				sendName = user.Name
			}
		}
		isDeleted := 0
		revoke := 0
		editedAt := 0
		readedCount := 0
		var payloadMap map[string]interface{}
		for _, extr := range msgExtrs {
			msgID, _ := strconv.ParseInt(extr.MessageID, 10, 64)
			if msgID == msg.MessageID {
				isDeleted = extr.IsDeleted
				revoke = extr.Revoke
				editedAt = extr.EditedAt
				readedCount = extr.ReadedCount
				if extr.ContentEdit.String != "" {
					err := util.ReadJsonByByte([]byte(extr.ContentEdit.String), &payloadMap)
					if err != nil {
						log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(extr.ContentEdit.String)))
					}
				}
			}
		}
		if payloadMap == nil {
			err := util.ReadJsonByByte(msg.Payload, &payloadMap)
			if err != nil {
				log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(msg.Payload)))
			}
		}
		var deviceDBID int64 = 0
		if strings.Contains(msg.ClientMsgNo, "_") {
			tempStrs := strings.Split(msg.ClientMsgNo, "_")
			if len(tempStrs) > 2 {
				str := tempStrs[1]
				if str != "" {
					deviceDBID, err = strconv.ParseInt(str, 10, 64)
					if err == nil {
						ids = append(ids, deviceDBID)
					} else {
						deviceDBID = 0
					}
				}
			}
		}
		messageId := strconv.FormatInt(msg.MessageID, 10)
		list = append(list, &recordVO{
			MessageID:   messageId,
			Sender:      msg.FromUID,
			SenderName:  sendName,
			Payload:     payloadMap,
			Signal:      msg.Signal,
			IsDeleted:   isDeleted,
			CreatedAt:   msg.CreatedAt.String(),
			EditedAt:    editedAt,
			Revoke:      revoke,
			DeviceDBID:  deviceDBID,
			ReadedCount: readedCount,
		})
	}
	var devices []*model.DeviceResp
	if len(ids) > 0 {
		modules := register.GetModules(m.ctx)
		for _, module := range modules {
			if module.BussDataSource.GetDevice != nil {
				devices, _ = module.BussDataSource.GetDevice(ids)
				break
			}
		}
	}
	if len(devices) > 0 && len(list) > 0 {
		for _, device := range devices {
			for _, msg := range list {
				if msg.DeviceDBID == device.ID {
					msg.DeviceID = device.DeviceID
					msg.DeviceName = device.DeviceName
					msg.DeviceModel = device.DeviceModel
					break
				}
			}
		}
	}
	c.Response(&recordResp{
		Count: count,
		List:  list,
	})
}
func (m *Manager) record(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	var channelID = c.Query("channel_id")
	pageIndex, pageSize := c.GetPage()
	msgs, err := m.managerDB.queryWithChannelID(channelID, uint64(pageIndex), uint64(pageSize))
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	count, err := m.managerDB.queryRecordCount(channelID)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	list := make([]*recordVO, 0)
	if len(msgs) == 0 {
		c.Response(list)
		return
	}
	uids := make([]string, 0)
	msgIds := make([]string, 0)
	for _, msg := range msgs {
		uids = append(uids, msg.FromUID)
		msgIds = append(msgIds, strconv.FormatInt(msg.MessageID, 10))
	}
	msgExtrs, err := m.managerDB.queryMsgExtrWithMsgIds(msgIds)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	userList, err := m.userService.GetUsers(uids)
	if err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	ids := make([]int64, 0)
	for _, msg := range msgs {
		sendName := ""
		for _, user := range userList {
			if user.UID == msg.FromUID {
				sendName = user.Name
			}
		}
		isDeleted := 0
		revoke := 0
		editedAt := 0
		readedCount := 0
		var payloadMap map[string]interface{}
		for _, extr := range msgExtrs {
			msgID, _ := strconv.ParseInt(extr.MessageID, 10, 64)
			if msgID == msg.MessageID {
				isDeleted = extr.IsDeleted
				revoke = extr.Revoke
				editedAt = extr.EditedAt
				readedCount = extr.ReadedCount
				if extr.ContentEdit.String != "" {
					err := util.ReadJsonByByte([]byte(extr.ContentEdit.String), &payloadMap)
					if err != nil {
						log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(extr.ContentEdit.String)))
					}
				}
			}
		}
		if payloadMap == nil {
			err := util.ReadJsonByByte(msg.Payload, &payloadMap)
			if err != nil {
				log.Warn("负荷数据不是json格式！", zap.Error(err), zap.String("payload", string(msg.Payload)))
			}
		}
		var deviceDBID int64 = 0
		if strings.Contains(msg.ClientMsgNo, "_") {
			tempStrs := strings.Split(msg.ClientMsgNo, "_")
			if len(tempStrs) > 2 {
				str := tempStrs[1]
				if str != "" {
					deviceDBID, err = strconv.ParseInt(str, 10, 64)
					if err == nil {
						ids = append(ids, deviceDBID)
					} else {
						deviceDBID = 0
					}
				}
			}
		}
		messageId := strconv.FormatInt(msg.MessageID, 10)

		list = append(list, &recordVO{
			MessageID:   messageId,
			MessageSeq:  msg.MessageSeq,
			Sender:      msg.FromUID,
			SenderName:  sendName,
			Payload:     payloadMap,
			Signal:      0,
			IsDeleted:   isDeleted,
			CreatedAt:   msg.CreatedAt.String(),
			EditedAt:    editedAt,
			Revoke:      revoke,
			DeviceDBID:  deviceDBID,
			ReadedCount: readedCount,
		})
	}

	var devices []*model.DeviceResp
	if len(ids) > 0 {
		modules := register.GetModules(m.ctx)
		for _, module := range modules {
			if module.BussDataSource.GetDevice != nil {
				devices, _ = module.BussDataSource.GetDevice(ids)
				break
			}
		}
	}
	if len(devices) > 0 && len(list) > 0 {
		for _, device := range devices {
			for _, msg := range list {
				if msg.DeviceDBID == device.ID {
					msg.DeviceID = device.DeviceID
					msg.DeviceName = device.DeviceName
					msg.DeviceModel = device.DeviceModel
					break
				}
			}
		}
	}
	c.Response(&recordResp{
		Count: count,
		List:  list,
	})
}
func (m *Manager) sendMsgToAllUsers(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	type SendMsgReq struct {
		Content string `json:"content"`
	}
	var req SendMsgReq
	if err := c.BindJSON(&req); err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	userList, err := m.userService.GetAllUsers()
	if err != nil {
		m.Error("查询用户列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	uids := make([][]string, 0)
	tempUserList := make([]string, 0)
	for _, user := range userList {
		if len(tempUserList) == 1000 {
			uids = append(uids, tempUserList)
			tempUserList = make([]string, 0)
		}
		tempUserList = append(tempUserList, user.UID)
	}
	if len(tempUserList) > 0 {
		uids = append(uids, tempUserList)
	}
	go m.sendMessageBatch(uids, req.Content)
	c.ResponseOK()
}
func (m *Manager) sendMessageBatch(uids [][]string, content string) error {
	for _, list := range uids {
		err := m.ctx.SendMessageBatch(&config.MsgSendBatch{
			Header: config.MsgHeader{
				RedDot: 1,
			},
			FromUID: m.ctx.GetConfig().Account.SystemUID,
			Payload: []byte(util.ToJson(map[string]interface{}{
				"content": content,
				"type":    1,
			})),
			Subscribers: list,
		})
		if err != nil {
			m.Error("发送消息错误", zap.Error(err))
			return errors.New("发送消息错误")
		}
		time.Sleep(time.Second) // 批量发送限流，避免消息风暴
	}
	return nil
}

// 发送消息
func (m *Manager) sendMsg(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	var req managerSendMsgReq
	if err := c.BindJSON(&req); err != nil {
		m.Error(common.ErrData.Error(), zap.Error(err))
		respondMessageRequestInvalid(c, "")
		return
	}
	if err := req.check(); err != nil {
		respondMessageRequestInvalid(c, "")
		return
	}
	var receiverName string = ""
	if req.ReceivedChannelType == int(common.ChannelTypePerson) {
		user, err := m.userService.GetUser(req.ReceivedChannelID)
		if err != nil {
			m.Error("查询接受的者信息错误", zap.Error(err), zap.String("uid", req.ReceivedChannelID))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if user == nil {
			httperr.ResponseErrorL(c, errcode.ErrMessageReceiverNotFound, nil, nil)
			return
		}
		receiverName = user.Name
	}
	if req.ReceivedChannelType == int(common.ChannelTypeGroup) {
		group, err := m.groupService.GetGroupWithGroupNo(req.ReceivedChannelID)
		if err != nil {
			m.Error("查询接受群信息错误", zap.Error(err), zap.String("groupNo", req.ReceivedChannelID))
			httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			return
		}
		if group == nil {
			httperr.ResponseErrorL(c, errcode.ErrMessageGroupNotFound, nil, nil)
			return
		}
		receiverName = group.Name
	}
	// YUJ-660 Medium-1 partial: super-admin sendMsg path also needs server-
	// authoritative payload.space_id. /v1/manager has no SpaceMiddleware
	// (super-admin endpoint, not Space-scoped) → senderSpaceID is "". For GROUP
	// / COMMUNITY_TOPIC the enrichment helper writes the group's authoritative
	// space_id; for PERSONAL the empty senderSpaceID strips any forged
	// payload.space_id (YUJ-660 High-3 fail-open fix).
	//
	// YUJ-660 R3 Finding C — product clarification: super-admin PERSONAL DMs
	// are intentionally Space-agnostic (operations / support broadcasts cut
	// across Spaces). Because /v1/manager carries no SpaceMiddleware and the
	// admin has no per-call Space context to propagate, the dispatched payload
	// has no space_id field on PERSONAL — receiver clients then render the
	// message in any Space view. This is the expected behavior, not a bug;
	// changing it requires a product decision about whether super-admin DMs
	// should be Space-bound. Group / CommunityTopic still get authoritative
	// SpaceID from the group lookup, so cross-Space leak via admin GROUP send
	// is not possible.
	adminPayload := map[string]interface{}{
		"content":  req.Content,
		"type":     1,
		"from_uid": req.Sender,
	}
	managerLookup := func(groupNo string) (string, error) {
		g, err := m.groupService.GetGroupWithGroupNo(groupNo)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", nil
		}
		return g.SpaceID, nil
	}
	adminPayload = enrichPayloadWithSpaceIDCore(
		req.ReceivedChannelID,
		uint8(req.ReceivedChannelType),
		adminPayload,
		"", // super-admin path has no sender SpaceID
		managerLookup,
		func(s string, fields ...zap.Field) { m.Warn(s, fields...) },
	)
	err = m.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		FromUID:     req.Sender,
		ChannelID:   req.ReceivedChannelID,
		ChannelType: uint8(req.ReceivedChannelType),
		Payload:     []byte(util.ToJson(adminPayload)),
	})
	if err != nil {
		m.Error("发送消息错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageNotifyFailed, nil, nil)
		return
	}
	// 添加发送消息记录
	err = m.managerDB.insertMsgHistory(&managerMsgModel{
		Sender:              req.Sender,
		SenderName:          req.SenderName,
		ReceiverChannelType: req.ReceivedChannelType,
		Receiver:            req.ReceivedChannelID,
		ReceiverName:        receiverName,
		HandlerUID:          c.GetLoginUID(),
		HandlerName:         c.GetLoginName(),
		Content:             req.Content,
	})
	if err != nil {
		m.Error("添加发送消息记录错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// 代发消息列表
func (m *Manager) list(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondMessageForbidden(c)
		return
	}
	pageIndex, pageSize := c.GetPage()
	list, err := m.managerDB.queryMsgWithPage(uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("查询代发消息记录错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	count, err := m.managerDB.queryMsgCount()
	if err != nil {
		m.Error("查询代发消息总数错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	result := make([]*managerSendMsgResp, 0)
	for _, model := range list {
		result = append(result, &managerSendMsgResp{
			Sender:              model.Sender,
			SenderName:          model.SenderName,
			Receiver:            model.Receiver,
			ReceiverName:        model.ReceiverName,
			ReceiverChannelType: model.ReceiverChannelType,
			HandlerUID:          model.HandlerUID,
			HandlerName:         model.HandlerName,
			Content:             model.Content,
			CreatedAt:           model.CreatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  result,
	})
}

func (m *managerSendMsgReq) check() error {
	if m.ReceivedChannelID == "" {
		return errors.New("接受者ID不能为空")
	}
	if m.Sender == "" {
		return errors.New("发送者ID不能为空")
	}
	if m.SenderName == "" {
		return errors.New("发送者名字不能为空")
	}
	if m.ReceivedChannelType != int(common.ChannelTypeGroup) && m.ReceivedChannelType != int(common.ChannelTypePerson) && m.ReceivedChannelType != int(common.ChannelTypeNone) {
		return errors.New("接受者类型错误")
	}
	return nil
}

func (m *Manager) genMessageExtraSeq(channelID string) (int64, error) {
	return m.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, channelID))
}

type managerSendMsgReq struct {
	Sender              string `json:"sender"`                // 发送者uid
	SenderName          string `json:"sender_name"`           // 发送者名字
	ReceivedChannelID   string `json:"received_channel_id"`   // 接受者id
	ReceivedChannelType int    `json:"received_channel_type"` // 接受类型
	Content             string `json:"content"`               // 发送内容
}

type managerSendMsgResp struct {
	Receiver            string `json:"receiver"`              // 接受者uid
	ReceiverName        string `json:"receiver_name"`         // 接受者名字
	ReceiverChannelType int    `json:"receiver_channel_type"` // 接受者频道类型
	Sender              string `json:"sender"`                // 发送者uid
	SenderName          string `json:"sender_name"`           // 发送者名字
	HandlerUID          string `json:"handler_uid"`           // 操作者uid
	HandlerName         string `json:"handler_name"`          // 操作者名字
	Content             string `json:"content"`               // 发送内容
	CreatedAt           string `json:"created_at"`            // 发送时间
}
type recordResp struct {
	Count int64       `json:"count"`
	List  []*recordVO `json:"list"`
}
type recordVO struct {
	MessageID   string                 `json:"message_id"`   // 消息编号
	MessageSeq  uint32                 `json:"message_seq"`  // 消息序号
	Sender      string                 `json:"sender"`       // 发送者uid
	SenderName  string                 `json:"sender_name"`  // 发送者名字
	Signal      int                    `json:"signal"`       // 是否加密
	Payload     map[string]interface{} `json:"payload"`      // 发送内容
	IsDeleted   int                    `json:"is_deleted"`   // 是否删除
	ReadedCount int                    `json:"readed_count"` // 已读人数
	Revoke      int                    `json:"revoke"`       // 是否撤回
	DeviceDBID  int64                  `json:"device_db_id"` // 设备数据库id
	DeviceID    string                 `json:"device_id"`    // 设备id
	DeviceName  string                 `json:"device_name"`  // 设备名称
	DeviceModel string                 `json:"device_model"` // 设备型号
	CreatedAt   string                 `json:"created_at"`   // 发送时间
	EditedAt    int                    `json:"edited_at"`    // 编辑时间
}
type prohibitWordsVO struct {
	Id        int64  `json:"id"`
	Content   string `json:"content"`    // 违禁词
	IsDeleted int    `json:"is_deleted"` // 是否删除
	Version   int64  `json:"version"`    // 版本
	CreatedAt string `json:"created_at"` // 时间
}
