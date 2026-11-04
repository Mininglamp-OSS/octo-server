package internal_membership

import (
	"net/url"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// projectLifecycle returns the minimal lifecycle facts used by Fleet to gate
// writes. It is a peer-authenticated scoped read, not a Project-content API.
func (m *Module) projectLifecycle(c *wkhttp.Context) {
	c.Writer.Header().Set("Cache-Control", "no-store")
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		respondInvalidParam(c, "")
		return
	}
	for key, values := range query {
		if key != "space_id" || len(values) != 1 {
			respondInvalidParam(c, key)
			return
		}
	}
	spaceID, ok := singleQueryValue(query, "space_id")
	if !ok || len(spaceID) > 256 {
		respondInvalidParam(c, "space_id")
		return
	}
	projectID := c.Param("project_id")
	if projectID == "" || len(projectID) > 256 || projectID != strings.TrimSpace(projectID) {
		respondInvalidParam(c, "project_id")
		return
	}

	lifecycle, err := m.store.Lifecycle(c.Request.Context(), spaceID, projectID)
	if err != nil {
		m.Error("read Project lifecycle failed",
			zap.Error(err), zap.String("space_id", spaceID), zap.String("project_id", projectID))
		respondInternal(c)
		return
	}
	if lifecycle == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}
	c.Response(lifecycle)
}
