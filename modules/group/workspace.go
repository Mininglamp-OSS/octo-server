package group

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/workspace"
)

// errGroupWorkspaceConflict is deliberately private. The wire code is owned
// by pkg/errcode and is selected at the HTTP boundary; service/database code
// must not depend on the renderer package.
var (
	errGroupWorkspaceConflict = errors.New("group workspace relation conflict")
	errGroupWorkspaceCorrupt  = errors.New("group workspace relation pair is corrupt")
)

func groupWorkspaceFromRow(row *groupWorkspaceRow) GroupWorkspace {
	if row == nil {
		return GroupWorkspace{}
	}
	workspaceID := workspaceIDFromPointer(row.WorkspaceID)
	if workspaceID == "" {
		return GroupWorkspace{
			GroupNo: row.GroupNo,
			Name:    row.Name,
		}
	}
	workspaceIDValue := workspaceID
	var linkedBy *string
	if row.WorkspaceLinkedBy != nil {
		value := strings.TrimSpace(*row.WorkspaceLinkedBy)
		if value != "" {
			linkedBy = &value
		}
	}
	return GroupWorkspace{
		GroupNo:     row.GroupNo,
		Name:        row.Name,
		WorkspaceID: &workspaceIDValue,
		LinkedBy:    linkedBy,
	}
}

func workspaceIDFromPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func validateGroupWorkspacePair(row *groupWorkspaceRow) error {
	if row == nil {
		return workspace.ErrNotFound
	}
	if row.WorkspaceID == nil && row.WorkspaceLinkedBy == nil {
		return nil
	}
	if row.WorkspaceID == nil || row.WorkspaceLinkedBy == nil {
		return errGroupWorkspaceCorrupt
	}
	if strings.TrimSpace(*row.WorkspaceID) == "" || strings.TrimSpace(*row.WorkspaceLinkedBy) == "" {
		return errGroupWorkspaceCorrupt
	}
	return nil
}

func groupWorkspaceAccessIDs(sourceID, targetID string) []string {
	ids := make([]string, 0, 2)
	if sourceID != "" {
		ids = append(ids, sourceID)
	}
	if targetID != "" && targetID != sourceID {
		ids = append(ids, targetID)
	}
	return ids
}

// changedSourceAllowed reports whether the association observed after the
// group-row lock can safely be handled by the authorization set locked before
// that row. A concurrent replacement to an unobserved source must fail closed;
// accepting it would turn an old source authorization into a new one.
func changedSourceAllowed(initialSource, currentSource, targetID string) bool {
	if initialSource == currentSource {
		return true
	}
	// An intervening unbind is a fresh bind, and an intervening bind to this
	// request's already-authorized target is an idempotent no-op. Any other
	// source was not included in LockAccessesTx and therefore cannot proceed.
	return currentSource == "" || currentSource == targetID
}

func normalizeWorkspacePageSize(size int) int {
	if size <= 0 {
		return 15
	}
	if size > 200 {
		return 200
	}
	return size
}

// workspacePageOffset computes an offset without allowing a page-index
// multiplication to wrap. Returning ok=false tells the caller to return an
// empty page after its count query, preserving the real total and avoiding a
// database-driver 5xx for pathological but syntactically valid page indexes.
func workspacePageOffset(page workspace.Page) (offset uint64, ok bool) {
	if page.Index <= 1 {
		return 0, true
	}
	size := normalizeWorkspacePageSize(page.Size)
	index := uint64(page.Index - 1)
	const maxInt64Uint = uint64(^uint64(0) >> 1)
	if index > maxInt64Uint/uint64(size) {
		return 0, false
	}
	return index * uint64(size), true
}

func validateWorkspaceKeyword(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > 30 {
		return "", fmt.Errorf("%w: keyword", workspace.ErrRequestInvalid)
	}
	return keyword, nil
}
func wrapGroupWorkspaceDependency(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", workspace.ErrDependencyUnavailable, operation, err)
}

// ReadGroupWorkspace owns the complete single-relation read protocol. The
// handler supplies only normalized route input and request scope; this method
// keeps the relation row, authorization checks, and public projection on one
// repeatable-read snapshot.
func (s *Service) ReadGroupWorkspace(ctx context.Context, groupNo string, scope workspace.Scope) (GroupWorkspace, error) {
	tx, err := s.ctx.DB().BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return GroupWorkspace{}, wrapGroupWorkspaceDependency("begin group Workspace read transaction", err)
	}
	defer tx.RollbackUnlessCommitted()

	row, err := s.db.queryGroupWorkspaceTx(tx, groupNo)
	if err != nil {
		return GroupWorkspace{}, wrapGroupWorkspaceDependency("query group Workspace relation", err)
	}
	if row == nil || row.Status == GroupStatusDisband {
		return GroupWorkspace{}, workspace.ErrNotFound
	}
	if strings.TrimSpace(row.SpaceID) == "" {
		return GroupWorkspace{}, workspace.ErrSpaceRequired
	}
	if err := validateGroupWorkspacePair(row); err != nil {
		return GroupWorkspace{}, err
	}

	workspaceID := workspaceIDFromPointer(row.WorkspaceID)
	if workspaceID == "" {
		active, err := s.db.ExistMemberActiveTx(tx, scope.UID, groupNo)
		if err != nil {
			return GroupWorkspace{}, wrapGroupWorkspaceDependency("check native group membership", err)
		}
		if !active {
			return GroupWorkspace{}, workspace.ErrForbidden
		}
		spaceAccess, err := s.db.queryGroupSpaceAccessTx(tx, row.SpaceID, scope.UID)
		if err != nil {
			return GroupWorkspace{}, wrapGroupWorkspaceDependency("check group Space access", err)
		}
		if !spaceAccess {
			return GroupWorkspace{}, workspace.ErrForbidden
		}
	} else {
		// A native group member is not enough to read a bound relation.
		// ReadWorkspaceTx revalidates active Workspace membership and
		// organization status on this same snapshot/connection, so a
		// Workspace-only member can read restricted relation metadata while a
		// group-only member cannot.
		ws, err := workspace.NewService(s.ctx).ReadWorkspaceTx(tx, scope, workspaceID)
		if err != nil {
			return GroupWorkspace{}, err
		}
		if ws == nil || ws.SpaceID != row.SpaceID {
			return GroupWorkspace{}, errGroupWorkspaceConflict
		}
	}

	if err := tx.Commit(); err != nil {
		return GroupWorkspace{}, wrapGroupWorkspaceDependency("commit group Workspace read", err)
	}
	return groupWorkspaceFromRow(row), nil
}

// ListWorkspaceGroups owns the authorized Workspace relation list read. Both
// count and rows are queried before committing the same repeatable-read
// snapshot, so pagination metadata cannot drift from the returned page.
func (s *Service) ListWorkspaceGroups(ctx context.Context, workspaceID, keyword string, page workspace.Page, scope workspace.Scope) (workspace.Pagination[GroupWorkspace], error) {
	tx, err := s.ctx.DB().BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return workspace.Pagination[GroupWorkspace]{}, wrapGroupWorkspaceDependency("begin Workspace group list transaction", err)
	}
	defer tx.RollbackUnlessCommitted()

	ws, err := workspace.NewService(s.ctx).ReadWorkspaceTx(tx, scope, workspaceID)
	if err != nil {
		return workspace.Pagination[GroupWorkspace]{}, err
	}
	if ws == nil || strings.TrimSpace(ws.SpaceID) == "" {
		return workspace.Pagination[GroupWorkspace]{}, workspace.ErrSpaceRequired
	}

	count, err := s.db.queryWorkspaceGroupCountTx(tx, ws.SpaceID, workspaceID, keyword)
	if err != nil {
		return workspace.Pagination[GroupWorkspace]{}, wrapGroupWorkspaceDependency("count Workspace groups", err)
	}
	list, err := s.db.queryWorkspaceGroupsTx(tx, ws.SpaceID, workspaceID, keyword, page)
	if err != nil {
		return workspace.Pagination[GroupWorkspace]{}, wrapGroupWorkspaceDependency("query Workspace groups", err)
	}
	if list == nil {
		list = make([]GroupWorkspace, 0)
	}
	if err := tx.Commit(); err != nil {
		return workspace.Pagination[GroupWorkspace]{}, wrapGroupWorkspaceDependency("commit Workspace group list", err)
	}
	return workspace.Pagination[GroupWorkspace]{Count: count, List: list}, nil
}

// resolveGroupWorkspaceSpace is the derived-space callback used by the four
// relation routes. The group row is authoritative; no client-provided Space
// value is used to choose a tenant.
func (g *Group) resolveGroupWorkspaceSpace(c *wkhttp.Context) (string, error) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		return "", workspace.ErrRequestInvalid
	}
	row, err := g.db.queryGroupWorkspace(groupNo)
	if err != nil {
		return "", fmt.Errorf("resolve group Space: %w: %v", workspace.ErrDependencyUnavailable, err)
	}
	if row == nil || row.Status == GroupStatusDisband {
		return "", workspace.ErrNotFound
	}
	if strings.TrimSpace(row.SpaceID) == "" {
		return "", workspace.ErrSpaceRequired
	}
	if err := validateGroupWorkspacePair(row); err != nil {
		return "", err
	}
	return row.SpaceID, nil
}

// resolveWorkspaceGroupsSpace derives the Space for GET /v1/groups from the
// target Workspace ID. Authorization is performed by the handler's
// transaction-owned ReadWorkspaceTx validation.
func (g *Group) resolveWorkspaceGroupsSpace(c *wkhttp.Context) (string, error) {
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	if workspaceID == "" {
		return "", workspace.ErrRequestInvalid
	}
	spaceID, err := workspace.NewService(g.ctx).ResolveSpace(workspaceID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(spaceID) == "" {
		return "", workspace.ErrSpaceRequired
	}
	return spaceID, nil
}
