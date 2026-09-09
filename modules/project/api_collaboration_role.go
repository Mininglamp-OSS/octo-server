package project

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

func (p *Project) requireCollaborationRoleWriteEnabled(c *wkhttp.Context, entry string) bool {
	if p.cfg.CollaborationRoleEnabled {
		return true
	}
	observeRejected(entry, reasonFlagOff)
	observeCollaborationRoleWrite(entry, collaborationRoleOutcomeRejected)
	httperr.ResponseErrorL(c, errcode.ErrProjectCollaborationRoleDisabled, nil, nil)
	return false
}

func (p *Project) listCollaborationRolesHandler(c *wkhttp.Context) {
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canViewMembers(requestProjectRole(c), requestSpaceRole(c)) {
		httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		return
	}
	roles, epoch, err := p.listCollaborationRoles(row.ProjectID)
	if err != nil {
		if errors.Is(err, errProjectGone) {
			respondProjectNotFound(c)
			return
		}
		p.Error("查询项目协作角色失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(&collaborationRoleCatalogResp{CollaborationRoleEpoch: epoch, Roles: roles})
}

func (p *Project) createCollaborationRoleHandler(c *wkhttp.Context) {
	if !p.requireCollaborationRoleWriteEnabled(c, entryCollaborationRoleCreate) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if requestProjectRole(c) != RoleOwner {
		p.rejectCollaborationRoleWrite(c, entryCollaborationRoleCreate)
		return
	}
	var req collaborationRoleNameReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	actorUID := c.GetLoginUID()
	role, err := p.createCollaborationRole(row.ProjectID, row.SpaceID, actorUID, req.Name)
	if err != nil {
		p.respondCollaborationRoleWriteError(c, entryCollaborationRoleCreate, row.ProjectID, "", err)
		return
	}
	observeCollaborationRoleWrite(entryCollaborationRoleCreate, collaborationRoleOutcomeChanged)
	p.audit(auditCollaborationRoleCreate, actorUID, "", row.ProjectID, row.SpaceID, "",
		zap.String("role_id", role.RoleID), zap.Int("role_count", 1))
	c.Response(collaborationRoleToResp(*role))
}

func (p *Project) renameCollaborationRoleHandler(c *wkhttp.Context) {
	if !p.requireCollaborationRoleWriteEnabled(c, entryCollaborationRoleRename) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if requestProjectRole(c) != RoleOwner {
		p.rejectCollaborationRoleWrite(c, entryCollaborationRoleRename)
		return
	}
	roleID := c.Param("role_id")
	if roleID == "" {
		respondParamInvalid(c, "role_id")
		return
	}
	var req collaborationRoleNameReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	actorUID := c.GetLoginUID()
	role, changed, err := p.renameCollaborationRole(
		row.ProjectID, row.SpaceID, actorUID, roleID, req.Name)
	if err != nil {
		p.respondCollaborationRoleWriteError(c, entryCollaborationRoleRename, row.ProjectID, "", err)
		return
	}
	outcome := collaborationRoleOutcomeNoop
	if changed {
		outcome = collaborationRoleOutcomeChanged
		p.audit(auditCollaborationRoleRename, actorUID, "", row.ProjectID, row.SpaceID, "",
			zap.String("role_id", role.RoleID), zap.Int("role_count", 1))
	}
	observeCollaborationRoleWrite(entryCollaborationRoleRename, outcome)
	c.Response(collaborationRoleToResp(*role))
}

func (p *Project) deleteCollaborationRoleHandler(c *wkhttp.Context) {
	if !p.requireCollaborationRoleWriteEnabled(c, entryCollaborationRoleDelete) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if requestProjectRole(c) != RoleOwner {
		p.rejectCollaborationRoleWrite(c, entryCollaborationRoleDelete)
		return
	}
	roleID := c.Param("role_id")
	if roleID == "" {
		respondParamInvalid(c, "role_id")
		return
	}
	actorUID := c.GetLoginUID()
	changed, err := p.deleteCollaborationRole(row.ProjectID, row.SpaceID, actorUID, roleID)
	if err != nil {
		p.respondCollaborationRoleWriteError(c, entryCollaborationRoleDelete, row.ProjectID, "", err)
		return
	}
	outcome := collaborationRoleOutcomeNoop
	if changed {
		outcome = collaborationRoleOutcomeChanged
		p.audit(auditCollaborationRoleDelete, actorUID, "", row.ProjectID, row.SpaceID, "",
			zap.String("role_id", roleID), zap.Int("role_count", 1))
	}
	observeCollaborationRoleWrite(entryCollaborationRoleDelete, outcome)
	c.ResponseOK()
}

func (p *Project) replaceMemberCollaborationRolesHandler(c *wkhttp.Context) {
	if !p.requireCollaborationRoleWriteEnabled(c, entryCollaborationRoleBind) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canManageMembers(requestProjectRole(c)) {
		p.rejectCollaborationRoleWrite(c, entryCollaborationRoleBind)
		return
	}
	targetUID := c.Param("uid")
	if targetUID == "" {
		respondParamInvalid(c, "uid")
		return
	}
	var req collaborationRoleBindingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	actorUID := c.GetLoginUID()
	changed, err := p.replaceMemberCollaborationRoles(
		row.ProjectID, row.SpaceID, actorUID, targetUID, req.RoleIDs)
	if err != nil {
		p.respondCollaborationRoleWriteError(c, entryCollaborationRoleBind, row.ProjectID, targetUID, err)
		return
	}
	outcome := collaborationRoleOutcomeNoop
	if changed {
		outcome = collaborationRoleOutcomeChanged
		reason := "replace"
		if len(req.RoleIDs) == 0 {
			reason = "clear"
		}
		p.audit(auditCollaborationRoleBind, actorUID, targetUID, row.ProjectID, row.SpaceID, reason,
			zap.Strings("role_ids", req.RoleIDs), zap.Int("role_count", len(req.RoleIDs)))
	}
	observeCollaborationRoleWrite(entryCollaborationRoleBind, outcome)
	c.ResponseOK()
}

func (p *Project) rejectCollaborationRoleWrite(c *wkhttp.Context, entry string) {
	observeRejected(entry, reasonPermissionDenied)
	observeCollaborationRoleWrite(entry, collaborationRoleOutcomeRejected)
	httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
}

func (p *Project) respondCollaborationRoleWriteError(
	c *wkhttp.Context, entry, projectID, targetUID string, err error,
) {
	observeCollaborationRoleWrite(entry, collaborationRoleOutcomeRejected)
	switch {
	case errors.Is(err, errActorNotSpaceMember):
		observeRejected(entry, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
	case errors.Is(err, errProjectGone):
		observeRejected(entry, reasonProjectDisbanded)
		respondProjectNotFound(c)
	case errors.Is(err, errPermissionDenied):
		observeRejected(entry, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
	case errors.Is(err, errCollaborationRoleNameInvalid):
		observeRejected(entry, reasonCollaborationRoleNameInvalid)
		httperr.ResponseErrorL(c, errcode.ErrProjectCollaborationRoleNameInvalid, nil, i18n.Details{
			"field": "name", "max_chars": maxCollaborationRoleNameChars,
		})
	case errors.Is(err, errCollaborationRoleInvalid):
		observeRejected(entry, reasonCollaborationRoleInvalid)
		httperr.ResponseErrorL(c, errcode.ErrProjectCollaborationRoleInvalid, nil, nil)
	case errors.Is(err, errCollaborationRoleTargetInvalid), errors.Is(err, errNotSpaceMember):
		observeRejected(entry, reasonCollaborationRoleTargetInvalid)
		httperr.ResponseErrorL(c, errcode.ErrProjectCollaborationRoleTargetInvalid, nil, nil)
	case errors.Is(err, errCollaborationRoleDuplicated):
		observeRejected(entry, reasonCollaborationRoleDuplicated)
		httperr.ResponseErrorL(c, errcode.ErrProjectCollaborationRoleDuplicated, nil, nil)
	case errors.Is(err, errQuotaCollaborationRoles):
		observeRejected(entry, reasonQuotaCollaborationRoles)
		respondProjectQuota(c, errcode.ErrProjectQuotaCollaborationRoles,
			p.cfg.CollaborationRoleMaxPerProject)
	case errors.Is(err, errQuotaMemberCollaborationRoles):
		observeRejected(entry, reasonQuotaMemberCollaborationRoles)
		respondProjectQuota(c, errcode.ErrProjectQuotaMemberCollaborationRoles,
			p.cfg.CollaborationRoleMaxPerMember)
	default:
		observeRejected(entry, outcomeStoreFailed)
		p.Error("项目协作角色写入失败", zap.Error(err), zap.String("projectId", projectID),
			zap.String("targetUid", targetUID), zap.String("entry", entry))
		respondStoreFailed(c)
	}
}
