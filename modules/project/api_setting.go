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
	if err := c.BindJSON(&req); err != nil {
		respondProjectRequestInvalid(c, "body")
		return
	}
	// An empty body is a no-op rather than a reset: a pointer field distinguishes
	// "not mentioned" from "set to false", so a client sending only the key it
	// changed cannot clear the ones it did not.
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
	}

	// Respond with the project as the caller now sees it, so the client can render
	// from the response instead of refetching the list to learn the new order.
	pinned, err := p.db.queryProjectPinned(row.ProjectID, uid)
	if err != nil {
		p.Error("查询项目置顶状态失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	memberCount, agentCount, err := p.db.countActiveSeatsByKind(row.ProjectID)
	if err != nil {
		p.Error("统计项目成员数失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}
	c.Response(p.toResp(row, requestProjectRole(c), requestSpaceRole(c), memberCount, agentCount, pinned))
}

// applyPin writes the pin, enforcing the per-Space cap.
//
// # Why the count and the write share a transaction
//
// So the check reads the same snapshot the write lands in. It does NOT make the
// pair atomic against a concurrent pin by the same user: two requests that both
// count 5 will both insert, leaving 7. That window is a double-click — the same
// uid, inside the 2 rps shared bucket — and its worst outcome is one extra pinned
// row that the user can remove, so it does not justify serialising every pin behind
// a lock on a table this hot. Written down rather than left for a reader to
// rediscover, because "there is a transaction here" reads like "this is atomic".
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
	if !pinned {
		return p.db.upsertProjectUserSetting(row.ProjectID, uid, false)
	}
	already, err := p.db.queryProjectPinned(row.ProjectID, uid)
	if err != nil {
		return err
	}
	if already {
		return nil
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
