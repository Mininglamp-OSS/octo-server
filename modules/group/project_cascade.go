package group

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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

// reconcileDedicatedGroupProjection performs the final, pointer-scoped
// decision immediately around the broker operation. The native group row is
// protected by the admission/removal transactions, but IM is a separate
// service: a stale removal may be blocked while a rejoin commits and sends
// IMAdd. Rechecking both before and after IMRemove makes that interleaving
// converge to the current projection instead of allowing a late IMRemove to
// strand the member. A pointer that moved away from groupNo is never touched.
func (g *Group) reconcileDedicatedGroupProjection(
	ctx *config.Context, removal projectmod.MemberRemoval, groupNo string,
) (resultErr error) {
	if ctx == nil || groupNo == "" || removal.UID == "" {
		return nil
	}
	defer func() {
		if !errors.Is(resultErr, projectpkg.ErrAdmittedButNotSubscribed) {
			return
		}
		// A Project rejoin may already have cancelled the removal job. This
		// callback must hand a failed late-IMRemove compensation to a fresh
		// durable projection; admission itself leaves retries with its caller.
		if err := spacemod.EnqueueMemberRejoinIntent(ctx, removal.SpaceID, removal.UID, removal.OperatorUID); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("group: persist dedicated projection retry: %w", err))
		}
	}()
	currentGroupNo, err := g.queryDedicatedGroupNo(removal.ProjectID)
	if err != nil {
		return err
	}
	if currentGroupNo != groupNo {
		return nil
	}
	projectActive, err := projectpkg.CheckMembership(ctx.DB(), removal.ProjectID, removal.UID)
	if err != nil {
		return fmt.Errorf("group: recheck Project seat for dedicated projection: %w", err)
	}
	if projectActive {
		spaceActive, checkErr := spacepkg.CheckMembership(ctx.DB(), removal.SpaceID, removal.UID)
		if checkErr != nil {
			return fmt.Errorf("group: recheck Space seat for dedicated projection: %w", checkErr)
		}
		if spaceActive {
			if err := g.admitToAllMemberGroup(ctx, removal.SpaceID, groupNo, removal.UID); err != nil {
				return fmt.Errorf("group: restore dedicated-group projection: %w", err)
			}
			return nil
		}
	}
	if err := removeDedicatedGroupSubscriber(ctx, groupNo, removal.UID); err != nil {
		return err
	}

	// IMRemove may have been blocked while the Space/Project admission
	// transaction committed and issued IMAdd. Re-run the same pointer-scoped
	// decision after it returns; only the current group may be repaired.
	currentGroupNo, err = g.queryDedicatedGroupNo(removal.ProjectID)
	if err != nil {
		return err
	}
	if currentGroupNo != groupNo {
		return nil
	}
	projectActive, err = projectpkg.CheckMembership(ctx.DB(), removal.ProjectID, removal.UID)
	if err != nil {
		return fmt.Errorf("group: recheck Project seat after dedicated IMRemove: %w", err)
	}
	if !projectActive {
		return nil
	}
	spaceActive, err := spacepkg.CheckMembership(ctx.DB(), removal.SpaceID, removal.UID)
	if err != nil {
		return fmt.Errorf("group: recheck Space seat after dedicated IMRemove: %w", err)
	}
	if !spaceActive {
		return nil
	}
	if err := g.admitToAllMemberGroup(ctx, removal.SpaceID, groupNo, removal.UID); err != nil {
		return fmt.Errorf("group: restore dedicated-group projection after late IMRemove: %w", err)
	}
	return nil
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
	// Recheck the Project and Space seats before touching either native
	// membership or IM. A stale removal callback can race a rejoin after its
	// worker-level fence; even with no native row left, it must reconcile the
	// current dedicated projection rather than returning and letting a late
	// IMRemove strand the active member. The Space check is deliberately the
	// authorization predicate (not CheckMembershipForCleanup): a preserved
	// Project Owner without a current Space seat is not admitted back.
	// Reconcile once before touching the native row or IM. This also covers
	// the missing/deleted-row branches: a rejoin may have committed while the
	// unlocked native lookup was in flight.
	readmitted, err := projectpkg.CheckMembership(
		ctx.DB(), removal.ProjectID, removal.UID,
	)
	if err != nil {
		return fmt.Errorf("group: recheck Project seat before dedicated removal: %w", err)
	}
	if readmitted {
		return g.reconcileDedicatedGroupProjection(ctx, removal, groupNo)
	}
	if member == nil || member.IsDeleted == 1 {
		return g.reconcileDedicatedGroupProjection(ctx, removal, groupNo)
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
			return g.reconcileDedicatedGroupProjection(ctx, removal, groupNo)
		}
		// The native transaction may have committed before the broker failed.
		// Reconcile around the broker operation so a rejoin that won the race
		// restores both the native row and IM subscription. If reconciliation
		// itself fails, preserve the original transport error for the retry.
		if resp != nil {
			if reconcileErr := g.reconcileDedicatedGroupProjection(ctx, removal, groupNo); reconcileErr == nil {
				return nil
			} else {
				return fmt.Errorf("%w (dedicated projection reconcile: %v)", err, reconcileErr)
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
			// If the seat became eligible again, the service deliberately
			// refused a destructive delete; reconcile the current projection
			// instead of reporting a false success.
			readmitted, checkErr := projectpkg.CheckMembership(
				ctx.DB(), removal.ProjectID, removal.UID,
			)
			if checkErr != nil {
				return fmt.Errorf("group: recheck Project seat after cancelled removal: %w", checkErr)
			}
			if readmitted {
				spaceMember, spaceErr := spacepkg.CheckMembership(
					ctx.DB(), removal.SpaceID, removal.UID,
				)
				if spaceErr != nil {
					return fmt.Errorf("group: recheck Space seat after cancelled removal: %w", spaceErr)
				}
				if spaceMember {
					return g.reconcileDedicatedGroupProjection(ctx, removal, groupNo)
				}
			}
			return fmt.Errorf(
				"group: dedicated-group member %s was not removed", removal.UID,
			)
		}
	}
	// The DB transaction and this callback are not one atomic operation with
	// the broker. Reconcile immediately before and after the IM operation:
	// a rejoin that committed while the native transaction was finishing must
	// not lose its IM access, and every missing/deleted-row early return above
	// follows the same pointer-scoped rule.
	return g.reconcileDedicatedGroupProjection(ctx, removal, groupNo)
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
