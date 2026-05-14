package bot_api

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// BotSendMessageReq is the request for sendMessage.
type BotSendMessageReq struct {
	ChannelID   string                 `json:"channel_id"`
	ChannelType uint8                  `json:"channel_type"`
	StreamNo    string                 `json:"stream_no"`
	Payload     map[string]interface{} `json:"payload"`
}

// sendMessage handles POST /v1/bot/sendMessage.
func (ba *BotAPI) sendMessage(c *wkhttp.Context) {
	var req BotSendMessageReq
	if err := c.BindJSON(&req); err != nil {
		ba.Error("数据格式有误", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}
	if req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空"))
		return
	}
	if len(req.Payload) == 0 {
		c.ResponseError(errors.New("payload不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	botKind := getBotKindFromContext(c)

	// Permission check based on bot kind
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, req.ChannelType); err != nil {
		c.ResponseError(err)
		return
	}

	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)

	// YUJ-644 / Mininglamp-OSS#33: PERSONAL DM 服务端权威 space_id 注入。
	// WuKongIM 对 DM 仅按裸 uid 路由（无 Space 概念），收端 SpaceFilter 只能依赖
	// payload.space_id；客户端上送任何值（包括缺省 / 伪造）都不可信。
	// 优先使用 gin-context 里 authAppBot 写入的 SpaceID（O(1)，无 DB 调用）；
	// 用户 Bot / 平台级 App Bot 落 querySpaceIDByRobotID。
	payload := req.Payload
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		payload = ba.enrichBotPayloadWithSpaceID(c, robotID, payload)
	}

	msgReq := &config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		StreamNo:    req.StreamNo,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		FromUID:     robotID,
		Payload:     []byte(util.ToJson(payload)),
	}
	result, err := ba.dispatchMsgSendReq(msgReq)
	if err != nil {
		ba.Error("发送消息失败", zap.Error(err))
		c.ResponseError(errors.New("发送消息失败"))
		return
	}

	// Reset typing throttle state
	ba.clearTypingThrottle(robotID, channelID, req.ChannelType)

	c.Response(result)
}

// checkSendPermission verifies the bot has permission to send to the target channel.
func (ba *BotAPI) checkSendPermission(c *wkhttp.Context, botKind, robotID, channelID string, channelType uint8) error {
	switch botKind {
	case BotKindApp:
		// Rule 1: App Bot only supports DM
		if channelType != common.ChannelTypePerson.Uint8() {
			return errors.New("app bot only supports direct messages")
		}
		// Rule 2: Must have friend relationship (user opt-in via /v1/robot/apply)
		isFriend, err := ba.userService.IsFriend(robotID, channelID)
		if err != nil {
			return errors.New("failed to verify relationship")
		}
		if !isFriend {
			return errors.New("user has not started conversation with this bot")
		}
		// Rule 3: Space bot — user must still be a space member (fail-closed)
		if scope, _ := c.Get(CtxKeyAppBotScope); scope == "space" {
			spaceIDStr, _ := c.Get(CtxKeyAppBotSpaceID)
			sid, _ := spaceIDStr.(string)
			if sid == "" {
				return errors.New("internal error: space bot missing space_id")
			}
			isMember, memberErr := ba.isSpaceMember(channelID, sid)
			if memberErr != nil {
				return errors.New("failed to verify space membership")
			}
			if !isMember {
				return errors.New("user is no longer a member of bot's space")
			}
		}
		return nil

	case BotKindUser:
		if channelType == common.ChannelTypeGroup.Uint8() {
			// Group: check bot is a group member
			var count int
			err := ba.db.session.SelectBySql(
				"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
				channelID, robotID,
			).LoadOne(&count)
			if err != nil {
				ba.Error("查询群成员失败", zap.Error(err))
				return errors.New("查询群成员失败")
			}
			if count == 0 {
				return errors.New("bot is not a member of this group")
			}
		} else if channelType == common.ChannelTypeCommunityTopic.Uint8() {
			// Thread: extract parent group_no and verify membership
			parts := strings.SplitN(channelID, threadChannelIDSeparator, 2)
			if len(parts) != 2 {
				return errors.New("invalid thread channel_id format")
			}
			var count int
			err := ba.db.session.SelectBySql(
				"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
				parts[0], robotID,
			).LoadOne(&count)
			if err != nil {
				ba.Error("查询群成员失败", zap.Error(err))
				return errors.New("查询群成员失败")
			}
			if count == 0 {
				return errors.New("bot is not a member of this group")
			}
		} else if channelType == common.ChannelTypePerson.Uint8() {
			// DM: creator can always talk to their bot; otherwise check friend
			robot := getRobotFromContext(c)
			isCreator := robot != nil && robot.CreatorUID == channelID
			if !isCreator {
				isFriend, err := ba.userService.IsFriend(robotID, channelID)
				if err != nil {
					ba.Error("查询好友关系失败", zap.Error(err))
					return errors.New("查询好友关系失败")
				}
				if !isFriend {
					return errors.New("bot is not a friend of this user")
				}
			}
		}
		return nil

	default:
		return errors.New("unknown bot kind")
	}
}

// isSpaceMember checks if a user is a member of the given space.
func (ba *BotAPI) isSpaceMember(uid, spaceID string) (bool, error) {
	var count int
	err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, uid,
	).LoadOne(&count)
	if err != nil {
		ba.Error("isSpaceMember query failed", zap.String("uid", uid), zap.String("spaceID", spaceID), zap.Error(err))
		return false, err
	}
	return count > 0, nil
}

// ==================== Read Receipt ====================

// BotReadReceiptReq is the request for readReceipt.
type BotReadReceiptReq struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	MessageIDs  []string `json:"message_ids"`
}

// readReceipt handles POST /v1/bot/readReceipt.
func (ba *BotAPI) readReceipt(c *wkhttp.Context) {
	var req BotReadReceiptReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelType := uint8(common.ChannelTypePerson)
	if req.ChannelType > 0 {
		channelType = req.ChannelType
	}

	// Permission check: bot must have access to this channel
	botKind := getBotKindFromContext(c)
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, channelType); err != nil {
		c.ResponseError(err)
		return
	}

	// If channel_type was defaulted (0) for an App Bot, verify the channel_id is
	// not actually a group — otherwise callers could bypass the DM-only restriction.
	if req.ChannelType == 0 && botKind == BotKindApp {
		var groupCount int
		grpErr := ba.db.session.SelectBySql(
			"SELECT COUNT(*) FROM `group` WHERE group_no=? AND is_deleted=0", req.ChannelID,
		).LoadOne(&groupCount)
		if grpErr != nil {
			c.ResponseError(errors.New("验证频道类型失败"))
			return
		}
		if groupCount > 0 {
			c.ResponseError(errors.New("app bot can only access direct message channels"))
			return
		}
	}

	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, channelType)

	// 1. Clear conversation unread badge
	err := ba.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         robotID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Unread:      0,
	})
	if err != nil {
		ba.Warn("清除未读计数失败", zap.Error(err))
	}

	// 2. Message-level read receipt
	if len(req.MessageIDs) > 100 {
		c.ResponseError(errors.New("message_ids exceeds maximum of 100"))
		return
	}
	if len(req.MessageIDs) > 0 {
		messageIDs := make([]int64, 0, len(req.MessageIDs))
		for _, idStr := range req.MessageIDs {
			mid, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				ba.Warn("解析消息ID失败", zap.String("id", idStr), zap.Error(parseErr))
				continue
			}
			messageIDs = append(messageIDs, mid)
		}
		if len(messageIDs) == 0 {
			c.ResponseOK()
			return
		}

		fakeChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			fakeChannelID = common.GetFakeChannelIDWith(channelID, robotID)
		}

		searchChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			searchChannelID = robotID
		}
		syncMsg, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   searchChannelID,
			ChannelType: channelType,
			MessageIds:  messageIDs,
			LoginUID:    robotID,
		})
		if err != nil {
			ba.Error("查询消息失败", zap.Error(err))
			c.ResponseError(errors.New("查询消息失败"))
			return
		}
		if syncMsg != nil && len(syncMsg.Messages) > 0 {
			valueStrings := make([]string, 0, len(syncMsg.Messages))
			valueArgs := make([]interface{}, 0, len(syncMsg.Messages)*4)
			for _, msg := range syncMsg.Messages {
				valueStrings = append(valueStrings, "(?, ?, ?, ?)")
				valueArgs = append(valueArgs, msg.MessageID, fakeChannelID, channelType, robotID)
			}
			stmt := fmt.Sprintf(`INSERT INTO member_readed (message_id, channel_id, channel_type, uid) VALUES %s ON DUPLICATE KEY UPDATE message_id=VALUES(message_id)`,
				strings.Join(valueStrings, ","))
			_, err = ba.db.session.InsertBySql(stmt, valueArgs...).Exec()
			if err != nil {
				ba.Error("插入已读记录失败", zap.Error(err))
				c.ResponseError(errors.New("保存已读记录失败"))
				return
			}

			// Write Redis cache for read receipt aggregation
			go func() {
				defer func() {
					if r := recover(); r != nil {
						ba.Error("goroutine panic",
							zap.Any("recover", r),
							zap.String("stack", string(debug.Stack())),
						)
					}
				}()
				for _, msg := range syncMsg.Messages {
					messageIDStr := strconv.FormatInt(msg.MessageID, 10)
					cacheData := map[string]interface{}{
						"MessageID":      msg.MessageID,
						"MessageIDStr":   messageIDStr,
						"MessageSeq":     msg.MessageSeq,
						"FromUID":        msg.FromUID,
						"ChannelID":      fakeChannelID,
						"ChannelType":    channelType,
						"LoginUID":       robotID,
						"ReqChannelID":   channelID,
						"ReqChannelType": channelType,
					}
					jsonStr, _ := json.Marshal(cacheData)
					ba.ctx.GetRedisConn().SetAndExpire(
						fmt.Sprintf("readedCount:%s", messageIDStr),
						string(jsonStr),
						time.Hour*24*7,
					)
				}
			}()
		}
	}

	c.ResponseOK()
}

// ==================== Message Edit ====================

// botMessageEdit handles POST /v1/bot/message/edit.
func (ba *BotAPI) botMessageEdit(c *wkhttp.Context) {
	var req struct {
		MessageID   string `json:"message_id"`
		MessageSeq  uint32 `json:"message_seq"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		ContentEdit string `json:"content_edit"`
	}
	if err := c.BindJSON(&req); err != nil {
		ba.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if req.MessageID == "" {
		c.ResponseError(errors.New("message_id 不能为空"))
		return
	}
	if req.ChannelID == "" {
		c.ResponseError(errors.New("channel_id 不能为空"))
		return
	}
	if strings.TrimSpace(req.ContentEdit) == "" {
		c.ResponseError(errors.New("content_edit 不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id 不能为空"))
		return
	}

	// Permission: bot can only edit its own messages
	var msgFromUID string
	if req.MessageSeq > 0 {
		resp, err := ba.ctx.IMGetWithChannelAndSeqs(req.ChannelID, req.ChannelType, robotID, []uint32{req.MessageSeq})
		if err != nil {
			ba.Error("查询消息错误", zap.Error(err))
			c.ResponseError(errors.New("查询消息错误"))
			return
		}
		if resp == nil || len(resp.Messages) == 0 {
			c.ResponseError(errors.New("消息不存在"))
			return
		}
		if req.MessageID != strconv.FormatInt(resp.Messages[0].MessageID, 10) {
			ba.Warn("message_id与message_seq不匹配，保持旧行为继续执行",
				zap.String("req_message_id", req.MessageID),
				zap.Int64("actual_message_id", resp.Messages[0].MessageID),
				zap.Uint32("message_seq", req.MessageSeq),
			)
		}
		msgFromUID = resp.Messages[0].FromUID
	} else {
		msgIDInt, parseErr := strconv.ParseInt(req.MessageID, 10, 64)
		if parseErr != nil {
			ba.Error("message_id格式错误", zap.String("message_id", req.MessageID), zap.Error(parseErr))
			c.ResponseError(errors.New("message_id格式错误"))
			return
		}
		syncResp, err := ba.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   req.ChannelID,
			ChannelType: req.ChannelType,
			MessageIds:  []int64{msgIDInt},
			LoginUID:    robotID,
		})
		if err != nil {
			ba.Error("查询消息错误", zap.Error(err))
			c.ResponseError(errors.New("查询消息错误"))
			return
		}
		if syncResp == nil || len(syncResp.Messages) == 0 {
			c.ResponseError(errors.New("消息不存在"))
			return
		}
		if syncResp.Messages[0].MessageSeq == 0 {
			c.ResponseError(errors.New("消息尚未投递完成，请稍后重试"))
			return
		}
		msgFromUID = syncResp.Messages[0].FromUID
		req.MessageSeq = syncResp.Messages[0].MessageSeq
	}
	if msgFromUID != robotID {
		c.ResponseError(errors.New("只能编辑自己发送的消息"))
		return
	}

	// App Bot: DM-only + must have friend relationship
	botKind := getBotKindFromContext(c)
	if botKind == BotKindApp {
		if req.ChannelType != common.ChannelTypePerson.Uint8() {
			c.ResponseError(errors.New("app bot can only edit direct messages"))
			return
		}
		isFriend, fErr := ba.userService.IsFriend(robotID, req.ChannelID)
		if fErr != nil || !isFriend {
			c.ResponseError(errors.New("user has not started conversation with this bot"))
			return
		}
	}

	contentEdit := dbr.NewNullString(req.ContentEdit).String
	contentMD5 := util.MD5(contentEdit)

	var existCount int
	err := ba.ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit_hash=?", req.MessageID, contentMD5).LoadOne(&existCount)
	if err != nil {
		ba.Error("查询是否存在相同正文失败！", zap.Error(err))
		c.ResponseError(errors.New("查询是否存在相同正文失败！"))
		return
	}
	if existCount > 0 {
		c.ResponseOK()
		return
	}

	fakeChannelID := req.ChannelID
	if req.ChannelType == common.ChannelTypePerson.Uint8() {
		fakeChannelID = common.GetFakeChannelIDWith(robotID, req.ChannelID)
	}

	version, err := ba.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		ba.Error("生成消息扩展序列号失败！", zap.Error(err))
		c.ResponseError(errors.New("生成消息扩展序列号失败！"))
		return
	}

	_, err = ba.ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version)",
		req.MessageID, req.MessageSeq, fakeChannelID, req.ChannelType, contentEdit, contentMD5, int(time.Now().Unix()), version,
	).Exec()
	if err != nil {
		ba.Error("添加或修改编辑内容失败！", zap.Error(err))
		c.ResponseError(errors.New("添加或修改编辑内容失败！"))
		return
	}

	err = ba.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		FromUID:     robotID,
		CMD:         common.CMDSyncMessageExtra,
	})
	if err != nil {
		ba.Error("发送 CMD 同步失败！", zap.Error(err))
	}

	c.ResponseOK()
}
