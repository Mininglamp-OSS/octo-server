package project

import (
	"fmt"
	"strings"
)

// updateProjectGroupSetting applies one user's pin preference for a currently
// associated group.  LockGroupProjectAccessTx is intentionally the first
// authorization step: it locks Space seats and rows before the Project and
// member rows, matching the group bind and unbind lock order.  The group row is
// then locked in the same transaction before its relation is accepted.
func (p *Project) updateProjectGroupSetting(projectID, spaceID, groupNo, uid string, pinned bool) error {
	projectID = strings.TrimSpace(projectID)
	spaceID = strings.TrimSpace(spaceID)
	groupNo = strings.TrimSpace(groupNo)
	uid = strings.TrimSpace(uid)
	if projectID == "" || spaceID == "" || groupNo == "" || uid == "" {
		return ErrGroupProjectInvalid
	}

	tx, err := p.db.session.Begin()
	if err != nil {
		return fmt.Errorf("%w: begin group pin: %v", ErrGroupProjectDependency, err)
	}
	defer tx.RollbackUnlessCommitted()

	access, err := LockGroupProjectAccessTx(tx, uid, projectID, spaceID)
	if err != nil {
		return err
	}

	target, err := p.db.lockProjectGroupPinTargetTx(tx, groupNo)
	if err != nil {
		return err
	}
	if target == nil || target.Status == groupStatusDisband {
		return ErrGroupProjectNotFound
	}
	if strings.TrimSpace(target.SpaceID) != access.SpaceID {
		return ErrGroupProjectSpaceConflict
	}
	if strings.TrimSpace(target.ProjectID) != access.ProjectID {
		return ErrGroupProjectNotFound
	}

	if err := p.db.upsertProjectGroupUserSettingTx(tx, access.SpaceID, access.ProjectID, groupNo, uid, pinned); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit group pin: %v", ErrGroupProjectDependency, err)
	}
	return nil
}
