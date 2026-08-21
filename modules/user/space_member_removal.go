package user

import (
	"fmt"
	"strings"

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
	// 摘和补必须成对注册：只注册前者会让「踢出 → 重新加入」永久断掉私聊
	// （在开启 Person 白名单校验的部署里）。见 restoreSpaceMemberDMs。
	space.RegisterMemberRejoinRestoreStep(spaceMemberDMRestoreStepName, f.restoreSpaceMemberDMs)
}

// cleanupSpaceMemberDMs 把被移出 Space 的成员与「因此失去私聊授权」的对端断开。
//
// 私聊授权来自 Person 频道白名单，而白名单的推导（person_whitelist.go 的
// derivePersonWhitelist）本来就是 friends(uid) ∪ coMembers(uid) —— 推导侧在成员行
// 置 0 的那一刻就已经正确了。坏的是 WuKongIM 缓存着旧白名单，没人去摘。
// 所以这里做的不是「重新定义规则」，而是把已经失效的缓存条目摘掉。
//
// 分两半，用的是同一条规则：
//  1. 覆写他**自己的裸**频道（reconcileRemovedMemberChannel）—— 关掉「谁还能在裸频道上
//     发给他」。无条件执行，不依赖枚举。注意只是裸频道：Space 前缀频道不在内，
//     范围与残余见该函数的说明。
//  2. 逐个对端摘**对端的**频道 —— 关掉「他能发给谁」。这一半只能靠枚举，
//     因此继承了枚举的盲区（会话被本地删掉的对端够不到，见 #797）。
//
// 关键边界：两个人可能同时在多个 Space，也可能是好友。从 X 移除后如果他们还共处
// Y，或者仍是双向好友，私聊必须照常可用 —— 所以每个对端都要重新判定，
// 不能因为「离开了这个 Space」就一刀切。
// 所有 DB / IM 访问一律走**传入的 ctx**，不混用 f.ctx：两者在生产里是同一个
// 单例，但混用会让「换一个 ctx 调用」静默读到另一套连接，测试也测不出真实路径。
func (f *Friend) cleanupSpaceMemberDMs(ctx *config.Context, removal space.MemberRemoval) error {
	var firstErr error

	// 第一件事，且**无条件**做：把「谁能发给被移除者」整个频道按推导值覆写一次。
	// 不依赖下面枚举出的对端集合，正是因为那个集合会漏（见 reconcileRemovedMemberChannel）。
	if err := f.reconcileRemovedMemberChannel(ctx, removal.UID); err != nil {
		f.Error("覆写被移除成员的 Person 频道白名单失败",
			zap.Error(err), zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID))
		firstErr = err
	}

	peers, err := f.dmPeersInSpace(ctx, removal)
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	if len(peers) == 0 {
		return firstErr
	}

	// 「他自己是不是 bot」与对端无关，循环外查一次即可（见 eitherSideIsBot）。
	//
	// 注意这不是纯粹的提取：以前这次查询在 cutOffDM 里、裸频道写完之后，一次失败
	// 只影响那一个对端的前缀频道，后面的对端照跑；现在它在任何对端被处理之前，
	// 所以 robot 表持续报错会让整步失败、20 次尝试一个裸频道都没摘。方向是变差的，
	// 但结果值不变（isRobot(uid) OR isRobot(peer)），重试幂等，属于收敛变慢而非泄漏。
	// 记在 brief 的 Deviations 里。
	selfIsBot, err := newFriendDB(ctx).isRobot(removal.UID)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("check whether removed member is a bot: %w", err)
		}
		return firstErr
	}

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
		if err := f.cutOffDM(ctx, removal.SpaceID, removal.UID, peer, cutInbound, cutOutbound, selfIsBot); err != nil {
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

// reconcileRemovedMemberChannel 把「被移除者自己的裸 Person 频道」白名单整个覆写为推导值。
//
// 覆盖范围要说准，别读成「谁能发给他」这一整个方向都关掉了：
//
//   - 只写**裸** Person 频道 `uid`。broker 的 allowSend(from, to) 查的是 to 的频道
//     （internal/service/permission.go），所以这一次覆写决定的是「谁还能在裸频道上
//     发给他」。
//   - **不碰** Space 前缀频道 `s{spaceID}_{uid}`。那上面的条目只由 cutOffDM 的
//     if bot 分支里的 removeSpaceScopedWhitelist 摘，而那条路径是要先枚举到对端的
//     —— 也就是说，前缀频道这一半仍然带着枚举的盲区。
//   - 反方向（他还能发给谁）在**别人的**频道上，覆写够不到，也只能靠枚举。
//
// 前缀频道这个残余有多大，说准确（核对过，不要凭印象读）：
// 全仓库往前缀 Person 频道写白名单的只有 bot 上线那四处（app_bot 一处、botfather 三处），
// 每一处都先写了双向好友行。所以「Space 移除后还留在 s{spaceID}_{uid} 上的条目」
// 对应的一对通常仍被好友关系授权，本来就不该摘 —— 这一类不是逃逸。
//
// 但**有**一类是真的：好友被删之后。handleDeleteFriend 只摘两个**裸**频道，
// 前缀频道从头到尾没人摘；它对 bot 追加的 IMBlacklistAdd 写在 **bot 自己的频道**上
// （ChannelID=bot, UIDs=[user]），挡的是 user→bot，**挡不住 bot→user**。
// 于是「删掉一个 Space 作用域的 bot 好友」之后，s{spaceID}_{user} 上那条授权仍然有效，
// bot 还能发进来。这条先于本 PR 存在（friend 删除路径的洞，不是移除路径的），
// 本次覆写也没有覆盖到它 —— 覆写只写裸频道。
// 一并记进 #797，与「白名单规则以哪种频道形态为准」同一个决定。
//
// 为什么需要它 —— 逐对端枚举有一个够不到的洞：
// dmPeersInSpace 的范围来自 IMSyncUserConversation，也就是**被移除者自己的会话列表**。
// 会话被他本地删过（/conversation/sync 在 DeletedAtMsgSeq 之后没有新消息时不返回该会话）、
// 或者超出 Conversation.UserMaxCount(默认 1000) 的截断范围，这个对端就枚举不到，
// 两个方向的白名单都没人摘 —— 他离开 Space 之后，对端仍然能发给他。
// 这一半现在由覆写按构造关掉：推导集合里根本不含「只因这个 Space 才有授权」的人，
// 不需要先把他们枚举出来。
//
// 为什么是覆写而不是「读出来做差集」：octo-lib 没有读白名单的封装
// （config/msg.go 只有 whitelist_add / whitelist_set / whitelist_remove）。
// 覆写的安全性取决于推导集合是否完备 —— 这一点在 derivePersonWhitelist 的注释里
// 逐个调用点核对过，两处共用同一个函数正是为了它不漂移。
//
// 代价，说清楚：推导集合含 coMembers(uid)，上界是他剩余所有 Space 的人数之和，
// 万人 Space 会产生一次万级 uid 的 POST。每个移除工单只发生一次（不是每个对端一次），
// 且移除本身低频，可以接受；这也正是逐对端判定那段循环**没有**改用这个集合的原因。
//
// 幂等：纯覆写，重跑得到同一个集合。工单重试安全。
//
// 关于窗口，这里**不**声称零窗口，两种都写清楚：
//
//  1. 「读到的授权已经失效、却照样写下去」：推导（DB 读）与 POST 之间仍有间隙。
//     但这一步没有 restoreDM 那种「先算再传进来」的放大——写下去的值就是刚读到的值，
//     而且两个方向的触发都会重跑它（再次移除会入新工单并重试，再次加入走恢复步骤），
//     所以是收敛的。
//
//  2. **覆写独有的一种：并发授予被盖掉。** 别的模块在这段间隙里给同一个频道
//     whitelist_add（好友通过、bot 通过），那条刚加的授权会被这次覆写抹掉，
//     而且没人会再补——加那一侧不会重跑。逐条 whitelist_remove 没有这个问题，
//     因为它只点名摘不该在的人。
//     接受它的理由是本 PR 一贯的那条不对称：漏摘是**越权**，必须收敛；
//     被误摘是**失能**，用户可见、可自行恢复（重新加好友 / 再次触发加入 / 运维补一次），
//     而且只影响这一对的一个方向。窗口是一次 DB 查询加一次 HTTP 往返。
//     真要消掉它，需要的是「读出当前值再做差集」，而 octo-lib 没有读白名单的封装。
//     记在 #797。
//
// 总之：收敛，但不是零延迟、也不是零代价，别把它当成不变量。
//
// 仍然开着的残余（#797）：
//   - 他能发给谁 —— 那些条目在对端的频道上，单边覆写够不到，
//     需要一份「谁授权过谁」的双边索引才能不枚举地收敛；
//   - 前缀频道那一半仍然受枚举盲区约束（见上）。
//
// 还有一个**扩权**方向的取舍，写在这里而不是留给下一轮发现：推导集合按
// derivePersonWhitelistOfUID 是不分频道形态的（1module.go 注册给 broker 的
// IMDatasource.Whitelist 也是先剥前缀再按同一条规则算），而 bot 上线在有共同 Space
// 时**只**往前缀频道写。于是一个「Space 作用域的 bot 好友」本来没有裸频道条目，
// 这次覆写会把它写进去 —— bot 拿到了一条它原先没有的、不受 Space 约束的私聊通路，
// 而且移除不动好友行，这条通路在他离开该 Space 之后还在。
// 这里按「声明的规则为准」处理（规则不分频道形态，覆写只是让 broker 与规则一致），
// 但这等于替一个仓库内部本就不自洽的地方做了裁决：要么规则该变成分频道形态的，
// 要么 bot 上线那四处该同时写裸频道。在 whitelistOffOfPerson 仍为默认 true 的部署里
// 两者都不生效；**开启白名单校验之前必须先定这件事**，见 #797。
func (f *Friend) reconcileRemovedMemberChannel(ctx *config.Context, uid string) error {
	// 走 OfUID 而不是 derivePersonWhitelist：后者会对 "s" 开头的入参做前缀剥离，
	// 而这里传的是一个确定的 uid，不是 channel_id。
	authorized, err := f.derivePersonWhitelistOfUID(ctx, uid)
	if err != nil {
		return fmt.Errorf("derive person whitelist (%s): %w", uid, err)
	}
	// 空集也照发：那正是「他谁都不认识了」的正确结果，跳过等于把旧条目留着。
	if err := ctx.IMWhitelistSet(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   uid,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: authorized,
	}); err != nil {
		return fmt.Errorf("set person whitelist (%s): %w", uid, err)
	}
	return nil
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

	candidates := dmPeerCandidates(conversations, removal.UID, removal.SpaceID)
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
// 口径直接对齐白名单推导（person_whitelist.go 的 derivePersonWhitelist）：
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
func (f *Friend) cutOffDM(ctx *config.Context, spaceID, removedUID, peer string, cutInbound, cutOutbound, selfIsBot bool) error {
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
	bot, err := f.eitherSideIsBot(ctx, selfIsBot, peer)
	if err != nil {
		return fmt.Errorf("check bot dm (%s/%s): %w", removedUID, peer, err)
	}
	if bot {
		// 与裸频道一样上抛失败，不再 best-effort。
		//
		// 之前吞掉的理由是「这两个频道未必存在」。实测（WuKongIM v2.2.4-20260313，
		// 与 CI 同 tag）对完全不存在的频道调 /channel/whitelist_remove 返回
		// HTTP 200 {"status":200}，所以「频道不存在」根本不会走到错误分支——非 200
		// 一律是真故障（网络、鉴权、broker 不可用），正是该重试的那一类。
		//
		// 重试也确实能生效：本步骤的范围 dmPeersInSpace 走 MembersEverInSpace，
		// 不看成员状态，所以下一次尝试仍会枚举到同一批对端并重跑到这里；
		// IMWhitelistRemove 本身幂等，重跑没有副作用。
		if cutInbound {
			if err := f.removeSpaceScopedWhitelist(ctx, spaceID, peer, removedUID); err != nil {
				return err
			}
		}
		if cutOutbound {
			if err := f.removeSpaceScopedWhitelist(ctx, spaceID, removedUID, peer); err != nil {
				return err
			}
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

// removeSpaceScopedWhitelist 摘 Space 前缀频道（bot 私聊）的白名单。
// 失败上抛，交由工单重试——理由见 cutOffDM 里的调用点。
func (f *Friend) removeSpaceScopedWhitelist(ctx *config.Context, spaceID, channelUID, uid string) error {
	channelID := spacepkg.BuildChannelID(spaceID, channelUID)
	if channelID == channelUID {
		return nil // 没有 Space 前缀可加，裸频道上面已经处理过了
	}
	if err := ctx.IMWhitelistRemove(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	}); err != nil {
		return fmt.Errorf("remove space-scoped whitelist (%s): %w", channelID, err)
	}
	return nil
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
//
// selfIsBot 由调用方在进入对端循环之前算好一次传进来：「本人是不是 bot」对所有
// 对端都是同一个答案，放在这里查就会随对端数量线性重复。对端那次没法省。
func (f *Friend) eitherSideIsBot(ctx *config.Context, selfIsBot bool, peer string) (bool, error) {
	if selfIsBot {
		return true, nil
	}
	return newFriendDB(ctx).isRobot(peer)
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
// 判定必须与 Person 频道白名单的推导同源（person_whitelist.go 的
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
// **先走 ParseChannelID，只在它认不出来时才用本次清理自己的 spaceID 兜底。**
// 两个方向都要防：
//
//   - 只靠 ParseChannelID 不行。它先查全局 knownSpaceIDs 缓存，不中才回落到
//     `^s[0-9a-f]{32}_` 正则。loadKnownSpaceIDs 只装 status=1 的 Space，而解散/封禁
//     路径恰恰在跑清理**之前**刷新那份缓存（modules/space/api.go 的 disbandSpace
//     与管理端强制解散）。于是对一个 id 不是 32 位 hex 的老 Space（例如
//     minglue_default，见 pkg/space/channel_test.go），解散之后
//     "sminglue_default_botfather" 既不中缓存也不中正则，整条 channel_id 被当成对端，
//     永远匹配不上 space_member.uid，那条 bot 私聊被静默跳过而工单标 done。
//
//   - 但也不能反过来让本次的 spaceID 无条件优先。knownSpaceIDs 是按长度倒序排的，
//     正因为 Space id 可以含 "_"、一个 id 可能是另一个的 "_" 分隔前缀
//     （pkg/space/channel.go）。若 minglue 与 minglue_default 同时存在、本次清理的是
//     minglue，无条件剥前缀会把 "sminglue_default_botfather" 剥成
//     "default_botfather"，而正确答案是 "botfather" —— 又回到同一种静默跳过，
//     而且这次连**活跃** Space 都会中招。
//
// 所以顺序是：能认出来的交给最长前缀匹配，认不出来（sid 为空）才用工单自己的
// spaceID 兜底。这样既不依赖那份可变全局状态来处理本 Space 的频道，也不覆盖它
// 对其他 Space 的正确判断。
//
// 保持入参顺序输出，方便调用方的日志和测试断言稳定。
func dmPeerCandidates(conversations []*config.SyncUserConversationResp, selfUID, spaceID string) []string {
	prefix := ""
	if spaceID != "" {
		prefix = spacepkg.SpaceChannelPrefix + spaceID + "_"
	}
	candidates := make([]string, 0, len(conversations))
	seen := make(map[string]bool, len(conversations))
	for _, conv := range conversations {
		if conv == nil || conv.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}
		sid, peer := spacepkg.ParseChannelID(conv.ChannelID)
		if sid == "" && prefix != "" && strings.HasPrefix(peer, prefix) {
			peer = peer[len(prefix):]
		}
		if peer == "" || peer == selfUID || seen[peer] {
			continue
		}
		seen[peer] = true
		candidates = append(candidates, peer)
	}
	return candidates
}

// ---------- 加入侧：把 dm_cutoff 摘掉的白名单补回来 ----------

// spaceMemberDMRestoreStepName 恢复步骤名，用于日志。
const spaceMemberDMRestoreStepName = "dm_restore"

// restoreSpaceMemberDMs 是 cleanupSpaceMemberDMs 的逆操作：成员（重新）加入
// Space 之后，把因为上一次移除而被摘掉、如今又重新获得授权的 Person 频道
// 白名单补回去。
//
// 为什么必须有这一步：WuKongIM 的白名单存储只被 whitelist_add / whitelist_remove
// 两个主动 API 改动，注册给它的 IMDatasource.Whitelist 回调在 CI pin 的版本
// （v2.2.4-20260313）里从不被调用。实测确认过这条链路：
//   - whitelistOffOfPerson=false 时，没有白名单条目的发送被拒
//     （ReasonNotInWhitelist=13），whitelist_add 之后被接受，whitelist_remove
//     之后再次被拒；
//   - 该开关默认为 true，此时白名单完全不参与判定，摘和补都不影响发送。
//
// 换句话说：开着白名单校验的部署里，只摘不补 = 踢出再加回来的两个人**永久**
// 发不了私聊；关着的部署里，这一步和 dm_cutoff 一样是空转。两种情况下补回来
// 都是正确且无害的。
//
// 范围与 dm_cutoff 逐一对应，两半各自同源 —— 但两半的范围**不**一样，
// 不要把它读成「补回来的正好等于摘掉的那些人」：
//   - 覆写那一半（reconcileRemovedMemberChannel）两侧调的是同一个函数、同一条规则，
//     所以他自己频道上的集合确实是精确还原的；代价是它会把**所有**共同成员写进去，
//     包括从没聊过的人 —— 那是推导规则本来就规定的授权，不是这一步开的口子。
//   - 逐对端那一半仍然走 dmPeersInSpace（会话列表 ∩ 曾是本 Space 成员），
//     范围小于覆写那一半。它补的是对端频道，而对端频道上的条目是覆写够不到的。
//
// 换句话说：这里不声称「摘和补严格互逆」。严格互逆的只有覆写那一半。
func (f *Friend) restoreSpaceMemberDMs(ctx *config.Context, rejoin space.MemberRejoin) error {
	var firstErr error

	// 与 dm_cutoff 对称，且**必须**对称：移除侧现在会把他自己的频道整个覆写
	// （reconcileRemovedMemberChannel），摘掉的范围严格大于逐对端枚举能摘的范围。
	// 恢复侧若只补枚举得到的那几个对端，就会补不回覆写摘掉的那些人——
	// 这正是这个 PR 反复踩到的「守卫只落在镜像的一半」。
	// 同一个函数、同一条推导规则：他重新成为成员之后，coMembers 里又有那些人了。
	if err := f.reconcileRemovedMemberChannel(ctx, rejoin.UID); err != nil {
		f.Warn("覆写加入成员的 Person 频道白名单失败（best-effort）",
			zap.Error(err), zap.String("spaceId", rejoin.SpaceID), zap.String("uid", rejoin.UID))
		firstErr = err
	}

	peers, err := f.dmPeersInSpace(ctx, space.MemberRemoval{SpaceID: rejoin.SpaceID, UID: rejoin.UID})
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	if len(peers) == 0 {
		return firstErr
	}

	selfIsBot, err := newFriendDB(ctx).isRobot(rejoin.UID)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("check whether joining member is a bot: %w", err)
		}
		return firstErr
	}

	for _, peer := range peers {
		// 授权判定不在这里做——它整个搬进了 restoreDM，紧挨着写。
		// 在这里算好再传进去，等于给「读到的授权已经失效、却照样写下去」留出一段
		// 窗口，而且那段窗口**只有加入者一侧**能被 restoreDM 的守卫看见，
		// 对端一侧在任何窗口尺寸下都没有覆盖。理由详见 restoreDM 的说明。
		if _, err := f.restoreDM(ctx, rejoin.SpaceID, rejoin.UID, peer, selfIsBot); err != nil {
			f.Warn("恢复私聊白名单失败",
				zap.Error(err), zap.String("spaceId", rejoin.SpaceID),
				zap.String("uid", rejoin.UID), zap.String("peer", peer))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// dmDirectionAuthorized 判断「sender 现在是否被允许发给 channelUID」。
//
// 恰好是 dmDirectionRevoked 的反面，口径同源：channelUID 的 Person 频道白名单 =
// friends(channelUID, is_alone=0) ∪ coMembers(channelUID)。共处 Space 已由
// 调用方算好传进来（对两个方向都成立），所以这里只需再看好友这一条。
func (f *Friend) dmDirectionAuthorized(ctx *config.Context, channelUID, sender string, shared bool) (bool, error) {
	if shared {
		return true, nil
	}
	friend, err := newFriendDB(ctx).isMutualFriend(channelUID, sender)
	if err != nil {
		return false, fmt.Errorf("query friendship (%s -> %s): %w", channelUID, sender, err)
	}
	return friend, nil
}

// restoreDM 按方向把 Person 频道白名单补回去。
//
// 与 cutOffDM 只有一半对称，别当成镜像：cutOffDM 的两个方向由调用方算好后作为参数
// 传进来，这里则在函数内部现算（理由见下面的顺序说明）。这个不对称是刻意的，
// 也是安全的那一侧——摘多了 fail closed，补多了才是越权。
//
// **授权判定就在这里做，紧挨着写。** 恢复是在加入路径上异步发出的（见
// modules/space 的 restoreAfterRejoin），判定与写 IM 之间每多隔一次查询，
// 就多一段「读到的授权已经失效、却照样写下去」的窗口：这中间任一方若被移除，
// 对应的 dm_cutoff 会正确摘掉白名单，而这条陈旧的恢复随后把它**加回去**——
// 工单已标 done，再没有任何东西会来摘第二次，于是留下一对永久越权的私聊。
//
// 早先的写法把 shared / grantInbound / grantOutbound 全在调用方循环里算好再传进来，
// 于是**只有加入者那一侧**被重查，对端一侧在任何窗口尺寸下都得不到覆盖：对端被
// 移除、它自己的 dm_cutoff 跑完标 done，加入者仍是活跃成员，陈旧的 add 照写不误。
// 三次读移进来之后两个方向对称，窗口都是「这几次查询到紧随其后的 IM 调用」。
//
// 与 cutOffDM 那侧的间隙不同：切断侧的交错是自相矛盾的（若加入早到，切断自己的
// 读就会看到 shared=true 而整对跳过），恢复侧不是——恢复的读合法地看到了活跃
// 成员，之后全是延迟，goroutine 一被调度器压住窗口就任意放大。
//
// **窗口不是零**，这里不声称任何不变量：这几次读和下面几次 IM 调用之间仍有间隙，
// 落进去依然会写出一条已经失效的授权。彻底关闭要靠成员纪元（读时取纪元、写时
// 带纪元条件），见 issue #797。
//
// 返回 (false, nil) 表示「判定下来无事可做或已失效」，调用方据此跳过。
func (f *Friend) restoreDM(ctx *config.Context, spaceID, uid, peer string, selfIsBot bool) (bool, error) {
	// 顺序是刻意的，每一步都紧挨着它真正守护的那个写：
	//
	//  1. 共处 / 逐方向判定 —— 裸 Person 频道的授权**就是**它们
	//     （friends ∪ coMembers(任一活跃 Space)）。CheckMembership 从来不是裸频道
	//     的正确守卫：本 Space 之外的共处或好友关系一样授权这对私聊。
	//  2. 裸频道写。
	//  3. CheckMembership —— 只有 s{spaceID}_ 前缀频道是**本 Space 作用域**的，
	//     把它绑在本 Space 成员身份上的只有这一个判断。所以它必须紧挨着那两个写，
	//     而不是隔着四次查询和两次 broker 往返（上一轮把三个授权读挪进来时，
	//     正是这一点被挪坏了：对端侧窗口从 1 次查询降到 0，加入者侧却从 0 涨到 4）。
	//  4. 顺带的好处：两个方向都不授权时整个函数在第 1 步就返回，
	//     CheckMembership 与 isRobot 都不会跑 —— 一次没什么可补的重新加入
	//     不再为每个会话对端各发一次多余查询。
	//
	// 仍然不是零窗口，也不声称是：真正的关闭方式是 space_member 上的成员纪元
	// （#797），不是重排。
	shared, err := spacepkg.SharesActiveSpace(ctx.DB(), uid, peer)
	if err != nil {
		return false, fmt.Errorf("check shared space: %w", err)
	}
	grantInbound, err := f.dmDirectionAuthorized(ctx, peer, uid, shared) // 加入者 -> 对端
	if err != nil {
		return false, err
	}
	grantOutbound, err := f.dmDirectionAuthorized(ctx, uid, peer, shared) // 对端 -> 加入者
	if err != nil {
		return false, err
	}
	if !grantInbound && !grantOutbound {
		return false, nil
	}

	if grantInbound {
		// 加入者 -> 对端：把 uid 加回对端频道
		if err := f.addPersonWhitelist(ctx, peer, uid); err != nil {
			return false, err
		}
	}
	if grantOutbound {
		// 对端 -> 加入者：把 peer 加回加入者频道
		if err := f.addPersonWhitelist(ctx, uid, peer); err != nil {
			return false, err
		}
	}

	// bot 私聊走 Space 前缀频道，与 cutOffDM 一致地一并处理。
	// 这两个写是本 Space 作用域的，所以成员身份判断放在它们正前面。
	stillMember, err := spacepkg.CheckMembership(ctx.DB(), spaceID, uid)
	if err != nil {
		return false, fmt.Errorf("re-check membership before space-scoped dm restore: %w", err)
	}
	if !stillMember {
		f.Info("加入者已不再是活跃成员，跳过 Space 前缀频道的白名单回补",
			zap.String("spaceId", spaceID), zap.String("uid", uid), zap.String("peer", peer))
		return true, nil
	}

	bot, err := f.eitherSideIsBot(ctx, selfIsBot, peer)
	if err != nil {
		return false, fmt.Errorf("check bot dm (%s/%s): %w", uid, peer, err)
	}
	if bot {
		if grantInbound {
			if err := f.addSpaceScopedWhitelist(ctx, spaceID, peer, uid); err != nil {
				return false, err
			}
		}
		if grantOutbound {
			if err := f.addSpaceScopedWhitelist(ctx, spaceID, uid, peer); err != nil {
				return false, err
			}
		}
	}

	// 两端各推一条 channelUpdate，让客户端把发送框重新点亮
	// （dm_forbidden 是按请求现算的，客户端重拉即可）。
	f.notifyDMChannelUpdate(ctx, uid, peer)
	f.notifyDMChannelUpdate(ctx, peer, uid)
	return true, nil
}

// addPersonWhitelist 把 uid 加进 channelUID 的 Person 频道白名单。
func (f *Friend) addPersonWhitelist(ctx *config.Context, channelUID, uid string) error {
	err := ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   channelUID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})
	if err != nil {
		return fmt.Errorf("add person whitelist (%s): %w", channelUID, err)
	}
	return nil
}

// addSpaceScopedWhitelist 补 Space 前缀频道（bot 私聊）的白名单。
func (f *Friend) addSpaceScopedWhitelist(ctx *config.Context, spaceID, channelUID, uid string) error {
	channelID := spacepkg.BuildChannelID(spaceID, channelUID)
	if channelID == channelUID {
		return nil // 没有 Space 前缀可加，裸频道上面已经处理过了
	}
	if err := ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	}); err != nil {
		return fmt.Errorf("add space-scoped whitelist (%s): %w", channelID, err)
	}
	return nil
}
