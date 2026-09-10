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
// Project disband still detaches associated groups to Space-direct while
// preserving native group membership. Project member-seat changes deliberately
// have no Group step: the relation is metadata and the native group roster is
// an independent permission source.
const projectDisbandStepName = "group_project_revert"

// registerProjectCascadeSteps is called from 1module.go at construction, beside
// the Space-side registrations.
func (g *Group) registerProjectCascadeSteps() {
	projectmod.RegisterProjectDisbandStep(projectDisbandStepName, g.revertProjectGroupsToSpace)
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
