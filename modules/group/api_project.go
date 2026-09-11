package group

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// routeProject mounts the group-owned side of the Group↔Project relation.
// Authorization is performed by the transaction service from the authoritative
// group, Project, and native group rows; no caller-selected Space is trusted.
func (g *Group) routeProject(r *wkhttp.WKHttp) {
	routes := r.Group("/v1/groups", g.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, g.ctx))
	routes.GET("/:group_no/project", g.groupProjectGet)
	routes.PUT("/:group_no/project", g.protectAIContainerMutation, g.groupProjectPut)
	routes.DELETE("/:group_no/project", g.protectAIContainerMutation, g.groupProjectDelete)
}

func (g *Group) groupProjectGet(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupProjectError(c, errProjectRelationInvalid)
		return
	}
	result, err := g.readGroupProject(c.Request.Context(), groupNo, c.GetLoginUID())
	if err != nil {
		respondGroupProjectError(c, err)
		return
	}
	c.Response(result)
}

type groupProjectPutRequest struct {
	ProjectID string `json:"project_id"`
}

func (g *Group) groupProjectPut(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupProjectError(c, errProjectRelationInvalid)
		return
	}
	var req groupProjectPutRequest
	if err := c.BindJSON(&req); err != nil {
		respondGroupProjectError(c, errProjectRelationInvalid)
		return
	}
	result, err := g.bindGroupProject(c.GetLoginUID(), groupNo, req.ProjectID)
	if err != nil {
		respondGroupProjectError(c, err)
		return
	}
	c.Response(result)
}

func (g *Group) groupProjectDelete(c *wkhttp.Context) {
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondGroupProjectError(c, errProjectRelationInvalid)
		return
	}
	result, err := g.unbindGroupProject(c.GetLoginUID(), groupNo)
	if err != nil {
		respondGroupProjectError(c, err)
		return
	}
	c.Response(result)
}

func respondGroupProjectError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, aiteampkg.ErrContainerProtected):
		httperr.ResponseErrorL(c, errcode.ErrAITeamContainerProtected, nil, nil)
	case errors.Is(err, errProjectRelationInvalid):
		httperr.ResponseErrorL(c, errcode.ErrGroupRequestInvalid, nil, nil)
	case errors.Is(err, errProjectRelationNotFound):
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
	case errors.Is(err, errProjectRelationForbidden):
		httperr.ResponseErrorL(c, errcode.ErrGroupViewForbidden, nil, nil)
	case errors.Is(err, errProjectRelationConflict):
		httperr.ResponseErrorL(c, errcode.ErrGroupProjectConflict, nil, nil)
	case errors.Is(err, errProjectRelationCorrupt), errors.Is(err, errProjectRelationDependency):
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
	default:
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
	}
}

// mapCreateProjectGroupError translates only the expected Project admission
// sentinels. CreateProjectGroup also returns ordinary DB/IM failures; those
// must stay on groupCreate's internal store_failed path rather than falling
// through respondGroupProjectError's generic query_failed mapping.
func mapCreateProjectGroupError(err error) (error, bool) {
	switch {
	case errors.Is(err, projectmod.ErrGroupProjectInvalid),
		errors.Is(err, projectmod.ErrGroupProjectNotFound),
		errors.Is(err, projectmod.ErrGroupProjectForbidden),
		errors.Is(err, projectmod.ErrGroupProjectSpaceConflict):
		return mapProjectAccessError(err), true
	default:
		return nil, false
	}
}
