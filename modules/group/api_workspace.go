package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// routeWorkspace mounts only the group-owned relation surface. It deliberately
// uses separate derived-Space resolvers for a group resource and a Workspace
// collection: neither path accepts a caller-selected Space as an authority.
func (g *Group) routeWorkspace(r *wkhttp.WKHttp) {
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, g.ctx)

	groupRoutes := r.Group("/v1/groups", g.ctx.AuthMiddleware(r), uidLimit)
	groupRoutes.GET("/:group_no/workspace",
		workspace.VerifiedSpaceMiddleware(g.ctx, g.resolveGroupWorkspaceSpace),
		g.groupWorkspaceGet)
	groupRoutes.PUT("/:group_no/workspace",
		workspace.VerifiedSpaceMiddleware(g.ctx, g.resolveGroupWorkspaceSpace),
		g.groupWorkspacePut)
	groupRoutes.DELETE("/:group_no/workspace",
		workspace.VerifiedSpaceMiddleware(g.ctx, g.resolveGroupWorkspaceSpace),
		g.groupWorkspaceDelete)

	workspaceRoutes := r.Group("/v1/groups", g.ctx.AuthMiddleware(r), uidLimit)
	workspaceRoutes.GET("",
		workspace.VerifiedSpaceMiddleware(g.ctx, g.resolveWorkspaceGroupsSpace),
		g.groupWorkspaceList)
}

func (g *Group) groupWorkspaceGet(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}

	result, err := g.groupService.ReadGroupWorkspace(
		c.Request.Context(), groupNo, workspace.RequestScope(c),
	)
	if err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	c.Response(result)
}

type groupWorkspacePutRequest struct {
	WorkspaceID string `json:"workspace_id"`
}

func (g *Group) groupWorkspacePut(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}
	var req groupWorkspacePutRequest
	if err := c.BindJSON(&req); err != nil {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}
	targetID := strings.TrimSpace(req.WorkspaceID)
	if targetID == "" {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}

	before, err := g.db.queryGroupWorkspace(groupNo)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "query group Workspace relation", err)
		return
	}
	if before == nil || before.Status == GroupStatusDisband {
		respondGroupWorkspaceError(c, workspace.ErrNotFound)
		return
	}
	if strings.TrimSpace(before.SpaceID) == "" {
		respondGroupWorkspaceError(c, workspace.ErrSpaceRequired)
		return
	}
	if err := validateGroupWorkspacePair(before); err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	initialSource := workspaceIDFromPointer(before.WorkspaceID)
	scope := workspace.RequestScope(c)

	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "begin group Workspace relation transaction", err)
		return
	}
	defer tx.RollbackUnlessCommitted()

	// Lock all source/target Workspace authorization records before taking the
	// group row. The helper checks current Space membership and Workspace
	// membership in the same transaction, avoiding source/target lock inversion.
	// The request header was already checked against the authoritative group
	// Space by VerifiedSpaceMiddleware; leave this assertion empty so a
	// cross-Space target can be authorized first and then reported as the
	// relation's generic semantic conflict below.
	accesses, err := workspace.NewService(g.ctx).LockAccessesTx(
		tx,
		scope.UID,
		groupWorkspaceAccessIDs(initialSource, targetID),
		"",
	)
	if err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}

	locked, err := g.db.lockGroupWorkspaceTx(groupNo, tx)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "lock group Workspace relation", err)
		return
	}
	if locked == nil || locked.Status == GroupStatusDisband {
		respondGroupWorkspaceError(c, workspace.ErrNotFound)
		return
	}
	if err := validateGroupWorkspacePair(locked); err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	if locked.SpaceID != before.SpaceID {
		respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
		return
	}
	currentSource := workspaceIDFromPointer(locked.WorkspaceID)
	if !changedSourceAllowed(initialSource, currentSource, targetID) {
		respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
		return
	}

	targetAccess, ok := accesses[targetID]
	if !ok {
		respondGroupWorkspaceError(c, workspace.ErrForbidden)
		return
	}
	if targetAccess.SpaceID != locked.SpaceID {
		// Do not report Space IDs: this path reaches conflict only after the
		// helper has authorized the target Workspace for this actor.
		respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
		return
	}
	if currentSource != "" {
		sourceAccess, ok := accesses[currentSource]
		if !ok {
			respondGroupWorkspaceError(c, workspace.ErrForbidden)
			return
		}
		if sourceAccess.SpaceID != locked.SpaceID {
			respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
			return
		}
	}

	manager, err := g.db.lockGroupManagerTx(groupNo, scope.UID, tx)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "lock native group manager", err)
		return
	}
	if !manager {
		respondGroupWorkspaceError(c, workspace.ErrForbidden)
		return
	}

	if currentSource == targetID {
		// Same-target PUT is an idempotent no-op and must preserve the original
		// linked_by actor, even if it differs from this request's actor.
		if err := tx.Commit(); err != nil {
			g.respondGroupWorkspaceDependency(c, "commit group Workspace no-op", err)
			return
		}
		c.Response(groupWorkspaceFromRow(locked))
		return
	}

	workspaceIDValue := strings.TrimSpace(targetID)
	linkedByValue := strings.TrimSpace(scope.UID)
	workspaceID := &workspaceIDValue
	linkedBy := &linkedByValue
	if err := g.db.updateGroupWorkspaceTx(groupNo, workspaceID, linkedBy, tx); err != nil {
		g.respondGroupWorkspaceDependency(c, "write group Workspace relation", err)
		return
	}
	locked.WorkspaceID = workspaceID
	locked.WorkspaceLinkedBy = linkedBy
	if err := tx.Commit(); err != nil {
		g.respondGroupWorkspaceDependency(c, "commit group Workspace relation", err)
		return
	}
	c.Response(groupWorkspaceFromRow(locked))
}

func (g *Group) groupWorkspaceDelete(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}

	before, err := g.db.queryGroupWorkspace(groupNo)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "query group Workspace relation", err)
		return
	}
	if before == nil || before.Status == GroupStatusDisband {
		respondGroupWorkspaceError(c, workspace.ErrNotFound)
		return
	}
	if strings.TrimSpace(before.SpaceID) == "" {
		respondGroupWorkspaceError(c, workspace.ErrSpaceRequired)
		return
	}
	if err := validateGroupWorkspacePair(before); err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	initialSource := workspaceIDFromPointer(before.WorkspaceID)
	scope := workspace.RequestScope(c)

	tx, err := g.ctx.DB().Begin()
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "begin group Workspace relation transaction", err)
		return
	}
	defer tx.RollbackUnlessCommitted()

	var accesses map[string]workspace.Access
	if initialSource != "" {
		accesses, err = workspace.NewService(g.ctx).LockAccessesTx(
			tx,
			scope.UID,
			[]string{initialSource},
			scope.ExpectedSpaceID,
		)
		if err != nil {
			respondGroupWorkspaceError(c, err)
			return
		}
	} else {
		accesses = make(map[string]workspace.Access)
		active, lockErr := g.db.lockGroupSpaceMemberTx(before.SpaceID, scope.UID, tx)
		if lockErr != nil {
			g.respondGroupWorkspaceDependency(c, "lock group Space membership", lockErr)
			return
		}
		if !active {
			respondGroupWorkspaceError(c, workspace.ErrForbidden)
			return
		}
	}

	locked, err := g.db.lockGroupWorkspaceTx(groupNo, tx)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "lock group Workspace relation", err)
		return
	}
	if locked == nil || locked.Status == GroupStatusDisband {
		respondGroupWorkspaceError(c, workspace.ErrNotFound)
		return
	}
	if err := validateGroupWorkspacePair(locked); err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	if locked.SpaceID != before.SpaceID {
		respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
		return
	}
	currentSource := workspaceIDFromPointer(locked.WorkspaceID)
	if !changedSourceAllowed(initialSource, currentSource, "") {
		respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
		return
	}
	if currentSource != "" {
		sourceAccess, ok := accesses[currentSource]
		if !ok {
			respondGroupWorkspaceError(c, workspace.ErrForbidden)
			return
		}
		if sourceAccess.SpaceID != locked.SpaceID {
			respondGroupWorkspaceError(c, errGroupWorkspaceConflict)
			return
		}
	}

	manager, err := g.db.lockGroupManagerTx(groupNo, scope.UID, tx)
	if err != nil {
		g.respondGroupWorkspaceDependency(c, "lock native group manager", err)
		return
	}
	if !manager {
		respondGroupWorkspaceError(c, workspace.ErrForbidden)
		return
	}

	if currentSource != "" {
		if err := g.db.updateGroupWorkspaceTx(groupNo, nil, nil, tx); err != nil {
			g.respondGroupWorkspaceDependency(c, "clear group Workspace relation", err)
			return
		}
		locked.WorkspaceID = nil
		locked.WorkspaceLinkedBy = nil
	}
	if err := tx.Commit(); err != nil {
		g.respondGroupWorkspaceDependency(c, "commit group Workspace unbind", err)
		return
	}
	c.Response(groupWorkspaceFromRow(locked))
}

func (g *Group) groupWorkspaceList(c *wkhttp.Context) {
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	if workspaceID == "" {
		respondGroupWorkspaceError(c, workspace.ErrRequestInvalid)
		return
	}
	keyword, err := validateWorkspaceKeyword(c.Query("keyword"))
	if err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	page := workspace.ParsePage(c)

	result, err := g.groupService.ListWorkspaceGroups(
		c.Request.Context(), workspaceID, keyword, page, workspace.RequestScope(c),
	)
	if err != nil {
		respondGroupWorkspaceError(c, err)
		return
	}
	c.Response(result)
}

func respondGroupWorkspaceError(c *wkhttp.Context, err error) {
	if errors.Is(err, errGroupWorkspaceConflict) {
		httperr.ResponseErrorL(c, errcode.ErrGroupWorkspaceConflict, nil, nil)
		return
	}
	workspace.RespondError(c, err)
}

func (g *Group) respondGroupWorkspaceDependency(c *wkhttp.Context, operation string, err error) {
	if errors.Is(err, errGroupWorkspaceCorrupt) {
		respondGroupWorkspaceError(c, err)
		return
	}
	respondGroupWorkspaceError(c, fmt.Errorf("%w: %s: %v", workspace.ErrDependencyUnavailable, operation, err))
}
