package project

import (
	"errors"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// errPinQuotaExceeded is the sentinel applyPin returns when the caller is already
// at the per-Space pin cap. A sentinel rather than a bool return so the quota
// cannot be confused with a storage failure at the call site: one is the caller's
// to fix, the other is ours.
var errPinQuotaExceeded = errors.New("project: pin quota exceeded")

// updateSettingHandler answers PUT /v1/projects/:project_id/setting — the caller's
// own preferences for one project. P2 ships one: pinning.
//
// # Why a settings endpoint and not /pin + /unpin
//
// Following PUT /v1/groups/:group_no/setting, which carries top / mute / save /
// remark through one route writing group_setting (group_no, uid). Two verb routes
// are simpler only while there is exactly one preference; the second one costs two
// more routes and two more handlers, where this costs a key. PUT is also idempotent
// by contract, so "pin an already-pinned project" needs no answer invented for it.
//
// # Why there is no role check
//
// decided that the caller is an active Project member. Pinning is a fact about
// the caller, not a membership mutation, which is why the row lives in its own
// table rather than on octo_project_member.
//
// # Why the feature gate applies
//
// requireWriteEnabled runs even though this is a personal preference. The flag's
// contract is that turning it off stops the feature producing NEW data while
// leaving existing data observable, and a settings row is new data. It costs a
// rolled-back deployment nothing: the same flag drives project_on in
// GET /v1/common/appconfig, so a client that cannot see the Project entry at all
// has nothing to pin.
func (p *Project) updateSettingHandler(c *wkhttp.Context) {
	if !p.requireWriteEnabled(c, entryProjectSetting) {
		return
	}
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	uid := c.GetLoginUID()

	var req settingReq
	// ShouldBindJSON, not BindJSON, and api_i18n.go says why: BindJSON calls gin's
	// AbortWithError(400), so the status is gin's rather than ours and the envelope
	// lands underneath it. Invisible today because ResponseErrorL pins the wire
	// status to 400 anyway, and not invisible the day this module moves to
	// ResponseErrorLWithStatus. Every other handler here binds the same way.
	if err := c.ShouldBindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "body")
		return
	}
	// A body that NAMES NO PREFERENCE is a no-op rather than a reset: a pointer
	// field distinguishes "not mentioned" from "set to false", so a client sending
	// only the key it changed cannot clear the ones it did not.
	//
	// "A body that names no preference", not "an empty body", and the difference is
	// one case wide: `{}` reaches here and no-ops, a ZERO-BYTE body does not — it
	// makes ShouldBindJSON return io.EOF and takes the 400 branch above. Unlike
	// leaveProjectHandler, which exempts io.EOF because an empty body is its normal
	// case, this route has no such case: a settings PUT that mentions nothing is a
	// client bug worth reporting, not a shape to accept silently. Stated because the
	// previous wording claimed the wider behaviour and the test sends `{}`.
	if req.Pinned != nil && *req.Pinned && p.rejectArchivedWrite(c, entryProjectSetting, row) {
		return
	}
	if req.Pinned != nil || req.ArchiveNoticeDismissedVersion != nil {
		updated, err := p.applyProjectSettings(row, uid, req)
		switch {
		case err == nil:
			row = updated
		case errors.Is(err, errProjectArchived):
			p.respondProjectArchivedError(c, entryProjectSetting)
			return
		case errors.Is(err, errArchiveVersionConflict):
			httperr.ResponseErrorL(c, errcode.ErrProjectArchiveVersionConflict, nil, nil)
			return
		case errors.Is(err, errProjectGone):
			respondProjectNotFound(c)
			return
		case errors.Is(err, errActorNotSpaceMember), errors.Is(err, errNotSpaceMember):
			observeRejected(entryProjectSetting, reasonNotSpaceMember)
			httperr.ResponseErrorL(c, errcode.ErrProjectActorNotSpaceMember, nil, nil)
			return
		case errors.Is(err, errMemberNotFound):
			httperr.ResponseErrorL(c, errcode.ErrProjectMemberNotFound, nil, nil)
			return
		case errors.Is(err, errPinQuotaExceeded):
			observeRejected(entryProjectSetting, reasonQuotaPinned)
			respondProjectQuota(c, errcode.ErrProjectQuotaPinned, p.cfg.MaxPinned)
			return
		default:
			p.Error("写入项目个人设置失败", zap.Error(err),
				zap.String("projectId", row.ProjectID))
			respondStoreFailed(c)
			return
		}
	}
	if req.Pinned != nil {
		if *req.Pinned {
			p.provisionSidebarSection(row.ProjectID, row.SpaceID, uid)
		} else {
			p.removeSidebarSection(row.ProjectID, row.SpaceID, uid)
		}
	}

	// Respond with the project as the caller now sees it, so the client can render
	// from the response instead of refetching the list to learn the new order.
	//
	// Both reads are fail-soft, and both go through the same helpers every other
	// route uses. applyPin has already COMMITTED by this point, so a display
	// aggregate that hiccups must not turn a successful pin into a 500 — the same
	// rule the update handler states in full.
	humans, agents := p.splitSeatCounts(row.ProjectID)
	c.Response(p.toResp(row, requestProjectRole(c), requestSpaceRole(c), humans, agents,
		p.pinnedOrFalse(row.ProjectID, uid)))
}

// applyProjectSettings writes the caller's pin and archive-round dismissal in
// one Project-locked transaction, preserving the other setting on each upsert.
func (p *Project) applyProjectSettings(row *Model, uid string, req settingReq) (*Model, error) {
	if req.Pinned == nil && req.ArchiveNoticeDismissedVersion == nil {
		return row, nil
	}
	seatRefs, err := p.db.resolveSpaceSeatIDs(row.SpaceID, []string{uid})
	if err != nil {
		return nil, err
	}
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if err := p.requireSpaceSeatsTx(tx, row.SpaceID, uid, seatRefs); err != nil {
		return nil, err
	}
	locked, err := p.db.lockActiveProjectTx(tx, row.ProjectID)
	if err != nil {
		return nil, err
	}
	if locked == nil || locked.SpaceID != row.SpaceID {
		return nil, errProjectGone
	}
	if req.Pinned != nil && *req.Pinned && locked.ArchivedAt.Valid {
		return nil, errProjectArchived
	}
	if version := req.ArchiveNoticeDismissedVersion; version != nil &&
		(!locked.ArchivedAt.Valid || *version <= 0 || *version != locked.ArchiveVersion) {
		return nil, errArchiveVersionConflict
	}
	member, err := p.db.queryMemberTx(tx, row.ProjectID, uid)
	if err != nil {
		return nil, err
	}
	if member == nil || member.Status != MemberStatusActive || member.Removing != 0 {
		return nil, errMemberNotFound
	}
	if req.Pinned != nil {
		shouldUpsert := !*req.Pinned
		if *req.Pinned {
			already, err := p.db.queryProjectPinnedReadTx(tx, row.ProjectID, uid)
			if err != nil {
				return nil, err
			}
			shouldUpsert = !already
			if shouldUpsert {
				n, err := p.db.countPinnedInSpaceTx(tx, locked.SpaceID, uid)
				if err != nil {
					return nil, err
				}
				if n >= p.cfg.MaxPinned {
					return nil, errPinQuotaExceeded
				}
			}
		}
		if shouldUpsert {
			if err := p.db.upsertProjectUserSettingTx(tx, row.ProjectID, uid, *req.Pinned); err != nil {
				return nil, err
			}
		}
	}
	if version := req.ArchiveNoticeDismissedVersion; version != nil {
		if err := p.db.upsertArchiveNoticeDismissalTx(tx, row.ProjectID, uid, *version, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return locked, nil
}
