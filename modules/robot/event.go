package robot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// getCreatorUID 带缓存地查询机器人的创建者UID
func (rb *Robot) getCreatorUID(robotID string) (string, error) {
	if v, ok := rb.creatorCache.Load(robotID); ok {
		return v.(string), nil
	}
	uid, err := rb.db.queryCreatorUID(robotID)
	if err != nil {
		return "", err
	}
	rb.creatorCache.Store(robotID, uid)
	return uid, nil
}

func (rb *Robot) existRobot(robotID string) (bool, error) {
	key := fmt.Sprintf("robot:exist:%s", robotID)
	exist, err := rb.ctx.GetRedisConn().GetString(key)
	if err != nil {
		return false, err
	}
	if exist == "1" {
		return true, nil
	}
	existB, err := rb.db.exist(robotID)
	if err != nil {
		return false, err
	}
	if existB {
		err = rb.ctx.GetRedisConn().SetAndExpire(key, "1", time.Hour*24)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil

}

func (rb *Robot) robotMessageListen(messages []*config.MessageResp) {
	for _, message := range messages {
		// Go 1.20 loopclosure: the goroutines spawned below close over `message`,
		// so bind a loop-local copy first. (Go 1.22 makes this implicit, but the
		// project still targets 1.20.)
		message := message
		payloadValue := gjson.ParseBytes(message.Payload)

		if !payloadValue.Exists() {
			continue
		}
		var robotID string
		var robotIDs []string

		if message.ChannelType == common.ChannelTypePerson.Uint8() {
			uid := common.GetToChannelIDWithFakeChannelID(message.ChannelID, message.FromUID)
			// Space channel_id 格式: s{spaceId}_{robotID}，提取真实 robotID
			realUID := uid
			if strings.HasPrefix(uid, "s") {
				if idx := strings.LastIndex(uid, "_"); idx >= 0 {
					realUID = uid[idx+1:]
				}
			}
			exist, err := rb.existRobot(realUID)
			if err != nil {
				rb.Error("查询有效robotID失败！", zap.Error(err))
				continue
			}
			rb.Debug("DM消息路由检测", zap.String("channelID", message.ChannelID), zap.String("fromUID", message.FromUID), zap.String("targetUID", uid), zap.String("realUID", realUID), zap.Bool("isRobot", exist))
			if exist {
				// BotFather 跳过好友关系校验
				if realUID != "botfather" {
					// 检查发送者是否为 Bot 创建者（使用缓存减少 DB 查询）
					creatorUID, err := rb.getCreatorUID(realUID)
					if err != nil {
						rb.Error("查询Bot创建者失败", zap.Error(err), zap.String("robotID", realUID))
						continue
					}
					// 创建者跳过好友关系校验
					if creatorUID != message.FromUID {
						isFriend, err := rb.userService.IsFriend(message.FromUID, realUID)
						if err != nil {
							rb.Error("查询好友关系失败", zap.Error(err), zap.String("fromUID", message.FromUID), zap.String("robotID", realUID))
							continue
						}
						// Space 场景：好友表可能使用原始 uid 格式(s{spaceId}_{robotID})，需同时检查
						if !isFriend && uid != realUID {
							isFriend, err = rb.userService.IsFriend(message.FromUID, uid)
							if err != nil {
								rb.Error("查询好友关系失败(Space格式)", zap.Error(err), zap.String("fromUID", message.FromUID), zap.String("uid", uid))
								continue
							}
						}
						if !isFriend {
							// 检查发送者是否为 bot 或系统账号，避免向 bot 发送系统提示导致消息循环
							isFromBot, _ := rb.existRobot(message.FromUID)
							if isFromBot || message.FromUID == rb.ctx.GetConfig().Account.SystemUID {
								rb.Warn("发送者为Bot或系统账号，跳过好友提示避免消息循环",
									zap.String("fromUID", message.FromUID), zap.String("robotID", realUID))
								continue
							}
							rb.Warn("用户与Bot非好友关系，拒绝转发消息", zap.String("fromUID", message.FromUID), zap.String("robotID", realUID))
							// YUJ-674 / Mininglamp-OSS#37: PERSONAL 走 NewPersonalMsgSendReq builder
							// (with bot's resolved SpaceID); GROUP / 其它 channel_type 保留直接构造。
							// "请先加好友"这条提示在 GROUP 路径下罕见，但保守保留旧语义。
							friendTipPayload := map[string]interface{}{
								"content": "请先添加好友后再与我对话",
								"type":    common.Text,
							}
							if message.ChannelType == common.ChannelTypePerson.Uint8() {
								rb.ctx.SendMessage(config.NewPersonalMsgSendReq(
									message.FromUID,
									realUID,
									friendTipPayload,
									rb.resolveBotActiveSpaceID(realUID),
									config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
								))
							} else {
								rb.ctx.SendMessage(&config.MsgSendReq{
									Header: config.MsgHeader{
										RedDot: 1,
									},
									FromUID:     realUID,
									ChannelID:   message.FromUID,
									ChannelType: message.ChannelType,
									Payload:     []byte(util.ToJson(friendTipPayload)),
								})
							}
							continue
						}
					}
				}
				robotID = realUID
			}

		}
		if len(robotID) == 0 {
			robotIDValue := payloadValue.Get("robot_id")
			if robotIDValue.Exists() {
				robotID = robotIDValue.String()
			} else if payloadValue.Get("mention").Exists() {
				rb.Debug("检测到@提及", zap.String("mention", payloadValue.Get("mention").String()))
				mentionValue := payloadValue.Get("mention")
				mentionUIDsValue := mentionValue.Get("uids")
				if mentionValue.Exists() && mentionUIDsValue.Exists() {
					uidsValues := mentionUIDsValue.Array()
					// 遍历所有被@的UID，找到其中的机器人
					for _, uidValue := range uidsValues {
						uid := uidValue.String()
						exist, err := rb.existRobot(uid)
						if err != nil {
							rb.Error("查询有效robotID失败！", zap.Error(err))
							continue
						}
						if exist {
							robotIDs = append(robotIDs, uid)
						}
					}
				}
				// YUJ-1393 / PR#82 review #2 R2 (Jerry-Xin 2026-05-19
				// follow-up): mention.ais=1 means "@所有 AI" (Plan X /
				// YUJ-1389). The robot event dispatcher previously only
				// considered robot_id / mention.uids / @username text,
				// so a payload carrying only mention.ais=1 (no uids,
				// no @username) silently skipped the robot event queue
				// and group bots never received the "@所有 AI" broadcast
				// — including the very common legacy `mention.all=1`
				// case, which the send-side rewrite chokepoints
				// (pkg/mentionrewrite/rewrite.go) normalize to also
				// carry `mention.ais=1`.
				//
				// Scope: GROUP channels only. PERSONAL DMs are already
				// dispatched via the realUID branch above and have no
				// notion of "all members of a channel".
				// COMMUNITY_TOPIC support is a deliberate follow-up —
				// it needs the parent-group lookup (see
				// modules/webhook/api.go parseThreadChannelID) and was
				// intentionally left out of this hotfix to keep the
				// change surface small.
				//
				// Dedup against any uid-matched robots already in
				// robotIDs so the goroutine fan-out below never
				// double-saves the same event for the same bot.
				if rb.mentionAisTruthy(mentionValue.Get("ais")) &&
					message.ChannelType == common.ChannelTypeGroup.Uint8() {
					groupRobotIDs, err := rb.collectGroupRobotIDs(message.ChannelID)
					if err != nil {
						rb.Error("查询群机器人成员失败！", zap.Error(err), zap.String("channelID", message.ChannelID))
					} else {
						robotIDs = appendUniqueRobotIDs(robotIDs, groupRobotIDs)
					}
				}
			} else {
				if common.ContentType(payloadValue.Get("type").Int()) == common.Text {
					content := payloadValue.Get("content").String()
					if strings.Contains(content, "@") {
						mentionUsernames := rb.mentionRegexp.FindAllString(content, -1)
						// 遍历所有@提及，找到其中的机器人
						for _, mentionUsername := range mentionUsernames {
							robotUsername := strings.TrimSpace(mentionUsername[1:])
							exist, err := rb.existRobot(robotUsername)
							if err != nil {
								rb.Error("查询有效robotID失败！", zap.Error(err))
								continue
							}
							if exist {
								robotIDs = append(robotIDs, robotUsername)
							}
						}
					}
				}
			}
		}
		if len(robotIDs) > 0 {
			for _, rid := range robotIDs {
				rb.Info("投递消息到机器人事件队列", zap.String("robotID", rid), zap.String("fromUID", message.FromUID), zap.Int64("messageID", message.MessageID))
				rid := rid // capture loop variable
				rb.msgSem <- struct{}{}
				go func() {
					defer func() {
						<-rb.msgSem
						if r := recover(); r != nil {
							rb.Error("panic in robot message goroutine", zap.Any("recover", r), zap.String("robotID", rid))
						}
					}()
					rb.saveRobotMessage(message, rid)
					rb.autoReadForBot(message, rid)
				}()
			}
		} else if len(robotID) > 0 {
			rb.Info("投递消息到机器人事件队列", zap.String("robotID", robotID), zap.String("fromUID", message.FromUID), zap.Int64("messageID", message.MessageID))
			rb.msgSem <- struct{}{}
			go func() {
				defer func() {
					<-rb.msgSem
					if r := recover(); r != nil {
						rb.Error("panic in robot message goroutine", zap.Any("recover", r), zap.String("robotID", robotID))
					}
				}()
				rb.saveRobotMessage(message, robotID)
				rb.autoReadForBot(message, robotID)
			}()
		}
	}
}

// autoReadForBot 为Bot自动标记消息已读
func (rb *Robot) autoReadForBot(message *config.MessageResp, robotID string) {
	channelID := message.ChannelID
	channelType := message.ChannelType
	if channelType == common.ChannelTypePerson.Uint8() {
		// DM场景：对Bot而言，会话的channelID是发送者的UID
		channelID = message.FromUID
	}
	err := rb.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         robotID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Unread:      0,
	})
	if err != nil {
		rb.Warn("Bot自动已读失败", zap.Error(err), zap.String("robotID", robotID), zap.String("channelID", channelID))
	}
}

func (rb *Robot) saveRobotMessage(message *config.MessageResp, robotID string) {

	seq, err := rb.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
	if err != nil {
		rb.Warn("GenSeq failed", zap.Error(err))
		return
	}
	messageUpdateJson := util.ToJson(&robotEvent{
		EventID: seq,
		Message: message,
		Expire:  time.Now().Add(rb.ctx.GetConfig().Robot.MessageExpire).Unix(),
	})
	key := fmt.Sprintf("%s%s", rb.robotEventPrefix, robotID)
	err = rb.ctx.GetRedisConn().ZAdd(key, float64(seq), messageUpdateJson)
	if err != nil {
		rb.Error("投递消息给机器人失败！", zap.Error(err), zap.String("robotID", robotID), zap.String("message", messageUpdateJson))
	}
	err = rb.ctx.GetRedisConn().Expire(key, rb.ctx.GetConfig().Robot.MessageExpire)
	if err != nil {
		rb.Warn("设置机器人消息过期时间失败！", zap.Error(err))
	}
}

func (rb *Robot) messagesListen(messages []*config.MessageResp) {
	for _, message := range messages {
		contentMap, err := util.JsonToMap(string(message.Payload))
		if err != nil {
			rb.Error("解析消息内容错误")
			continue
		}
		if contentMap != nil && contentMap["robot_id"] != nil {
			robotIDRaw := contentMap["robot_id"]
			robotID, ok := robotIDRaw.(string)
			if ok && robotID != "" {
				if robotID == config.New().Account.SystemUID {
					content, _ := contentMap["content"].(string)
					entities, _ := contentMap["entities"].([]interface{})
					var key string
					if entities != nil {
						var offset int64
						var length int64
						var offsetOK, lengthOK bool
						for _, entitiesObj := range entities {
							entitiesMap, ok := entitiesObj.(map[string]interface{})
							if !ok {
								continue
							}
							if entitiesMap["type"] == "bot_command" {
								// Safely extract offset
								if offsetVal, ok := entitiesMap["offset"].(json.Number); ok {
									offset, _ = offsetVal.Int64()
									offsetOK = true
								}
								// Safely extract length
								if lengthVal, ok := entitiesMap["length"].(json.Number); ok {
									length, _ = lengthVal.Int64()
									lengthOK = true
								}
								break
							}
						}
						contentRunes := []rune(content)
						contentLen := int64(len(contentRunes))
						// Validate bounds before slicing - require both offset and length to be valid
						if offsetOK && lengthOK && offset >= 0 && length > 0 && offset < contentLen && offset+length <= contentLen {
							key = string(contentRunes[offset : offset+length])
						}
					}

					channelID := message.ChannelID
					if message.ChannelType == common.ChannelTypePerson.Uint8() {
						channelID = message.FromUID
					}
					sendContent := ""
					for _, m := range systemRobotMap {
						if m.CMD == key {
							sendContent = m.ReplyContent
							break
						}
					}
					if sendContent == "" {
						sendContent = "抱歉，无法解析您发送的命令"
					}
					// YUJ-674 / Mininglamp-OSS#37: PERSONAL 走 NewPersonalMsgSendReq builder。
					sysReplyPayload := map[string]interface{}{
						"content": sendContent,
						"type":    common.Text,
					}
					if message.ChannelType == common.ChannelTypePerson.Uint8() {
						rb.ctx.SendMessage(config.NewPersonalMsgSendReq(
							channelID,
							robotID,
							sysReplyPayload,
							rb.resolveBotActiveSpaceID(robotID),
							config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
						))
					} else {
						rb.ctx.SendMessage(&config.MsgSendReq{
							Header: config.MsgHeader{
								RedDot: 1,
							},
							FromUID:     robotID,
							ChannelID:   channelID,
							ChannelType: message.ChannelType,
							Payload:     []byte(util.ToJson(sysReplyPayload)),
						})
					}
				}

			}
		}
	}
}
