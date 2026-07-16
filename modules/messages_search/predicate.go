package messages_search

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 归一化可读谓词（决策九）。bot 主体的「单一可读谓词」，供 #C/#D（单频道门单点求值）与
// #E（global allowlist 枚举）共用，杜绝两路各写一套鉴权规则而漂移。
//
//	canReadChannel(principal, channel)         // 单频道门对一个频道求值
//	enumerateReadableChannels(principal)       // 全局 allowlist 枚举同一谓词
//
// 本层（YUJ-48）定义接口 + dispatch；各主体分支语义分文件落地：
//   - user / uk：真人语义主体 → 复用现有 checkChannelAccess / buildAllowlist，仅主体 uid
//     由 principal.SubjectUID() 提供（uk=key UID）。
//   - obo：grantor 真人分支 ∩ OBO 已授 scope（YUJ-53 / #F，见 obo.go）——单频道门与
//     allowlist 共用同一 oboChecker 谓词。
//   - as-bot：#C/#D（canReadChannel）与 #E（enumerateReadableChannels）的接线点：
//     #C（YUJ-50）已接线 DM 门（IsFriend，跳过 blacklist）、#D（YUJ-51）已接线群/子区门
//     （复用 ExistMemberActive，主体=botUID）、#E（YUJ-52）已接线 global allowlist 枚举。

// canReadChannel 是单频道门——#C/#D 按 principal 分支细化、#E 的 allowlist 枚举同一谓词。
// 真人语义主体（user / obo / uk）走现有 checkChannelAccess（主体 uid 来自 principal）；
// as-bot 分支由 #C/#D 用 bot 谓词替换本层占位。
func (h *Handler) canReadChannel(c *wkhttp.Context, p Principal, channelType uint8, channelID string) bool {
	switch p.Kind() {
	case principalKindUserBot:
		return h.botCanReadChannel(c, p, channelType, channelID)
	case principalKindOBO:
		// as-user(OBO)：grantor 真人分支 ∩ OBO 已授 scope（YUJ-53 / #F，见 obo.go）。
		return h.oboCanReadChannel(c, p, channelType, channelID)
	default:
		// user / uk：真人语义，主体 uid 各异。checkChannelAccess 内部的
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
	case principalKindOBO:
		// as-user(OBO)：grantor 真人可达集 ∩ OBO 已授 scope，与 oboCanReadChannel 同一
		// 谓词（决策九，见 obo.go）。
		return h.oboEnumerateReadableChannels(c, p)
	default:
		return h.buildAllowlist(c, p.SubjectUID(), p.SpaceID())
	}
}

// principalForSubject 为一个「已解析的真人语义主体」返回其 principal 载体，供内部枚举
// 路径（resolveGlobalScope）经归一化谓词求值时使用。优先返回路由显式写入的 principal
// （bot / uk / obo，#B），否则用传入的 uid/spaceID 组装真人载体——这保证现网真人路径与
// 直接以 (loginUID, spaceID) 驱动枚举的既有单测行为完全不变。
func principalForSubject(c *wkhttp.Context, uid, spaceID string) Principal {
	if v, ok := c.Get(principalCtxKey); ok {
		if p, ok := v.(Principal); ok && p != nil {
			return p
		}
	}
	return userPrincipal{uid: uid, spaceID: spaceID}
}

// botCanReadChannel 是 as-bot 单频道门（决策九），按 channelType 分支——bot 谓词 =
// IsFriend(botUID, peer) 的 DM ∪ ExistMemberActive(group, botUID) 的群/子区，跳过 Space 段
// 与全部 P2P blacklist：
//
//   - DM（#C / YUJ-50）：botCheckP2PAccess(c, botUID, peer)（见 authz.go），谓词 =
//     IsFriend(botUID, peer)，是 #E buildBotAllowlist 的 GetFriends(botUID) 枚举对偶；
//     跳过 Space 与双向 blacklist。
//   - 群（#D / YUJ-51）：checkGroupAccess(c, groupNo, botUID)。as-bot 群门与真人群门完全同源
//     ——都落在 ExistMemberActive(groupNo, botUID)：bot 有自己的 group_member 行（发消息路径
//     bot_api/send.go:394-404 已在用），status=Normal 天然排除被移出 / 被群拉黑的 bot；#354 的
//     group/bot_cascade.go expandBlacklistTargetsWithOwnedBots 让「拉黑用户连带拉黑其在群 bot」
//     → 被拉黑者的 bot status!=Normal → 天然搜不到该群，群级黑名单白拿，无需在此另写一套规则。
//   - 子区（#D / YUJ-51）：checkThreadAccess(c, channelID, botUID)。子区继承父群成员身份
//     ——门即 ExistMemberActive(parentGroupNo, botUID)，与群门同一谓词；bot 无子区免打扰
//     设置，GetThread 的 mute 查询仅返回空，不参与放行判定。
//   - 其他：validate.go 已在入口拒绝未知 channelType；此处 defense-in-depth fail-closed 兜底。
//
// 归一化（决策九硬约束）：群/子区门是 ExistMemberActive 的单点求值，与 #E buildBotAllowlist
// 的 ExistMembersActive 枚举严格同源——保证「单频道门放行 ⇔ 出现在 global allowlist」
//（#G 跨路一致性）。所有拒绝渲染 NOT_FOUND/resource=channel（反枚举，与真人拒绝一致），
// DB 错由 botCheckP2PAccess / checkGroupAccess / checkThreadAccess 内部 fail-closed。
// #B 接线 bot 路由前不会触达此分支。
func (h *Handler) botCanReadChannel(c *wkhttp.Context, p Principal, channelType uint8, channelID string) bool {
	botUID := p.SubjectUID()
	switch channelType {
	case channelTypePerson:
		return h.botCheckP2PAccess(c, botUID, channelID)
	case channelTypeGroup:
		return h.checkGroupAccess(c, channelID, botUID)
	case channelTypeThread:
		return h.checkThreadAccess(c, channelID, botUID)
	default:
		// validate.go 已在入口拒绝未知 channel_type；此处 defense-in-depth 兜底。
		h.Warn("messages_search: as-bot gate unexpected channel_type; denying fail-closed",
			zap.String("bot_uid", botUID),
			zap.Uint8("channel_type", channelType),
			zap.String("channel_id", channelID))
		respondNotFound(c, "channel")
		return false
	}
}

// botEnumerateReadableChannels 是 as-bot global allowlist 枚举（#E / YUJ-52），与
// botCanReadChannel（#C/#D）严格同一谓词（决策九）：
//   - 群/子区：ExistMemberActive(group, botUID) 为真的群及其活跃子区（子区继承父群成员身份）；
//   - DM：IsFriend(botUID, peer) 的每个好友对端（无黑名单、无 Space、无 bot-in-Space 抑制）。
//
// 实现委托给 buildBotAllowlist（与真人 buildAllowlist 同处 search_global.go），复用
// 既有 enumerateThreadsForGroups 与 per-group/聚合 thread 上限——不为 bot 另设上限，也
// 不在此内联重写任何鉴权规则（有界性与归一化理由见 buildBotAllowlist 文档）。
//
// App Bot：一期禁用 global。authenticateUserBot 已在 principal 构造阶段 fail-closed 拒绝
// App Bot（errPrincipalAppBotDenied），只有 User Bot 能构造出 userBotPrincipal 抵达此处，
// 故无需在此重复运行时判定（本主体类型即 App-Bot-已排除 的保证）。
func (h *Handler) botEnumerateReadableChannels(_ *wkhttp.Context, p Principal) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	return h.buildBotAllowlist(p.SubjectUID())
}
