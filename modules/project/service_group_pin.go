package project

import (
	"fmt"
	"strings"

	"github.com/gocraft/dbr/v2"
)

// updateProjectGroupSetting applies one user's pin preference for a currently
// associated group. LockGroupProjectAccessTx is intentionally the first
// authorization step: it locks Space seats and rows before the Project and
// member rows, matching the group bind and unbind lock order. The group row is
// then locked in the same transaction before its relation is accepted.
//
// Lock-conflict retries replay only this local database transaction. Commit is
// deliberately outside the retry callback, and no post-commit side effect is
// attached to this operation.
func (p *Project) updateProjectGroupSetting(projectID, spaceID, groupNo, uid string, pinned bool) error {
	projectID = strings.TrimSpace(projectID)
	spaceID = strings.TrimSpace(spaceID)
	groupNo = strings.TrimSpace(groupNo)
	uid = strings.TrimSpace(uid)
	if projectID == "" || spaceID == "" || groupNo == "" || uid == "" {
		return ErrGroupProjectInvalid
	}

	var pendingTx *dbr.Tx
	err := retryOnLockConflict(func() error {
		seatRefs, prepErr := PrepareGroupProjectSpaceSeatRefs(p.db.session, spaceID, []string{uid})
		if prepErr != nil {
			return prepErr
		}
		tx, beginErr := p.db.session.Begin()
		if beginErr != nil {
			return fmt.Errorf("%w: begin group pin: %w", ErrGroupProjectDependency, beginErr)
		}
		access, lockErr := LockGroupProjectAccessTx(tx, uid, projectID, spaceID, seatRefs)
		if lockErr != nil {
			_ = tx.Rollback()
			return lockErr
		}
		target, lockErr := p.db.lockProjectGroupPinTargetTx(tx, groupNo)
		if lockErr != nil {
			_ = tx.Rollback()
			return lockErr
		}
		if target == nil || target.Status == groupStatusDisband {
			_ = tx.Rollback()
			return ErrGroupProjectNotFound
		}
		if strings.TrimSpace(target.SpaceID) != access.SpaceID {
			_ = tx.Rollback()
			return ErrGroupProjectSpaceConflict
		}
		if strings.TrimSpace(target.ProjectID) != access.ProjectID {
			_ = tx.Rollback()
			return ErrGroupProjectNotFound
		}

		if writeErr := p.db.upsertProjectGroupUserSettingTx(tx, access.SpaceID, access.ProjectID, groupNo, uid, pinned); writeErr != nil {
			_ = tx.Rollback()
			return writeErr
		}
		pendingTx = tx
		return nil
	})
	if err != nil {
		return err
	}
	if pendingTx == nil {
		return fmt.Errorf("%w: group pin produced no transaction", ErrGroupProjectDependency)
	}
	defer pendingTx.RollbackUnlessCommitted()
	if err := pendingTx.Commit(); err != nil {
		return fmt.Errorf("%w: commit group pin: %w", ErrGroupProjectDependency, err)
	}
	return nil
}
