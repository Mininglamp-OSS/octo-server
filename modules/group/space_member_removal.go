package group

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
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
	groups, err := g.db.queryGroupsWithMemberUIDAndSpaceID(removal.UID, removal.SpaceID)
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
		if err := g.exitSpaceMemberFromGroup(groupModel.GroupNo, removal, operatorName); err != nil {
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
func (g *Group) exitSpaceMemberFromGroup(groupNo string, removal spacemod.MemberRemoval, operatorName string) error {
	member, err := g.db.QueryMemberWithUID(removal.UID, groupNo)
	if err != nil {
		return fmt.Errorf("query group member: %w", err)
	}
	if member == nil || member.IsDeleted == 1 {
		return nil // 已经不在群里，幂等返回
	}

	if member.Role == MemberRoleCreator {
		if err := g.handOverGroupCreator(groupNo, removal.UID); err != nil {
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
	spaceGone := removal.Reason == spacemod.MemberRemoveReasonSpaceDisbanded
	suppressNotice := selfExit || spaceGone

	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo: groupNo,
		Members: []string{removal.UID},
		// 操作者取 Space 侧的操作者。他可能并不是本群成员——系统 Tip 只用其展示名，
		// 不依赖群内身份。
		OperatorUID:          removal.OperatorUID,
		OperatorName:         operatorName,
		SuppressRemoveNotice: suppressNotice,
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
		g.sendGroupExitTip(groupNo, removal.UID)
	}
	return nil
}

// sendGroupExitTip 发「主动退群」提示，可见范围与既有 groupExit 一致：
// 只给一位管理员/群主看，不打扰全群。best-effort，失败只记日志——文案发不出去
// 不该让整条清理工单重试。
func (g *Group) sendGroupExitTip(groupNo, uid string) {
	adminUIDs, err := g.db.QueryGroupManagerOrCreatorUIDS(groupNo)
	if err != nil {
		g.Warn("查询群管理员失败，跳过退群提示", zap.Error(err), zap.String("groupNo", groupNo))
		return
	}
	visibles := make([]string, 0, 1)
	for _, admin := range adminUIDs {
		if admin != uid {
			visibles = append(visibles, admin)
			break
		}
	}
	if len(visibles) == 0 {
		return
	}
	showName := uid
	if member, err := g.userDB.QueryByUID(uid); err == nil && member != nil && member.Name != "" {
		showName = member.Name
	}
	if err := g.ctx.SendGroupExit(groupNo, uid, showName, visibles); err != nil {
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
func (g *Group) handOverGroupCreator(groupNo, leaverUID string) error {
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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit creator handover: %w", err)
	}
	if successor != nil {
		g.Info("Space 成员移除触发群主交接",
			zap.String("groupNo", groupNo),
			zap.String("from", leaverUID),
			zap.String("to", successor.UID))
	} else {
		g.Warn("群主被移出 Space 但群内无可继任者，群将无主",
			zap.String("groupNo", groupNo), zap.String("uid", leaverUID))
	}
	return nil
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
