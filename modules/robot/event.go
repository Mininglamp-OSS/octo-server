package robot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/common"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/config"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/util"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

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
		payloadValue := gjson.ParseBytes(message.Payload)

		if !payloadValue.Exists() {
			continue
		}
		var robotID string
		var robotIDs []string

		if message.ChannelType == common.ChannelTypePerson.Uint8() {
			uid := common.GetToChannelIDWithFakeChannelID(message.ChannelID, message.FromUID)
			exist, err := rb.existRobot(uid)
			if err != nil {
				rb.Error("查询有效robotID失败！", zap.Error(err))
				continue
			}
			rb.Debug("DM消息路由检测", zap.String("channelID", message.ChannelID), zap.String("fromUID", message.FromUID), zap.String("targetUID", uid), zap.Bool("isRobot", exist))
			if exist {
				robotID = uid
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
								robotID = robotUsername
								break
							}
						}
					}
				}
			}
		}
		if len(robotIDs) > 0 {
			for _, rid := range robotIDs {
				rb.Info("投递消息到机器人事件队列", zap.String("robotID", rid), zap.String("fromUID", message.FromUID), zap.Int64("messageID", message.MessageID))
				go rb.saveRobotMessage(message, rid)
				go rb.autoReadForBot(message, rid)
			}
		} else if len(robotID) > 0 {
			rb.Info("投递消息到机器人事件队列", zap.String("robotID", robotID), zap.String("fromUID", message.FromUID), zap.Int64("messageID", message.MessageID))
			go rb.saveRobotMessage(message, robotID)
			go rb.autoReadForBot(message, robotID)
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

	seq := rb.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))
	messageUpdateJson := util.ToJson(&robotEvent{
		EventID: seq,
		Message: message,
		Expire:  time.Now().Add(rb.ctx.GetConfig().Robot.MessageExpire).Unix(),
	})
	key := fmt.Sprintf("%s%s", rb.robotEventPrefix, robotID)
	err := rb.ctx.GetRedisConn().ZAdd(key, float64(seq), messageUpdateJson)
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
			robotID := contentMap["robot_id"]
			if robotID != nil {
				if robotID == config.New().Account.SystemUID {
					content, _ := contentMap["content"].(string)
					entities := contentMap["entities"].([]interface{})
					var key string
					if entities != nil {
						var offset int64
						var length int64
						for _, entitiesObj := range entities {
							entitiesMap := entitiesObj.(map[string]interface{})
							if entitiesMap["type"] == "bot_command" {
								offset, _ = entitiesMap["offset"].(json.Number).Int64()
								length, _ = entitiesMap["length"].(json.Number).Int64()
								break
							}
						}
						contentRuns := []rune(content)
						key = string(contentRuns[offset:length])
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
					rb.ctx.SendMessage(&config.MsgSendReq{
						Header: config.MsgHeader{
							RedDot: 1,
						},
						FromUID:     robotID.(string),
						ChannelID:   channelID,
						ChannelType: message.ChannelType,
						Payload: []byte(util.ToJson(map[string]interface{}{
							"content": sendContent,
							"type":    common.Text,
						})),
					})
				}

			}
		}
	}
}
