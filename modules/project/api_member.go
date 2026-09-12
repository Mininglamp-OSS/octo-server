package project

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// memberOutcome remains an array response for successful batch writes. A
// rejected add is a single transaction-level error; it never carries a partial
// success report because the batch is atomic.
type memberOutcome struct {
	UID       string `json:"uid"`
	OK        bool   `json:"ok"`
	Reason    string `json:"reason,omitempty"`
	committed bool   `json:"-"`
}

const (
	outcomeNotSpaceMember = reasonNotSpaceMember
	outcomeQuotaMembers   = reasonQuotaMembers
	outcomeNotMember      = "not_member"
	outcomeForbidden      = reasonPermissionDenied
	outcomeLastOwner      = reasonLastOwner
	outcomeStoreFailed    = "store_failed"
	outcomeNotAttempted   = "not_attempted"
)

func (p *Project) addMembersHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryMemberAdd) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canManageMembers(requestProjectRole(c)) {
		observeRejected(entryMemberAdd, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	var req membersReq
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Members) == 0 {
		respondProjectRequestInvalid(c, "members")
		return
	}
	if len(req.Members) > p.cfg.MemberBatchMax {
		respondProjectBatchTooLarge(c, p.cfg.MemberBatchMax)
		return
	}
	members, err := normalizeMemberAdds(req.Members)
	if err != nil {
		switch {
		case errors.Is(err, errMemberRoleInvalid):
			httperr.ResponseErrorL(c, errcode.ErrProjectRoleInvalid, nil, nil)
		case errors.Is(err, errMemberRoleConflict):
			httperr.ResponseErrorL(c, errcode.ErrProjectMemberRoleConflict, nil, nil)
		default:
			respondProjectRequestInvalid(c, "members")
		}
		return
	}
	changed, err := p.addMembers(row.ProjectID, row.SpaceID, c.GetLoginUID(), members)
	if err != nil {
		switch {
		case errors.Is(err, errProjectGone):
			respondProjectNotFound(c)
		case errors.Is(err, errActorNotSpaceMember):
			observeRejected(entryMemberAdd, reasonNotSpaceMember)
			httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
		case errors.Is(err, errNotSpaceMember):
			observeRejected(entryMemberAdd, reasonNotSpaceMember)
			httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotSpaceMember, nil, nil)
		case errors.Is(err, errPermissionDenied):
			observeRejected(entryMemberAdd, reasonPermissionDenied)
			httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		case errors.Is(err, errAgentNotEligible):
			observeRejected(entryMemberAdd, reasonAgentNotEligible)
			var ineligible *agentNotEligibleError
			if errors.As(err, &ineligible) {
				respondProjectAgentNotEligible(c, ineligible.UIDs)
			} else {
				respondProjectAgentNotEligible(c, nil)
			}
		case errors.Is(err, errMemberRoleConflict):
			httperr.ResponseErrorL(c, errcode.ErrProjectMemberRoleConflict, nil, nil)
		case errors.Is(err, errQuotaMembers):
			observeRejected(entryMemberAdd, reasonQuotaMembers)
			respondProjectQuota(c, errcode.ErrProjectQuotaMembers, p.cfg.MaxMembers)
		default:
			p.Error("批量添加项目成员失败", zap.Error(err), zap.String("projectId", row.ProjectID))
			respondStoreFailed(c)
		}
		return
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, uid := range changed {
		changedSet[uid] = struct{}{}
		p.audit(auditMemberAdd, c.GetLoginUID(), uid, row.ProjectID, row.SpaceID, "")
	}
	outcomes := make([]memberOutcome, 0, len(members))
	for _, item := range members {
		_, committed := changedSet[item.UID]
		outcomes = append(outcomes, memberOutcome{UID: item.UID, OK: true, committed: committed})
	}
	c.Response(outcomes)
}

func (p *Project) removeMembersHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryMemberRemove) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canManageMembers(requestProjectRole(c)) {
		observeRejected(entryMemberRemove, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	var req memberUIDsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "uids")
		return
	}
	uids := sanitizeUIDs(req.UIDs)
	if len(uids) == 0 {
		respondProjectRequestInvalid(c, "uids")
		return
	}
	if len(uids) > p.cfg.MemberBatchMax {
		respondProjectBatchTooLarge(c, p.cfg.MemberBatchMax)
		return
	}
	actorUID := c.GetLoginUID()
	outcomes := make([]memberOutcome, 0, len(uids))
	for i, uid := range uids {
		removed, err := p.removeOneFn(row.ProjectID, row.SpaceID, actorUID, uid)
		switch {
		case err == nil:
			outcomes = append(outcomes, memberOutcome{UID: uid, OK: true, committed: removed})
			if removed {
				p.audit(auditMemberRemove, actorUID, uid, row.ProjectID, row.SpaceID, "kicked")
			}
		case errors.Is(err, errActorNotSpaceMember):
			if anyApplied(outcomes) {
				outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeNotSpaceMember})
				outcomes = appendNotAttemptedUIDs(outcomes, uids[i+1:])
				c.Response(outcomes)
				return
			}
			httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
			return
		case errors.Is(err, errPermissionDenied):
			if anyApplied(outcomes) {
				outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeForbidden})
				outcomes = appendNotAttemptedUIDs(outcomes, uids[i+1:])
				c.Response(outcomes)
				return
			}
			httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
			return
		case errors.Is(err, errTargetProtected), errors.Is(err, errSelfRemovalNotAllowed):
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeForbidden})
		case errors.Is(err, errMemberNotFound):
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeNotMember})
		case errors.Is(err, errLastOwnerMustTransfer):
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeLastOwner})
		case errors.Is(err, errProjectGone):
			if anyApplied(outcomes) {
				outcomes = append(outcomes, memberOutcome{UID: uid, Reason: "project_disbanded"})
				outcomes = appendNotAttemptedUIDs(outcomes, uids[i+1:])
				c.Response(outcomes)
				return
			}
			respondProjectNotFound(c)
			return
		default:
			p.Error("移除项目成员失败", zap.Error(err), zap.String("projectId", row.ProjectID), zap.String("targetUid", uid))
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeStoreFailed})
		}
	}
	c.Response(outcomes)
}

func (p *Project) leaveProjectHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryLeave) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !capabilitiesFor(requestProjectRole(c), requestSpaceRole(c)).CanLeave {
		observeRejected(entryLeave, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	err := p.leaveProject(row.ProjectID, row.SpaceID, c.GetLoginUID())
	switch {
	case err == nil:
		p.audit(auditLeave, c.GetLoginUID(), c.GetLoginUID(), row.ProjectID, row.SpaceID, "left")
		c.ResponseOK()
	case errors.Is(err, errActorNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
	case errors.Is(err, errMemberNotFound):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
	case errors.Is(err, errLastOwnerMustTransfer):
		httperr.ResponseErrorL(c, errcode.ErrProjectLastOwnerMustTransfer, nil, nil)
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
	default:
		p.Error("退出项目失败", zap.Error(err), zap.String("projectId", row.ProjectID), zap.String("uid", c.GetLoginUID()))
		respondStoreFailed(c)
	}
}

func (p *Project) updateMemberRoleHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryRoleChange) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	if !canChangeMemberRole(requestProjectRole(c)) {
		observeRejected(entryRoleChange, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	targetUID := c.Param("uid")
	if targetUID == "" {
		respondParamInvalid(c, "uid")
		return
	}
	var req roleReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Role == nil {
		respondProjectRequestInvalid(c, "role")
		return
	}
	if *req.Role != RoleCommon && *req.Role != RoleAdmin {
		httperr.ResponseErrorL(c, errcode.ErrProjectRoleInvalid, nil, nil)
		return
	}
	changed, err := p.changeMemberRole(row.ProjectID, row.SpaceID, c.GetLoginUID(), targetUID, *req.Role)
	switch {
	case err == nil:
		if changed {
			p.audit(auditRoleChange, c.GetLoginUID(), targetUID, row.ProjectID, row.SpaceID, "", zap.Int("new_role", *req.Role))
		}
		c.ResponseOK()
	case errors.Is(err, errActorNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
	case errors.Is(err, errNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotSpaceMember, nil, nil)
	case errors.Is(err, errMemberNotFound):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
	case errors.Is(err, errTargetProtected), errors.Is(err, errPermissionDenied):
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
	default:
		p.Error("修改项目成员角色失败", zap.Error(err), zap.String("projectId", row.ProjectID), zap.String("targetUid", targetUID))
		respondStoreFailed(c)
	}
}

func (p *Project) transferOwnerHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryRoleChange) {
		return
	}
	row := requestProject(c)
	if row == nil {
		respondQueryFailed(c)
		return
	}
	if requestProjectRole(c) != RoleOwner {
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}
	var req ownerTransferReq
	if err := c.ShouldBindJSON(&req); err != nil || req.UID == "" {
		respondProjectRequestInvalid(c, "uid")
		return
	}
	err := p.transferProjectOwner(row.ProjectID, row.SpaceID, c.GetLoginUID(), req.UID)
	switch {
	case err == nil:
		p.audit(auditRoleChange, c.GetLoginUID(), req.UID, row.ProjectID, row.SpaceID, "ownership_transferred", zap.Int("new_role", RoleOwner))
		p.audit(auditRoleChange, c.GetLoginUID(), c.GetLoginUID(), row.ProjectID, row.SpaceID, "ownership_transferred", zap.Int("new_role", RoleAdmin))
		c.ResponseOK()
	case errors.Is(err, errActorNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
	case errors.Is(err, errNotSpaceMember):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotSpaceMember, nil, nil)
	case errors.Is(err, errMemberNotFound):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
	case errors.Is(err, errMemberRoleConflict):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberRoleConflict, nil, nil)
	case errors.Is(err, errPermissionDenied):
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
	default:
		p.Error("转让项目 Owner 失败", zap.Error(err), zap.String("projectId", row.ProjectID), zap.String("uid", req.UID))
		respondStoreFailed(c)
	}
}

func anyApplied(outcomes []memberOutcome) bool {
	for _, o := range outcomes {
		if o.committed {
			return true
		}
	}
	return false
}

func appendNotAttemptedUIDs(outcomes []memberOutcome, rest []string) []memberOutcome {
	for _, uid := range rest {
		outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeNotAttempted})
	}
	return outcomes
}
