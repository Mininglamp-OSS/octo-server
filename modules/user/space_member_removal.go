package user

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// spaceMemberDMCutoffStepName 清理步骤名，同时作为工单 last_error 的前缀。
const spaceMemberDMCutoffStepName = "dm_cutoff"

// registerSpaceMemberRemovalCleanup 把「断掉失去授权的私聊」注册为成员移除清理步骤。
// 由 1module.go 在 friend 模块构造时调用（modules/user 已 import modules/space，
// 反向注册避免成环）。
func (f *Friend) registerSpaceMemberRemovalCleanup() {
	space.RegisterMemberRemovalCleanupStep(spaceMemberDMCutoffStepName, f.cleanupSpaceMemberDMs)
}

// cleanupSpaceMemberDMs 把被移出 Space 的成员与「因此失去私聊授权」的对端断开。
//
// 私聊授权来自 Person 频道白名单，而白名单的推导（modules/user/1module.go 的
// IMDatasource.Whitelist）本来就是 friends(uid) ∪ GetCoMemberUIDs(uid) —— 推导侧
// 在成员行置 0 的那一刻就已经正确了。坏的是 WuKongIM 缓存着旧白名单，没人去摘。
// 所以这里做的不是「重新定义规则」，而是把已经失效的缓存条目摘掉。
//
// 关键边界：两个人可能同时在多个 Space，也可能是好友。从 X 移除后如果他们还共处
// Y，或者仍是双向好友，私聊必须照常可用 —— 所以每个对端都要重新判定，
// 不能因为「离开了这个 Space」就一刀切。
// 所有 DB / IM 访问一律走**传入的 ctx**，不混用 f.ctx：两者在生产里是同一个
// 单例，但混用会让「换一个 ctx 调用」静默读到另一套连接，测试也测不出真实路径。
func (f *Friend) cleanupSpaceMemberDMs(ctx *config.Context, removal space.MemberRemoval) error {
	peers, err := f.dmPeersInSpace(ctx, removal)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return nil
	}

	var firstErr error
	for _, peer := range peers {
		// 逐个对端判定，而不是一次性拉出「他所有剩余共同成员」：对端集合的上界是
		// 他自己的私聊数（很小），而共同成员集合的上界是所有剩余 Space 的人数
		// （可能上万），后者会为了几个判断把整张名单拉进内存。
		//
		// 谓词与 queryCoMemberUIDs 完全一致（两侧 status=1 且 space.status=1），
		// 也就是与 Person 频道白名单的推导同源。此刻他的 space_member 行已经置 0，
		// 所以「仅因本 Space 而同处」的人自然判不出共处。
		shared, err := spacepkg.SharesActiveSpace(ctx.DB(), removal.UID, peer)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("check remaining shared space: %w", err)
			}
			continue
		}
		if shared {
			continue // 还有别的共同 Space，白名单本来就该留着
		}
		// 两个方向**各自**判定，不能用 OR 短路。
		//
		// 白名单是按频道推导的：X 的 Person 频道白名单 = friends(X, is_alone=0) ∪
		// coMembers(X)，也就是「谁能发给 X」。单向好友时（A 留着 A→B 的好友行，B 早把
		// A 删了）只有 A 的频道仍授权 B，B 的频道并不授权 A。若任一方向有好友行就整个
		// 跳过，B 频道上那条早已失效的白名单就没人摘 —— A 还能继续发给 B，而 B 的
		// 客户端（annotateDMSendability 只查正确的那个方向）显示的是"不可发送"。
		cutInbound, err := f.dmDirectionRevoked(ctx, peer, removal.UID) // 被移除者 -> 对端
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		cutOutbound, err := f.dmDirectionRevoked(ctx, removal.UID, peer) // 对端 -> 被移除者
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !cutInbound && !cutOutbound {
			continue
		}
		if err := f.cutOffDM(ctx, removal.SpaceID, removal.UID, peer, cutInbound, cutOutbound); err != nil {
			f.Error("断开被移除成员的私聊失败",
				zap.Error(err),
				zap.String("spaceId", removal.SpaceID),
				zap.String("uid", removal.UID),
				zap.String("peer", peer))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// dmPeersInSpace 找出「与被移除者有私聊、且属于本 Space 范畴」的对端。
//
// 两步收窄，都有界：
//  1. 从 IM 拉他自己的会话列表 —— 上界是他的会话数，不是 Space 的人数。
//     这一步本身就是「有过私聊」的证据。
//  2. 收窄到在本 Space 有过成员行的人（一次 IN 查询，不看状态，理由见
//     MembersEverInSpace）。
//
// 刻意**不**再拿 dm_space_presence 当硬门槛。那张表是 message webhook 上尽力而为地
// 写入的增量索引：不回填、只覆盖带 space_id 的非加密 Person 消息、写失败只记日志
// （见 pkg/space/dm_presence.go 的说明，连它自己都强调读侧要 OR 上兜底，
// 「缺一行绝不能让一个会话消失」）。把它当唯一门槛，等于让任何在该表上线前聊过、
// 或用加密私聊、或那次 upsert 恰好失败的一对，静默地跳过整个隔离清理。
// 真正决定切不切的是后面逐对端的授权判定，这一步只负责圈定范围。
func (f *Friend) dmPeersInSpace(ctx *config.Context, removal space.MemberRemoval) ([]string, error) {
	conversations, err := ctx.IMSyncUserConversation(removal.UID, 0, 1, "", nil)
	if err != nil {
		return nil, fmt.Errorf("sync conversations of removed member: %w", err)
	}

	candidates := dmPeerCandidates(conversations, removal.UID)
	if len(candidates) == 0 {
		return nil, nil
	}

	// 用「有过成员行」而不是「当前仍是活跃成员」来收窄：清理总是在成员行已经置 0
	// 之后才跑，解散时连 space.status 也已经是 0。按活跃口径筛会让这一步在
	// 「空间被解散」和「同批移除多人」两种情况下静默返回空集，一条私聊都切不掉。
	memberSet, err := spacepkg.MembersEverInSpace(ctx.DB(), removal.SpaceID, candidates)
	if err != nil {
		return nil, fmt.Errorf("narrow dm peers to space members: %w", err)
	}
	if len(memberSet) == 0 {
		return nil, nil
	}

	peers := make([]string, 0, len(memberSet))
	for _, peer := range candidates {
		if memberSet[peer] {
			peers = append(peers, peer)
		}
	}
	return peers, nil
}

// dmDirectionRevoked 判断「sender 是否已经不再被允许发给 channelUID」。
//
// 口径直接对齐白名单推导（modules/user/1module.go 的 IMDatasource.Whitelist）：
// channelUID 的 Person 频道白名单 = GetFriends(channelUID) 中 is_alone=0 的那些
// ∪ coMembers(channelUID)。调用方已经确认两人不再共处任一活跃 Space，所以这里
// 只剩好友这一条依据，且**方向敏感**：看的是 channelUID 是否持有指向 sender 的
// 有效双向好友行。
func (f *Friend) dmDirectionRevoked(ctx *config.Context, channelUID, sender string) (bool, error) {
	friend, err := newFriendDB(ctx).isMutualFriend(channelUID, sender)
	if err != nil {
		return false, fmt.Errorf("query friendship (%s -> %s): %w", channelUID, sender, err)
	}
	return !friend, nil
}

// cutOffDM 按方向摘掉 Person 频道白名单，并给两端推 channelUpdate。
//
// 形状照搬 event_friend.go 的 handleDeleteFriend：要挡住 A→B，必须把 A 从 B 的
// Person 频道白名单里摘掉。两个方向由调用方独立判定后传进来——单向好友时只有
// 一个方向该被切，一刀切两边会误伤仍被好友关系授权的那个方向。
func (f *Friend) cutOffDM(ctx *config.Context, spaceID, removedUID, peer string, cutInbound, cutOutbound bool) error {
	if cutInbound {
		// 被移除者 -> 对端：摘对端频道上的 removedUID
		if err := f.removePersonWhitelist(ctx, peer, removedUID); err != nil {
			return err
		}
	}
	if cutOutbound {
		// 对端 -> 被移除者：摘被移除者频道上的 peer
		if err := f.removePersonWhitelist(ctx, removedUID, peer); err != nil {
			return err
		}
	}

	// Bot 私聊用的是 Space 前缀频道（见 app_bot / botfather 的 IMWhitelistAdd），
	// 只摘裸 uid 频道会把 bot 私聊漏在开着的状态。
	//
	// 这里的查库失败必须上抛而不是吞掉：吞掉的话 cutOffDM 返回 nil、工单标 done，
	// 前缀频道的白名单就永远没人摘了，而 brief 的验收明确要求 bot 私聊「被处理，
	// 不能静默跳过」。步骤契约本来就带重试，重试也是幂等的，上抛不花任何代价。
	bot, err := f.eitherSideIsBot(ctx, removedUID, peer)
	if err != nil {
		return fmt.Errorf("check bot dm (%s/%s): %w", removedUID, peer, err)
	}
	if bot {
		// 这两个频道未必存在（这一对可能从没在本 Space 下建过前缀频道），
		// 因此按 best-effort 处理：失败只告警，不让整条工单一直重试到 abandoned。
		if cutInbound {
			f.removeSpaceScopedWhitelist(ctx, spaceID, peer, removedUID)
		}
		if cutOutbound {
			f.removeSpaceScopedWhitelist(ctx, spaceID, removedUID, peer)
		}
	}

	// 两端各推一条 channelUpdate：客户端据此重拉对端频道信息，把发送框置灰。
	// 推送失败不回滚白名单 —— 服务端的拦截已经生效，客户端下次拉取也会自愈。
	f.notifyDMChannelUpdate(ctx, removedUID, peer)
	f.notifyDMChannelUpdate(ctx, peer, removedUID)
	return nil
}

// removePersonWhitelist 把 uid 从 channelUID 的 Person 频道白名单里摘掉。
func (f *Friend) removePersonWhitelist(ctx *config.Context, channelUID, uid string) error {
	err := ctx.IMWhitelistRemove(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   channelUID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})
	if err != nil {
		return fmt.Errorf("remove person whitelist (%s): %w", channelUID, err)
	}
	return nil
}

// removeSpaceScopedWhitelist 摘 Space 前缀频道的白名单，best-effort。
func (f *Friend) removeSpaceScopedWhitelist(ctx *config.Context, spaceID, channelUID, uid string) {
	channelID := spacepkg.BuildChannelID(spaceID, channelUID)
	if channelID == channelUID {
		return // 没有 Space 前缀可加，裸频道上面已经处理过了
	}
	if err := ctx.IMWhitelistRemove(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	}); err != nil {
		f.Warn("摘除 Space 前缀频道白名单失败（best-effort）",
			zap.Error(err), zap.String("channelId", channelID), zap.String("uid", uid))
	}
}

// notifyDMChannelUpdate 给 recipient 推一条 channelUpdate，让其客户端重拉 peer 的频道信息。
// 形状对齐 api_setting.go 里既有的 Person 频道 channelUpdate 推送。
func (f *Friend) notifyDMChannelUpdate(ctx *config.Context, recipient, peer string) {
	if err := ctx.SendCMD(config.MsgCMDReq{
		ChannelID:   recipient,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDChannelUpdate,
		Param: map[string]interface{}{
			"channel_id":   peer,
			"channel_type": common.ChannelTypePerson.Uint8(),
		},
	}); err != nil {
		f.Warn("推送私聊频道更新失败",
			zap.Error(err), zap.String("recipient", recipient), zap.String("peer", peer))
	}
}

// eitherSideIsBot 判断这对私聊里是否有 bot。
func (f *Friend) eitherSideIsBot(ctx *config.Context, uid, peer string) (bool, error) {
	db := newFriendDB(ctx)
	isBot, err := db.isRobot(peer)
	if err != nil {
		return false, err
	}
	if isBot {
		return true, nil
	}
	return db.isRobot(uid)
}

// DM 可发送性下发给客户端的 extra key。
//
// 只在**不可发送**时下发，可发送时整个 key 不出现：老客户端读不到就当没有，
// 行为与今天一致；新客户端据此把发送框置灰（参照 octo-web 对已解散群的只读处理）。
//
// 刻意不复用 be_deleted / be_blacklist：前者是「对方把你删了好友」（源自
// friend.is_alone），后者是黑名单，客户端已按各自语义在用，挤进第三种含义会让
// 三者都失真。
const (
	dmForbiddenExtraKey       = "dm_forbidden"
	dmForbiddenReasonExtraKey = "dm_forbidden_reason"
	// dmForbiddenReasonNoSharedSpace 双方既非好友、也不再同处任一活跃 Space。
	dmForbiddenReasonNoSharedSpace = "no_shared_space"
)

// annotateDMSendability 给 Person 频道信息补一个「当前登录用户能否给该对端发消息」的标记。
//
// 判定必须与 Person 频道白名单的推导同源（modules/user/1module.go 的
// IMDatasource.Whitelist = friends(peer, is_alone=0) ∪ coMembers(peer)）：
// loginUID 能发给 peer，当且仅当 loginUID 落在 peer 频道的白名单里。口径不一致的话，
// 前端可能展示成可发但被 WuKongIM 拒收，或反过来无谓置灰。
//
// 只读判定，任何一步失败都按「可发送」放行（不下发 key）：这里是展示层提示，
// 真正的拦截在 WuKongIM 白名单上，fail-open 不会造成越权。
func annotateDMSendability(ctx *config.Context, friendDB *friendDB, resp *model.ChannelResp, peerUID, loginUID string) {
	if resp == nil || loginUID == "" || peerUID == "" || peerUID == loginUID {
		return // 自己的频道永远可写
	}
	shared, err := spacepkg.SharesActiveSpace(ctx.DB(), loginUID, peerUID)
	if err != nil {
		ctx.Warn("判定共同 Space 失败，DM 可发送性按可发送处理",
			zap.String("loginUID", loginUID), zap.String("peer", peerUID), zap.Error(err))
		return
	}
	if shared {
		return
	}
	mutual, err := friendDB.isMutualFriend(peerUID, loginUID)
	if err != nil {
		ctx.Warn("判定好友关系失败，DM 可发送性按可发送处理",
			zap.String("loginUID", loginUID), zap.String("peer", peerUID), zap.Error(err))
		return
	}
	if mutual {
		return
	}
	if resp.Extra == nil {
		resp.Extra = make(map[string]interface{})
	}
	resp.Extra[dmForbiddenExtraKey] = 1
	resp.Extra[dmForbiddenReasonExtraKey] = dmForbiddenReasonNoSharedSpace
}

// dmPeerCandidates 从会话列表里抽出去重后的私聊对端 UID。
//
// 单独拆出来是因为它的边界都在这一层，且不需要任何 DB / IM 就能验证：
//   - 只认 Person 频道，群 / 子区一律跳过；
//   - bot 私聊的 channel_id 带 Space 前缀（s{spaceID}_{uid}），必须剥掉才是真实对端，
//     否则会拿着 "s...._uid" 去查成员表，永远查不到，bot 私聊就被静默漏掉；
//   - 自己的频道和重复项都要去掉。
//
// 保持入参顺序输出，方便调用方的日志和测试断言稳定。
func dmPeerCandidates(conversations []*config.SyncUserConversationResp, selfUID string) []string {
	candidates := make([]string, 0, len(conversations))
	seen := make(map[string]bool, len(conversations))
	for _, conv := range conversations {
		if conv == nil || conv.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}
		_, peer := spacepkg.ParseChannelID(conv.ChannelID)
		if peer == "" || peer == selfUID || seen[peer] {
			continue
		}
		seen[peer] = true
		candidates = append(candidates, peer)
	}
	return candidates
}
