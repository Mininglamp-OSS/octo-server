package project

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
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
// Anyone who can SEE the project can pin it, and projectMiddleware has already
// decided who that is. A Space admin who never joined a space_listed project gets
// it in their list, so they can pin it — pinning is a fact about the caller, not a
// membership fact, which is also why the row lives in its own table rather than on
// octo_project_member.
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
	if req.Pinned != nil {
		if err := p.applyPin(row, uid, *req.Pinned); err != nil {
			if errors.Is(err, errPinQuotaExceeded) {
				observeRejected(entryProjectSetting, reasonQuotaPinned)
				respondProjectQuota(c, errcode.ErrProjectQuotaPinned, p.cfg.MaxPinned)
				return
			}
			p.Error("写入项目个人设置失败", zap.Error(err),
				zap.String("projectId", row.ProjectID))
			respondStoreFailed(c)
			return
		}
		if *req.Pinned {
			// applyPin commits before this best-effort hook runs. A pin is an
			// explicit request to surface the Project in the caller's personal
			// Follow sidebar, including for a Space-listed Project the caller can
			// see without holding a Project seat. The list path repairs a transient
			// failure, so a sidebar write must not turn a committed pin into a 5xx.
			p.provisionSidebarSection(row.ProjectID, row.SpaceID, uid)
		} else {
			// An explicit unpin is also an explicit removal from Follow. The
			// pinned=0 setting prevents the membership repair path from bringing it
			// back; this hook hides the retained ordering row without coupling this
			// module to category's table.
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

// applyPin writes the pin, enforcing the per-Space cap.
//
// # Why the count and the write share a transaction
//
// So the check reads the same snapshot the write lands in. It does NOT make the
// pair atomic against concurrent pins by the same user: every request that reads
// n < cap inserts, so the overshoot is the concurrency degree, not one.
//
// An earlier version of this comment called that "a double-click ... one extra
// pinned row", and PR #861's review corrected it with the number: SharedUIDRateLimiter
// is 2 rps with a BURST OF 60 (pkg/wkhttp/ratelimit_helper.go), so ~60 in-flight
// pins of distinct projects can each read 5 and each insert, landing ~65 rows
// against a cap of 6. The trade is still the one taken, on grounds that survive the
// correction — the overshoot is confined to the caller's own pin list, every row is
// self-removable because unpin is never refused, and the PUT response re-reads the
// state so the wire never reports a false success — but it is taken with the real
// number rather than a comfortable one. A cap that ever becomes billing- or
// audit-bearing needs SELECT ... FOR UPDATE on the caller's rows instead.
//
// # Why unpinning is never refused
//
// It lowers the count. Gating it would strand a user who somehow got over the cap
// (see the window above, or a cap lowered by configuration) at exactly the
// operation that would fix it.
//
// # Why re-pinning something already pinned is not refused either
//
// The row already exists and counts toward the total, so the upsert rewrites it
// rather than adding one. Counting first and comparing >= cap would refuse an
// operation that changes nothing — the shape of bug where turning a toggle on
// twice fails the second time.
func (p *Project) applyPin(row *Model, uid string, pinned bool) error {
	// One read serves both directions, and it is what keeps either direction from
	// writing a row that changes nothing: unpinning something never pinned used to
	// INSERT a pinned = 0 tombstone, and nothing anywhere deletes those.
	already, err := p.db.queryProjectPinned(row.ProjectID, uid)
	if err != nil {
		return err
	}
	if already == pinned {
		return nil
	}
	if !pinned {
		return p.db.upsertProjectUserSetting(row.ProjectID, uid, false)
	}

	tx, err := p.db.session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()

	n, err := p.db.countPinnedInSpaceTx(tx, row.SpaceID, uid)
	if err != nil {
		return err
	}
	if n >= p.cfg.MaxPinned {
		return errPinQuotaExceeded
	}
	if err := p.db.upsertProjectUserSettingTx(tx, row.ProjectID, uid, true); err != nil {
		return err
	}
	return tx.Commit()
}
