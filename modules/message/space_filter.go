package message

import (
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// FilterConversationsBySpace 对已获取的会话列表按 spaceID 过滤。
// 关键逻辑：
// - 群聊 space_id 不在 channel_id 前缀中，需查 group 表
// - 系统 Bot (botfather, u_10000, fileHelper) 所有 Space 可见
// - 普通 Bot 需查 space_member 表确认是否在目标 Space
// - 默认 Space（用户最早加入的）中显示裸 UID 旧会话
// - DB 查询失败时 skipBotFilter=true，不过滤避免误删
func FilterConversationsBySpace(
	conversations []*SyncUserConversationResp,
	filterSpaceID string,
	loginUID string,
	ctx *config.Context,
	groupService group.IService,
) []*SyncUserConversationResp {
	if len(conversations) == 0 {
		return conversations
	}

	// 查用户的默认 Space（最早加入的），裸 UID 旧会话只在默认 Space 显示
	defaultSpaceID := space.GetUserDefaultSpaceID(ctx, loginUID)

	// 群聊的 channel_id 是裸 group_no（没有 Space 前缀），ParseChannelID 返回 spaceID=""。
	// 需要从 group 表查出真实 space_id。
	var bareGroupNos []string
	var bareDMUIDs []string
	for _, conv := range conversations {
		if conv.SpaceID == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			bareGroupNos = append(bareGroupNos, conv.ChannelID)
		}
		if conv.SpaceID == "" && conv.ChannelType == common.ChannelTypePerson.Uint8() {
			bareDMUIDs = append(bareDMUIDs, conv.ChannelID)
		}
	}

	// 构建 groupNo -> spaceID 映射
	skipGroupFilter := false
	groupSpaceMap, err := spacepkg.GetGroupSpaceMap(bareGroupNos, func(nos []string) ([]spacepkg.GroupSpaceInfo, error) {
		infos, err := groupService.GetGroups(nos)
		if err != nil {
			return nil, err
		}
		result := make([]spacepkg.GroupSpaceInfo, 0, len(infos))
		for _, g := range infos {
			result = append(result, spacepkg.GroupSpaceInfo{GroupNo: g.GroupNo, SpaceID: g.SpaceID})
		}
		return result, nil
	})
	if err != nil {
		log.Warn("查询群 SpaceID 错误，跳过群过滤", zap.Error(err))
		skipGroupFilter = true
	}

	// Bot DM 过滤
	botSet, botInSpace, skipBotFilter := resolveBotFilter(ctx, filterSpaceID, bareDMUIDs)

	return filterConversationsCore(conversations, filterSpaceID, defaultSpaceID, groupSpaceMap, botSet, botInSpace, skipGroupFilter, skipBotFilter)
}

// filterConversationsCore 是纯过滤逻辑，不依赖 DB/ctx，便于单元测试。
func filterConversationsCore(
	conversations []*SyncUserConversationResp,
	filterSpaceID string,
	defaultSpaceID string,
	groupSpaceMap map[string]string,
	botSet map[string]bool,
	botInSpace map[string]bool,
	skipGroupFilter bool,
	skipBotFilter bool,
) []*SyncUserConversationResp {
	filtered := make([]*SyncUserConversationResp, 0, len(conversations))
	for _, conv := range conversations {
		spaceID := conv.SpaceID
		// 群聊用 group 表的 space_id 替代 ParseChannelID 的结果
		if spaceID == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			if skipGroupFilter {
				// 查询失败时不过滤群聊，直接保留
				filtered = append(filtered, conv)
				continue
			}
			spaceID = groupSpaceMap[conv.ChannelID]
		}

		if spaceID == filterSpaceID {
			filtered = append(filtered, conv)
		} else if spaceID == "" && filterSpaceID == defaultSpaceID {
			// 裸 UID 旧会话只在默认 Space 显示
			// Bot DM：Bot 不在默认 Space 则排除（查询失败时不过滤，避免误删）
			if !skipBotFilter && conv.ChannelType == common.ChannelTypePerson.Uint8() && botSet[conv.ChannelID] && !botInSpace[conv.ChannelID] {
				continue
			}
			filtered = append(filtered, conv)
		} else if spaceID == "" && conv.ChannelType == common.ChannelTypePerson.Uint8() {
			// 非默认 Space 中的 DM 会话
			if skipBotFilter {
				// 查询失败时不过滤，保留所有 DM
				filtered = append(filtered, conv)
			} else if spacepkg.SystemBots[conv.ChannelID] {
				// 系统 Bot → 所有 Space 可见
				filtered = append(filtered, conv)
			} else if botSet[conv.ChannelID] && botInSpace[conv.ChannelID] {
				// 普通 Bot 在此 Space → 显示
				filtered = append(filtered, conv)
			} else if !botSet[conv.ChannelID] {
				// 普通 DM（非 Bot）→ 仅在 Recents 中有匹配 space_id 的消息时显示
				if personConvHasSpaceMessages(conv, filterSpaceID) {
					filtered = append(filtered, conv)
				}
			}
			// Bot 不在此 Space → 不显示
		} else if spaceID == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			// 旧群（无 space_id）在所有 Space 可见
			// 这些是 Space 功能上线前创建的群，无法确定归属
			// 成员关系已由 groupVailds 兜底，不会泄漏给非成员
			filtered = append(filtered, conv)
		}
	}
	return filtered
}

// personConvHasSpaceMessages 检查 Person 会话的 Recents 中是否有 space_id 匹配的消息。
// 用于判断该 DM 会话是否"属于"目标 Space（有过消息往来）。
func personConvHasSpaceMessages(conv *SyncUserConversationResp, targetSpaceID string) bool {
	if conv == nil || len(conv.Recents) == 0 {
		return false
	}
	for _, msg := range conv.Recents {
		if msg.Payload != nil {
			if sid, ok := msg.Payload["space_id"]; ok {
				if sidStr, ok := sid.(string); ok && sidStr == targetSpaceID {
					return true
				}
			}
		}
	}
	return false
}

// resolveBotFilter 批量查询 Bot 状态和 Space 成员关系。
// 返回 botSet（哪些 UID 是 Bot）、botInSpace（哪些 Bot 在 filterSpaceID 中）、skipBotFilter（DB 错误时为 true）。
func resolveBotFilter(ctx *config.Context, filterSpaceID string, bareDMUIDs []string) (botSet map[string]bool, botInSpace map[string]bool, skipBotFilter bool) {
	botSet = make(map[string]bool)
	botInSpace = make(map[string]bool)

	if filterSpaceID == "" || len(bareDMUIDs) == 0 {
		return
	}

	var err error
	botSet, err = spacepkg.GetBotUIDs(ctx.DB(), bareDMUIDs)
	if err != nil {
		log.Warn("查询Bot UID错误，跳过Bot过滤", zap.Error(err))
		skipBotFilter = true
		return
	}

	if len(botSet) == 0 {
		return
	}

	botInSpace, err = spacepkg.CheckBotsInSpace(ctx.DB(), filterSpaceID, botSet)
	if err != nil {
		log.Warn("查询Bot Space成员错误，跳过Bot过滤", zap.Error(err))
		skipBotFilter = true
		return
	}
	return
}
