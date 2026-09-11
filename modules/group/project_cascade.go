package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"go.uber.org/zap"
)

// Project → Group cascade.
//
// The member-removal step is deliberately pointer-scoped: only the Project's
// all_member_group_no projection is live-synchronized. Ordinary Project-
// associated groups retain their initial native snapshot.
const (
	projectMemberRemovalStepName = "group_project_detach"
	projectDisbandStepName       = "group_project_revert"
)

// registerProjectCascadeSteps is called from 1module.go at construction, beside
// the Space-side registrations.
func (g *Group) registerProjectCascadeSteps() {
	projectmod.RegisterProjectMemberRemovalStep(
		projectMemberRemovalStepName, g.detachMemberFromProjectGroups,
	)
	projectmod.RegisterProjectDisbandStep(projectDisbandStepName, g.revertProjectGroupsToSpace)
}

// queryDedicatedGroupNo returns the currently valid all-member group pointer.
// It intentionally does not inspect ordinary Project-associated groups.
func (g *Group) queryDedicatedGroupNo(projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	var pointers []string
	if _, err := g.db.session.SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` "+
			"WHERE project_id = ? AND status = 1 AND all_member_group_no <> '' LIMIT 1",
		projectID,
	).Load(&pointers); err != nil {
		return "", fmt.Errorf("group: query dedicated-group pointer: %w", err)
	}
	if len(pointers) == 0 || pointers[0] == "" {
		return "", nil
	}
	var groups []struct {
		ProjectID string `db:"project_id"`
		Status    int    `db:"status"`
	}
	if _, err := g.db.session.SelectBySql(
		"SELECT project_id, status FROM `group` WHERE group_no = ? LIMIT 1",
		pointers[0],
	).Load(&groups); err != nil {
		return "", fmt.Errorf("group: query dedicated-group row: %w", err)
	}
	if len(groups) == 0 || groups[0].Status == GroupStatusDisband ||
		groups[0].ProjectID != projectID {
		return "", nil
	}
	return pointers[0], nil
}

// detachMemberFromProjectGroups removes one closing Project seat from the
// dedicated group. The worker has already marked the seat removing=1; the
// removal service rechecks that state under Project → group → group_member
// locks, so a concurrent re-admission cannot remove the new membership.
func (g *Group) detachMemberFromProjectGroups(
	ctx *config.Context, removal projectmod.MemberRemoval,
) error {
	if ctx == nil || removal.ProjectID == "" || removal.UID == "" {
		return nil
	}
	groupNo, err := g.queryDedicatedGroupNo(removal.ProjectID)
	if err != nil || groupNo == "" {
		return err
	}
	member, err := g.db.QueryMemberWithUID(removal.UID, groupNo)
	if err != nil {
		return fmt.Errorf("group: query dedicated-group member for removal: %w", err)
	}
	// Recheck the Project seat before touching either native membership or IM.
	// A stale removal callback can race a rejoin after its worker-level fence;
	// even with no native row left, it must not unsubscribe the new membership.
	readmitted, err := projectpkg.CheckMembership(
		ctx.DB(), removal.ProjectID, removal.UID,
	)
	if err != nil {
		return fmt.Errorf("group: recheck Project seat before dedicated removal: %w", err)
	}
	if readmitted {
		return nil
	}
	if member == nil {
		// The native row may already have been removed by a previous attempt
		// whose broker call failed. Keep retrying the idempotent unsubscribe;
		// otherwise the outbox would observe an empty group result and retire
		// the job while the user still receives group traffic.
		return removeDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
	}
	if member.IsDeleted == 1 {
		return removeDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
	}
	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:              groupNo,
		Members:              []string{removal.UID},
		OperatorUID:          removal.OperatorUID,
		SuppressRemoveNotice: true,
		ProjectRemoval:       true,
		ProjectID:            removal.ProjectID,
	})
	if err != nil {
		// Another cleanup path may have removed the native row after the unlocked
		// precheck above. Treat that race as idempotent at the database layer, but
		// still reconcile the broker subscription; a real transport failure keeps
		// the outbox job retryable.
		if errors.Is(err, errGroupMemberNotInGroup) {
			readmitted, checkErr := projectpkg.CheckMembership(
				ctx.DB(), removal.ProjectID, removal.UID,
			)
			if checkErr != nil {
				return fmt.Errorf("group: recheck Project seat after concurrent removal: %w", checkErr)
			}
			if readmitted {
				return addDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
			}
			return removeDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
		}
		// The native transaction may have committed before the broker failed.
		// If a rejoin won the race after that commit, restore the subscription
		// rather than letting the stale callback strand the active member.
		if resp != nil {
			readmitted, checkErr := projectpkg.CheckMembership(
				ctx.DB(), removal.ProjectID, removal.UID,
			)
			if checkErr != nil {
				return fmt.Errorf("group: recheck Project seat after dedicated removal: %w", checkErr)
			}
			if readmitted {
				return addDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
			}
		}
		return err
	}
	if resp != nil && resp.Removed == 0 {
		// A concurrent retry may have removed the row already. Re-read it to
		// distinguish that harmless idempotent result from a stale relation.
		member, readErr := g.db.QueryMemberWithUID(removal.UID, groupNo)
		if readErr != nil {
			return readErr
		}
		if member != nil {
			return fmt.Errorf(
				"group: dedicated-group member %s was not removed", removal.UID,
			)
		}
	}
	// The DB transaction and this callback are not one atomic operation with
	// the broker. Reconcile once after the removal too: a rejoin that committed
	// while the native transaction was finishing must not lose its IM access.
	readmitted, err = projectpkg.CheckMembership(ctx.DB(), removal.ProjectID, removal.UID)
	if err != nil {
		return fmt.Errorf("group: recheck Project seat after dedicated removal: %w", err)
	}
	if readmitted {
		return addDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
	}
	if member == nil {
		return removeDedicatedGroupSubscriber(ctx, groupNo, removal.UID)
	}
	return nil
}

func addDedicatedGroupSubscriber(ctx *config.Context, groupNo, uid string) error {
	if ctx == nil || groupNo == "" || uid == "" {
		return nil
	}
	if err := ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{uid},
	}); err != nil {
		return fmt.Errorf("group: restore dedicated-group IM subscriber: %w", err)
	}
	return nil
}

func removeDedicatedGroupSubscriber(ctx *config.Context, groupNo, uid string) error {
	if ctx == nil || groupNo == "" || uid == "" {
		return nil
	}
	if err := ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{uid},
	}); err != nil {
		return fmt.Errorf("group: remove dedicated-group IM subscriber: %w", err)
	}
	return nil
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
