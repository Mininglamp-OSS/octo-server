package project

import (
	"fmt"
	"time"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

func (p *Project) setProjectArchived(
	projectID, spaceID, actorUID string, archived bool,
) (*Model, int, error) {
	var project *Model
	var actorRole int
	err := retryOnLockConflict(func() error {
		var attemptErr error
		project, actorRole, attemptErr = p.setProjectArchivedOnce(projectID, spaceID, actorUID, archived)
		return attemptErr
	})
	return project, actorRole, err
}

func (p *Project) setProjectArchivedOnce(
	projectID, spaceID, actorUID string, archived bool,
) (*Model, int, error) {
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID})
	if err != nil {
		return nil, 0, err
	}
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, 0, fmt.Errorf("project: begin archive transition: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs); err != nil {
		return nil, 0, err
	}
	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, 0, err
	}
	if project == nil || project.SpaceID != spaceID {
		return nil, 0, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, 0, err
	}
	if actorRole != RoleOwner {
		return nil, 0, errPermissionDenied
	}
	actorClass, err := p.db.queryAgentClassTx(tx, actorUID)
	if err != nil {
		return nil, 0, err
	}
	if actorClass.IsBot || !actorClass.AccountUsable || spacepkg.IsSystemBot(actorUID) {
		return nil, 0, errPermissionDenied
	}
	if !p.lifecycleEventsEnabled() {
		return nil, 0, errProjectLifecycleDisabled
	}
	if !project.ActivatedAt.Valid {
		return nil, 0, errProjectNotReady
	}
	ready, err := p.db.fleetWorkspaceReadyTx(tx, projectID, project.SpaceID)
	if err != nil {
		return nil, 0, err
	}
	if !ready {
		return nil, 0, errProjectNotReady
	}
	if project.ArchivedAt.Valid == archived {
		return project, actorRole, nil
	}

	now := time.Now().UTC()
	archiveVersion := project.ArchiveVersion
	var archivedAt interface{}
	eventType := LifecycleEventRestored
	var payload any
	if archived {
		archiveVersion = project.LifecycleVersion + 1
		archivedAt = now
		eventType = LifecycleEventArchived
		payload = archivedPayload{Reason: "owner_requested"}
	} else {
		if archiveVersion <= 0 || archiveVersion >= project.LifecycleVersion+1 {
			return nil, 0, fmt.Errorf("project: invalid archive version %d for restore of %s", archiveVersion, projectID)
		}
		payload = restoredPayload{ArchiveVersion: archiveVersion}
	}
	version, err := p.db.updateArchiveStateTx(tx, projectID, archivedAt, archiveVersion, now)
	if err != nil {
		return nil, 0, err
	}
	if version != project.LifecycleVersion+1 {
		return nil, 0, fmt.Errorf("project: archive lifecycle version advanced from %d to %d", project.LifecycleVersion, version)
	}
	if err := p.enqueueLifecycleEventTx(tx, lifecycleEventInput{
		EventType: eventType, ProjectID: projectID, SpaceID: project.SpaceID,
		ProjectVersion: &version, Payload: payload, OccurredAt: now,
	}, now); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("project: commit archive transition: %w", err)
	}

	project.LifecycleVersion = version
	project.ArchiveVersion = archiveVersion
	project.ArchivedAt.Valid = archived
	if archived {
		project.ArchivedAt.Time = now
	} else {
		project.ArchivedAt.Time = time.Time{}
	}
	project.UpdatedAt = now
	return project, actorRole, nil
}
