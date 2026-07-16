package messages_search

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 归一化可读谓词（决策九）。bot 主体的「单一可读谓词」，供 #C/#D（单频道门单点求值）与
// #E（global allowlist 枚举）共用，杜绝两路各写一套鉴权规则而漂移。
//
//	canReadChannel(principal, channel)         // 单频道门对一个频道求值
//	enumerateReadableChannels(principal)       // 全局 allowlist 枚举同一谓词
//
// 本层（YUJ-48）只定义接口 + dispatch，不改鉴权语义：
//   - user / obo / uk 三类真人语义主体 → 复用现有 checkChannelAccess / buildAllowlist，
//     仅主体 uid 由 principal.SubjectUID() 提供（obo=grantor、uk=key UID）。
//   - as-bot 分支是 #C/#D（canReadChannel）与 #E（enumerateReadableChannels）的接线点，
//     本层 fail-closed 占位（#B 之前无 bot 路由，不影响现网）。

// errBotPredicateNotImplemented as-bot 可读谓词尚未接线（#C/#D/#E）。
var errBotPredicateNotImplemented = errors.New("messages_search: as-bot readable predicate not implemented (YUJ-50/51/52)")

// canReadChannel 是单频道门——#C/#D 按 principal 分支细化、#E 的 allowlist 枚举同一谓词。
// 真人语义主体（user / obo / uk）走现有 checkChannelAccess（主体 uid 来自 principal）；
// as-bot 分支由 #C/#D 用 bot 谓词替换本层占位。
func (h *Handler) canReadChannel(c *wkhttp.Context, p Principal, channelType uint8, channelID string) bool {
	switch p.Kind() {
	case principalKindUserBot:
		return h.botCanReadChannel(c, p, channelType, channelID)
	default:
		// user / obo / uk：真人语义，主体 uid 各异。checkChannelAccess 内部的
		// 双向黑名单由 blacklistPolicy 决定（真人语义主体均为 bidirectional）。
		return h.checkChannelAccess(c, channelType, channelID, p.SubjectUID())
	}
}

// enumerateReadableChannels 是 canReadChannel 的 global-allowlist 对偶：必须枚举出与
// canReadChannel 对同一 principal 放行完全一致的频道集（决策九）。真人语义主体委托给
// buildAllowlist；as-bot 分支是 #E 的接线点。
//
// 返回 (allowGroup, allowDM, allowThread, timings, err)，与 buildAllowlist 同形。
func (h *Handler) enumerateReadableChannels(c *wkhttp.Context, p Principal) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	switch p.Kind() {
	case principalKindUserBot:
		return h.botEnumerateReadableChannels(c, p)
	default:
		return h.buildAllowlist(c, p.SubjectUID(), p.SpaceID())
	}
}

// principalForSubject 为一个「已解析的真人语义主体」返回其 principal 载体，供内部枚举
// 路径（resolveGlobalScope）经归一化谓词求值时使用。优先返回路由显式写入的 principal
//（bot / uk / obo，#B），否则用传入的 uid/spaceID 组装真人载体——这保证现网真人路径与
// 直接以 (loginUID, spaceID) 驱动枚举的既有单测行为完全不变。
func principalForSubject(c *wkhttp.Context, uid, spaceID string) Principal {
	if v, ok := c.Get(principalCtxKey); ok {
		if p, ok := v.(Principal); ok && p != nil {
			return p
		}
	}
	return userPrincipal{uid: uid, spaceID: spaceID}
}

// botCanReadChannel 是 as-bot 单频道门的接线点（#C/#D）。bot 谓词 =
// IsFriend(botUID, peer) 的 DM ∪ ExistMemberActive(group, botUID) 的群/子区，
// 且跳过 Space 段与全部 P2P blacklist。本层 fail-closed 占位：渲染 NOT_FOUND
//（反枚举，与真人拒绝一致）。#B 接线 bot 路由前不会触达此分支。
func (h *Handler) botCanReadChannel(c *wkhttp.Context, p Principal, channelType uint8, channelID string) bool {
	h.Warn("messages_search: as-bot channel gate not yet implemented (YUJ-50/51); denying fail-closed",
		zap.String("bot_uid", p.SubjectUID()),
		zap.Uint8("channel_type", channelType),
		zap.String("channel_id", channelID))
	respondNotFound(c, "channel")
	return false
}

// botEnumerateReadableChannels 是 as-bot global allowlist 枚举的接线点（#E）。
// allowlist = bot 所在群/子区 ∪ IsFriend(botUID) 的 DM 对端，与 botCanReadChannel
// 同一谓词。本层 fail-closed 占位返回错误（#B 接线 bot 路由前不会触达此分支）。
func (h *Handler) botEnumerateReadableChannels(_ *wkhttp.Context, p Principal) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	h.Warn("messages_search: as-bot global allowlist not yet implemented (YUJ-52); failing closed",
		zap.String("bot_uid", p.SubjectUID()))
	return nil, nil, nil, allowlistTimings{}, errBotPredicateNotImplemented
}
