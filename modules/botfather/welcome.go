package botfather

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

const (
	// welcomeSentKeyPrefix Redis key prefix for tracking welcome message sent status
	welcomeSentKeyPrefix = "botfather:welcome:sent:"
)

var (
	// welcomeSentTTL TTL for welcome sent flag (7 days)
	welcomeSentTTL = 7 * 24 * time.Hour
)

// handleUserRegisterEvent handles user registration event to send welcome message
func (bf *BotFather) handleUserRegisterEvent(data []byte, commit config.EventCommit) {
	// Parse event data
	var req map[string]interface{}
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		bf.Error("解析用户注册事件数据失败", zap.Error(err))
		commit(nil) // Don't block on parse error
		return
	}

	uid, ok := req["uid"].(string)
	if !ok || uid == "" {
		bf.Error("用户注册事件缺少uid")
		commit(nil)
		return
	}

	// Skip if it's a special system user
	if uid == BotFatherUID || uid == "u_10000" || uid == "fileHelper" {
		commit(nil)
		return
	}

	// Check if user already belongs to any Space.
	// If yes, SpaceMemberJoin event handler will send the welcome message instead.
	var spaceCount int
	_, err = bf.ctx.DB().SelectBySql("SELECT COUNT(*) FROM space_member WHERE uid=?", uid).Load(&spaceCount)
	if err != nil {
		bf.Warn("查询用户Space失败，降级发送bare-UID欢迎消息", zap.Error(err), zap.String("uid", uid))
		// Fall through to send bare-UID welcome as safety net
	} else if spaceCount > 0 {
		bf.Debug("用户已有Space，跳过bare-UID欢迎消息（由SpaceMemberJoin事件处理）", zap.String("uid", uid))
		commit(nil)
		return
	}

	// Safety net: user has no Spaces, send bare-UID welcome
	sentKey := fmt.Sprintf("%s%s", welcomeSentKeyPrefix, uid)
	sentValue, err := bf.ctx.GetRedisConn().GetString(sentKey)
	if err != nil && err.Error() != "redis: nil" {
		bf.Warn("检查欢迎消息发送状态失败", zap.Error(err), zap.String("uid", uid))
	}
	if sentValue != "" {
		bf.Debug("欢迎消息已发送，跳过", zap.String("uid", uid))
		commit(nil)
		return
	}

	err = bf.sendWelcomeMessage(uid, "")
	if err != nil {
		bf.Error("发送bare-UID欢迎消息失败", zap.Error(err), zap.String("uid", uid))
		commit(nil)
		return
	}

	err = bf.ctx.GetRedisConn().SetAndExpire(sentKey, "1", welcomeSentTTL)
	if err != nil {
		bf.Warn("标记欢迎消息已发送失败", zap.Error(err), zap.String("uid", uid))
	}

	bf.Info("bare-UID欢迎消息发送成功（用户无Space）", zap.String("uid", uid))
	commit(nil)
}

// sendWelcomeMessage sends a welcome message from BotFather to the new user
// Note: DM always uses bare UID as channelID (WuKongIM DM doesn't support Space prefix)
func (bf *BotFather) sendWelcomeMessage(toUID string, spaceID string) error {
	// Localize per recipient. This runs from a lifecycle event (no request ctx),
	// so language is resolved from the recipient's stored preference, falling
	// back to OCTO_DEFAULT_LANGUAGE — see recipientLanguage.
	lang := recipientLanguage(bf.cmdHandler.langSvc, toUID)
	welcomeContent, err := botMessages.Render(MsgWelcome, lang, nil)
	if err != nil {
		bf.Error("渲染欢迎消息失败", zap.String("lang", lang), zap.String("uid", toUID), zap.Error(err))
		return err
	}

	// DM must use bare UID — WuKongIM doesn't support Space-prefixed DM channel_id
	channelID := toUID

	// Send message via IM
	payload := map[string]interface{}{
		"type":    common.Text,
		"content": welcomeContent,
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
	_, err = bf.ctx.SendMessageWithResult(config.NewPersonalMsgSendReq(
		channelID,
		BotFatherUID,
		payload,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))

	return err
}

// handleSpaceMemberJoinEvent handles space member join event to send welcome message
func (bf *BotFather) handleSpaceMemberJoinEvent(data []byte, commit config.EventCommit) {
	var req map[string]interface{}
	err := util.ReadJsonByByte(data, &req)
	if err != nil {
		bf.Error("解析SpaceMemberJoin事件数据失败", zap.Error(err))
		commit(nil)
		return
	}

	uid, _ := req["uid"].(string)
	spaceID, _ := req["space_id"].(string)
	if uid == "" || spaceID == "" {
		bf.Error("SpaceMemberJoin事件缺少uid或space_id")
		commit(nil)
		return
	}

	// Skip system users
	if uid == BotFatherUID || uid == "u_10000" || uid == "fileHelper" {
		commit(nil)
		return
	}

	// Deduplicate with Redis — per uid+spaceID (each Space gets its own welcome)
	sentKey := fmt.Sprintf("botfather:welcome:sent:%s:%s", uid, spaceID)
	sentValue, err := bf.ctx.GetRedisConn().GetString(sentKey)
	if err != nil && err.Error() != "redis: nil" {
		bf.Warn("检查Space欢迎消息发送状态失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
	}
	if sentValue != "" {
		bf.Debug("Space欢迎消息已发送，跳过", zap.String("uid", uid), zap.String("spaceID", spaceID))
		commit(nil)
		return
	}

	err = bf.sendWelcomeMessage(uid, spaceID)
	if err != nil {
		bf.Error("发送Space欢迎消息失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
		commit(nil)
		return
	}

	// Mark as sent
	err = bf.ctx.GetRedisConn().SetAndExpire(sentKey, "1", welcomeSentTTL)
	if err != nil {
		bf.Warn("标记Space欢迎消息已发送失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
	}

	bf.Info("Space欢迎消息发送成功", zap.String("uid", uid), zap.String("spaceID", spaceID))
	commit(nil)
}
