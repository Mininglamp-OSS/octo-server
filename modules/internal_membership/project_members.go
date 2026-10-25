package internal_membership

import (
	"errors"
	"net/url"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// listProjectMemberships is a peer-authenticated Project roster read. The peer
// must authorize its caller before invoking it; no human session or OBO grant
// is accepted here. The data and wire projection are shared with the public API.
func (m *Module) listProjectMemberships(c *wkhttp.Context) {
	c.Writer.Header().Set("Cache-Control", "no-store")
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		respondInvalidParam(c, "")
		return
	}
	for key, values := range query {
		if (key != "space_id" && key != "project_id" && key != "page" && key != "limit") || len(values) != 1 {
			respondInvalidParam(c, key)
			return
		}
	}
	spaceID, ok := singleQueryValue(query, "space_id")
	if !ok || len(spaceID) > 256 {
		respondInvalidParam(c, "space_id")
		return
	}
	projectID, ok := singleQueryValue(query, "project_id")
	if !ok || len(projectID) > 256 {
		respondInvalidParam(c, "project_id")
		return
	}
	result, err := projectmod.ListProjectMembersInSpace(c.Request.Context(), m.ctx.DB(), spaceID, projectID, projectmod.MemberReadPageParams(query))
	if errors.Is(err, projectmod.ErrMemberListNotFound) {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}
	if err != nil || result == nil {
		m.Error("list Project memberships failed", zap.Error(err), zap.String("space_id", spaceID), zap.String("project_id", projectID))
		respondInternal(c)
		return
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(result.Total, 10))
	c.Response(result.Members)
}
