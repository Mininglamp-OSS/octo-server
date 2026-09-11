package workspace

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

const spaceMemberRemovalStepName = "workspace_membership"

func registerSpaceMemberRemovalCleanup() {
	spacemod.RegisterMemberRemovalCleanupStep(spaceMemberRemovalStepName, cleanupSpaceMemberWorkspaces)
}

func cleanupSpaceMemberWorkspaces(ctx *config.Context, removal spacemod.MemberRemoval) error {
	if ctx == nil || ctx.DB() == nil || removal.SpaceID == "" || removal.UID == "" {
		return nil
	}
	stillMember, err := spacepkg.CheckMembershipForCleanup(ctx.DB(), removal.SpaceID, removal.UID)
	if err != nil {
		return fmt.Errorf("workspace: re-check space membership before cleanup: %w", err)
	}
	if stillMember {
		return nil
	}
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `octo_workspace_member` wm INNER JOIN `octo_workspace` w ON w.workspace_id = wm.workspace_id "+
			"SET wm.status = ?, wm.updated_at = ? WHERE w.space_id = ? AND wm.uid = ? AND wm.status = ? AND w.owner_uid <> ?",
		MemberStatusInactive, time.Now().UTC(), removal.SpaceID, removal.UID, MemberStatusActive, removal.UID,
	).Exec()
	if err != nil {
		return fmt.Errorf("workspace: deactivate memberships after space removal: %w", err)
	}
	return nil
}
