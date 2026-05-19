package bot_api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// BotTypingReq is the request for typing.
type BotTypingReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// typing handles POST /v1/bot/typing.
func (ba *BotAPI) typing(c *wkhttp.Context) {
	var req BotTypingReq
	if err := c.BindJSON(&req); err != nil {
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

	robotID := getRobotIDFromContext(c)
	channelID := ba.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)

	// Permission check: bot must have access to this channel.
	// PR#82 R7 — typing has no `on_behalf_of` field and always
	// dispatches AS the bot (CMDTyping carries from_uid=robotID below),
	// so the OBO friend-gate bypass MUST NOT apply here
	// (hasOBOContext=false). Allowing it would let a bot that holds an
	// unrelated grant signal typing in a DM with a user that has not
	// opted in to that bot.
	botKind := getBotKindFromContext(c)
	if err := ba.checkSendPermission(c, botKind, robotID, req.ChannelID, req.ChannelType, false); err != nil {
		c.ResponseError(err)
		return
	}

	// Typing throttle: allow forwarding within 90s window, then suppress.
	// Design: bots get max 3 consecutive 90s typing windows per 180s TTL cycle.
	// After 180s both keys expire and bot can start fresh windows — this is intentional
	// (prevents rapid bursts, not indefinite blocking). Matches original botfather behavior.
	typingStartKey := fmt.Sprintf("typing_start:%s:%s:%d", robotID, channelID, req.ChannelType)
	typingCountKey := fmt.Sprintf("typing_count:%s:%s:%d", robotID, channelID, req.ChannelType)
	const typingMaxDuration = 90
	const typingMaxWindows = 3
	const typingKeyTTL = 180

	startStr, _ := ba.ctx.GetRedisConn().GetString(typingStartKey)
	now := time.Now().Unix()
	if startStr != "" {
		startTime, _ := strconv.ParseInt(startStr, 10, 64)
		if now-startTime > int64(typingMaxDuration) {
			c.ResponseOK()
			return
		}
	} else {
		countStr, _ := ba.ctx.GetRedisConn().GetString(typingCountKey)
		if countStr != "" {
			count, _ := strconv.ParseInt(countStr, 10, 64)
			if count >= int64(typingMaxWindows) {
				c.ResponseOK()
				return
			}
		}
		ba.ctx.GetRedisConn().SetAndExpire(typingStartKey, fmt.Sprintf("%d", now), time.Duration(typingKeyTTL)*time.Second)
		ba.ctx.GetRedisConn().Incr(typingCountKey)
		// Unconditional SetExpire: if a prior crash left typingCountKey without TTL, this fixes it.
		// Worst case: TTL refreshed on new window creation (acceptable for typing throttle).
		ba.ctx.GetRedisConn().SetExpire(typingCountKey, time.Duration(typingKeyTTL)*time.Second)
	}

	paramChannelID := channelID
	if req.ChannelType == uint8(common.ChannelTypePerson) {
		paramChannelID = robotID
	}
	err := ba.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		CMD:         common.CMDTyping,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		Param: map[string]interface{}{
			"from_uid":     robotID,
			"channel_id":   paramChannelID,
			"channel_type": req.ChannelType,
		},
	})
	if err != nil {
		ba.Error("发送typing失败", zap.Error(err))
		c.ResponseError(errors.New("发送typing失败"))
		return
	}
	c.ResponseOK()
}

// heartbeat handles POST /v1/bot/heartbeat.
func (ba *BotAPI) heartbeat(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	key := fmt.Sprintf("%s%s", heartbeatKeyPrefix, robotID)
	err := ba.ctx.GetRedisConn().SetAndExpire(key, "1", time.Second*heartbeatTTL)
	if err != nil {
		ba.Error("设置心跳失败", zap.Error(err))
		c.ResponseError(errors.New("设置心跳失败"))
		return
	}
	c.ResponseOK()
}
