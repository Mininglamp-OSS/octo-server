package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// ErrMemberListNotFound folds unavailable or incorrectly scoped Projects together.
var ErrMemberListNotFound = errors.New("project member list is not available in this scope")

// ListProjectMembersInSpace is a peer-authorized read service. Its caller must
// authorize the end user first. The active Space/Project checks, counted page,
// identities and roles share one bounded read-only snapshot. It creates no
// Project API instance, workers or runtime registrations.
func ListProjectMembersInSpace(ctx context.Context, session *dbr.Session, spaceID, projectID string, page MemberReadPage) (*MemberListResult, error) {
	if session == nil {
		return nil, fmt.Errorf("project: database session unavailable")
	}
	if spaceID == "" || projectID == "" ||
		spaceID != strings.TrimSpace(spaceID) || projectID != strings.TrimSpace(projectID) ||
		len(spaceID) > 256 || len(projectID) > 256 ||
		page.Limit < 1 || page.Limit > projectMaxPageLimit || page.Offset < 0 || page.Offset > projectMaxPage*page.Limit {
		return nil, ErrMemberListNotFound
	}
	timeout := projectpkg.EpochsReadTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tx, err := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	tx.Timeout = timeout
	qualified, found, err := space.ResolveSpaceID(tx, spaceID)
	if err != nil {
		return nil, err
	}
	if !found || projectpkg.FoldID(qualified) != projectpkg.FoldID(spaceID) {
		return nil, ErrMemberListNotFound
	}
	active, err := space.IsActiveSpace(tx, qualified)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrMemberListNotFound
	}
	db := &DB{session: session}
	project, err := db.queryProjectReadTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || project.Status != StatusNormal ||
		projectpkg.FoldID(project.ProjectID) != projectpkg.FoldID(projectID) ||
		projectpkg.FoldID(project.SpaceID) != projectpkg.FoldID(qualified) {
		return nil, ErrMemberListNotFound
	}
	result, err := readProjectMembersTx(db, tx, project.ProjectID, projectReadPage{Offset: page.Offset, Limit: page.Limit})
	if err != nil {
		return nil, err
	}
	return &MemberListResult{Members: projectMemberListResponse(result.Rows), Total: result.Total}, nil
}
