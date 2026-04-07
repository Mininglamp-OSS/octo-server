package message

import (
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// fillPersonSpaceUnread 为 Person 频道计算 per-Space 未读计数，
// 并填充 SpaceLastMessage（该 Space 的最后一条消息预览）。
// 仅处理 channelType=1 的会话。
// 通过解析消息 payload 中的 space_id 字段来统计属于指定 Space 的未读消息数。
func fillPersonSpaceUnread(
	conversations []*SyncUserConversationResp,
	rawConversations []*config.SyncUserConversationResp,
	spaceID string,
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

		// 从 Recents 中找该 Space 的最后一条消息作为预览
		spaceLastMsg := findSpaceLastMessage(conv.Recents, spaceID)
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
					loginUID, spaceID, uint32(raw.LastMsgSeq), ctx,
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

		count := countSpaceUnreadFromMessages(messages, spaceID, readSeq)
		conv.SpaceUnread = &count
	}
}

// findSpaceLastMessage 从 Recents 中倒序查找最后一条 space_id 匹配的消息。
// 用于会话列表的消息预览，确保每个 Space 显示该 Space 的最后一条消息。
func findSpaceLastMessage(recents []*MsgSyncResp, spaceID string) *MsgSyncResp {
	for i := len(recents) - 1; i >= 0; i-- {
		msg := recents[i]
		if msg.Payload != nil {
			if sid, ok := msg.Payload["space_id"]; ok {
				if sidStr, ok := sid.(string); ok && sidStr == spaceID {
					return msg
				}
			}
		}
	}
	return nil
}

// findSpaceLastMessageFallback 在 Recents 找不到匹配消息时，
// 从 WuKongIM 向前拉取历史消息（最多 200 条），查找最后一条 space_id 匹配的消息。
func findSpaceLastMessageFallback(
	channelID string, channelType uint8,
	loginUID string, spaceID string,
	lastMsgSeq uint32, ctx *config.Context,
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
		if sid, ok := payloadMap["space_id"].(string); ok && sid == spaceID {
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

// countSpaceUnreadFromMessages 遍历消息列表，统计 seq > readSeq 且 payload.space_id == spaceID 的消息数。
func countSpaceUnreadFromMessages(messages []*config.MessageResp, spaceID string, readSeq int64) int {
	count := 0
	for _, msg := range messages {
		if int64(msg.MessageSeq) <= readSeq {
			continue
		}
		payloadMap, err := msg.GetPayloadMap()
		if err != nil || payloadMap == nil {
			continue
		}
		if sid, ok := payloadMap["space_id"].(string); ok && sid == spaceID {
			count++
		}
	}
	return count
}
