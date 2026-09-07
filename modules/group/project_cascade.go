package group

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"go.uber.org/zap"
)

// Project → Group cascade.
//
// Two steps, both reverse-registered into modules/project because
// modules/project must never import modules/group:
//
//   - projectMemberRemovalStepName: a project seat closed, so that uid leaves
//     every group of that project;
//   - projectDisbandStepName: a project ended, so its groups revert to
//     Space-direct with their members untouched.
//
// Both satisfy the step contract stated on modules/project's registry:
// idempotent, self-deciding, and safe to re-run after a partial failure.
const (
	projectMemberRemovalStepName = "group_project_detach"
	projectDisbandStepName       = "group_project_revert"
)

// registerProjectCascadeSteps is called from 1module.go at construction, beside
// the Space-side registrations.
func (g *Group) registerProjectCascadeSteps() {
	projectmod.RegisterProjectMemberRemovalStep(projectMemberRemovalStepName, g.detachMemberFromProjectGroups)
	projectmod.RegisterProjectDisbandStep(projectDisbandStepName, g.revertProjectGroupsToSpace)
}

// detachMemberFromProjectGroups removes uid from every group of the project
// whose seat is closing.
//
// # Idempotence
//
// The group set comes from queryProjectGroupNosWithActiveMember, which only
// returns groups where the uid still has an undeleted member row. A re-run after
// a partial failure therefore sees a shorter list, and a complete re-run sees an
// empty one and does nothing.
//
// # Why RemoveGroupMembers rather than a delete
//
// Removal is not "set is_deleted = 1". RemoveGroupMembers also unsubscribes from
// the IM channel, emits CMDGroupMemberUpdate so clients update their member
// lists, cascades to bots the departing member invited, cleans up thread_member
// and thread_setting, clears Space-scoped pinned messages and conversation
// extras, and reclaims the group's is_external_group flag. Every one of those is
// an omission waiting to happen in a reimplementation, and
// modules/group/space_member_removal.go says so in as many words about the
// Space-side cascade.
//
// SuppressRemoveNotice is set: "X was removed by Y" is the wrong sentence. The
// operator here is whoever closed the project seat, and the group is not where
// that decision was made or explained. BotCascadeTipAction is left at its
// default, so a bot the departing member brought in still produces its own tip —
// group members seeing a bot vanish are owed the reason, which is a separate
// question from who removed whom.
//
// # The group-creator case
//
// RemoveGroupMembers SILENTLY SKIPS role = creator. Left alone, a project member
// who owns a group would keep their group_member row forever while their project
// seat closed, which is an I2 violation the reconcile scan would report and no
// automatic path would repair.
//
// So the creator's group gets its NORMAL owner handover first: ownership passes
// to the senior remaining member who is ALSO an active member of the project,
// and the departing uid is then an ordinary member and removable. That narrowing
// is what keeps the handover from creating the violation it is preventing.
//
// When no such successor exists the group is DETACHED to Space-direct instead,
// with its members untouched — the same rule as project disband, deliberately,
// so the two situations are one rule rather than two. It is not disbanded: group
// disband only flips group.status and leaves every group_member row in place, so
// it would destroy access without cleaning anything up.
func (g *Group) detachMemberFromProjectGroups(ctx *config.Context, removal projectmod.MemberRemoval) error {
	if removal.ProjectID == "" || removal.UID == "" {
		return nil
	}
	groupNos, err := g.db.queryProjectGroupNosWithActiveMember(removal.SpaceID, removal.ProjectID, removal.UID)
	if err != nil {
		return fmt.Errorf("group: list project groups for cascade: %w", err)
	}
	if len(groupNos) == 0 {
		return nil
	}

	var firstErr error
	for _, groupNo := range groupNos {
		if err := g.detachMemberFromOneProjectGroup(groupNo, removal); err != nil {
			// One group failing must not abandon the rest: partial progress is
			// durable (a group already left does not come back in the next
			// round's list), and the job retries what remains.
			g.Error("项目级联：从群移除成员失败",
				zap.String("group_no", groupNo),
				zap.String("project_id", removal.ProjectID),
				zap.String("uid", removal.UID),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (g *Group) detachMemberFromOneProjectGroup(groupNo string, removal projectmod.MemberRemoval) error {
	// Handover, if needed, in its own transaction before the removal. It has to
	// be separate: RemoveGroupMembers opens and commits its own transaction, and
	// nesting is not available.
	handedOver, detached, err := g.handOverProjectGroupIfCreator(groupNo, removal)
	if err != nil {
		return err
	}
	if detached {
		// The group left the project entirely; its members, including the
		// departing uid, stay. Nothing further to do for this group.
		g.Info("项目级联：群无可继任的项目成员，已改为 Space 直属",
			zap.String("group_no", groupNo),
			zap.String("project_id", removal.ProjectID),
			zap.String("uid", removal.UID))
		return nil
	}
	if handedOver {
		// An ownership change nobody asked for, performed by a background job.
		// It is the right outcome (see handOverProjectGroupIfCreator) but it is
		// not a thing to do silently: this line is the only record of WHY a group
		// changed hands, and it is what an operator will look for when the new
		// owner asks.
		g.Info("项目级联：群主随项目席位关闭而交接",
			zap.String("group_no", groupNo),
			zap.String("project_id", removal.ProjectID),
			zap.String("former_creator", removal.UID))
	}

	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:              groupNo,
		Members:              []string{removal.UID},
		OperatorUID:          removal.OperatorUID,
		SuppressRemoveNotice: true,
	})
	if err != nil {
		return err
	}
	if resp != nil && resp.Removed == 0 {
		// Nothing was removed and no error: either the uid was already gone
		// (idempotent re-run) or they are still the creator, which the handover
		// above should have resolved. Report the second case rather than looping.
		creator, cErr := g.creatorOf(groupNo)
		if cErr == nil && creator == removal.UID {
			return fmt.Errorf(
				"group: %s still owned by departing project member %s after handover",
				groupNo, removal.UID)
		}
	}
	return nil
}

// creatorOf reads a group's creator outside a transaction, for diagnostics only.
//
// One statement. The first version listed the managers and creator and then asked
// "is this one the creator?" per uid — an N+1 on the error path of a background
// job, i.e. exactly where nobody would notice it.
func (g *Group) creatorOf(groupNo string) (string, error) {
	var uids []string
	_, err := g.db.session.SelectBySql(
		"SELECT uid FROM group_member "+
			"WHERE group_no = ? AND role = ? AND is_deleted = 0 LIMIT 1",
		groupNo, MemberRoleCreator,
	).Load(&uids)
	if err != nil {
		return "", err
	}
	if len(uids) == 0 {
		return "", nil
	}
	return uids[0], nil
}

// handOverProjectGroupIfCreator transfers ownership away from the departing
// member, or detaches the group when nobody can take it.
//
// Returns (handedOver, detached, error). Both false means the departing member
// was not the creator and nothing was needed.
func (g *Group) handOverProjectGroupIfCreator(groupNo string, removal projectmod.MemberRemoval) (bool, bool, error) {
	tx, err := g.ctx.DB().Begin()
	if err != nil {
		return false, false, fmt.Errorf("group: begin handover: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	creator, err := g.db.queryGroupCreatorTx(tx, groupNo)
	if err != nil {
		return false, false, fmt.Errorf("group: read group creator: %w", err)
	}
	if creator != removal.UID {
		return false, false, tx.Commit()
	}

	successor, err := g.db.querySuccessorForProjectGroupTx(tx, groupNo, removal.ProjectID, removal.UID)
	if err != nil {
		return false, false, fmt.Errorf("group: pick successor: %w", err)
	}

	if successor == "" {
		// Nobody in this group is still in the project. Revert the group to
		// Space-direct and leave everyone in it — including the departing
		// member, who keeps a group they own. Disbanding would destroy a working
		// group; force-transferring to a non-project member would create the I2
		// violation this cascade exists to prevent.
		version, err := g.ctx.GenSeq(common.GroupSeqKey)
		if err != nil {
			return false, false, fmt.Errorf("group: GenSeq for detach: %w", err)
		}
		changed, err := g.db.detachGroupFromProjectTx(tx, groupNo, removal.ProjectID, version)
		if err != nil {
			return false, false, fmt.Errorf("group: detach group from project: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, false, fmt.Errorf("group: commit detach: %w", err)
		}
		if changed {
			g.Info("项目级联：群主即将离开项目且群内已无其他项目成员，群回落为 Space 直属",
				zap.String("group_no", groupNo),
				zap.String("project_id", removal.ProjectID),
				zap.String("creator", removal.UID))
			projectGroupDetachedTotal.WithLabelValues(detachReasonNoSuccessor).Inc()
		}
		return false, true, nil
	}

	// Normal owner handover: the successor becomes creator, the departing member
	// becomes an ordinary member and is then removable by the funnel.
	successorVersion, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return false, false, fmt.Errorf("group: GenSeq for successor: %w", err)
	}
	if err := g.db.UpdateMemberRoleTx(groupNo, successor, MemberRoleCreator, successorVersion, tx); err != nil {
		return false, false, fmt.Errorf("group: promote successor: %w", err)
	}
	departingVersion, err := g.ctx.GenSeq(common.GroupMemberSeqKey)
	if err != nil {
		return false, false, fmt.Errorf("group: GenSeq for departing member: %w", err)
	}
	if err := g.db.UpdateMemberRoleTx(groupNo, removal.UID, MemberRoleCommon, departingVersion, tx); err != nil {
		return false, false, fmt.Errorf("group: demote departing creator: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("group: commit handover: %w", err)
	}
	g.Info("项目级联：群主离开项目，群主已交接",
		zap.String("group_no", groupNo),
		zap.String("project_id", removal.ProjectID),
		zap.String("from", removal.UID),
		zap.String("to", successor))
	projectGroupHandoverTotal.Inc()
	return true, false, nil
}

// revertProjectGroupsToSpace reverts every group of a disbanded project to
// Space-direct, leaving group_member rows untouched.
//
// Reachable from BOTH the disband handler and P0's ownerless-project cascade
// branch, which is why it lives behind a registry rather than being called from
// one handler: after P0's round-2 review a background worker can disband a
// project when the Space cascade finds no successor, so "a project ended" is no
// longer synonymous with "a human clicked disband".
//
// Members are deliberately left alone. The group keeps working as an ordinary
// Space group, which is what the product confirmed. Disbanding the groups
// instead would destroy data and, because group disband only flips group.status
// and leaves every group_member row, would not even clean up.
//
// Idempotent: detachGroupFromProjectTx is guarded on the current project_id, so
// a re-run affects zero rows.
func (g *Group) revertProjectGroupsToSpace(ctx *config.Context, disband projectmod.ProjectDisband) error {
	if disband.ProjectID == "" {
		return nil
	}
	groupNos, err := g.db.queryProjectGroupNos(disband.SpaceID, disband.ProjectID)
	if err != nil {
		return fmt.Errorf("group: list project groups for disband: %w", err)
	}
	if len(groupNos) == 0 {
		return nil
	}

	reason := detachReasonDisband
	if disband.ByCascade {
		reason = detachReasonOwnerlessDisband
	}

	var firstErr error
	for _, groupNo := range groupNos {
		if err := g.revertOneGroupToSpace(groupNo, disband.ProjectID, reason); err != nil {
			g.Error("项目解散级联：群回落 Space 直属失败",
				zap.String("group_no", groupNo),
				zap.String("project_id", disband.ProjectID),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (g *Group) revertOneGroupToSpace(groupNo, projectID, reason string) error {
	version, err := g.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		return fmt.Errorf("group: GenSeq for revert: %w", err)
	}
	tx, err := g.ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("group: begin revert: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	changed, err := g.db.detachGroupFromProjectTx(tx, groupNo, projectID, version)
	if err != nil {
		return fmt.Errorf("group: revert group to space: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group: commit revert: %w", err)
	}
	if changed {
		projectGroupDetachedTotal.WithLabelValues(reason).Inc()
	}
	return nil
}
