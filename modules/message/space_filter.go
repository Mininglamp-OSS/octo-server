package message

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

	// 查用户的默认 Space（最早加入的），裸 UID 旧会话只在默认 Space 显示。
	// defaultSpaceID 决定空 space_id 群/子区的归属与默认-Space DM catch-all。
	//
	// 查询失败时置为 ""（sentinel），而非 filterSpaceID —— #519 reviewer P1
	// (yujiawei / OctoBoooot)：若置为 filterSpaceID，非默认请求会满足下方裸 DM
	// catch-all 门（filterSpaceID == defaultSpaceID → return true），把用户全部裸
	// DM（含纯属他空间的）泄漏到当前 Space。per-Space DM 隔离是本 PR 要建立的安全
	// 属性，错误路径必须 fail-closed（偏向隐藏，不偏向显示）。filterSpaceID 在过滤器
	// 运行时恒非空（两个调用点都在 `if spaceID != ""` 内），故 "" 是「永不等于真实
	// filterSpaceID」的 sentinel：非默认请求 catch-all 不触发、落到 presence/Recents
	// 扫描保持隔离；空 space_id 群/子区本次请求隐藏（fail-closed，是 pre-PR「全 Space
	// 可见」的子集，非回归）。等价于 pre-PR GetUserDefaultSpaceID 出错返回 "" 的语义。
	defaultSpaceID, defaultSpaceErr := space.GetUserDefaultSpaceIDE(ctx, loginUID)
	if defaultSpaceErr != nil {
		log.Warn("查询默认 Space 失败，Space 过滤 fail-closed（defaultSpaceID 置空 sentinel）", zap.Error(defaultSpaceErr), zap.String("loginUID", loginUID))
		defaultSpaceID = ""
	}

	// 群聊的 channel_id 是裸 group_no（没有 Space 前缀），ParseChannelID 返回 spaceID=""。
	// 需要从 group 表查出真实 space_id。
	groupNoSeen := make(map[string]struct{})
	var bareGroupNos []string
	var bareDMUIDs []string
	addGroupNo := func(no string) {
		if _, ok := groupNoSeen[no]; ok {
			return
		}
		groupNoSeen[no] = struct{}{}
		bareGroupNos = append(bareGroupNos, no)
	}
	for _, conv := range conversations {
		if conv.SpaceID == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			addGroupNo(conv.ChannelID)
		}
		if conv.SpaceID == "" && conv.ChannelType == common.ChannelTypePerson.Uint8() {
			bareDMUIDs = append(bareDMUIDs, conv.ChannelID)
		}
		// 子区会话需要按父群的 space_id 决定可见性，把父群 groupNo 也加入查询。
		// 同一父群的多个子区/父群本身都可能命中，dedup 避免下游 GetGroups 重复查询。
		if conv.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			if parentNo, _, err := thread.ParseChannelID(conv.ChannelID); err == nil {
				addGroupNo(parentNo)
			}
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

	// 查询用户作为外部成员加入的群 → { groupNo: sourceSpaceID }
	externalGroupMap, err := group.NewDB(ctx).QueryExternalGroupNosForUser(loginUID)
	if err != nil {
		log.Warn("查询外部群失败，跳过外部群过滤", zap.Error(err))
		externalGroupMap = make(map[string]string)
	}

	// Bot DM 过滤
	botSet, botInSpace, skipBotFilter := resolveBotFilter(ctx, filterSpaceID, bareDMUIDs)

	// YUJ-4185 P0-3：子区过滤纳入父群成员校验。space_filter 之前只按父群 space_id
	// 决定子区可见性，不校验调用者仍是父群成员 → 被移除者会话列表仍见子区并拉历史
	// （越权读 P0）。在 Space 过滤前先剔除“父群已非成员”的子区会话（fail-closed）。
	conversations = filterThreadConvsByParentMembership(
		conversations,
		func(c *SyncUserConversationResp) string { return c.ChannelID },
		func(c *SyncUserConversationResp) uint8 { return c.ChannelType },
		loginUID, groupService,
	)

	// issue #484：DM per-Space 存在性（窗口无关的权威信号）。
	dmPresentSet := resolveDMPresence(ctx, loginUID, filterSpaceID, bareDMUIDs)

	filtered := filterConversationsCore(conversations, filterSpaceID, defaultSpaceID, groupSpaceMap, externalGroupMap, botSet, botInSpace, skipGroupFilter, skipBotFilter, dmPresentSet)

	// issue #484 follow-up：默认 Space catch-all 收紧。仅在默认 Space 请求、且默认
	// Space 解析成功时启用；presence 证据表明 DM 只属于其他 Space 且 Recents 无反证
	// → 隐藏。任何查询失败降级为不隐藏（见 space_filter_default_catchall.go）。
	if filterSpaceID == defaultSpaceID && defaultSpaceErr == nil {
		elsewhereOnly := resolveDMElsewhereOnly(ctx, loginUID, defaultSpaceID, bareDMUIDs)
		filtered = hideElsewhereOnlyDMsInDefaultSpace(
			filtered,
			func(c *SyncUserConversationResp) string { return c.ChannelID },
			func(c *SyncUserConversationResp) uint8 { return c.ChannelType },
			func(c *SyncUserConversationResp) bool { return personConvAllRecentsTaggedElsewhere(c, defaultSpaceID) },
			elsewhereOnly, botSet, skipBotFilter,
		)
	}
	return filtered
}

// resolveDMPresence 批量解析 DM 在 filterSpaceID 下的存在性（issue #484）。
// 输入是会话里的裸 DM 对端 UID 列表；按 common.GetFakeChannelIDWith(loginUID, peer)
// 规范化成 dm_space_presence 的主键查询，再把命中结果回映射成“对端 UID -> true”，
// 以便 decideConvKeepInSpace 直接用 conv.ChannelID 命中。
//
// 失败/空输入返回 nil（优雅降级）：读侧对 nil map 取值得 false，会回退到
// Recents 窗口扫描的 OR 项，不比现状更差，也不会误隐藏存量 DM。
func resolveDMPresence(ctx *config.Context, loginUID, filterSpaceID string, bareDMUIDs []string) map[string]bool {
	if ctx == nil || loginUID == "" || filterSpaceID == "" || len(bareDMUIDs) == 0 {
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
	presentFakes, err := spacepkg.DMSpacePresenceSet(ctx.DB(), fakeIDs, filterSpaceID)
	if err != nil {
		log.Warn("查询 dm_space_presence 失败，回退 Recents 兜底", zap.Error(err))
		return nil
	}
	out := make(map[string]bool, len(presentFakes))
	for fake := range presentFakes {
		out[fakeToPeer[fake]] = true
	}
	return out
}

// filterThreadConvsByParentMembership 剔除“调用者已不是父群成员”的子区(CommunityTopic)
// 会话，非子区会话原样保留。YUJ-4185 P0-3：子区无独立成员表，权威成员身份在父群；
// 被踢/退群/拉黑后子区会话仍可能残留在 IM 返回里，必须按父群成员校验。
//
// fail-closed：父群成员查询失败时 drop 全部子区会话（宁可让用户多刷一次，也不放行
// 可能越权的子区）。channelID 解析失败的子区同样 drop。泛型适配 v1
// (*message.SyncUserConversationResp) 与 v2 (*config.SyncUserConversationResp) 两种载荷。
func filterThreadConvsByParentMembership[T any](
	conversations []T,
	channelID func(T) string,
	channelType func(T) uint8,
	loginUID string,
	groupService group.IService,
) []T {
	if len(conversations) == 0 {
		return conversations
	}
	// 收集所有子区会话的父群 groupNo（去重）。
	parentSeen := make(map[string]struct{})
	parentNos := make([]string, 0)
	hasThread := false
	for _, conv := range conversations {
		if channelType(conv) != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}
		hasThread = true
		parentNo, _, err := thread.ParseChannelID(channelID(conv))
		if err != nil || parentNo == "" {
			continue
		}
		if _, ok := parentSeen[parentNo]; ok {
			continue
		}
		parentSeen[parentNo] = struct{}{}
		parentNos = append(parentNos, parentNo)
	}
	if !hasThread {
		return conversations
	}
	memberParents := make(map[string]struct{})
	if len(parentNos) > 0 {
		// CR 整改：用 ExistMembersActive（排除黑名单）而非 ExistMembers，否则被拉黑
		// (status=Blacklist、is_deleted=0) 用户的子区会话仍会透出。
		memberNos, err := groupService.ExistMembersActive(parentNos, loginUID)
		if err != nil {
			// fail-closed：无法确认成员身份时丢弃全部子区会话。
			log.Warn("子区父群成员校验失败，按 fail-closed 丢弃子区会话", zap.Error(err))
		} else {
			for _, no := range memberNos {
				memberParents[no] = struct{}{}
			}
		}
	}
	filtered := make([]T, 0, len(conversations))
	for _, conv := range conversations {
		if channelType(conv) != common.ChannelTypeCommunityTopic.Uint8() {
			filtered = append(filtered, conv)
			continue
		}
		parentNo, _, err := thread.ParseChannelID(channelID(conv))
		if err != nil || parentNo == "" {
			continue
		}
		if _, member := memberParents[parentNo]; !member {
			continue
		}
		filtered = append(filtered, conv)
	}
	return filtered
}

// filterConversationsCore 是纯过滤逻辑，不依赖 DB/ctx，便于单元测试。
func filterConversationsCore(
	conversations []*SyncUserConversationResp,
	filterSpaceID string,
	defaultSpaceID string,
	groupSpaceMap map[string]string,
	externalGroupMap map[string]string,
	botSet map[string]bool,
	botInSpace map[string]bool,
	skipGroupFilter bool,
	skipBotFilter bool,
	dmPresentSet map[string]bool,
) []*SyncUserConversationResp {
	filtered := make([]*SyncUserConversationResp, 0, len(conversations))
	for _, conv := range conversations {
		keep := decideConvKeepInSpace(
			conv.ChannelID, conv.ChannelType, conv.SpaceID,
			filterSpaceID, defaultSpaceID,
			groupSpaceMap, externalGroupMap, botSet, botInSpace,
			skipGroupFilter, skipBotFilter,
			// v1 兼容：群表查询失败时不过滤（与历史 FilterConversationsBySpace 一致）。
			false,
			dmPresentSet,
			func(target string) bool { return personConvHasSpaceMessages(conv, target) },
		)
		if keep {
			filtered = append(filtered, conv)
		}
	}
	return filtered
}

// decideConvKeepInSpace 是单条会话是否在目标 Space 显示的纯决策函数。
// 抽取出来是为了让 v1 (message.SyncUserConversationResp) 和 v2
// (config.SyncUserConversationResp) 共用同一套过滤规则 —— payload 形态不同
// 但规则一致；hasSpaceMsg 由调用方按各自的 Recents 表示填入。
//
// 参数：
//   - convSpaceID: 调用方已对 channel_id 做过 ParseChannelID 后得到的 space_id。
//     可为空，群聊/子区会进一步查 groupSpaceMap。
//   - hasSpaceMsg: 仅对非默认 Space 的 Person DM 生效，判断 conv.Recents 内是否
//     有 payload.space_id == targetSpaceID 的消息。
//   - failClosedOnUnknownGroupSpace: 当 skipGroupFilter=true（group service 查询
//     失败、无法确认群的 space_id）时的语义切换。
//   - false（v1 兼容默认）：保留群/子区，不让一次 DB 抖动影响存量行为。
//   - true（v2 sidebar 用，PR #21 Round-6 P0-1）：drop 群/子区，避免跨 Space
//     泄露（reviewer Jerry-Xin / yujiawei）。这是 fail-closed —— 用户多刷
//     一次即可，但绝不让 Space A 的群在 Space B 请求里露出。
func decideConvKeepInSpace(
	channelID string,
	channelType uint8,
	convSpaceID string,
	filterSpaceID, defaultSpaceID string,
	groupSpaceMap, externalGroupMap map[string]string,
	botSet, botInSpace map[string]bool,
	skipGroupFilter, skipBotFilter bool,
	failClosedOnUnknownGroupSpace bool,
	dmPresentSet map[string]bool,
	hasSpaceMsg func(targetSpaceID string) bool,
) bool {
	spaceID := convSpaceID
	if spaceID == "" && channelType == common.ChannelTypeGroup.Uint8() {
		if skipGroupFilter {
			if failClosedOnUnknownGroupSpace {
				return false
			}
			return true
		}
		spaceID = groupSpaceMap[channelID]
	}

	if spaceID == filterSpaceID && channelType != common.ChannelTypeCommunityTopic.Uint8() {
		return true
	}
	if channelType == common.ChannelTypeCommunityTopic.Uint8() {
		return filterThreadConvCore(channelID, filterSpaceID, defaultSpaceID, groupSpaceMap, externalGroupMap, skipGroupFilter, failClosedOnUnknownGroupSpace)
	}
	if channelType == common.ChannelTypeGroup.Uint8() {
		if sourceSpace, ok := externalGroupMap[channelID]; ok {
			eff := sourceSpace
			if eff == "" {
				eff = defaultSpaceID
			}
			if eff == filterSpaceID {
				return true
			}
		}
		if spaceID == "" {
			// issue #484 follow-up：无法归属的群（group.space_id 为空 / group 表无
			// 记录）不再全 Space 可见 —— 生产实证这是最近列表串空间的确定路径
			// （客户端拿到 conv 级 space_id=null 只能 fail-open 渲染到每个 Space）。
			// 归属到用户默认 Space，与 #337 裸 DM、#484 无标签 DM 历史同口径；
			// 群表回填 space_id 后自动恢复精确归属。
			return filterSpaceID == defaultSpaceID
		}
		return false
	}
	if spaceID == "" && filterSpaceID == defaultSpaceID && channelType != common.ChannelTypeCommunityTopic.Uint8() {
		if !skipBotFilter && channelType == common.ChannelTypePerson.Uint8() && botSet[channelID] && !botInSpace[channelID] {
			return false
		}
		return true
	}
	if spaceID == "" && channelType == common.ChannelTypePerson.Uint8() {
		if skipBotFilter {
			return true
		}
		if spacepkg.SystemBots[channelID] {
			return true
		}
		if botSet[channelID] && botInSpace[channelID] {
			return true
		}
		if !botSet[channelID] {
			// issue #484：DM 可见性以持久化的 dm_space_presence 为权威信号，
			// 与历史 Recents 窗口扫描 OR —— presence 解决“跨窗口被挤出而隐藏”
			// （症状2），Recents 兜底保证存量 DM 不因尚未写索引而消失。
			if dmPresentSet[channelID] {
				return true
			}
			return hasSpaceMsg != nil && hasSpaceMsg(filterSpaceID)
		}
	}
	return false
}

// filterThreadConvCore 是 filterThreadConv 的 channelID-only 变体，便于 v2
// 不持有完整 SyncUserConversationResp 时复用。
//
// failClosedOnUnknownGroupSpace 参见 decideConvKeepInSpace 注释：
// v1 兼容传 false，v2 sidebar 传 true。
func filterThreadConvCore(
	channelID string,
	filterSpaceID, defaultSpaceID string,
	groupSpaceMap, externalGroupMap map[string]string,
	skipGroupFilter bool,
	failClosedOnUnknownGroupSpace bool,
) bool {
	parentNo, _, err := thread.ParseChannelID(channelID)
	if err != nil {
		return false
	}
	if skipGroupFilter {
		if failClosedOnUnknownGroupSpace {
			return false
		}
		return true
	}
	parentSpaceID := groupSpaceMap[parentNo]
	if parentSpaceID == filterSpaceID {
		return true
	}
	if sourceSpace, ok := externalGroupMap[parentNo]; ok {
		eff := sourceSpace
		if eff == "" {
			eff = defaultSpaceID
		}
		if eff == filterSpaceID {
			return true
		}
	}
	// issue #484 follow-up：父群无法归属（space_id 为空/群表无记录）的子区与父群
	// 同口径 —— 只在用户默认 Space 露出，不再全 Space 可见。
	return parentSpaceID == "" && filterSpaceID == defaultSpaceID
}

// filterThreadConv 判断子区会话是否应在 filterSpaceID 中显示。
// 规则：跟父群一致——按父群的 space_id 匹配，外部群走 source Space 兜底，旧群（无 space_id）所有 Space 可见。
// channel_id 解析失败的子区会话会被丢弃。
//
// Deprecated: prefer filterThreadConvCore 以避免对 SyncUserConversationResp 类型依赖；
// 此包装目前未被新代码使用，保留是为最小化 diff（PR #21 Space filter 重构）。
func filterThreadConv(
	conv *SyncUserConversationResp,
	filterSpaceID string,
	defaultSpaceID string,
	groupSpaceMap map[string]string,
	externalGroupMap map[string]string,
	skipGroupFilter bool,
) bool {
	// v1 兼容：失败时 fail-open（与旧 filterThreadConv 一致）。
	return filterThreadConvCore(conv.ChannelID, filterSpaceID, defaultSpaceID, groupSpaceMap, externalGroupMap, skipGroupFilter, false)
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

// spaceizePersonRecents 把每条 Person (DM) 会话的 Recents 收敛到当前 Space —— 直接复用
// filterPersonMessagesBySpace（/v1/message/channel/sync 的同一份 DM 空间归属规则：精确匹配，
// 或「无标签 = 默认 Space」的裸 DM 约定；系统 Bot 走 rule 4，只保留精确打标当前 Space 的消息）。
// 复用而非再抄一遍判定逻辑，避免与历史过滤漂移出两套「哪条消息属于哪个 Space」的口径。
//
// 背景（本 PR 修复的会话缺失 bug）：客户端 SpaceFilter 按会话「最后一条消息」（recents[0]）的
// payload.space_id 判定该会话归属哪个 Space；而服务端此前把 WuKongIM 的 Recents（物理频道的
// 全局最新窗口，与 Space 无关）原样透传。于是一条在 Space A、Space B 都有消息的 DM，其全局最新
// 一条属于 B，客户端就把整条会话过滤出 Space A —— 即便服务端 decideConvKeepInSpace 已判定它属于
// Space A（presence / hasSpaceMsg），也会在客户端消失。这与历史过滤 filterPersonMessagesBySpace
// 是同一类问题，只是会话同步的 Recents 之前漏做了空间化。
//
// 空间化后窗口里每一条都属于当前 Space，recents[0]（无论客户端把哪端当「最新」）恒为当前 Space
// 的消息，可见性判定不再被跨 Space 的最新消息带偏。过滤后为空（当前 Space 的消息在窗口外）时
// Recents 保持为空：客户端读不到跨 Space 的 recents[0]，不再误隐藏该会话（它已被
// decideConvKeepInSpace 判定属于当前 Space，会以空预览行正常展示 —— 时间/排序走会话级 timestamp、
// 未读走 space_unread、预览走 space_last_message）。不用 SpaceLastMessage 回填 recents：预览已由
// space_last_message 承载，空 recents 也不会触发隐藏；回填反而会把一条绕过 channelOffset/
// deviceOffset、缺富化字段的兜底消息塞进 recents[0]，与其余 recents 口径不一致。
//
// 隔离安全（space-isolation 规则）：只放行归属当前 Space 的消息，不会把 Space B 的消息内容泄漏进
// Space A 的响应；当前 Space 无消息时收敛为空（fail-closed，宁可空预览也不泄漏跨 Space 消息）。
//
// 仅处理 ChannelTypePerson；Group / CommunityTopic 的 Space 隔离在 channel_id 层完成，其 Recents
// 不受影响。系统 Bot（botfather / fileHelper / u_10000 等）同样空间化：filterPersonMessagesBySpace
// 内部按 isSysBot 走 rule 4，untagged 历史一律不归属任何 Space、只保留精确打标当前 Space 的消息，
// 与预览/未读 (fillPersonSpaceUnread) 同口径。系统 Bot 的会话可见性另由 EnsureSystemBotsPresent +
// 客户端豁免保证（始终展示，与本收敛无关）；这里只收敛其 Recents，避免历史里被隐藏的 untagged /
// 跨 Space 消息经 recents[0] 当预览漏出（#532 同口径）。
//
// 会话级 last_msg_seq / last_client_msg_no 不改动：它们是 WuKongIM 增量同步游标（客户端据其推进
// version），须保持全局口径；Web/扩展端不读它们做 Space 过滤，跨端 Space 判定的权威信号是消息级
// payload.space_id（即 Recents）。
//
// 必须在 fillPersonSpaceUnread 与 FilterConversationsBySpace 之后调用：两者都读原始（未收窄）
// Recents —— 前者据此算 space_last_message / space_unread，后者据此做 keep 判定（hasSpaceMsg），
// 提前收窄会破坏它们。
func spaceizePersonRecents(conversations []*SyncUserConversationResp, spaceID, defaultSpaceID string) {
	if spaceID == "" {
		return
	}
	for _, conv := range conversations {
		if conv == nil || conv.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}
		conv.Recents = filterPersonMessagesBySpace(conv.Recents, conv.ChannelID, spaceID, defaultSpaceID)
	}
}

// EnsureSystemBotsPresent 保证 Space-scoped sync 响应中一定包含系统 Bot
// （目前 botfather / u_10000 / fileHelper）的 conversation entry。
//
// 背景 (YUJ-216 / GH#1280)：
//   - POST /v1/conversation/sync 带 X-Space-ID 时，IM 核心只会返回自
//     `version` 之后有新消息的 conversation。系统 Bot 若没有新消息就不会
//     出现在增量响应中，经 Space 过滤后客户端也拿不到。移动端没有 Web
//     那样的前端兜底，就会导致用户在某些 Space 下"消失"了 botfather 私聊。
//   - 修复策略：只要调用方开启了 Space 过滤，就在最终响应中显式补齐每一个
//     系统 Bot 的 entry。已经存在的 entry（有真实 Recents）保持不变；缺席的
//     以最小占位形式注入，兼容老客户端。
//
// 占位 entry 的字段原则：
//   - ChannelID / ChannelType：对齐 Person DM；
//   - SpaceID: 空串 —— 系统 Bot 不属于任何 Space；
//   - Recents / LastMsgSeq / Unread / Version / Timestamp 保持零值，避免
//     客户端误以为有新消息或错把占位写回 ack；
//   - 其他字段沿用结构体默认值，等价于"已知此频道、无新内容"。
//
// 不影响消息级 space_id 过滤：本函数只补 conversation-level 占位，
// 对 Recents 内 payload.space_id 字段不做任何修改。
func EnsureSystemBotsPresent(conversations []*SyncUserConversationResp) []*SyncUserConversationResp {
	systemBots := spacepkg.SystemBotList()
	if len(systemBots) == 0 {
		return conversations
	}

	present := make(map[string]bool, len(conversations))
	for _, conv := range conversations {
		if conv == nil {
			continue
		}
		if conv.ChannelType == common.ChannelTypePerson.Uint8() && spacepkg.IsSystemBot(conv.ChannelID) {
			present[conv.ChannelID] = true
		}
	}

	for _, uid := range systemBots {
		if present[uid] {
			continue
		}
		conversations = append(conversations, newSystemBotPlaceholder(uid))
	}
	return conversations
}

// newSystemBotPlaceholder 构造一个空的 Person 会话占位，字段口径与
// newSyncUserConversationResp 生成的真实会话保持一致，避免新老客户端解码
// 差异。Recents 明确初始化为空切片，保证 JSON 序列化为 `[]` 而非 `null`。
func newSystemBotPlaceholder(uid string) *SyncUserConversationResp {
	return &SyncUserConversationResp{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
		SpaceID:     "",
		Recents:     []*MsgSyncResp{},
	}
}

// filterPersonMessagesBySpace 按 X-Space-ID 过滤 Person (DM) 历史消息列表。
//
// 背景（YUJ-219-A / GH#1283，对应 analysis-report.md §4.1）：
//   - /v1/message/channel/sync 原先对消息级 payload.space_id 0 过滤。客户端
//     进入 botfather / u_10000 / fileHelper 或历史 DM 会话时，会拿到跨 Space
//     的全部历史消息；配合三端不一致的渲染过滤，用户实际看到跨 Space 消息。
//   - Phase 3 五层 Defense-in-Depth 全部作用在 conversation-list，message-level
//     没有权威 Space 标签，这是"BotFather 历史消息跨 Space 可见"回归的根因。
//
// 本函数仅针对 Person (DM) 路径：
//   - GROUP channel_id 本身做 Space 隔离（不同 Space 的群 channel_id 不同），
//     对历史消息再过滤反而会误杀老群，因此 GROUP/COMMUNITY_TOPIC 路径不走这里。
//   - 规则（issue #484 后；与三端 SpaceFilter 口径对齐）：
//     1) payload.space_id == spaceID                         → 保留（精确匹配）
//     2) payload.space_id == "" && !isSystemBot && 默认Space  → 保留（无标签 DM
//     历史只在用户默认 Space 向前兼容，避免出现在每个 Space —— 症状1）
//     3) payload.space_id == "" && !isSystemBot && 非默认Space→ 丢弃（不再 fail-open）
//     4) payload.space_id == "" &&  isSystemBot               → 丢弃（SystemBot 无
//     space 标签的老消息默认隐藏，避免 fileHelper/u_10000 老消息跨 Space 泄露）
//     5) payload.space_id != "" && != spaceID                → 丢弃（跨 Space 污染）
//
// 调用方需保证 spaceID != ""（空串视为未启用 Space 过滤，直接返回原列表），
// 传入 defaultSpaceID（用户默认 Space，决定规则 2/3），并只对 ChannelTypePerson 调用。
func filterPersonMessagesBySpace(msgs []*MsgSyncResp, channelID, spaceID, defaultSpaceID string) []*MsgSyncResp {
	if spaceID == "" || len(msgs) == 0 {
		return msgs
	}
	isSysBot := spacepkg.IsSystemBot(channelID)
	filtered := make([]*MsgSyncResp, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		msid := extractPayloadSpaceID(m.Payload)
		switch {
		case msid == spaceID:
			// 精确匹配当前 Space → 保留
			filtered = append(filtered, m)
		case msid == "" && !isSysBot && spaceID == defaultSpaceID:
			// issue #484：无 space_id 的普通 DM 消息（发送方未带 X-Space-ID、
			// Space 化之前的老消息、转发/名片）只在用户默认 Space 向前兼容保留，
			// 不再出现在每个 Space —— 修复症状1（跨 Space 历史泄漏）。
			filtered = append(filtered, m)
		case msid == "" && !isSysBot:
			// 非默认 Space：无标签 DM 消息不再 fail-open 放行，丢弃。
			continue
		case msid == "" && isSysBot:
			// 系统 Bot 的无 space_id 历史消息一律隐藏。对齐 Android
			// filterSystemBotMessages 和 iOS filterMessagesBySpace，避免
			// 老的 botfather/fileHelper/u_10000 对话全量跨 Space 暴露。
			continue
		case msid != spaceID:
			// 明确跨 Space，丢弃。
			continue
		}
	}
	return filtered
}

// extractPayloadSpaceID 从已反序列化的消息 payload 中读取 space_id 字段。
// payload 非 map、字段缺失或类型不符时返回 ""，调用方据此走"无 space_id"分支。
func extractPayloadSpaceID(payload map[string]interface{}) string {
	if len(payload) == 0 {
		return ""
	}
	v, ok := payload["space_id"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
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

// CollectGroupSpaceMap 是 (groupNo -> spaceID) 的批量推导器：扫描
// conversation 列表里的群和子区父群，去重后调一次 group service，输出
// 客户端可见性判定所需的映射表。
//
// extraGroupNos 用于补充 conversations 之外的 groupNo（典型场景：v2 sidebar
// 的 DB-only thread ext 行的父群可能不在 IM 返回里 —— GH octo-server#153
// Round-2 Critical 2）。传 nil / 空切片表示纯走 conversations。
//
// 调用方：
//   - FilterRawConversationsBySpace / FilterConversationsBySpace：Space 过滤；
//   - api_sidebar.go Sidebar.Sync：把 group.SpaceID 回填到 SidebarItem.SpaceID
//     （GH octo-server#153），让客户端 WebSocket 实时消息能正确路由到当前
//     Space tab，避免 conversation-level SpaceID 缺失导致 fail-open。
//
// 返回 (map, ok)。ok=false 表示底层 group service 调用失败 —— 调用方据此决定
// fail-open 还是 fail-closed（v2 sidebar 必须 fail-closed，参见
// decideConvKeepInSpace.failClosedOnUnknownGroupSpace 注释）。
func CollectGroupSpaceMap(
	conversations []*config.SyncUserConversationResp,
	extraGroupNos []string,
	groupService group.IService,
) (map[string]string, bool) {
	seen := make(map[string]struct{})
	var bareGroupNos []string
	add := func(no string) {
		if no == "" {
			return
		}
		if _, ok := seen[no]; ok {
			return
		}
		seen[no] = struct{}{}
		bareGroupNos = append(bareGroupNos, no)
	}
	for _, conv := range conversations {
		if conv == nil {
			continue
		}
		sid, _ := spacepkg.ParseChannelID(conv.ChannelID)
		if sid == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			add(conv.ChannelID)
		}
		if conv.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			if parentNo, _, err := thread.ParseChannelID(conv.ChannelID); err == nil {
				add(parentNo)
			}
		}
	}
	for _, no := range extraGroupNos {
		add(no)
	}
	if len(bareGroupNos) == 0 {
		return map[string]string{}, true
	}
	m, err := spacepkg.GetGroupSpaceMap(bareGroupNos, func(nos []string) ([]spacepkg.GroupSpaceInfo, error) {
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
		return nil, false
	}
	return m, true
}

// FilterRawConversationsBySpace 是 FilterConversationsBySpace 在 v2 sidebar 上的
// 对应版本：v2 直接操作 IM 返回的 *config.SyncUserConversationResp（没有
// enriched SpaceID/parsed Payload），所以单独写一个入口，但内部沿用
// decideConvKeepInSpace 同一套规则，保证 v1/v2 Space 可见性一致。
//
// 背景 (PR #21 review by Jerry-Xin)：原 v2 实现根本没做 Space 过滤，导致
// X-Space-ID=B 的请求会拿到 Space A 的活跃 DM/Group/Thread。
//
// 差异点：
//   - SpaceID 通过 spacepkg.ParseChannelID 推导，与 v1
//     newSyncUserConversationResp 中的 line 1345 同一份逻辑。
//   - hasSpaceMsg：DM Recents 的 Payload 是 []byte（IM 原始 JSON），需要 lazily
//     json.Unmarshal；解析失败的消息当作"无 space_id"处理（保守不放行）。
func FilterRawConversationsBySpace(
	conversations []*config.SyncUserConversationResp,
	filterSpaceID string,
	loginUID string,
	ctx *config.Context,
	groupService group.IService,
) []*config.SyncUserConversationResp {
	if len(conversations) == 0 {
		return conversations
	}

	// YUJ-4185 P0-3：先按父群成员身份剔除越权子区会话（fail-closed），再做 Space 过滤。
	// 与 v1 FilterConversationsBySpace 同口径，保证 sidebar 不暴露被移除者的子区。
	conversations = filterThreadConvsByParentMembership(
		conversations,
		func(c *config.SyncUserConversationResp) string { return c.ChannelID },
		func(c *config.SyncUserConversationResp) uint8 { return c.ChannelType },
		loginUID, groupService,
	)
	if len(conversations) == 0 {
		return conversations
	}

	// 同 v1：defaultSpaceID 决定空 space_id 群/子区归属与 DM catch-all；失败时置为 ""
	// sentinel（非 filterSpaceID），使非默认请求的裸 DM catch-all fail-closed，避免
	// 跨空间 DM 泄漏（#519 reviewer P1，详见 v1 注释）。
	defaultSpaceID, defaultSpaceErr := space.GetUserDefaultSpaceIDE(ctx, loginUID)
	if defaultSpaceErr != nil {
		log.Warn("v2 sidebar: 查询默认 Space 失败，Space 过滤 fail-closed（defaultSpaceID 置空 sentinel）", zap.Error(defaultSpaceErr), zap.String("loginUID", loginUID))
		defaultSpaceID = ""
	}

	groupNoSeen := make(map[string]struct{})
	var bareGroupNos []string
	var bareDMUIDs []string
	addGroupNo := func(no string) {
		if _, ok := groupNoSeen[no]; ok {
			return
		}
		groupNoSeen[no] = struct{}{}
		bareGroupNos = append(bareGroupNos, no)
	}
	// v2 没有 enriched SpaceID 字段，直接 ParseChannelID。
	convSpaceIDs := make([]string, len(conversations))
	for i, conv := range conversations {
		sid, _ := spacepkg.ParseChannelID(conv.ChannelID)
		convSpaceIDs[i] = sid
		if sid == "" && conv.ChannelType == common.ChannelTypeGroup.Uint8() {
			addGroupNo(conv.ChannelID)
		}
		if sid == "" && conv.ChannelType == common.ChannelTypePerson.Uint8() {
			bareDMUIDs = append(bareDMUIDs, conv.ChannelID)
		}
		if conv.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			if parentNo, _, err := thread.ParseChannelID(conv.ChannelID); err == nil {
				addGroupNo(parentNo)
			}
		}
	}

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
		log.Warn("v2 sidebar: 查询群 SpaceID 错误，跳过群过滤", zap.Error(err))
		skipGroupFilter = true
	}

	externalGroupMap, err := group.NewDB(ctx).QueryExternalGroupNosForUser(loginUID)
	if err != nil {
		log.Warn("v2 sidebar: 查询外部群失败，跳过外部群过滤", zap.Error(err))
		externalGroupMap = make(map[string]string)
	}

	botSet, botInSpace, skipBotFilter := resolveBotFilter(ctx, filterSpaceID, bareDMUIDs)

	// issue #484：DM per-Space 存在性（与 v1 同口径，窗口无关的权威信号）。
	dmPresentSet := resolveDMPresence(ctx, loginUID, filterSpaceID, bareDMUIDs)

	filtered := make([]*config.SyncUserConversationResp, 0, len(conversations))
	for i, conv := range conversations {
		keep := decideConvKeepInSpace(
			conv.ChannelID, conv.ChannelType, convSpaceIDs[i],
			filterSpaceID, defaultSpaceID,
			groupSpaceMap, externalGroupMap, botSet, botInSpace,
			skipGroupFilter, skipBotFilter,
			// v2 sidebar 必须 fail-closed：群表查询失败时无法确认 space，drop
			// 群/子区以免跨 Space 泄露（PR #21 Round-6 P0-1 by Jerry-Xin / yujiawei）。
			true,
			dmPresentSet,
			func(target string) bool { return rawConvHasSpaceMessages(conv, target) },
		)
		if keep {
			filtered = append(filtered, conv)
		}
	}

	// issue #484 follow-up：默认 Space catch-all 收紧（与 v1 同口径，见
	// space_filter_default_catchall.go）。
	if filterSpaceID == defaultSpaceID && defaultSpaceErr == nil {
		elsewhereOnly := resolveDMElsewhereOnly(ctx, loginUID, defaultSpaceID, bareDMUIDs)
		filtered = hideElsewhereOnlyDMsInDefaultSpace(
			filtered,
			func(c *config.SyncUserConversationResp) string { return c.ChannelID },
			func(c *config.SyncUserConversationResp) uint8 { return c.ChannelType },
			func(c *config.SyncUserConversationResp) bool {
				return rawConvAllRecentsTaggedElsewhere(c, defaultSpaceID)
			},
			elsewhereOnly, botSet, skipBotFilter,
		)
	}
	return filtered
}

// rawConvHasSpaceMessages 是 personConvHasSpaceMessages 在原始 IM Payload []byte
// 形态下的对应实现。容错地 lazy-unmarshal：解析失败的消息直接跳过 —— 保守
// 不放行胜过误暴露 Space A 的消息给 Space B 请求。
func rawConvHasSpaceMessages(conv *config.SyncUserConversationResp, targetSpaceID string) bool {
	if conv == nil || len(conv.Recents) == 0 {
		return false
	}
	for _, msg := range conv.Recents {
		if len(msg.Payload) == 0 {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			continue
		}
		sid, ok := payload["space_id"]
		if !ok {
			continue
		}
		if sidStr, ok := sid.(string); ok && sidStr == targetSpaceID {
			return true
		}
	}
	return false
}

// EnsureSystemBotsPresentRaw 与 EnsureSystemBotsPresent 等价但操作
// *config.SyncUserConversationResp（v2 sidebar 用）。系统 Bot 占位写法对齐 v1：
// ChannelID/ChannelType 设置好，其它字段保持零值。
func EnsureSystemBotsPresentRaw(conversations []*config.SyncUserConversationResp) []*config.SyncUserConversationResp {
	systemBots := spacepkg.SystemBotList()
	if len(systemBots) == 0 {
		return conversations
	}
	present := make(map[string]bool, len(conversations))
	for _, conv := range conversations {
		if conv == nil {
			continue
		}
		if conv.ChannelType == common.ChannelTypePerson.Uint8() && spacepkg.IsSystemBot(conv.ChannelID) {
			present[conv.ChannelID] = true
		}
	}
	for _, uid := range systemBots {
		if present[uid] {
			continue
		}
		conversations = append(conversations, &config.SyncUserConversationResp{
			ChannelID:   uid,
			ChannelType: common.ChannelTypePerson.Uint8(),
		})
	}
	return conversations
}
