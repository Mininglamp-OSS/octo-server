package group

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
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

	_, err = g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo: groupNo,
		Members: []string{removal.UID},
		// 操作者取 Space 侧的操作者。他可能并不是本群成员——系统 Tip 只用其展示名，
		// 不依赖群内身份。自助退出（reason=left）时 OperatorUID 等于被移除者本人。
		OperatorUID:  removal.OperatorUID,
		OperatorName: operatorName,
	})
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	return nil
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
	successor, err := g.db.QuerySecondOldestMemberExcludingBotsOf(groupNo, leaverUID)
	if err != nil {
		return fmt.Errorf("query successor: %w", err)
	}

	tx, err := g.db.session.Begin()
	if err != nil {
		return fmt.Errorf("begin creator handover: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

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
