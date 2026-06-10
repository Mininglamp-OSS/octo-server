package botfather

import (
	"fmt"
	"strings"
	"time"

	common "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	user "github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

const (
	// Redis key for pending bot friend applies: botfather:friend_apply:<robot_id>:<apply_uid>
	botFriendApplyPrefix = "botfather:friend_apply:"
	botFriendApplyTTL    = 7 * 24 * time.Hour // 7 days
	// Redis SET key to track all pending applies for a robot
	botFriendApplySetPrefix = "botfather:friend_apply_set:"
)

// BotFriendApply 机器人好友申请记录
type BotFriendApply struct {
	ApplyUID  string `json:"apply_uid"`
	ApplyName string `json:"apply_name"`
	RobotID   string `json:"robot_id"`
	Remark    string `json:"remark"`
	Token     string `json:"token"`    // friend apply token from cache
	SpaceID   string `json:"space_id"` // 申请来源 Space，用于隔离通知
	CreatedAt int64  `json:"created_at"`
}

// StoreBotFriendApply 存储机器人好友申请
func (h *commandHandler) StoreBotFriendApply(apply *BotFriendApply) error {
	key := fmt.Sprintf("%s%s:%s", botFriendApplyPrefix, apply.RobotID, apply.ApplyUID)
	setKey := fmt.Sprintf("%s%s", botFriendApplySetPrefix, apply.RobotID)

	apply.CreatedAt = time.Now().Unix()
	data := util.ToJson(apply)

	err := h.ctx.GetRedisConn().SetAndExpire(key, data, botFriendApplyTTL)
	if err != nil {
		return err
	}
	// Add to set for listing, refresh set TTL
	err = h.ctx.GetRedisConn().SAdd(setKey, apply.ApplyUID)
	if err != nil {
		return err
	}
	return h.ctx.GetRedisConn().Expire(setKey, botFriendApplyTTL)
}

// GetBotFriendApply 获取某个好友申请
func (h *commandHandler) GetBotFriendApply(robotID, applyUID string) (*BotFriendApply, error) {
	key := fmt.Sprintf("%s%s:%s", botFriendApplyPrefix, robotID, applyUID)
	data, err := h.ctx.GetRedisConn().GetString(key)
	if err != nil {
		return nil, err
	}
	if data == "" {
		return nil, nil
	}
	var apply BotFriendApply
	err = util.ReadJsonByByte([]byte(data), &apply)
	if err != nil {
		return nil, err
	}
	return &apply, nil
}

// DeleteBotFriendApply 删除好友申请记录
func (h *commandHandler) DeleteBotFriendApply(robotID, applyUID string) error {
	key := fmt.Sprintf("%s%s:%s", botFriendApplyPrefix, robotID, applyUID)
	setKey := fmt.Sprintf("%s%s", botFriendApplySetPrefix, robotID)
	err := h.ctx.GetRedisConn().Del(key)
	if err != nil {
		return err
	}
	return h.ctx.GetRedisConn().SRem(setKey, applyUID)
}

// GetPendingApplies 获取某个机器人的所有待审批好友申请
func (h *commandHandler) GetPendingApplies(robotID string) ([]*BotFriendApply, error) {
	setKey := fmt.Sprintf("%s%s", botFriendApplySetPrefix, robotID)
	members, err := h.ctx.GetRedisConn().SMembers(setKey)
	if err != nil {
		return nil, err
	}
	applies := make([]*BotFriendApply, 0)
	for _, applyUID := range members {
		apply, err := h.GetBotFriendApply(robotID, applyUID)
		if err != nil {
			h.Warn("查询好友申请失败", zap.String("robotID", robotID), zap.String("applyUID", applyUID), zap.Error(err))
			continue
		}
		if apply == nil {
			// expired, clean up set
			_ = h.ctx.GetRedisConn().SRem(setKey, applyUID)
			continue
		}
		applies = append(applies, apply)
	}
	return applies, nil
}

// GetAllPendingAppliesForOwner 获取某个 owner 所有 bot 的待审批好友申请
func (h *commandHandler) GetAllPendingAppliesForOwner(ownerUID string) ([]*BotFriendApply, error) {
	bots, err := h.queryBotsForUser(ownerUID)
	if err != nil {
		return nil, err
	}
	all := make([]*BotFriendApply, 0)
	for _, bot := range bots {
		applies, err := h.GetPendingApplies(bot.RobotID)
		if err != nil {
			continue
		}
		all = append(all, applies...)
	}
	return all, nil
}

// approveFriend 通过好友申请：建立双向好友关系 + 通知双方
func (h *commandHandler) approveFriend(ownerUID string, applyUID string, robotID string) {
	// 1. 验证 bot 归属
	bot, err := h.db.queryRobotByRobotID(robotID)
	if err != nil || bot == nil {
		h.replyL(ownerUID, MsgFriendBotNotFound, nil)
		return
	}
	if bot.CreatorUID != ownerUID {
		h.replyL(ownerUID, MsgFriendNotCreator, nil)
		return
	}
	// 1.5 Space 隔离校验：bot 必须属于当前 Space
	if currentSpace := h.resolveSpaceID(ownerUID); currentSpace != "" {
		inSpace, _ := h.db.isBotInSpace(robotID, currentSpace)
		if !inSpace {
			h.replyL(ownerUID, MsgFriendBotNotInSpace, nil)
			return
		}
	}

	// 2. 检查申请是否存在
	apply, err := h.GetBotFriendApply(robotID, applyUID)
	if err != nil || apply == nil {
		h.replyL(ownerUID, MsgFriendApplyNotFound, nil)
		return
	}

	// 3. 建立双向好友关系
	err = h.userService.AddFriend(applyUID, &user.FriendReq{
		UID:   applyUID,
		ToUID: robotID,
	})
	if err != nil {
		h.Warn("添加好友关系(user->bot)失败", zap.Error(err))
		h.replyL(ownerUID, MsgFriendAddFailed, nil)
		return
	}
	err = h.userService.AddFriend(robotID, &user.FriendReq{
		UID:   robotID,
		ToUID: applyUID,
	})
	if err != nil {
		h.Warn("添加好友关系(bot->user)失败", zap.Error(err))
		h.replyL(ownerUID, MsgFriendAddFailed, nil)
		return
	}

	// 获取 Space ID：优先从申请记录读取，其次从当前消息处理链获取
	applySpaceID := apply.SpaceID
	if applySpaceID == "" {
		applySpaceID = h.resolveSpaceID(ownerUID)
	}

	// 4. 添加 IM 白名单（双向），允许双方发消息
	userChannelID := applyUID
	botChannelID := robotID
	if applySpaceID != "" {
		userChannelID = fmt.Sprintf("s%s_%s", applySpaceID, applyUID)
		botChannelID = fmt.Sprintf("s%s_%s", applySpaceID, robotID)
	}
	err = h.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   userChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{robotID},
	})
	if err != nil {
		h.Warn("添加IM白名单(user channel)失败", zap.Error(err))
	}
	err = h.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   botChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{applyUID},
	})
	if err != nil {
		h.Warn("添加IM白名单(bot channel)失败", zap.Error(err))
	}

	// 5. 修复 friend version
	h.fixFriendVersion(applyUID, robotID)
	h.fixFriendVersion(robotID, applyUID)

	// 6. 更新好友申请记录状态
	applyRecord, _ := h.queryFriendApplyRecord(robotID, applyUID)
	if applyRecord != nil {
		_ = h.updateFriendApplyStatus(robotID, applyUID, 1) // 1=已通过
	}

	// 7. 通知双方：运营配置（Friend.AddedTipsText）优先，未配置时按收件人语言本地化
	content := h.ctx.GetConfig().Friend.AddedTipsText
	if content == "" {
		lang := recipientLanguage(h.langSvc, applyUID)
		if rendered, err := botMessages.Render(MsgFriendAddedTip, lang, nil); err != nil {
			h.Error("渲染好友提示失败", zap.String("lang", lang), zap.Error(err))
		} else {
			content = rendered
		}
	}

	// Send CMD to sync friend list on both clients
	cmdParam := map[string]interface{}{
		"to_uid":   applyUID,
		"from_uid": robotID,
	}
	if applySpaceID != "" {
		cmdParam["space_id"] = applySpaceID
	}
	_ = h.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{applyUID, robotID},
		Param:       cmdParam,
	})

	// Send tip message — skip when content is empty so a render failure (logged
	// above) doesn't push a blank Tip; the CMD above already synced both clients.
	if content != "" {
		tipPayload := map[string]interface{}{
			"content": content,
			"type":    common.Tip,
		}
		// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
		_ = h.ctx.SendMessage(config.NewPersonalMsgSendReq(
			applyUID,
			robotID,
			tipPayload,
			applySpaceID,
			config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
		))
	}

	// 8. 清理 Redis
	_ = h.DeleteBotFriendApply(robotID, applyUID)

	// 9. 回复 owner
	h.replyL(ownerUID, MsgFriendApproved, map[string]any{"ApplyName": apply.ApplyName, "RobotID": robotID})
}

// rejectFriend 拒绝好友申请
func (h *commandHandler) rejectFriend(ownerUID string, applyUID string, robotID string) {
	bot, err := h.db.queryRobotByRobotID(robotID)
	if err != nil || bot == nil {
		h.replyL(ownerUID, MsgFriendBotNotFound, nil)
		return
	}
	if bot.CreatorUID != ownerUID {
		h.replyL(ownerUID, MsgFriendNotCreator, nil)
		return
	}
	// Space 隔离校验：bot 必须属于当前 Space
	if currentSpace := h.resolveSpaceID(ownerUID); currentSpace != "" {
		inSpace, _ := h.db.isBotInSpace(robotID, currentSpace)
		if !inSpace {
			h.replyL(ownerUID, MsgFriendBotNotInSpace, nil)
			return
		}
	}

	apply, err := h.GetBotFriendApply(robotID, applyUID)
	if err != nil || apply == nil {
		h.replyL(ownerUID, MsgFriendApplyNotFound, nil)
		return
	}

	// 更新状态
	_ = h.updateFriendApplyStatus(robotID, applyUID, 2) // 2=已拒绝

	// 清理
	_ = h.DeleteBotFriendApply(robotID, applyUID)

	h.replyL(ownerUID, MsgFriendRejected, map[string]any{"ApplyName": apply.ApplyName, "RobotID": robotID})
}

// queryFriendApplyRecord 查询好友申请记录
func (h *commandHandler) queryFriendApplyRecord(robotID, applyUID string) (map[string]interface{}, error) {
	var count int
	err := h.db.session.Select("count(*)").From("friend_apply_record").
		Where("uid=? and to_uid=?", robotID, applyUID).LoadOne(&count)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return map[string]interface{}{"exists": true}, nil
}

// updateFriendApplyStatus 更新好友申请状态
func (h *commandHandler) updateFriendApplyStatus(robotID, applyUID string, status int) error {
	_, err := h.db.session.Update("friend_apply_record").
		Set("status", status).
		Where("uid=? and to_uid=?", robotID, applyUID).Exec()
	return err
}

// RegisterFriendApplyHook 注册好友申请通知回调到 user 模块
func RegisterFriendApplyHook(ctx *config.Context) {
	user.RegisterBotFriendApplyHook(func(applyUID, applyName, robotID, remark, token, spaceID string) {
		notifyOwnerFriendApply(ctx, applyUID, applyName, robotID, remark, token, spaceID)
	})
}

// notifyOwnerFriendApply 通知 bot owner 有好友申请
func notifyOwnerFriendApply(ctx *config.Context, applyUID, applyName, robotID, remark, token, spaceID string) {
	handler := newCommandHandler(ctx)
	l := log.NewTLog("BotFather")

	// 查询 bot
	bot, err := handler.db.queryRobotByRobotID(robotID)
	if err != nil || bot == nil {
		l.Warn("查询机器人失败", zap.String("robotID", robotID), zap.Error(err))
		return
	}

	// 存储申请（含 spaceID，approve/reject 时读取）
	apply := &BotFriendApply{
		ApplyUID:  applyUID,
		ApplyName: applyName,
		RobotID:   robotID,
		Remark:    remark,
		Token:     token,
		SpaceID:   spaceID,
	}
	err = handler.StoreBotFriendApply(apply)
	if err != nil {
		l.Warn("存储好友申请失败", zap.Error(err))
		return
	}

	// 设置 Space 上下文后调用 replyL，确保通知发送到正确的 Space
	// 此函数从 hook 回调调用（非消息处理链），sync.Map 中无 owner 的 Space 信息
	if spaceID != "" {
		cleanup := setSpaceIDFromPayload(bot.CreatorUID, spaceID)
		defer cleanup()
	}
	// Remark 直接传入；模板在非空时渲染本地化的「备注」行
	handler.replyL(bot.CreatorUID, MsgFriendApplyNotify, map[string]any{
		"ApplyName": applyName,
		"ApplyUID":  applyUID,
		"RobotID":   robotID,
		"Remark":    remark,
	})
}

// handlePending 处理 /pending 命令
func (h *commandHandler) handlePending(fromUID string) {
	applies, err := h.GetAllPendingAppliesForOwner(fromUID)
	if err != nil {
		h.replyL(fromUID, MsgFriendQueryFailed, nil)
		return
	}
	if len(applies) == 0 {
		h.replyL(fromUID, MsgFriendPendingEmpty, nil)
		return
	}

	lang := recipientLanguage(h.langSvc, fromUID)
	items := make([]pendingApplyItem, 0, len(applies))
	for i, apply := range applies {
		items = append(items, pendingApplyItem{
			Num:       i + 1,
			ApplyName: apply.ApplyName,
			ApplyUID:  apply.ApplyUID,
			RobotID:   apply.RobotID,
			Ago:       relativeAgo(lang, apply.CreatedAt),
			Remark:    apply.Remark,
		})
	}
	content, err := botMessages.Render(MsgFriendPendingList, lang, map[string]any{
		"Count": len(applies),
		"Items": items,
	})
	if err != nil {
		h.Error("渲染待审批好友申请列表失败", zap.String("lang", lang), zap.Error(err))
		return
	}
	h.reply(fromUID, content)
}

// relativeAgo renders a localized relative-time phrase ("just now", "3 days
// ago", ...) for a past unix-second timestamp. Plural handling lives in the
// per-language ago templates, so this stays a thin dispatcher. A render error
// (structurally impossible given the startup completeness matrix) is logged at
// Debug and yields an empty phrase rather than failing the whole list.
func relativeAgo(lang string, createdAt int64) string {
	ago := time.Since(time.Unix(createdAt, 0)).Truncate(time.Minute)
	key := MsgFriendAgoJustNow
	var data map[string]any
	switch {
	case ago >= time.Hour*24:
		key, data = MsgFriendAgoDays, map[string]any{"N": int(ago.Hours() / 24)}
	case ago >= time.Hour:
		key, data = MsgFriendAgoHours, map[string]any{"N": int(ago.Hours())}
	case ago >= time.Minute:
		key, data = MsgFriendAgoMinutes, map[string]any{"N": int(ago.Minutes())}
	}
	s, err := botMessages.Render(key, lang, data)
	if err != nil {
		msgLog.Debug("渲染相对时间短语失败",
			zap.String("key", key), zap.String("lang", lang), zap.Error(err))
	}
	return s
}

// handleApprove 处理 /approve 命令
func (h *commandHandler) handleApprove(fromUID string, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		h.replyL(fromUID, MsgFriendApproveUsage, nil)
		return
	}

	targetUID := parts[0]
	robotID := parts[1]

	if targetUID == "all" {
		// 批量通过
		applies, err := h.GetPendingApplies(robotID)
		if err != nil || len(applies) == 0 {
			h.replyL(fromUID, MsgFriendNoPendingForBot, map[string]any{"RobotID": robotID})
			return
		}
		for _, apply := range applies {
			h.approveFriend(fromUID, apply.ApplyUID, robotID)
		}
		return
	}

	h.approveFriend(fromUID, targetUID, robotID)
}

// handleReject 处理 /reject 命令
func (h *commandHandler) handleReject(fromUID string, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		h.replyL(fromUID, MsgFriendRejectUsage, nil)
		return
	}

	targetUID := parts[0]
	robotID := parts[1]

	if targetUID == "all" {
		applies, err := h.GetPendingApplies(robotID)
		if err != nil || len(applies) == 0 {
			h.replyL(fromUID, MsgFriendNoPendingForBot, map[string]any{"RobotID": robotID})
			return
		}
		for _, apply := range applies {
			h.rejectFriend(fromUID, apply.ApplyUID, robotID)
		}
		return
	}

	h.rejectFriend(fromUID, targetUID, robotID)
}
