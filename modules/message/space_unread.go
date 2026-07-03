package message

import (
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// fillPersonSpaceUnread 为 Person 频道计算 per-Space 未读计数，
// 并填充 SpaceLastMessage（该 Space 的最后一条消息预览）。
// 仅处理 channelType=1 的会话。
// 通过解析消息 payload 中的 space_id 字段来统计属于指定 Space 的未读消息数。
//
// defaultSpaceID 用于「无标签 = 默认 Space」归属（见 dmSpaceMatch）：DM 在默认
// Space 发送的消息不带 payload.space_id，若严格等值匹配就永远命中不到 —— 会导致
// 默认 Space 的 space_last_message 取不到、per-Space 未读漏计，与可见性兜底
// (decideConvKeepInSpace) / 历史过滤 (filterPersonMessagesBySpace) 口径不一致。
func fillPersonSpaceUnread(
	conversations []*SyncUserConversationResp,
	rawConversations []*config.SyncUserConversationResp,
	spaceID string,
	defaultSpaceID string,
	loginUID string,
	ctx *config.Context,
) {
	if spaceID == "" || len(conversations) == 0 {
		return
	}

	// channelID -> raw conversation
	rawMap := make(map[string]*config.SyncUserConversationResp, len(rawConversations))
	for _, raw := range rawConversations {
		rawMap[raw.ChannelID] = raw
	}

	for _, conv := range conversations {
		if conv.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}

		// 系统 Bot 的无标签历史在 filterPersonMessagesBySpace rule 4 里被隐藏，
		// 预览/未读须同口径（见 dmSpaceMatch）。按频道身份判定一次即可。
		isSysBot := spacepkg.IsSystemBot(conv.ChannelID)

		// 从 Recents 中找该 Space 的最后一条消息作为预览
		spaceLastMsg := findSpaceLastMessage(conv.Recents, spaceID, defaultSpaceID, isSysBot)
		if spaceLastMsg != nil {
			conv.SpaceLastMessage = spaceLastMsg
		}

		// Recents 中未找到匹配消息，从 WuKongIM 拉取更多历史消息查找。
		// 典型场景：BotFather 等全局单例 Bot，用户在 Space B 发了大量消息后，
		// Space A 的最后一条消息已被挤出 Recents 窗口。
		if conv.SpaceLastMessage == nil && ctx != nil {
			raw := rawMap[conv.ChannelID]
			if raw != nil && raw.LastMsgSeq > 0 {
				fallbackMsg := findSpaceLastMessageFallback(
					conv.ChannelID, conv.ChannelType,
					loginUID, spaceID, defaultSpaceID, isSysBot, uint32(raw.LastMsgSeq), ctx,
				)
				if fallbackMsg != nil {
					conv.SpaceLastMessage = fallbackMsg
				}
			}
		}

		// 未读计数仅在 unread > 0 时处理
		if conv.Unread <= 0 {
			continue
		}

		raw := rawMap[conv.ChannelID]
		if raw == nil {
			continue
		}

		readSeq := raw.LastMsgSeq - int64(raw.Unread)

		var messages []*config.MessageResp

		if len(raw.Recents) >= raw.Unread {
			// Recents 覆盖了所有未读消息，直接使用
			messages = raw.Recents
		} else {
			// Recents 不足，从 WuKongIM 拉取未读消息
			startSeq := readSeq + 1
			if startSeq < 1 {
				startSeq = 1
			}
			resp, err := ctx.IMSyncChannelMessage(config.SyncChannelMessageReq{
				LoginUID:        loginUID,
				ChannelID:       conv.ChannelID,
				ChannelType:     conv.ChannelType,
				StartMessageSeq: uint32(startSeq),
				EndMessageSeq:   uint32(raw.LastMsgSeq),
				Limit:           raw.Unread,
				PullMode:        config.PullModeDown,
			})
			if err != nil {
				log.Warn("获取Person未读消息失败，跳过space_unread",
					zap.Error(err),
					zap.String("channelID", conv.ChannelID),
					zap.String("loginUID", loginUID))
				continue
			}
			messages = resp.Messages
		}

		count := countSpaceUnreadFromMessages(messages, spaceID, defaultSpaceID, isSysBot, readSeq)
		conv.SpaceUnread = &count
	}
}

// dmMessageSpaceID 读取消息 payload 的 space_id（缺失 / 空 / 非字符串一律视为 ""，
// 即「无标签」）。
func dmMessageSpaceID(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	if sid, ok := payload["space_id"].(string); ok {
		return sid
	}
	return ""
}

// dmSpaceMatch 判断一条 space_id 为 msgSpaceID 的 DM 消息是否归属 targetSpaceID，
// 与历史过滤 filterPersonMessagesBySpace 的 rule 2 / rule 4 逐条对齐：
//   - msgSpaceID == targetSpaceID：显式打标，归属该 Space；
//   - 未打标（msgSpaceID == ""）且 targetSpaceID 为默认 Space 且**非系统 Bot**：
//     归属默认 Space（rule 2）；
//   - 未打标且**是系统 Bot**：一律不归属（rule 4）—— 系统 Bot（botfather /
//     fileHelper / notification / u_10000）的无标签历史在 /message/channel/sync
//     里被隐藏，预览/未读必须同口径,否则默认 Space 会出现清不掉的幽灵未读 +
//     历史里不存在的预览（PR #532 review by yujiawei/mochashanyao/OctoBoooot）。
//
// defaultSpaceID == "" 时兜底分支关闭（仅严格等值），非默认 Space 查询不受影响。
func dmSpaceMatch(msgSpaceID, targetSpaceID, defaultSpaceID string, isSysBot bool) bool {
	if msgSpaceID == targetSpaceID {
		return true
	}
	return !isSysBot && msgSpaceID == "" && targetSpaceID == defaultSpaceID
}

// findSpaceLastMessage 从 Recents 中倒序查找最后一条归属 spaceID 的消息。
// 用于会话列表的消息预览，确保每个 Space 显示该 Space 的最后一条消息。
func findSpaceLastMessage(recents []*MsgSyncResp, spaceID, defaultSpaceID string, isSysBot bool) *MsgSyncResp {
	for i := len(recents) - 1; i >= 0; i-- {
		msg := recents[i]
		if msg.Payload == nil {
			continue
		}
		if dmSpaceMatch(dmMessageSpaceID(msg.Payload), spaceID, defaultSpaceID, isSysBot) {
			return msg
		}
	}
	return nil
}

// findSpaceLastMessageFallback 在 Recents 找不到匹配消息时，
// 从 WuKongIM 向前拉取历史消息（最多 200 条），查找最后一条归属 spaceID 的消息。
func findSpaceLastMessageFallback(
	channelID string, channelType uint8,
	loginUID string, spaceID, defaultSpaceID string,
	isSysBot bool, lastMsgSeq uint32, ctx *config.Context,
) *MsgSyncResp {
	if lastMsgSeq == 0 {
		return nil
	}

	// 从最新消息向前拉取 200 条
	endSeq := lastMsgSeq
	startSeq := uint32(1)
	if endSeq > 200 {
		startSeq = endSeq - 200 + 1
	}

	resp, err := ctx.IMSyncChannelMessage(config.SyncChannelMessageReq{
		LoginUID:        loginUID,
		ChannelID:       channelID,
		ChannelType:     channelType,
		StartMessageSeq: startSeq,
		EndMessageSeq:   endSeq,
		Limit:           200,
		PullMode:        config.PullModeDown,
	})
	if err != nil {
		log.Warn("findSpaceLastMessageFallback: 拉取历史消息失败",
			zap.Error(err),
			zap.String("channelID", channelID),
			zap.String("loginUID", loginUID))
		return nil
	}

	// 从最新到最旧遍历，找第一条匹配的（跳过已删除消息）
	for i := len(resp.Messages) - 1; i >= 0; i-- {
		msg := resp.Messages[i]
		if msg.IsDeleted == 1 {
			continue
		}
		payloadMap, err := msg.GetPayloadMap()
		if err != nil || payloadMap == nil {
			continue
		}
		if dmSpaceMatch(dmMessageSpaceID(payloadMap), spaceID, defaultSpaceID, isSysBot) {
			return msgRespToSyncResp(msg)
		}
	}
	return nil
}

// msgRespToSyncResp 将 config.MessageResp 转换为 MsgSyncResp（用于预览）。
// 包含 IsDeleted、Revoke、Setting 等前端渲染所需字段。
func msgRespToSyncResp(msg *config.MessageResp) *MsgSyncResp {
	payloadMap, _ := msg.GetPayloadMap()
	return &MsgSyncResp{
		MessageID:    msg.MessageID,
		MessageIDStr: strconv.FormatInt(msg.MessageID, 10),
		MessageSeq:   msg.MessageSeq,
		ClientMsgNo:  msg.ClientMsgNo,
		FromUID:      msg.FromUID,
		ToUID:        msg.ToUID,
		ChannelID:    msg.ChannelID,
		ChannelType:  msg.ChannelType,
		Timestamp:    msg.Timestamp,
		Setting:      msg.Setting,
		IsDeleted:    msg.IsDeleted,
		Payload:      payloadMap,
	}
}

// countSpaceUnreadFromMessages 遍历消息列表，统计 seq > readSeq 且归属 spaceID 的消息数
// （归属判断含「无标签 = 默认 Space」约定 + 系统 Bot 例外，见 dmSpaceMatch）。
func countSpaceUnreadFromMessages(messages []*config.MessageResp, spaceID, defaultSpaceID string, isSysBot bool, readSeq int64) int {
	count := 0
	for _, msg := range messages {
		if int64(msg.MessageSeq) <= readSeq {
			continue
		}
		payloadMap, err := msg.GetPayloadMap()
		if err != nil || payloadMap == nil {
			continue
		}
		if dmSpaceMatch(dmMessageSpaceID(payloadMap), spaceID, defaultSpaceID, isSysBot) {
			count++
		}
	}
	return count
}
