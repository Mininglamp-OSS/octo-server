package project

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

func (p *Project) listProjectsReadHandler(c *wkhttp.Context) {
	spaceID := strings.TrimSpace(spacepkg.GetSpaceID(c))
	if spaceID == "" {
		spaceID = strings.TrimSpace(c.Param("space_id"))
	}
	uid := strings.TrimSpace(c.GetLoginUID())
	page := projectReadPageParams(c)
	result, err := p.readProjects(spaceID, uid, strings.TrimSpace(c.Query("keyword")), page)
	if err != nil {
		p.respondProjectReadError(c, err, "查询项目列表失败", spaceID, uid)
		return
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(result.Total, 10))
	resps := make([]*Resp, 0, len(result.Rows))
	for _, row := range result.Rows {
		resps = append(resps, p.toResp(&row.Model, row.MyRole, roleNonMember, row.Humans, row.Agents, row.Pinned == 1))
	}
	c.Response(resps)
}

func (p *Project) getProjectReadHandler(c *wkhttp.Context) {
	spaceID := ""
	if row := requestProject(c); row != nil {
		spaceID = row.SpaceID
	}
	if spaceID == "" {
		spaceID = spacepkg.GetSpaceID(c)
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	uid := strings.TrimSpace(c.GetLoginUID())
	result, err := p.readProject(spaceID, projectID, uid)
	if err != nil {
		p.respondProjectReadError(c, err, "查询项目详情失败", projectID, uid)
		return
	}
	c.Response(p.toResp(result.Project, result.Role, result.SpaceRole, result.Humans, result.Agents, result.Pinned))
}

func (p *Project) listMembersReadHandler(c *wkhttp.Context) {
	spaceID := ""
	if row := requestProject(c); row != nil {
		spaceID = row.SpaceID
	}
	if spaceID == "" {
		spaceID = spacepkg.GetSpaceID(c)
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	uid := strings.TrimSpace(c.GetLoginUID())
	result, err := p.readProjectMembers(spaceID, projectID, uid, projectReadPageParams(c))
	if err != nil {
		p.respondProjectReadError(c, err, "查询项目成员失败", projectID, uid)
		return
	}
	resps := make([]*MemberResp, 0, len(result.Rows))
	for _, member := range result.Rows {
		resps = append(resps, projectMemberReadResponse(member))
	}
	c.Writer.Header().Set("X-Total-Count", strconv.FormatInt(result.Total, 10))
	c.Response(resps)
}

func (p *Project) getMemberReadHandler(c *wkhttp.Context) {
	spaceID := ""
	if row := requestProject(c); row != nil {
		spaceID = row.SpaceID
	}
	if spaceID == "" {
		spaceID = spacepkg.GetSpaceID(c)
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	actorUID := strings.TrimSpace(c.GetLoginUID())
	targetUID := strings.TrimSpace(c.Param("uid"))
	member, err := p.readProjectMember(spaceID, projectID, actorUID, targetUID)
	if err != nil {
		p.respondProjectReadError(c, err, "查询项目成员失败", projectID, actorUID)
		return
	}
	c.Response(projectMemberReadResponse(member))
}

func projectMemberReadResponse(member *projectReadMemberRow) *MemberResp {
	roles := member.Roles
	if roles == nil || member.Robot == 1 {
		roles = []CollaborationRoleResp{}
	}
	return &MemberResp{
		UID:                member.UID,
		Name:               member.Name,
		Role:               member.Role,
		InviteUID:          member.InviteUID,
		Robot:              member.Robot,
		OwnerUID:           member.OwnerUID,
		CollaborationRoles: roles,
		CreatedAt:          formatTime(member.CreatedAt),
		JoinedAt:           formatTime(member.JoinedAt),
	}
}

func (p *Project) respondProjectReadError(c *wkhttp.Context, err error, message, resource, uid string) {
	switch {
	case errors.Is(err, errProjectReadNotFound), errors.Is(err, errProjectGone):
		p.Debug(message+"：项目不可读", zap.String("resource", resource), zap.String("uid", uid))
		respondProjectNotFound(c)
	case errors.Is(err, errProjectReadForbidden):
		respondForbidden(c)
	default:
		p.Error(message, zap.Error(err), zap.String("resource", resource), zap.String("uid", uid))
		respondQueryFailed(c)
	}
}

func projectReadPageParams(c *wkhttp.Context) projectReadPage {
	limit := int64(projectDefaultPageLimit)
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			limit = value
		}
	}
	if limit > int64(projectMaxPageLimit) {
		limit = int64(projectMaxPageLimit)
	}
	page := int64(1)
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			page = value
		}
	}
	if page > int64(projectMaxPage) {
		page = int64(projectMaxPage)
	}
	return projectReadPage{Offset: (page - 1) * limit, Limit: limit}
}
