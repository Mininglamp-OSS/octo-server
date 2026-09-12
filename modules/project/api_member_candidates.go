package project

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

type memberCandidateResp struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// listMemberCandidatesHandler returns the current organization's human
// directory annotated with the Project membership state. It is intentionally a
// Project-only route: the add endpoint remains the final authority over target
// eligibility at write time.
func (p *Project) listMemberCandidatesHandler(c *wkhttp.Context) {
	spaceID := ""
	if row := requestProject(c); row != nil {
		spaceID = row.SpaceID
	}
	if spaceID == "" {
		spaceID = spacepkg.GetSpaceID(c)
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	actorUID := strings.TrimSpace(c.GetLoginUID())
	result, err := p.readProjectMemberCandidates(
		spaceID, projectID, actorUID, c.Query("keyword"), projectReadPageParams(c),
	)
	if err != nil {
		switch {
		case errors.Is(err, errProjectReadNotFound), errors.Is(err, errProjectGone):
			p.Debug("查询项目成员候选不可读",
				zap.String("projectId", projectID), zap.String("uid", actorUID))
			respondProjectNotFound(c)
		case errors.Is(err, errPermissionDenied):
			httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		default:
			p.Error("查询项目成员候选失败", zap.Error(err),
				zap.String("projectId", projectID), zap.String("uid", actorUID))
			respondQueryFailed(c)
		}
		return
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(result.Total, 10))
	resps := make([]*memberCandidateResp, 0, len(result.Rows))
	for _, row := range result.Rows {
		resps = append(resps, &memberCandidateResp{
			UID:    row.UID,
			Name:   row.Name,
			Status: row.Status,
		})
	}
	c.Response(resps)
}
