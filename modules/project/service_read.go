package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gocraft/dbr/v2"
)

var (
	errProjectReadNotFound  = errors.New("project: project is not readable")
	errProjectReadForbidden = errors.New("project: project read requires an active space member")
)

func (p *Project) beginProjectReadTx() (*dbr.Tx, error) {
	if p == nil || p.db == nil || p.db.session == nil {
		return nil, fmt.Errorf("project: begin read: database session unavailable")
	}
	tx, err := p.db.session.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("project: begin read: %w", err)
	}
	return tx, nil
}

// projectReadSpaceAccessTx validates the caller's account and active Space
// membership in the read transaction. It deliberately uses point reads instead
// of joins: the legacy tables may have different collations in imported
// deployments, and authorization must not depend on a cross-table comparison.
func (p *Project) projectReadSpaceAccessTx(tx *dbr.Tx, spaceID, uid string) (int, bool, error) {
	spaceID = strings.TrimSpace(spaceID)
	uid = strings.TrimSpace(uid)
	if spaceID == "" || uid == "" {
		return roleNonMember, false, nil
	}
	eligible, err := p.db.queryReadUserEligibleTx(tx, uid)
	if err != nil {
		return roleNonMember, false, err
	}
	if !eligible {
		return roleNonMember, false, nil
	}
	active, err := p.db.queryReadSpaceActiveTx(tx, spaceID)
	if err != nil {
		return roleNonMember, false, err
	}
	if !active {
		return roleNonMember, false, nil
	}
	role, member, err := p.db.queryReadSpaceMemberTx(tx, spaceID, uid)
	if err != nil {
		return roleNonMember, false, err
	}
	if !member {
		return roleNonMember, false, nil
	}
	return role, true, nil
}

// readProjectAccessTx is the single project-resource read authorization path.
// It is transaction-consuming so callers can read the project, caller seat,
// counts, pin and roster from one repeatable-read snapshot.
func (p *Project) readProjectAccessTx(tx *dbr.Tx, projectID, uid, expectedSpaceID string) (*projectReadAccess, error) {
	projectID = strings.TrimSpace(projectID)
	expectedSpaceID = strings.TrimSpace(expectedSpaceID)
	if projectID == "" || uid == "" {
		return nil, errProjectReadNotFound
	}
	spaceRole, ok, err := p.projectReadSpaceAccessTx(tx, expectedSpaceID, uid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errProjectReadNotFound
	}
	project, err := p.db.queryProjectReadTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || project.Status != StatusNormal || project.SpaceID != expectedSpaceID {
		return nil, errProjectReadNotFound
	}
	projectRole, member, err := p.db.queryProjectReadMemberRoleTx(tx, projectID, project.SpaceID, uid)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errProjectReadNotFound
	}
	return &projectReadAccess{
		Project:     project,
		ProjectRole: projectRole,
		SpaceRole:   spaceRole,
	}, nil
}

func (p *Project) readProjects(spaceID, uid, keyword string, page projectReadPage) (*projectReadListResult, error) {
	spaceID = strings.TrimSpace(spaceID)
	uid = strings.TrimSpace(uid)
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
	if _, ok, err := p.projectReadSpaceAccessTx(tx, spaceID, uid); err != nil {
		return nil, err
	} else if !ok {
		return nil, errProjectReadForbidden
	}
	result, err := p.db.listProjectsReadTx(tx, spaceID, uid, keyword, page)
	if err != nil {
		return nil, err
	}
	projectIDs := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		projectIDs = append(projectIDs, row.ProjectID)
	}
	counts, err := p.db.projectReadSeatCountsTx(tx, projectIDs)
	if err != nil {
		return nil, err
	}
	for _, row := range result.Rows {
		count := counts[row.ProjectID]
		row.Humans = count.Humans
		row.Agents = count.Agents
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit project list read: %w", err)
	}
	return result, nil
}

func (p *Project) readProject(spaceID, projectID, uid string) (*projectReadProjectResult, error) {
	spaceID = strings.TrimSpace(spaceID)
	projectID = strings.TrimSpace(projectID)
	uid = strings.TrimSpace(uid)
	tx, err := p.beginProjectReadTx()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	access, err := p.readProjectAccessTx(tx, projectID, uid, spaceID)
	if err != nil {
		return nil, err
	}
	counts, err := p.db.projectReadSeatCountsTx(tx, []string{projectID})
	if err != nil {
		return nil, err
	}
	pinned, err := p.db.queryProjectPinnedReadTx(tx, projectID, uid)
	if err != nil {
		return nil, err
	}
	count := counts[projectID]
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit project detail read: %w", err)
	}
	return &projectReadProjectResult{
		Project:   access.Project,
		Role:      access.ProjectRole,
		SpaceRole: access.SpaceRole,
		Humans:    count.Humans,
		Agents:    count.Agents,
		Pinned:    pinned,
	}, nil
}

func (p *Project) readProjectMember(spaceID, projectID, actorUID, targetUID string) (*projectReadMemberRow, error) {
	spaceID = strings.TrimSpace(spaceID)
	projectID = strings.TrimSpace(projectID)
	actorUID = strings.TrimSpace(actorUID)
	targetUID = strings.TrimSpace(targetUID)
	if targetUID == "" {
		return nil, errProjectReadNotFound
	}
	tx, err := p.beginProjectReadTx()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if _, err := p.readProjectAccessTx(tx, projectID, actorUID, spaceID); err != nil {
		return nil, err
	}
	member, err := p.db.queryProjectMemberReadTx(tx, projectID, targetUID)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, errProjectReadNotFound
	}
	if err := p.readProjectMemberRolesTx(tx, projectID, []*projectReadMemberRow{member}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit project member read: %w", err)
	}
	return member, nil
}

func (p *Project) readProjectMemberRolesTx(tx *dbr.Tx, projectID string, members []*projectReadMemberRow) error {
	uids := make([]string, 0, len(members))
	for _, member := range members {
		uids = append(uids, member.UID)
	}
	roles, err := p.db.memberCollaborationRolesReadTx(tx, projectID, uids)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Robot == 1 {
			roles[member.UID] = []CollaborationRoleResp{}
		}
		member.Roles = roles[member.UID]
	}
	return nil
}

func (p *Project) readProjectMembers(spaceID, projectID, uid string, page projectReadPage) (*projectReadMembersResult, error) {
	spaceID = strings.TrimSpace(spaceID)
	projectID = strings.TrimSpace(projectID)
	uid = strings.TrimSpace(uid)
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
	if _, err := p.readProjectAccessTx(tx, projectID, uid, spaceID); err != nil {
		return nil, err
	}
	result, err := p.db.listProjectMembersReadTx(tx, projectID, page)
	if err != nil {
		return nil, err
	}
	if err := p.readProjectMemberRolesTx(tx, projectID, result.Rows); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit project members read: %w", err)
	}
	return result, nil
}

type projectReadAccess struct {
	Project     *Model
	ProjectRole int
	SpaceRole   int
}
