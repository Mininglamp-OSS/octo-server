package group

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// spaceMemberRemovalStepName 清理步骤名，同时用于工单的 last_error 前缀。
const spaceMemberRemovalStepName = "group_cascade"

// registerSpaceMemberRemovalCleanup 把「退出该 Space 下所有群」注册为成员移除清理步骤。
//
// 由 1module.go 在模块构造时调用。反向注册而非让 space 直接调用 group：
// modules/group 已经 import modules/space，反过来 import 即成环。
func (g *Group) registerSpaceMemberRemovalCleanup() {
	spacemod.RegisterMemberRemovalCleanupStep(spaceMemberRemovalStepName, g.cleanupSpaceMemberGroups)
}

// cleanupSpaceMemberGroups 把被移出 Space 的成员从该 Space 下的每个群里清出去。
//
// 幂等：群集合来自 queryGroupsWithMemberUIDAndSpaceID，它只返回 is_deleted=0 的成员行，
// 所以已经退掉的群在重跑时天然不再命中。
//
// 单个群失败不中断其余群——部分完成是持久的（退掉的群不会再出现在下一轮的集合里），
// 最后返回首个错误让整条工单重试剩下的部分。
func (g *Group) cleanupSpaceMemberGroups(ctx *config.Context, removal spacemod.MemberRemoval) error {
	// 动手前**在这一步内**再确认一次他确实不在 Space 里了。
	//
	// worker 在认领工单时已经查过一次成员身份，但认领与本步骤真正动手之间隔着
	// 排队和其它已注册步骤的执行时间，可能是秒级的。窗口里他若重新加入，
	// joinPresetGroups 刚写好的 group_member 行就会被下面这段全部删掉，
	// 留下一个「是 Space 活跃成员、却不在任何群里」的人，而且没有任何东西会补回来。
	//
	// 这不能把窗口缩到零（这次查询和下面那次群集合查询之间仍有间隙），但把它从
	// 「认领到动手的整段时长」压到了一次查询的间隔。彻底关闭要给 space_member
	// 加成员纪元并在每一步里校验，记在 brief 的 follow-up 里。
	//
	// 谓词用 CheckMembershipForCleanup（sm.status=1 且 space.status <> 0），
	// 与 worker 外层那道门是同一个，两层必须回答同一个问题。
	//
	// 解散（status=0）判定为「席位已失效」，清理照常进行——正是所需；封禁
	// （status=2）判定为「席位仍在」，跳过清理。以前这里用 CheckMembership，
	// 它要求 space.status=1，于是一名完全在职的成员会因为空间被封禁而被拆出
	// 所有群，而 Manager.addMembers 只挡解散、往封禁空间加人是允许的。
	stillMember, err := spacepkg.CheckMembershipForCleanup(ctx.DB(), removal.SpaceID, removal.UID)
	if err != nil {
		return fmt.Errorf("re-check space membership before group cascade: %w", err)
	}
	if stillMember {
		g.Info("被移除成员已重新加入 Space，跳过群级联",
			zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID))
		return nil
	}

	groups, err := g.db.queryGroupsWithMemberUIDAndSpaceID(removal.UID, removal.SpaceID)
	if err != nil {
		return fmt.Errorf("query groups of removed space member: %w", err)
	}
	if len(groups) == 0 {
		return nil
	}

	// 解散路径既不发被移出通知、也不发群内广播、也不通告群主交接，两个展示名
	// 一个都用不上——一个几千人的 Space 解散就是几千次纯浪费的 user 查询。
	// 注意这里查的是**全局**兜底名；真正用的是群内 remark 优先，见
	// exitSpaceMemberFromGroup 里的 displayName。
	var operatorName, memberName string
	if removal.Reason != spacemod.MemberRemoveReasonSpaceDisbanded {
		operatorName = g.resolveDisplayName(removal.OperatorUID)
		memberName = g.resolveDisplayName(removal.UID)
	}

	var firstErr error
	for _, groupModel := range groups {
		if groupModel == nil || groupModel.Status == GroupStatusDisband {
			continue
		}
		if err := g.exitSpaceMemberFromGroup(groupModel.GroupNo, removal, operatorName, memberName); err != nil {
			g.Error("被移出 Space 的成员退群失败",
				zap.Error(err),
				zap.String("groupNo", groupModel.GroupNo),
				zap.String("spaceId", removal.SpaceID),
				zap.String("uid", removal.UID))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// exitSpaceMemberFromGroup 让单个群走完整的移除流程。
//
// 复用 RemoveGroupMembers 而不是自己删行：它已经承担 IM 退订、被移除系统消息、
// CMDGroupMemberUpdate、邀请人名下 bot 级联（#354 / #1186）、子区成员与订阅清理、
// 按 Space 隔离的置顶与会话扩展清理、外部群标记回收。自己写一遍必然漏项。
//
// RemoveGroupMembers 会静默跳过 role=creator 的成员，所以群主必须先交接、再走移除。
//
// ⚠️ 已知缺口：IM 退订失败会永久泄漏，且**没有**任何东西兜底。
//
// RemoveGroupMembers 内部那次 IMRemoveSubscriber（service.go）失败时只记日志。
// 早先这里写过一段注释，说 1module.go 的 IMDatasource.Subscribers 回调是权威
// 订阅源、WuKongIM 下次重载会自愈——**那是错的**，已按 CI pin 的 broker 版本
// wukongim v2.2.4-20260313 逐条核实：
//   - internal/server/server.go 里 `s.datasource = NewDatasource(s)` 是全树
//     唯一一处 `.datasource`，赋值之后再没有任何地方读它；
//     datasource.GetSubscribers / GetWhitelist 零调用者，HasDatasource() 只在
//     manager_systemaccount.go 里为 getSystemUIDs 服务。
//   - 发送侧走 broker 自己的存储：internal/service/permission.go 的
//     hasPermissionForCommChannel 查 Store.ExistSubscriber，不通过就是
//     ReasonSubscriberNotExist。
//   - 本仓库 CI 与部署都没有配置 datasource。
//
// 也就是说这个回调根本不会被调用，订阅存储只被 subscriber_add / subscriber_remove
// 两个主动 API 改动。退订失败一次，人就**永久**留在群频道里：照收推送、照能发言，
// 而工单被标成 done。db.go querySubscribableMemberUIDsWithGroupNo 上那条
// 「下次重载会把他加回来」的 YUJ-4185 注释，在这个版本同样不成立。
//
// 为什么这里没有顺手修掉：光把错误上抛没用——删行之后
// queryGroupsWithMemberUIDAndSpaceID 已经查不到这个群，重跑是空转，只会把一次
// 真实故障洗成 done；把 IMRemoveSubscriber 提到删行之前也没用，那只是换一个
// 时刻失败。真正的修法是在删行的同一个事务里写一条持久化的 IM-pending 记录
// （范围不依赖 group_member 活跃行），由本 worker 消费。但它修的是
// RemoveGroupMembers 这个既有原语的所有调用方，不只是本步骤，因此单独立项。
// 见 issue #797。
//
// 上面那次 CheckMembershipForCleanup 覆盖的是「重新加入发生在读之前」，也就是真正
// 宽的那个窗口；它并不覆盖读到随后写之间的间隙。彻底关闭同样要靠成员纪元，见 #797。
func (g *Group) exitSpaceMemberFromGroup(groupNo string, removal spacemod.MemberRemoval, operatorName, memberName string) error {
	member, err := g.db.QueryMemberWithUID(removal.UID, groupNo)
	if err != nil {
		return fmt.Errorf("query group member: %w", err)
	}
	if member == nil || member.IsDeleted == 1 {
		return nil // 已经不在群里，幂等返回
	}

	// 展示名按群取：group_member.remark 是本群内的称呼，优先于全局 user.name。
	// 这是本仓库既有的展示名规则——groupExit 就是 `loginMember.Remark` 优先
	// （api.go:3458）、花名册也渲染 Remark。用全局名会在群里播一个没人认识的名字。
	// memberName 是循环外查好的全局名兜底。
	displayName := member.Remark
	if displayName == "" {
		displayName = memberName
	}

	spaceGone := removal.Reason == spacemod.MemberRemoveReasonSpaceDisbanded

	// 群主交接**连同它的群内通告**都在 handOverGroupCreator 里完成，不要挪到本函数
	// 后半段去发。
	//
	// 交接自己提交事务，而它之后到函数返回之间还有 RemoveGroupMembers 这一大段可能
	// 失败（DB 错误、bot 级联失败、Removed==0 的并发守卫）。一旦失败，工单重试时
	// 这里读到的角色已经是 MemberRoleCommon，交接分支不再进入——通告就**永久丢失**，
	// 正是本改动要消灭的「群里凭空多出一个新群主」。
	//
	// 放在提交处则天然幂等：重试时 handOverGroupCreator 在行锁内看到已非群主，
	// 直接返回，不会重复通告。
	if member.Role == MemberRoleCreator {
		if err := g.handOverGroupCreator(groupNo, removal.UID, displayName, removal.SpaceID, spaceGone); err != nil {
			return fmt.Errorf("hand over group creator: %w", err)
		}
	}

	// 何时抑制默认的「被 X 移出群聊」系统消息：
	//   - reason=left：操作者就是本人，默认文案会渲染成「X 被 X 移出群聊」；
	//     改发与既有 groupExit 一致的退群提示。
	//   - reason=space_disbanded：解散不会解散群，于是每个成员在每个群里各触发一次。
	//     N 个成员 × M 个群就是 N×M 条系统消息（1000 人 × 50 群 = 五万条），
	//     全都堆给最后被移除的那个人看。空间已经没了，逐个通告没有意义。
	selfExit := removal.Reason == spacemod.MemberRemoveReasonLeft
	suppressNotice := selfExit || spaceGone

	// bot 连带移除的 Tip 动作词：自助退出说「退出了」，其余沿用默认的「被移出」。
	//
	// 不要把被移出 Space 的情形也改成「退出了」：那是被动的，说成主动就是错的，
	// 而且 TestGroupCascadeKickStillSendsBotTip 正是钉这条契约的正向对照。
	cascadeAction := ""
	if selfExit {
		cascadeAction = "退出了"
	}

	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo: groupNo,
		Members: []string{removal.UID},
		// 操作者取 Space 侧的操作者。他可能并不是本群成员——系统 Tip 只用其展示名，
		// 不依赖群内身份。
		OperatorUID:          removal.OperatorUID,
		OperatorName:         operatorName,
		SuppressRemoveNotice: suppressNotice,
		// bot 连带移除的 Tip 是另一条群可见持久化消息，单独控制：
		//   - 自助退出：照发（群里看见 bot 消失，有权知道原因），但动作词换成「退出了」，
		//     否则群历史里会留下一句「X 被移出群聊」，正是上面要抑制的那个措辞。
		//   - 解散：整条不发，与上面同一个 N×M 理由。
		BotCascadeTipAction:   cascadeAction,
		SuppressBotCascadeTip: spaceGone,
	})
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	// 必须检查 Removed，不能只看 error。
	//
	// RemoveGroupMembers 对群主是**静默跳过 + 返回 nil**。上面那次
	// QueryMemberWithUID 是无锁读，目标可能在读到调用之间被提升为群主
	// （群主转让接口，或另一条清理工单为它做的交接）——那样这次移除就是一次
	// 无声的空操作，而工单会被标成 done：人永久留在群里、IM 订阅还在，
	// 却再没有任何东西会回来看一眼。
	// 返回错误让工单重试：下一次尝试读到 role=creator，先交接再移除，收敛。
	if resp == nil || resp.Removed == 0 {
		return fmt.Errorf("group member not removed, role changed concurrently: group=%s uid=%s",
			groupNo, removal.UID)
	}
	if selfExit {
		// 群内备注必须用**移除前**读到的这一行：sendGroupExitTip 跑在
		// RemoveGroupMembers 之后，那时 QueryMemberWithUID（where is_deleted=0）
		// 已经查不到人了。
		g.sendGroupExitTip(groupNo, removal.UID, member.Remark)
	}
	// 普通成员被移出 Space 时**不**向全群广播——刻意的产品取舍，别再加回来。
	//
	// 「某人走了」在成员列表里看得见，而「群主换人了」看不见：信息量不对等，所以
	// 只有后者值得一条群消息（在 handOverGroupCreator 里发）。
	//
	// 反过来做（每个被移除成员都广播一条）会把两个批量入口直接变成消息洪水：
	// members/remove 与管理端 removeMembers 各自最多 200 个 uid
	// （managerMaxBatchUIDs），每个 uid 一条工单、每条工单遍历其所有群，
	// 200 人 × 50 群 = 一万条 NoPersist=0 的永久群消息，量级与解散被抑制的理由相同。
	// 被移除者本人仍会收到 RemoveGroupMembers 发的私人通知（「你被{0}移除群聊」），
	// 群内成员列表则由 CMDGroupMemberUpdate 静默刷新。
	return nil
}

// sendGroupExitTip 发「主动退群」提示，可见范围与既有 groupExit 一致：
// 全员可见 + RedDot:0（见 sendGroupExitNotice）。best-effort，失败只记日志——
// 文案发不出去不该让整条清理工单重试。
//
// 此前这里要先查管理员、把 `visibles` 白名单收窄到一位管理员/群主，且在群里没有
// 其他管理员时直接 return（提示被静默吞掉）。可见性白名单去掉后这两步都不再需要：
// 无其他管理员时同样照发。
func (g *Group) sendGroupExitTip(groupNo, uid, groupRemark string) {
	showName := resolveExitShowName(groupRemark, func() string {
		if member, err := g.userDB.QueryByUID(uid); err == nil && member != nil {
			return member.Name
		}
		return ""
	})
	if err := sendGroupExitNotice(g.ctx, groupNo, uid, showName); err != nil {
		g.Warn("发送退群提示失败", zap.Error(err), zap.String("groupNo", groupNo))
	}
}

// handOverGroupCreator 把群主交接给第二元老（排除 bot），并把离开者降为普通成员。
//
// 两步必须在同一事务里：只提升继任者而没降走原群主会出现两个 creator；只降原群主
// 而没提升继任者会留下无主群（无继任者时这是可接受的终局，见下）。
//
// 没有可继任者（群里只剩他自己，或只剩 bot）时仍然要把他降为普通成员，否则
// RemoveGroupMembers 会跳过 creator，人就永远留在群里了。此时群成为无主空群，
// 与既有 groupExit 在同样情形下的终局一致。
// 交接成功后**在本函数内**通告全群，不把继任者回传给调用方去发：调用方到函数返回
// 之间还有 RemoveGroupMembers 可能失败，而重试时离开者已被降级、不再走进交接分支，
// 通告就永久丢了。放在这里则天然幂等——重试时锁内重读已非 creator，直接返回。
//
// suppressNotice=true 时只做交接、不通告（解散场景，见调用方）。
func (g *Group) handOverGroupCreator(groupNo, leaverUID, leaverName, spaceID string, suppressNotice bool) error {
	tx, err := g.db.session.Begin()
	if err != nil {
		return fmt.Errorf("begin creator handover: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// 事务内、行锁下重读离开者的角色。调用方那次 QueryMemberWithUID 是无锁读，
	// 而租约到期后同一条工单可能被另一个 worker 并发重跑：两边都读到 creator，
	// 就会各自提升一个继任者，群里留下两个 role=creator 的行。而
	// RemoveGroupMembers 会静默跳过 creator，于是那个成员被永久卡在群里。
	// 锁内重读后若已不是 creator，说明别人刚交接过，直接当成功返回。
	var roles []int
	if _, err = tx.SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0 FOR UPDATE",
		groupNo, leaverUID,
	).Load(&roles); err != nil {
		return fmt.Errorf("re-read leaver role: %w", err)
	}
	if len(roles) == 0 || roles[0] != MemberRoleCreator {
		return nil // 已经不是群主（并发交接已完成 / 人已不在群）
	}

	// 继任者也在同一事务内查，避免用事务外的陈旧快照
	successor, err := querySecondOldestNonBotMemberTx(tx, groupNo, leaverUID)
	if err != nil {
		return fmt.Errorf("query successor: %w", err)
	}

	if successor != nil {
		version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return fmt.Errorf("generate successor version: %w", err)
		}
		if err := g.db.UpdateMemberRoleTx(groupNo, successor.UID, MemberRoleCreator, version, tx); err != nil {
			return fmt.Errorf("promote successor: %w", err)
		}
	}

	version, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return fmt.Errorf("generate leaver version: %w", err)
	}
	if err := g.db.UpdateMemberRoleTx(groupNo, leaverUID, MemberRoleCommon, version, tx); err != nil {
		return fmt.Errorf("demote leaver: %w", err)
	}

	// 继任者是否也在待移除队列里 —— **在事务内**查，理由见下面通告处的注释。
	// 只查、不据此改变事务的成败：查询失败按「不在队列里」处理，照常通告。
	successorPending := false
	if successor != nil && !suppressNotice {
		if pending, perr := spacemod.HasPendingRemovalCleanup(tx, spaceID, successor.UID); perr != nil {
			g.Warn("查询继任者待移除状态失败，按未待移除处理",
				zap.Error(perr), zap.String("groupNo", groupNo), zap.String("successor", successor.UID))
		} else {
			successorPending = pending
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit creator handover: %w", err)
	}
	if successor == nil {
		g.Warn("群主被移出 Space 但群内无可继任者，群将无主",
			zap.String("groupNo", groupNo), zap.String("uid", leaverUID))
		return nil
	}
	g.Info("Space 成员移除触发群主交接",
		zap.String("groupNo", groupNo),
		zap.String("from", leaverUID),
		zap.String("to", successor.UID))
	if suppressNotice {
		return nil
	}

	// 继任者自己也在待移除队列里 → 这次交接是链条中间的一环，当场就会作废，不通告。
	//
	// 批量移除按 uid 逐条建工单，若被移除的几个人正好是本群里连续的元老，交接会沿元老
	// 顺序连锁：C→S2、S2→S3、S3→S4，一个群里三条「已成为新群主」，前两条在写下时就
	// 已经不成立。两个批量入口各自上限 200 uid（managerMaxBatchUIDs），最坏 200 条。
	// 这与解散被抑制的机制完全相同，只是触发方式不同。
	//
	// 跳过中间环之后，只有最后一环（继任者不在队列里）会通告，且通告的正是最终群主。
	//
	// 链条能否收敛成一条，取决于「同批工单在任何 worker 起跑前是否全部可见」。
	// 三个批量入口现在都满足（PR #804 round-4/5 review 查证 + 修复）：
	//
	//   - 解散：enqueueMemberRemovalCleanupBatchTx，单事务原子入队。解散本来就整条
	//     不通告（suppressNotice），这里用不上。
	//   - 超管强制移除 removeMembersForce：一个事务里翻完全部成员行、入完队再提交。
	//   - 用户端 members/remove → removeMembersLocked：**本次改动**。此前是
	//     removeMemberLocked 一人一事务逐个提交，前提不成立，reason=kicked 又不抑制，
	//     于是 10s tick 落在循环中途就会读到尚未入队的兄弟工单，把「马上要被移除的
	//     继任者」当成「不在队列」，发出写下时就已作废的通告。实测：整批可见 → 1 条；
	//     逐条提交且每轮都被 tick 命中 → 3 条；只有首次提交后一个 tick → 2 条。
	//     现已改为整批单事务，见 space/db_manager.go removeMembersLocked。
	//
	// 那个缺口还会与下面「抑制只判定一次」复合成更坏的结果：若最终继任者自己挂着一条
	// pending 工单，最后一环反而被抑制，于是群历史里**最后**一条群主消息指向一个已经
	// 不在群里的人，而真正的群主从未被通告。整批原子入队之后这个复合不再成立——最坏
	// 退化成「一条都不发」，即基线的静默，不比 main 差。
	//
	// 守这条的是一对测试，缺一不可：
	//   - space/TestRemoveMembersLockedEnqueuesAtomically 用一把竞争行锁把整批卡在
	//     中间，断言此刻一条工单都不可见（变异回逐条提交立刻变红）——它证明前提；
	//   - group/TestGroupCascadeLastNoticeNamesActualOwner 在前提成立时断言两种结局
	//     都不说错话（继任者挂陈旧 pending → 静默；正常 → 恰好一条且点名最终群主）。
	// 只看后者会把前提当结论，见 learnings/pending/a-test-can-encode-the-premise.md。
	//
	// 另一个**互相独立**的窗口：多副本。本次一并修掉。
	//
	// 早先这个检查跑在 tx.Commit() 之后，那时已不再持有继任者的行锁：若 worker A 在
	// 提交后、检查前停顿足够久（约 100ms 量级），worker B 可以把 S2 的整条工单跑完并
	// 置为 done，A 随后读到 done → 不抑制 → 发出已作废的 C→S2。它与上面那条一样，
	// 也能和「抑制只判定一次」复合成「最后一条消息指向已离开的人」。
	//
	// 现在检查挪进了事务（发送仍留在提交后）：事务内 A 仍持着 S2 的行锁，B 连自己交接
	// 的第一次 FOR UPDATE 都过不去，读到的必然是 pending。已实测确认该行锁确实挡住
	// 兄弟工单。不需要任何测试钩子——HasPendingRemovalCleanup 收 dbr.SessionRunner，
	// *dbr.Tx 本身就满足（早先注释说"需要在生产代码里加测试钩子"，那是错的）。
	//
	// 注意查询**只影响“发不发通告”，不主动改变事务成败**：它失败时按「不在队列里」
	// 处理并照常通告，代码本身不会因为一次 COUNT 出错去回滚交接。
	// （更精确地说：若是连接级/死锁级失败，紧接着的 tx.Commit() 同样会失败、交接随之
	//  回滚——但那是安全的，工单会重试；若只是良性失败，事务完好、按 fail-open 通告。
	//  总之不会出现「交接半提交」。）
	//
	// ⚠️ 这一处**没有**确定性红测试守着，和上面那条不一样。把它挪回提交之后，现有用例
	// 全部照绿——因为要自然复现需要 A 在一条 COMMIT 和一次带索引的 COUNT 之间停顿到
	// 足够 B 跑完一整条工单（实测 30 轮并发，多余通告 0 次）。它的正确性依据是行锁
	// 论证本身，不是一条会变红的用例。改动这一段时请重新推导，别指望测试拦你。
	//
	// 查询失败按「不在队列里」处理并照常通告：宁可多发一条，也不要因为一次 DB 抖动
	// 把唯一一条有效通告吞掉——群里凭空换群主正是本改动要消灭的。
	if successorPending {
		g.Info("继任者本人也在待移除队列，跳过本环交接通告",
			zap.String("groupNo", groupNo), zap.String("successor", successor.UID))
		return nil
	}

	// 报文形状与取舍见 sendSpaceRemovalHandoverNotice 的文档注释。
	//
	// 展示名取群内 remark 优先，与 groupExit / 花名册一致；successor 是事务里
	// FOR UPDATE 选出的那一行，Remark 直接可用，无需再查一次库。
	successorName := successor.Remark
	if successorName == "" {
		successorName = g.resolveDisplayName(successor.UID)
	}
	// best-effort：交接已经提交，通告发不出去不该让整条工单重试——重跑时锁内重读
	// 已非 creator，只会空转到 abandoned，而交接本身是对的。
	if err := g.sendSpaceRemovalHandoverNotice(
		groupNo, leaverUID, leaverName, successor.UID, successorName); err != nil {
		g.Warn("发送群主交接通告失败",
			zap.Error(err), zap.String("groupNo", groupNo), zap.String("to", successor.UID))
	}
	return nil
}

// sendSpaceRemovalHandoverNotice 发「谁走了、于是谁接手」的群内通告。
//
// 为什么不直接用 octo-lib 的 SendGroupTransferGrouper：它的文案写死成单句
// 「“{0}”已成为新群主」，而级联场景需要把**原因**一并交代——群里看到的是
// 「换了群主」这个结果，缺了「因为原群主离开了空间」就没头没尾。手动转让不需要
// 这个前半句（操作者就在群里、是主动行为），所以那条路径保持原样、不受影响。
//
// 仍然沿用同一个 content type GroupTransferGrouper(1008)：
//   - 客户端按同一条路径渲染（octo-web module.tsx 把 1000-2000 统一交给 SystemCell，
//     1008 另被 Model.tsx 列入「不计未读的系统消息」），不会出现同一件事两种样子；
//   - 名字放 extra 走 {N} 占位符、**不拼进 content**，所以用户可控的展示名没有注入面，
//     也不需要自己做净化截断。
//
// 结构化只买到「无注入面」这一半，**买不到**「改名后不留旧名」：三端客户端
// （iOS WKSystemContent、Android StringUtils、Web SDK）都直接渲染 extra[i].name，
// 没有一端按 uid 重新解析，NoPersist=0 的历史消息里留的仍是发送时刻的名字。
// extra 带 uid 让重解析成为可能，但今天没有客户端这么做。
//
// 两个占位符不是新发明：octo-lib SendGroupMemberScanJoin(1007) 的
// 「“{0}”通过“{1}”的二维码加入群聊」就是 {0}/{1} + 两元素 extra 的既有写法；
// 三端都按 extra 长度泛化替换，无 1008 专属渲染逻辑（PR #804 review 逐端核对）。
//
// ⚠️ i18n 注意：Android 用 MessageFormat，ASCII 单引号 ' 是转义字符。当前中文全角
// 引号安全，但将来译成英文若写成 the group's owner，占位符只在 Android 端被静默吞掉。
func (g *Group) sendSpaceRemovalHandoverNotice(groupNo, leaverUID, leaverName, successorUID, successorName string) error {
	if leaverName == "" {
		leaverName = leaverUID
	}
	if successorName == "" {
		successorName = successorUID
	}
	return g.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			NoPersist: 0,
			RedDot:    1, // 与 octo-lib 全部群系统消息一致
			SyncOnce:  0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": `“{0}”已离开当前空间，“{1}”已成为新群主`,
			"extra": []config.UserBaseVo{
				{UID: leaverUID, Name: leaverName},
				{UID: successorUID, Name: successorName},
			},
			"type": common.GroupTransferGrouper,
		})),
	})
}

// resolveDisplayName 取展示名，查不到就退回 UID。
// 只用于系统 Tip 文案，失败不该让整条清理工单重试。
func (g *Group) resolveDisplayName(uid string) string {
	if uid == "" {
		return ""
	}
	u, err := g.userDB.QueryByUID(uid)
	if err != nil || u == nil {
		return uid
	}
	if u.Name == "" {
		return uid
	}
	return u.Name
}

// querySecondOldestNonBotMemberTx 是 DB.QuerySecondOldestMemberExcludingBotsOf 的
// 事务内版本，语义完全一致（含「他人的 bot 仍可继任」这条被
// TestQuerySecondOldestMemberExcludingBotsOf_OnlyBotsLeft 钉住的既有契约）。
// 单独一份是为了让选主与角色重校验落在同一个事务、同一份读视图里。
func querySecondOldestNonBotMemberTx(tx *dbr.Tx, groupNo, leaverUID string) (*MemberModel, error) {
	var member *MemberModel
	// FOR UPDATE 锁住选中的继任者：不锁的话它可能正被另一条清理工单删除，
	// UpdateMemberRoleTx 的 WHERE 带 is_deleted=0，会静默影响 0 行，
	// 群就此无主。
	_, err := tx.SelectBySql(
		"SELECT gm.* FROM group_member gm "+
			"LEFT JOIN robot r ON r.robot_id = gm.uid AND r.status = 1 AND r.creator_uid = ? "+
			"WHERE gm.group_no = ? AND gm.role <> ? AND gm.is_deleted = 0 "+
			"AND r.robot_id IS NULL "+
			"ORDER BY gm.created_at ASC LIMIT 1 FOR UPDATE",
		leaverUID, groupNo, MemberRoleCreator,
	).Load(&member)
	return member, err
}
