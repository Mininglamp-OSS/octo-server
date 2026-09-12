package project

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// listProjectGroupsHandler answers GET /v1/projects/:project_id/groups.
//
// The relation list is Project-scoped: every live group currently associated
// with this Project is returned, regardless of whether the caller is a native
// member of that group. Project membership is the authorization boundary;
// native group membership remains an independent content-access fact.
func (p *Project) listProjectGroupsHandler(c *wkhttp.Context) {
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !isProjectMember(requestProjectRole(c)) {
		httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		return
	}

	keyword := strings.TrimSpace(c.Query("keyword"))
	offset, limit := pageParams(c)
	items, total, err := p.db.listProjectGroupRelations(
		c.Request.Context(), row.ProjectID, row.SpaceID, c.GetLoginUID(), keyword, offset, limit,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrGroupProjectInvalid):
			respondProjectRequestInvalid(c, "keyword")
		case errors.Is(err, ErrGroupProjectNotFound), errors.Is(err, ErrGroupProjectSpaceConflict):
			respondProjectNotFound(c)
		case errors.Is(err, ErrGroupProjectForbidden):
			httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		default:
			p.Error("查询项目关联群列表失败", zap.Error(err),
				zap.String("projectId", row.ProjectID), zap.String("spaceId", row.SpaceID))
			respondQueryFailed(c)
		}
		return
	}
	c.Header("X-Total-Count", strconv.FormatInt(total, 10))
	if items == nil {
		items = make([]ProjectGroupRelation, 0)
	}
	c.Response(items)
}

// updateProjectGroupSettingHandler answers PUT /v1/projects/:project_id/groups/:group_no/setting.
//
// The route is Project-member scoped, but the transaction performs the
// authoritative Space, Project, and group-relation checks again before writing.
// A Project member need not be a native member of the group: pinning only
// changes this user's Project group list and grants no chat access.
func (p *Project) updateProjectGroupSettingHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectSetting) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}

	var req settingReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Pinned == nil {
		respondProjectRequestInvalid(c, "pinned")
		return
	}
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		respondProjectRequestInvalid(c, "group_no")
		return
	}

	err := p.updateProjectGroupSetting(
		row.ProjectID,
		row.SpaceID,
		groupNo,
		c.GetLoginUID(),
		*req.Pinned,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrGroupProjectInvalid):
			respondProjectRequestInvalid(c, "group_no")
		case errors.Is(err, ErrGroupProjectNotFound), errors.Is(err, ErrGroupProjectSpaceConflict):
			respondProjectNotFound(c)
		case errors.Is(err, ErrGroupProjectForbidden):
			httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		default:
			p.Error("写入项目关联群个人设置失败", zap.Error(err),
				zap.String("projectId", row.ProjectID),
				zap.String("groupNo", groupNo),
				zap.String("spaceId", row.SpaceID))
			respondStoreFailed(c)
		}
		return
	}
	c.ResponseOK()
}
