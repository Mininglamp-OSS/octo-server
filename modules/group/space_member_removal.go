package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
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
// 幂等：群集合来自 queryAllGroupsWithMemberUIDAndSpaceID，它只返回 is_deleted=0 的成员行，
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

	groups, err := g.db.queryAllGroupsWithMemberUIDAndSpaceID(removal.UID, removal.SpaceID)
	if err != nil {
		return fmt.Errorf("query groups of removed space member: %w", err)
	}
	if len(groups) == 0 {
		return nil
	}

	operatorName := g.resolveOperatorName(removal.OperatorUID)

	var firstErr error
	for _, groupModel := range groups {
		if groupModel == nil || groupModel.Status == GroupStatusDisband {
			continue
		}
		if err := g.exitSpaceMemberFromGroup(
			groupModel.GroupNo, removal, operatorName, groupModel.ProjectID,
		); err != nil {
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
// 普通群的群主必须先交接、再走移除；当前 Project 专属群不做 handover，
// 而是在同一事务内把失去 Space 资格的 creator 降为 common 后删除。
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
// queryAllGroupsWithMemberUIDAndSpaceID 已经查不到这个群，重跑是空转，只会把一次
// 真实故障洗成 done；把 IMRemoveSubscriber 提到删行之前也没用，那只是换一个
// 时刻失败。真正的修法是在删行的同一个事务里写一条持久化的 IM-pending 记录
// （范围不依赖 group_member 活跃行），由本 worker 消费。但它修的是
// RemoveGroupMembers 这个既有原语的所有调用方，不只是本步骤，因此单独立项。
// 见 issue #797。
//
// 上面那次 CheckMembershipForCleanup 覆盖的是「重新加入发生在读之前」，也就是真正
// 宽的那个窗口；它并不覆盖读到随后写之间的间隙。彻底关闭同样要靠成员纪元，见 #797。
func (g *Group) exitSpaceMemberFromGroup(
	groupNo string, removal spacemod.MemberRemoval, operatorName, projectID string,
) error {
	member, err := g.db.QueryMemberWithUID(removal.UID, groupNo)
	if err != nil {
		return fmt.Errorf("query group member: %w", err)
	}
	if member == nil || member.IsDeleted == 1 {
		return nil // 已经不在群里，幂等返回
	}

	// 何时抑制默认的「被 X 移出群聊」系统消息：
	//   - reason=left：操作者就是本人，默认文案会渲染成「X 被 X 移出群聊」；
	//     改发与既有 groupExit 一致的退群提示。
	//   - reason=space_disbanded：解散不会解散群，于是每个成员在每个群里各触发一次。
	//     N 个成员 × M 个群就是 N×M 条系统消息（1000 人 × 50 群 = 五万条），
	//     全都堆给最后被移除的那个人看。空间已经没了，逐个通告没有意义。
	//   - reason=bot_deleted：Bot 被其所有者整体删除。「X 被 Y 移出群聊」在这里
	//     是一句错话——没有人把它移出这个群，是这个账号不存在了。BotFather 的
	//     删除命令自己走漏斗时就传 SuppressRemoveNotice=true 并在注释里写明那是
	//     刻意保持现状；工单路径是同一次删除的另一半，必须给出同一个答案，
	//     否则「同步那遍失败了、异步这遍补上」会顺带在群里冒出那句话。
	//     要不要发一条「该 Bot 已被删除」是产品问题，两条路径都不擅自决定。
	selfExit := removal.Reason == spacemod.MemberRemoveReasonLeft
	spaceGone := removal.Reason == spacemod.MemberRemoveReasonSpaceDisbanded
	botGone := removal.Reason == spacemod.MemberRemoveReasonBotDeleted
	suppressNotice := selfExit || spaceGone || botGone

	// bot 连带移除的 Tip 动作词：自助退出说「退出了」，其余沿用默认的「被移出」。
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
		AllowProtected:        true,
		SpaceMemberRemoval:    true,
		SpaceID:               removal.SpaceID,
		ProjectID:             projectID,
	})
	if err != nil {
		if errors.Is(err, errGroupMemberNotInGroup) {
			return nil
		}
		return fmt.Errorf("remove group member: %w", err)
	}
	if resp != nil && resp.LifecycleNoop {
		return nil
	}
	// Space 资格恢复的 no-op 已在上面返回。此处
	// Removed==0 只表示锁内发现目标并非可移除角色（例如普通群并发交接）；
	// 返回错误让工单重试，而不是把仍在群里的成员误判为完成。
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

// resolveOperatorName 取操作者展示名，查不到就退回 UID。
// 只用于系统 Tip 文案，失败不该让整条清理工单重试。
func (g *Group) resolveOperatorName(operatorUID string) string {
	if operatorUID == "" {
		return ""
	}
	operator, err := g.userDB.QueryByUID(operatorUID)
	if err != nil || operator == nil {
		return operatorUID
	}
	if operator.Name == "" {
		return operatorUID
	}
	return operator.Name
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
