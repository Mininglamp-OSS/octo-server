package group

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
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
		return GroupProjectRelation{}, fmt.Errorf("%w: query relation: %v", errProjectRelationDependency, err)
	}
	if before == nil || before.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	if strings.TrimSpace(before.SpaceID) == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	if err := validateGroupProjectRelation(before); err != nil {
		return GroupProjectRelation{}, err
	}
	initialSource := strings.TrimSpace(before.ProjectID)

	tx, err := g.ctx.DB().Begin()
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: begin relation bind: %v", errProjectRelationDependency, err)
	}
	defer tx.RollbackUnlessCommitted()

	accesses, err := projectmod.LockGroupProjectAccessesTx(
		tx, actorUID, projectRelationAccessIDs(initialSource, targetID), before.SpaceID,
	)
	if err != nil {
		return GroupProjectRelation{}, mapProjectAccessError(err)
	}
	locked, err := g.db.lockGroupProjectRelationTx(tx, groupNo)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: lock relation: %v", errProjectRelationDependency, err)
	}
	if locked == nil || locked.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	if err := validateGroupProjectRelation(locked); err != nil {
		return GroupProjectRelation{}, err
	}
	if locked.SpaceID != before.SpaceID {
		return GroupProjectRelation{}, errProjectRelationConflict
	}
	currentSource := strings.TrimSpace(locked.ProjectID)
	if !changedProjectSourceAllowed(initialSource, currentSource, targetID) {
		return GroupProjectRelation{}, errProjectRelationConflict
	}
	if targetAccess, ok := accesses[targetID]; !ok || targetAccess.SpaceID != locked.SpaceID {
		return GroupProjectRelation{}, errProjectRelationConflict
	}
	if currentSource != "" {
		if sourceAccess, ok := accesses[currentSource]; !ok || sourceAccess.SpaceID != locked.SpaceID {
			return GroupProjectRelation{}, errProjectRelationConflict
		}
	}
	manager, err := g.db.lockGroupManagerTx(tx, groupNo, actorUID)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: lock native manager: %v", errProjectRelationDependency, err)
	}
	if !manager {
		return GroupProjectRelation{}, errProjectRelationForbidden
	}

	if currentSource == targetID {
		if err := tx.Commit(); err != nil {
			return GroupProjectRelation{}, fmt.Errorf("%w: commit idempotent bind: %v", errProjectRelationDependency, err)
		}
		return groupProjectRelationFromRow(locked), nil
	}
	if err := g.db.updateGroupProjectRelationTx(tx, groupNo, targetID, actorUID); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: update relation: %v", errProjectRelationDependency, err)
	}
	linkedBy := actorUID
	locked.ProjectID = targetID
	locked.ProjectLinkedBy = &linkedBy
	if err := tx.Commit(); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: commit relation bind: %v", errProjectRelationDependency, err)
	}
	return groupProjectRelationFromRow(locked), nil
}

func (g *Group) unbindGroupProject(actorUID, groupNo string) (GroupProjectRelation, error) {
	actorUID = strings.TrimSpace(actorUID)
	groupNo = strings.TrimSpace(groupNo)
	if actorUID == "" || groupNo == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	before, err := g.db.queryGroupProjectRelation(groupNo)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: query relation: %v", errProjectRelationDependency, err)
	}
	if before == nil || before.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	if strings.TrimSpace(before.SpaceID) == "" {
		return GroupProjectRelation{}, errProjectRelationInvalid
	}
	if err := validateGroupProjectRelation(before); err != nil {
		return GroupProjectRelation{}, err
	}
	initialSource := strings.TrimSpace(before.ProjectID)

	tx, err := g.ctx.DB().Begin()
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: begin relation unbind: %v", errProjectRelationDependency, err)
	}
	defer tx.RollbackUnlessCommitted()
	if initialSource != "" {
		if _, err := projectmod.LockGroupProjectAccessTx(tx, actorUID, initialSource, before.SpaceID); err != nil {
			return GroupProjectRelation{}, mapProjectAccessError(err)
		}
	} else {
		active, err := g.db.lockGroupSpaceMemberTx(tx, before.SpaceID, actorUID)
		if err != nil {
			return GroupProjectRelation{}, fmt.Errorf("%w: lock group Space member: %v", errProjectRelationDependency, err)
		}
		if !active {
			return GroupProjectRelation{}, errProjectRelationForbidden
		}
		eligible, err := g.db.lockGroupUserEligibleTx(tx, actorUID)
		if err != nil {
			return GroupProjectRelation{}, fmt.Errorf("%w: lock user: %v", errProjectRelationDependency, err)
		}
		if !eligible {
			return GroupProjectRelation{}, errProjectRelationForbidden
		}
	}
	locked, err := g.db.lockGroupProjectRelationTx(tx, groupNo)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: lock relation: %v", errProjectRelationDependency, err)
	}
	if locked == nil || locked.Status == GroupStatusDisband {
		return GroupProjectRelation{}, errProjectRelationNotFound
	}
	if err := validateGroupProjectRelation(locked); err != nil {
		return GroupProjectRelation{}, err
	}
	if locked.SpaceID != before.SpaceID {
		return GroupProjectRelation{}, errProjectRelationConflict
	}
	currentSource := strings.TrimSpace(locked.ProjectID)
	if !changedProjectSourceAllowed(initialSource, currentSource, "") {
		return GroupProjectRelation{}, errProjectRelationConflict
	}
	manager, err := g.db.lockGroupManagerTx(tx, groupNo, actorUID)
	if err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: lock native manager: %v", errProjectRelationDependency, err)
	}
	if !manager {
		return GroupProjectRelation{}, errProjectRelationForbidden
	}
	if currentSource != "" {
		if err := g.db.updateGroupProjectRelationTx(tx, groupNo, "", ""); err != nil {
			return GroupProjectRelation{}, fmt.Errorf("%w: clear relation: %v", errProjectRelationDependency, err)
		}
		locked.ProjectID = ""
		locked.ProjectLinkedBy = nil
	}
	if err := tx.Commit(); err != nil {
		return GroupProjectRelation{}, fmt.Errorf("%w: commit relation unbind: %v", errProjectRelationDependency, err)
	}
	return groupProjectRelationFromRow(locked), nil
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
		return fmt.Errorf("%w: %v", errProjectRelationDependency, err)
	default:
		return fmt.Errorf("%w: %v", errProjectRelationDependency, err)
	}
}
