package project

import (
	"fmt"
	"strings"
)

func (p *Project) readProjectMemberCandidates(
	spaceID, projectID, actorUID, keyword string,
	page projectReadPage,
) (*projectMemberCandidatesResult, error) {
	spaceID = strings.TrimSpace(spaceID)
	projectID = strings.TrimSpace(projectID)
	actorUID = strings.TrimSpace(actorUID)
	keyword = strings.TrimSpace(keyword)
	if page.Limit <= 0 {
		page.Limit = projectDefaultPageLimit
	}
	if page.Offset < 0 {
		page.Offset = 0
	}

	tx, err := p.beginProjectReadTx()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	// This transaction re-checks the active account, Space seat, Project seat,
	// and Project status together. The middleware intentionally leaves this GET
	// path unresolved so a stale cached role cannot authorize the picker.
	access, err := p.readProjectAccessTx(tx, projectID, actorUID, spaceID)
	if err != nil {
		return nil, err
	}
	if !canManageMembers(access.ProjectRole) {
		return nil, errPermissionDenied
	}
	result, err := p.db.listProjectMemberCandidatesReadTx(
		tx, projectID, access.Project.SpaceID, actorUID, keyword, page,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit member candidates read: %w", err)
	}
	return result, nil
}
