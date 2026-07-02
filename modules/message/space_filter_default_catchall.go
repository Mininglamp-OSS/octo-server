package message

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// 默认-Space DM catch-all 收紧（issue #484 follow-up）。
//
// decideConvKeepInSpace 的 catch-all 分支在 filterSpaceID == defaultSpaceID 时无
// 条件保留所有裸 DM —— 包括「消息只属于其他 Space」的 DM，导致默认 Space 的最近
// 会话列表串入他空间私聊（生产可复现）。本文件是过滤后的补充 pass：仅当存在
// **正向持久证据**（dm_space_presence 有行、且没有一行属于默认 Space、且 Recents
// 里没有默认/未打标消息作反证）时，才把该 DM 从默认 Space 列表中隐藏。
//
// 设计约束（与 #484 主修复同口径，fail-open on infra errors）：
//   - 无任何 presence 行的存量/未打标 DM 保持 catch-all 现状（不凭空隐藏）；
//   - presence / bot 解析任一查询失败 → 本 pass 整体跳过，不比现状隐藏更多；
//   - 系统 Bot 永不隐藏（EnsureSystemBotsPresent 的占位契约不受影响）；
//   - 普通 Bot 的可见性由 catch-all 内已有的成员子判定决定，本 pass 不二次裁决；
//   - 自愈：用户在默认 Space 发一条消息即写入 presence(pair, default)，恢复可见。

// resolveDMElsewhereOnly 返回「已被 presence 索引跟踪、但没有任何一行属于
// defaultSpaceID」的裸 DM 对端集合（键为对端 UID）。任何失败返回 nil —— 调用方
// 对 nil 集合跳过隐藏（优雅降级，不比现状更差）。
func resolveDMElsewhereOnly(ctx *config.Context, loginUID, defaultSpaceID string, bareDMUIDs []string) map[string]bool {
	if ctx == nil || loginUID == "" || defaultSpaceID == "" || len(bareDMUIDs) == 0 {
		return nil
	}
	fakeToPeer := make(map[string]string, len(bareDMUIDs))
	fakeIDs := make([]string, 0, len(bareDMUIDs))
	for _, peer := range bareDMUIDs {
		fake := common.GetFakeChannelIDWith(loginUID, peer)
		if _, ok := fakeToPeer[fake]; ok {
			continue
		}
		fakeToPeer[fake] = peer
		fakeIDs = append(fakeIDs, fake)
	}
	anySet, err := spacepkg.DMSpacePresenceAnySet(ctx.DB(), fakeIDs)
	if err != nil {
		log.Warn("查询 dm_space_presence(any) 失败，跳过默认 Space catch-all 收紧", zap.Error(err))
		return nil
	}
	if len(anySet) == 0 {
		return nil
	}
	inDefault, err := spacepkg.DMSpacePresenceSet(ctx.DB(), fakeIDs, defaultSpaceID)
	if err != nil {
		log.Warn("查询 dm_space_presence(default) 失败，跳过默认 Space catch-all 收紧", zap.Error(err))
		return nil
	}
	out := make(map[string]bool, len(anySet))
	for fake := range anySet {
		if !inDefault[fake] {
			out[fakeToPeer[fake]] = true
		}
	}
	return out
}

// hideElsewhereOnlyDMsInDefaultSpace 从默认 Space 的过滤结果中移除
// elsewhereOnly 命中的裸 DM。allRecentsTaggedElsewhere 提供本地反证检查：
// Recents 里只要有一条默认-Space 消息或未打标消息就保留（见下方两个实现）。
// skipBotFilter=true（bot 解析失败，身份不可知）时整体跳过。
func hideElsewhereOnlyDMsInDefaultSpace[T any](
	convs []T,
	chID func(T) string,
	chType func(T) uint8,
	allRecentsTaggedElsewhere func(T) bool,
	elsewhereOnly map[string]bool,
	botSet map[string]bool,
	skipBotFilter bool,
) []T {
	if len(elsewhereOnly) == 0 || skipBotFilter {
		return convs
	}
	kept := make([]T, 0, len(convs))
	for _, conv := range convs {
		id := chID(conv)
		if chType(conv) == common.ChannelTypePerson.Uint8() &&
			!spacepkg.SystemBots[id] && !botSet[id] &&
			elsewhereOnly[id] && allRecentsTaggedElsewhere(conv) {
			continue
		}
		kept = append(kept, conv)
	}
	return kept
}

// personConvAllRecentsTaggedElsewhere 判断 v1 会话的 Recents 是否**全部**明确
// 属于其他 Space（每条都带非空 space_id 且都 != defaultSpaceID）。出现未打标 /
// 解析不出 space_id / 默认-Space 消息即返回 false（反证成立 → 保留）。
// 空 Recents 返回 true —— 没有本地反证，由权威 presence 决定。
func personConvAllRecentsTaggedElsewhere(conv *SyncUserConversationResp, defaultSpaceID string) bool {
	if conv == nil {
		return true
	}
	for _, msg := range conv.Recents {
		if msg == nil {
			continue
		}
		sid := extractPayloadSpaceID(msg.Payload)
		if sid == "" || sid == defaultSpaceID {
			return false
		}
	}
	return true
}

// rawConvAllRecentsTaggedElsewhere 是 personConvAllRecentsTaggedElsewhere 在原始
// IM Payload []byte 形态下的对应实现（v2 sidebar 用）。payload 解析失败视同
// 未打标（返回 false 保留）—— 隐藏决策只认清晰证据。
func rawConvAllRecentsTaggedElsewhere(conv *config.SyncUserConversationResp, defaultSpaceID string) bool {
	if conv == nil {
		return true
	}
	for _, msg := range conv.Recents {
		if msg == nil {
			continue
		}
		sid := ""
		if len(msg.Payload) > 0 {
			var payload map[string]interface{}
			if err := json.Unmarshal(msg.Payload, &payload); err == nil {
				if s, ok := payload["space_id"].(string); ok {
					sid = s
				}
			}
		}
		if sid == "" || sid == defaultSpaceID {
			return false
		}
	}
	return true
}
