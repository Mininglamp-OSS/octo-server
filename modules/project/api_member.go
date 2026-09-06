package project

import (
	"errors"
	"io"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// memberOutcome is one target's result in a batch response. The batch reports per
// uid rather than failing whole: an admin adding twelve people, one of whom left the
// Space an hour ago, should get eleven seats and a named rejection — not a 403 with
// nothing done and no indication which uid was the problem.
type memberOutcome struct {
	UID    string `json:"uid"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// committed reports whether THIS target changed the database, as opposed to being an
	// idempotent no-op that still succeeds. It is deliberately not on the wire: the client
	// cares that the add succeeded (ok), while only the partial-batch decision cares whether
	// anything was written.
	committed bool `json:"-"`
}

// Rejection reasons surfaced per uid in a batch response. Low-cardinality enum,
// identical to the metric reasons so a client-visible string and an alert label
// cannot drift.
const (
	outcomeNotSpaceMember = reasonNotSpaceMember
	outcomeQuotaMembers   = reasonQuotaMembers
	outcomeNotMember      = "not_member"
	outcomeForbidden      = reasonPermissionDenied
	outcomeLastOwner      = reasonLastOwner
	outcomeStoreFailed    = "store_failed"
	// outcomeNotAttempted marks targets the handler stopped before reaching, after an
	// actor-level failure mid-batch. It exists so a partially-applied batch can be reported
	// accurately instead of being flattened into one status code.
	outcomeNotAttempted = "not_attempted"
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
	// Cheap pre-check off the middleware's cached role; addOneMember re-reads it under the
	// project lock, which is where the decision actually binds.
	if !canManageMembers(requestProjectRole(c)) {
		observeRejected(entryMemberAdd, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}

	var req membersReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	uids := sanitizeUIDs(req.UIDs)
	if len(uids) == 0 {
		respondProjectRequestInvalid(c, "uids")
		return
	}
	// Structural cap on top of any byte cap: a well-formed payload of ten thousand
	// uids would otherwise become ten thousand membership transactions from one
	// request.
	if len(uids) > p.cfg.MemberBatchMax {
		respondProjectBatchTooLarge(c, p.cfg.MemberBatchMax)
		return
	}

	actorUID := c.GetLoginUID()
	results, err := p.addMembers(row.ProjectID, row.SpaceID, actorUID, uids)
	if err != nil {
		p.Error("批量添加项目成员失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondStoreFailed(c)
		return
	}

	outcomes := make([]memberOutcome, 0, len(results))
	for _, res := range results {
		switch {
		case res.Err == nil:
			outcomes = append(outcomes, memberOutcome{UID: res.UID, OK: true, committed: res.Admitted})
			if res.Admitted {
				p.audit(auditMemberAdd, actorUID, res.UID, row.ProjectID, row.SpaceID, "")
			}
		case errors.Is(res.Err, errNotSpaceMember):
			observeRejected(entryMemberAdd, reasonNotSpaceMember)
			outcomes = append(outcomes, memberOutcome{UID: res.UID, Reason: outcomeNotSpaceMember})
		case errors.Is(res.Err, errQuotaMembers):
			observeRejected(entryMemberAdd, reasonQuotaMembers)
			outcomes = append(outcomes, memberOutcome{UID: res.UID, Reason: outcomeQuotaMembers})
		case errors.Is(res.Err, errPermissionDenied), errors.Is(res.Err, errProjectGone):
			// ACTOR-level or project-level: nothing after this point can succeed, so stop.
			//
			// But HOW we stop depends on whether anything already committed. Each target runs in
			// its own transaction, so by the time the actor loses their rights (or the project is
			// disbanded) mid-batch, earlier targets are durably applied. Returning a bare 403/404
			// here used to discard the outcomes collected so far, telling the caller "nothing
			// happened" while the database disagreed — the worst of the two answers, because a
			// client retrying the whole batch would then re-apply the successful part.
			//
			// With nothing committed yet, the single status code IS the honest answer.
			reason := reasonPermissionDenied
			if errors.Is(res.Err, errProjectGone) {
				reason = reasonProjectDisbanded
			}
			observeRejected(entryMemberAdd, reason)
			if !anyApplied(outcomes) {
				if errors.Is(res.Err, errProjectGone) {
					respondProjectNotFound(c)
				} else {
					httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
				}
				return
			}
			outcomes = append(outcomes, memberOutcome{UID: res.UID, Reason: reason})
			// uids[len(results):], NOT results[i+1:]: addMembers stops the batch at the first
			// actor-level failure, so results is SHORTER than uids. The tail of uids is what
			// was genuinely never attempted; results[i+1:] is empty here and would have made
			// those uids vanish from the response entirely.
			outcomes = appendNotAttemptedUIDs(outcomes, uids[len(results):])
			c.Response(outcomes)
			return
		default:
			observeRejected(entryMemberAdd, outcomeStoreFailed)
			p.Error("添加项目成员失败", zap.Error(res.Err),
				zap.String("projectId", row.ProjectID), zap.String("targetUid", res.UID))
			outcomes = append(outcomes, memberOutcome{UID: res.UID, Reason: outcomeStoreFailed})
		}
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
	// A cheap pre-check off the middleware's cached role, so an obviously unauthorized batch
	// is refused without opening a transaction per uid. It is NOT the authorization
	// decision: removeMember re-reads the actor's role under the project lock, because this
	// one was resolved from cache before any transaction existed.
	if !canManageMembers(requestProjectRole(c)) {
		observeRejected(entryMemberRemove, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
		return
	}

	var req membersReq
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
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
		removed, err := removeOneMemberForTest(p, row.ProjectID, row.SpaceID, actorUID, uid)
		switch {
		case err == nil:
			outcomes = append(outcomes, memberOutcome{UID: uid, OK: true, committed: removed})
			if removed {
				p.audit(auditMemberRemove, actorUID, uid, row.ProjectID, row.SpaceID, "kicked")
			}
		case errors.Is(err, errPermissionDenied):
			// ACTOR-level: the in-lock re-read says the caller no longer holds the role this
			// endpoint needs. Nothing further can succeed, so stop — but report what already
			// committed rather than collapsing a partially-applied batch into one 403. See the
			// add path for why discarding the outcomes is the worse of the two answers.
			observeRejected(entryMemberRemove, reasonPermissionDenied)
			if !anyApplied(outcomes) {
				httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
				return
			}
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeForbidden})
			outcomes = appendNotAttemptedUIDs(outcomes, uids[i+1:])
			c.Response(outcomes)
			return
		case errors.Is(err, errTargetProtected), errors.Is(err, errSelfRemovalNotAllowed):
			// TARGET-level: the caller is authorized, just not against this member. The wire
			// reason stays "permission_denied" so clients need no new string.
			observeRejected(entryMemberRemove, reasonPermissionDenied)
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeForbidden})
		case errors.Is(err, errMemberNotFound):
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeNotMember})
		case errors.Is(err, errLastOwnerMustTransfer):
			observeRejected(entryMemberRemove, reasonLastOwner)
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeLastOwner})
		case errors.Is(err, errProjectGone):
			// ACTOR-level or project-level: nothing after this point can succeed, so stop —
			// but HOW depends on whether anything already committed. Mirrors the add path: a
			// bare 404 here discarded the removals that had already committed and been
			// audited, telling the caller "nothing happened" while the database disagreed.
			// Jerry-Xin review, PR #841 round 1: the permission branch had the partial report;
			// this branch did not.
			observeRejected(entryMemberRemove, reasonProjectDisbanded)
			if !anyApplied(outcomes) {
				respondProjectNotFound(c)
				return
			}
			outcomes = append(outcomes, memberOutcome{UID: uid, Reason: reasonProjectDisbanded})
			outcomes = appendNotAttemptedUIDs(outcomes, uids[i+1:])
			c.Response(outcomes)
			return
		default:
			observeRejected(entryMemberRemove, outcomeStoreFailed)
			p.Error("移除项目成员失败", zap.Error(err),
				zap.String("projectId", row.ProjectID), zap.String("targetUid", uid))
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
	if !isProjectMember(requestProjectRole(c)) {
		observeRejected(entryLeave, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectNotMember, nil, nil)
		return
	}

	// An empty body is legitimate here — only the last owner needs transfer_to — so a
	// bind failure is tolerated rather than rejected.
	//
	// ShouldBindJSON, not BindJSON: gin's BindJSON calls AbortWithError(400) on failure,
	// which writes the 400 header ITSELF before the handler can respond. On this route an
	// empty body is the normal case, so BindJSON would pin every ordinary leave to a 400
	// header and then serve a 200 body underneath it.
	// An ABSENT body is normal on this route (leave takes no arguments unless the caller is the
	// last owner and must name a successor), so io.EOF is tolerated. Every other bind error is
	// rejected.
	//
	// Swallowing all of them was a real hazard, not untidiness: a truncated or malformed body —
	// a proxy cutting the request short, a client serialising `transfer_to` wrongly — parsed as
	// an empty struct, so `transfer_to` silently became "" and the member left the project
	// anyway. A destructive action must not be the failure mode of a broken payload.
	var req leaveReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		respondProjectRequestInvalid(c, "")
		return
	}

	uid := c.GetLoginUID()
	successor, err := p.leaveProject(row.ProjectID, row.SpaceID, uid, req.TransferTo)
	switch {
	case err == nil:
		p.audit(auditLeave, uid, uid, row.ProjectID, row.SpaceID, "left")
		if successor != "" {
			// The same transaction promoted a successor. Without its own entry the audit trail
			// shows only that the last owner left, and who now controls the project is
			// unrecoverable from the log — which is the one fact an ownership handover has to
			// leave behind.
			p.audit(auditRoleChange, uid, successor, row.ProjectID, row.SpaceID,
				"ownership_transferred_on_leave", zap.Int("new_role", RoleOwner))
		}
		c.ResponseOK()
	case errors.Is(err, errNotSpaceMember):
		// The named successor is no longer an active Space member. Their project seat
		// survives only because the Space-removal cascade has not run yet; promoting them
		// would hand ownership to a seat that is already scheduled for closure, and once
		// the cascade closed it the project would have no owner at all.
		observeRejected(entryLeave, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotSpaceMember, nil, nil)
	case errors.Is(err, errLastOwnerMustTransfer):
		observeRejected(entryLeave, reasonLastOwner)
		httperr.ResponseErrorL(c, errcode.ErrProjectLastOwnerMustTransfer, nil, nil)
	case errors.Is(err, errMemberNotFound):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
	default:
		p.Error("退出项目失败", zap.Error(err),
			zap.String("projectId", row.ProjectID), zap.String("uid", uid))
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
	// Role change is owner-only.
	//
	// The brief forbids an admin demoting an admin or the owner but is silent on
	// promotion. Letting an admin promote would let them mint a peer admin and then
	// act through them, which routes around the rule the brief does state — so the
	// conservative reading is the implemented one. Widening this later is a one-line
	// change plus a rule for how high an admin may promote.
	// Cheap pre-check off the middleware's cached role; changeMemberRole re-reads it under
	// the project lock, which is where the decision actually binds.
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
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "")
		return
	}
	if !IsValidRole(req.Role) {
		httperr.ResponseErrorL(c, errcode.ErrProjectRoleInvalid, nil, nil)
		return
	}

	actorUID := c.GetLoginUID()
	changed, successor, err := p.changeMemberRole(row.ProjectID, row.SpaceID, actorUID, targetUID, req.Role, req.TransferTo)
	switch {
	case err == nil:
		if changed {
			p.audit(auditRoleChange, actorUID, targetUID, row.ProjectID, row.SpaceID, "",
				zap.Int("new_role", req.Role))
		}
		if successor != "" {
			// Demoting the last owner promotes a successor in the same transaction. Auditing
			// only the demotion loses the more important half — who holds the project now.
			p.audit(auditRoleChange, actorUID, successor, row.ProjectID, row.SpaceID,
				"ownership_transferred_on_demote", zap.Int("new_role", RoleOwner))
		}
		c.ResponseOK()
	case errors.Is(err, errNotSpaceMember):
		// Same as in leave: a successor whose Space seat is already gone must not inherit
		// ownership, or the cascade closing their seat leaves the project unmanageable.
		observeRejected(entryRoleChange, reasonNotSpaceMember)
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotSpaceMember, nil, nil)
	case errors.Is(err, errLastOwnerMustTransfer):
		observeRejected(entryRoleChange, reasonLastOwner)
		httperr.ResponseErrorL(c, errcode.ErrProjectLastOwnerMustTransfer, nil, nil)
	case errors.Is(err, errMemberNotFound):
		httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
	case errors.Is(err, errPermissionDenied), errors.Is(err, errTargetProtected):
		// Single-target endpoint, so both levels render the same 403; the distinction only
		// matters where a batch has other targets to report on.
		observeRejected(entryRoleChange, reasonPermissionDenied)
		httperr.ResponseErrorL(c, errcode.ErrProjectPermissionDenied, nil, nil)
	case errors.Is(err, errProjectGone):
		respondProjectNotFound(c)
	default:
		p.Error("修改项目成员角色失败", zap.Error(err),
			zap.String("projectId", row.ProjectID), zap.String("targetUid", targetUID))
		respondStoreFailed(c)
	}
}

// anyApplied reports whether any target in the batch actually changed the database. Only a
// successful outcome counts: a rejected one committed nothing, so a batch of pure rejections can
// still be answered with a single status code.
// anyApplied reports whether any target in the batch CHANGED the database.
//
// The predicate is committed, not OK. A re-add of an existing member returns OK=true with
// nothing written; counting it as applied would turn "nothing committed yet" into a false
// positive and answer a fully no-op batch with a 200 partial report where the single status
// code is still the honest answer. Jerry-Xin review, PR #841 round 1.
func anyApplied(outcomes []memberOutcome) bool {
	for _, o := range outcomes {
		if o.committed {
			return true
		}
	}
	return false
}

// appendNotAttemptedUIDs is appendNotAttempted for the remove path, which iterates uids.
func appendNotAttemptedUIDs(outcomes []memberOutcome, rest []string) []memberOutcome {
	for _, uid := range rest {
		outcomes = append(outcomes, memberOutcome{UID: uid, Reason: outcomeNotAttempted})
	}
	return outcomes
}

// addOneMemberForTest is the per-target seam for the add batch. See removeOneMemberForTest
// for why a package var: a test must be able to make the actor's rights expire PART WAY
// through a batch. Not thread-safe; tests must restore it and must not run in parallel.
var addOneMemberForTest = func(p *Project, projectID, spaceID, actorUID, uid string) (bool, error) {
	return p.addOneMember(projectID, spaceID, actorUID, uid)
}

// removeOneMemberForTest is the per-target seam for the remove batch.
//
// A package var so a test can make the actor lose their rights PART WAY through a batch — the
// only way to reach the partial-commit path, which needs one target to commit and a later one to
// be refused. Not thread-safe; tests must restore it and must not run in parallel.
var removeOneMemberForTest = func(p *Project, projectID, spaceID, actorUID, targetUID string) (bool, error) {
	return p.removeMember(projectID, spaceID, actorUID, targetUID)
}
