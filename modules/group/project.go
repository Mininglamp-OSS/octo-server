package group

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/gocraft/dbr/v2"
)

var (
	errProjectRelationConflict   = errors.New("group Project relation conflict")
	errProjectRelationCorrupt    = errors.New("group Project relation is corrupt")
	errProjectRelationNotFound   = errors.New("group Project relation not found")
	errProjectRelationForbidden  = errors.New("group Project relation forbidden")
	errProjectRelationInvalid    = errors.New("group Project relation request invalid")
	errProjectRelationDependency = errors.New("group Project relation dependency unavailable")
)

func changedProjectSourceAllowed(initialSource, currentSource, targetID string) bool {
	initialSource = strings.TrimSpace(initialSource)
	currentSource = strings.TrimSpace(currentSource)
	targetID = strings.TrimSpace(targetID)
	if initialSource == currentSource {
		return true
	}
	// An intervening unbind is a fresh bind. An intervening bind to this
	// request's already-authorized target is an idempotent no-op. Any other
	// replacement was not included in the authorization lock set.
	return currentSource == "" || currentSource == targetID
}

func projectGroupLike(keyword string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(keyword)
	return "%" + escaped + "%"
}

func validateProjectRelationKeyword(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > 30 {
		return "", errProjectRelationInvalid
	}
	return keyword, nil
}

func (g *Group) readGroupProject(ctx context.Context, groupNo, actorUID string) (GroupProjectRelation, error) {
	if strings.TrimSpace(groupNo) == "" || strings.TrimSpace(actorUID) == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	tx, err := g.ctx.DB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: begin relation read: %v", errProjectRelationDependency, err)
	}
	defer tx.RollbackUnlessCommitted()
	row, err := g.db.queryGroupProjectRelationTx(tx, strings.TrimSpace(groupNo))
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: query relation: %v", errProjectRelationDependency, err)
	}
	if row == nil || row.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	if strings.TrimSpace(row.SpaceID) == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	if err := validateGroupProjectRelation(row); err != nil {
		if errors.Is(err, errProjectRelationNotFound) {
			return GroupProjectRelation{}, err
		}
		return GroupProjectRelation{}, errProjectRelationCorrupt
	}
	projectID := strings.TrimSpace(row.ProjectID)
	if projectID == "" {
		active, err := g.db.queryGroupNativeReadAccessTx(tx, row.SpaceID, row.GroupNo, actorUID)
		if err != nil {
			return GroupProjectRelation{}, fmt.Errorf("%w: query native group access: %v", errProjectRelationDependency, err)
		}
		if !active {
			return GroupProjectRelation{}, errProjectRelationForbidden
		}
	} else {
		if _, err := projectmod.AuthorizeGroupProjectReadTx(tx, actorUID, projectID, row.SpaceID); err != nil {
			return GroupProjectRelation{}, mapProjectAccessError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: commit relation read: %v", errProjectRelationDependency, err)
	}
	return groupProjectRelationFromRow(row), nil
}

func (g *Group) bindGroupProject(actorUID, groupNo, targetID string) (GroupProjectRelation, error) {
	actorUID = strings.TrimSpace(actorUID)
	groupNo = strings.TrimSpace(groupNo)
	targetID = strings.TrimSpace(targetID)
	if actorUID == "" || groupNo == "" || targetID == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	before, err := g.db.queryGroupProjectRelation(groupNo)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: query relation: %w", errProjectRelationDependency, err)
	}
	if before == nil || before.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	spaceID := strings.TrimSpace(before.SpaceID)
	if spaceID == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	if err := validateGroupProjectRelation(before); err != nil {
		return GroupProjectRelation{}, err
	}
	initialSource := strings.TrimSpace(before.ProjectID)

	var pendingTx *dbr.Tx
	var pendingRow *groupProjectRelationRow
	err = projectmod.RetryGroupProjectLockConflict(func() error {
		seatRefs, prepErr := projectmod.PrepareGroupProjectSpaceSeatRefs(
			g.ctx.DB(), spaceID, []string{actorUID},
		)
		if prepErr != nil {
			return mapProjectAccessError(prepErr)
		}
		tx, beginErr := g.ctx.DB().Begin()
		if beginErr != nil {
			return fmt.Errorf("%w: begin relation bind: %w", errProjectRelationDependency, beginErr)
		}
		accesses, lockErr := projectmod.LockGroupProjectAccessesTx(
			tx, actorUID, projectRelationAccessIDs(initialSource, targetID), spaceID, seatRefs,
		)
		if lockErr != nil {
			_ = tx.Rollback()
			return mapProjectAccessError(lockErr)
		}
		locked, lockErr := g.db.lockGroupProjectRelationTx(tx, groupNo)
		if lockErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: lock relation: %w", errProjectRelationDependency, lockErr)
		}
		if locked == nil || locked.Status == GroupStatusDisband {
			_ = tx.Rollback()
			return errProjectRelationNotFound
		}
		if lockErr := validateGroupProjectRelation(locked); lockErr != nil {
			_ = tx.Rollback()
			return lockErr
		}
		if locked.SpaceID != spaceID {
			_ = tx.Rollback()
			return errProjectRelationConflict
		}
		currentSource := strings.TrimSpace(locked.ProjectID)
		if !changedProjectSourceAllowed(initialSource, currentSource, targetID) {
			_ = tx.Rollback()
			return errProjectRelationConflict
		}
		if targetAccess, ok := accesses[targetID]; !ok || targetAccess.SpaceID != locked.SpaceID {
			_ = tx.Rollback()
			return errProjectRelationConflict
		}
		if currentSource != "" {
			if sourceAccess, ok := accesses[currentSource]; !ok || sourceAccess.SpaceID != locked.SpaceID {
				_ = tx.Rollback()
				return errProjectRelationConflict
			}
		}
		manager, lockErr := g.db.lockGroupManagerTx(tx, groupNo, actorUID)
		if lockErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: lock native manager: %w", errProjectRelationDependency, lockErr)
		}
		if !manager {
			_ = tx.Rollback()
			return errProjectRelationForbidden
		}

		if currentSource != targetID {
			if updateErr := g.db.updateGroupProjectRelationTx(tx, groupNo, targetID, actorUID); updateErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: update relation: %w", errProjectRelationDependency, updateErr)
			}
			linkedBy := actorUID
			locked.ProjectID = targetID
			locked.ProjectLinkedBy = &linkedBy
		}
		pendingTx = tx
		pendingRow = locked
		return nil
	})
	if err != nil {
		return GroupProjectRelation{}, err
	}
	if pendingTx == nil || pendingRow == nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: relation bind produced no transaction", errProjectRelationDependency)
	}
	defer pendingTx.RollbackUnlessCommitted()
	if err := pendingTx.Commit(); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: commit relation bind: %w", errProjectRelationDependency, err)
	}
	return groupProjectRelationFromRow(pendingRow), nil
}

func (g *Group) unbindGroupProject(actorUID, groupNo string) (GroupProjectRelation, error) {
	actorUID = strings.TrimSpace(actorUID)
	groupNo = strings.TrimSpace(groupNo)
	if actorUID == "" || groupNo == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	before, err := g.db.queryGroupProjectRelation(groupNo)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: query relation: %w", errProjectRelationDependency, err)
	}
	if before == nil || before.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	spaceID := strings.TrimSpace(before.SpaceID)
	if spaceID == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	if err := validateGroupProjectRelation(before); err != nil {
		return GroupProjectRelation{}, err
	}
	initialSource := strings.TrimSpace(before.ProjectID)

	var pendingTx *dbr.Tx
	var pendingRow *groupProjectRelationRow
	err = projectmod.RetryGroupProjectLockConflict(func() error {
		seatRefs, prepErr := projectmod.PrepareGroupProjectSpaceSeatRefs(
			g.ctx.DB(), spaceID, []string{actorUID},
		)
		if prepErr != nil {
			return mapProjectAccessError(prepErr)
		}
		tx, beginErr := g.ctx.DB().Begin()
		if beginErr != nil {
			return fmt.Errorf("%w: begin relation unbind: %w", errProjectRelationDependency, beginErr)
		}
		if initialSource != "" {
			if _, lockErr := projectmod.LockGroupProjectAccessTx(
				tx, actorUID, initialSource, spaceID, seatRefs,
			); lockErr != nil {
				_ = tx.Rollback()
				return mapProjectAccessError(lockErr)
			}
		} else {
			active, lockErr := projectmod.LockGroupProjectSpaceAccessTx(
				tx, actorUID, spaceID, seatRefs,
			)
			if lockErr != nil {
				_ = tx.Rollback()
				return mapProjectAccessError(lockErr)
			}
			if !active {
				_ = tx.Rollback()
				return errProjectRelationForbidden
			}
		}
		locked, lockErr := g.db.lockGroupProjectRelationTx(tx, groupNo)
		if lockErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: lock relation: %w", errProjectRelationDependency, lockErr)
		}
		if locked == nil || locked.Status == GroupStatusDisband {
			_ = tx.Rollback()
			return errProjectRelationNotFound
		}
		if lockErr := validateGroupProjectRelation(locked); lockErr != nil {
			_ = tx.Rollback()
			return lockErr
		}
		if locked.SpaceID != spaceID {
			_ = tx.Rollback()
			return errProjectRelationConflict
		}
		currentSource := strings.TrimSpace(locked.ProjectID)
		if !changedProjectSourceAllowed(initialSource, currentSource, "") {
			_ = tx.Rollback()
			return errProjectRelationConflict
		}
		manager, lockErr := g.db.lockGroupManagerTx(tx, groupNo, actorUID)
		if lockErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: lock native manager: %w", errProjectRelationDependency, lockErr)
		}
		if !manager {
			_ = tx.Rollback()
			return errProjectRelationForbidden
		}
		if currentSource != "" {
			if updateErr := g.db.updateGroupProjectRelationTx(tx, groupNo, "", ""); updateErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: clear relation: %w", errProjectRelationDependency, updateErr)
			}
			locked.ProjectID = ""
			locked.ProjectLinkedBy = nil
		}
		pendingTx = tx
		pendingRow = locked
		return nil
	})
	if err != nil {
		return GroupProjectRelation{}, err
	}
	if pendingTx == nil || pendingRow == nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: relation unbind produced no transaction", errProjectRelationDependency)
	}
	defer pendingTx.RollbackUnlessCommitted()
	if err := pendingTx.Commit(); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: commit relation unbind: %w", errProjectRelationDependency, err)
	}
	return groupProjectRelationFromRow(pendingRow), nil
}

func mapProjectAccessError(err error) error {
	switch {
	case errors.Is(err, projectmod.ErrGroupProjectInvalid):
		return errProjectRelationInvalid
	case errors.Is(err, projectmod.ErrGroupProjectNotFound):
		return errProjectRelationNotFound
	case errors.Is(err, projectmod.ErrGroupProjectForbidden):
		return errProjectRelationForbidden
	case errors.Is(err, projectmod.ErrGroupProjectSpaceConflict):
		return errProjectRelationConflict
	case errors.Is(err, projectmod.ErrGroupProjectDependency):
		return fmt.Errorf("%w: %w", errProjectRelationDependency, err)
	default:
		return fmt.Errorf("%w: %w", errProjectRelationDependency, err)
	}
}
