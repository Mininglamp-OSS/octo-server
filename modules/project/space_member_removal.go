package project

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// spaceMemberRemovalStepName is the cleanup step's name, which also prefixes the
// job's last_error.
const spaceMemberRemovalStepName = "project_member"

// cascadePageSize bounds ONE QUERY over a removed member's projects; cascadeMaxPages bounds
// how many such pages one invocation will walk.
//
// The paging exists because the step shares its job — and its 10-minute lease — with the
// group and conversation cleanup steps, so it must not issue one unbounded query. But the
// walk has to FINISH, and an earlier version of this file got that wrong in a way worth
// recording: it processed a single page and returned nil. The worker then marked the job
// `done`, and nothing re-drove it — the reconcile job is read-only by design (D7), so it
// only ever REPORTS. A member with more than one page of seats in one Space kept every seat
// past the first page, permanently. The per-Space project quota is 1000, so that was
// reachable, not theoretical.
//
// So: loop until a short page comes back, and if the budget runs out first, return a
// retryable error so the job is re-claimed. Each pass makes progress (closed seats stop
// matching the status filter), so retries converge rather than spin — which is the
// difference between using the retry budget and burning it.
// Package vars rather than consts so a test can shrink them and exercise the multi-page and
// budget-exhausted paths without seeding hundreds of projects. Same injection habit as
// modules/space's generateInviteCodeFn; not thread-safe, so tests that change them must not
// run in parallel and must restore the originals.
var (
	cascadePageSize = 200
	// cascadeMaxPages * cascadePageSize = 5000, comfortably above the 1000-projects-per-Space
	// quota, so the retry path is a backstop for a moved quota rather than the normal case.
	cascadeMaxPages = 25
)

// errCascadeIncomplete asks the worker to re-claim the job because seats remain.
//
// A distinct error rather than a generic one so `last_error` distinguishes "more work" from
// "something broke": the two need different responses from whoever reads it.
var errCascadeIncomplete = errors.New("project: cascade page budget exhausted, seats remain")

// registerSpaceMemberRemovalCleanup registers "deactivate this member's project
// seats" as a Space member-removal cleanup step.
//
// Reverse registration rather than having modules/space call us: modules/project
// imports modules/space, so the reverse import would be a cycle. Same mechanism
// modules/group uses. Registration is by name and latest-wins, which is what lets a
// test substitute a deliberately failing step.
func (p *Project) registerSpaceMemberRemovalCleanup() {
	spacemod.RegisterMemberRemovalCleanupStep(spaceMemberRemovalStepName, p.cleanupSpaceMemberProjects)
}

// cleanupSpaceMemberProjects closes every project seat a removed Space member still
// holds in that Space.
//
// This is the ASYNCHRONOUS half of invariant I1, and it is asynchronous because the
// machinery is — not by choice. The work is enqueued in the Space-removal
// transaction but executed by a poller with a lease, exponential backoff and a
// terminal abandoned state. Between the Space removal committing and this step
// running, octo_project_member rows exist with status=1 and no active Space seat.
// P0 tolerates that window because a project seat grants nothing yet: no group, no
// channel, no message. The tolerance expires in P1, where the same row gates group
// admission.
//
// Contract compliance (modules/space/member_removal.go:56-64):
//
//   - Idempotent. The status filter means a rerun finds no active row, affects zero
//     rows, and therefore does NOT bump member_epoch a second time.
//   - Decides "nothing to do" itself and returns nil rather than erroring.
//   - Assumes nothing about the other registered steps.
//   - Self-limiting on failure. It shares the job with the group and conversation
//     steps, and a step that keeps erroring keeps the whole job being re-claimed,
//     burning lease cycles and crowding healthy removals out of each batch. So the
//     only thing that returns an error here is a real database failure; everything
//     else resolves to nil.
//
// The membership predicate is CheckMembershipForCleanup, NOT CheckMembership, and
// the difference is load-bearing: they differ on exactly one axis — in a BANNED
// Space (status=2) the member still holds their seat, so cleanup must skip. A step
// written against CheckMembership would deactivate every project membership in a
// Space the moment it was banned, and un-banning would not restore them.
// CleanupSpaceMemberProjects is the registered step body, exported so the external test
// package can restore the real step after injecting a failing one (the registry is
// latest-wins, and a no-op left behind would silently disable the cascade for the rest of
// the test binary's run - yujiawei Q9, PR #841 round 1).
func (p *Project) CleanupSpaceMemberProjects(ctx *config.Context, removal spacemod.MemberRemoval) error {
	return p.cleanupSpaceMemberProjects(ctx, removal)
}

func (p *Project) cleanupSpaceMemberProjects(ctx *config.Context, removal spacemod.MemberRemoval) error {
	stillMember, err := spacepkg.CheckMembershipForCleanup(ctx.DB(), removal.SpaceID, removal.UID)
	if err != nil {
		return fmt.Errorf("project: re-check space membership before cascade: %w", err)
	}
	if stillMember {
		// Either the Space is banned (the seat is real and must survive) or the member
		// rejoined between the removal committing and this step running. Both mean
		// "do not tear their seats down".
		p.Info("被移除成员仍持有 Space 席位，跳过项目级联",
			zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID))
		return nil
	}

	var (
		firstErr error
		closed   int
		// budgetSpent starts FALSE and is only ever set true by finishing the loop with a
		// full page still coming back. An earlier version of this fix defaulted it to true and
		// relied on each break to clear it — the `len == 0` break did not, so a removed member
		// with NO project seats got errCascadeIncomplete. That is the majority of removals, and
		// it would have burned the shared job's whole retry budget on every one of them,
		// pushing jobs to `abandoned` and degrading the group cascade for everybody. Caught by
		// TestCascadeIsNoOpWithNothingToDo, which exists for exactly that contract clause.
		budgetSpent bool
	)
	for page := 0; page < cascadeMaxPages; page++ {
		// No cursor: each pass re-queries for ACTIVE seats, and the ones just closed no
		// longer match. A cursor would be wrong here — it would skip past the seats a failing
		// project left behind.
		projectIDs, err := p.db.queryActiveProjectIDsForSpaceMember(
			removal.SpaceID, removal.UID, cascadePageSize)
		if err != nil {
			return fmt.Errorf("project: query active project seats of removed member: %w", err)
		}
		if len(projectIDs) == 0 {
			budgetSpent = false
			break
		}

		progressed := false
		for _, projectID := range projectIDs {
			changed, err := p.deactivateSeatForCascade(
				projectID, removal.SpaceID, removal.UID, removal.OperatorUID, removal.Reason)
			if err != nil {
				p.Error("被移出 Space 的成员退出项目失败",
					zap.Error(err), zap.String("projectId", projectID),
					zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if changed {
				closed++
				progressed = true
				p.audit(auditCascade, removal.OperatorUID, removal.UID, projectID,
					removal.SpaceID, removal.Reason)
			}
		}
		// Nothing changed this pass, so the next query would return the same rows: looping
		// again would spin against the same failures until the budget ran out. Stop and let
		// the job's own backoff space the retries out (firstErr, if any, asks for one).
		if !progressed {
			budgetSpent = false
			break
		}
		if len(projectIDs) < cascadePageSize {
			budgetSpent = false
			break
		}
		budgetSpent = true
	}

	if closed > 0 {
		p.Info("Space 成员移除级联关闭项目席位",
			zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID),
			zap.Int("closed", closed))
	}
	if firstErr != nil {
		return firstErr
	}
	if !budgetSpent {
		return nil
	}
	// The budget ran out on a full page. Confirm a seat really remains before asking for a
	// retry: a seat count that is an exact multiple of the page size lands here with nothing
	// left, and returning an error then would be a retry for no reason. One cheap LIMIT 1.
	remaining, err := p.db.queryActiveProjectIDsForSpaceMember(removal.SpaceID, removal.UID, 1)
	if err != nil {
		return fmt.Errorf("project: confirm remaining seats after cascade budget: %w", err)
	}
	if len(remaining) == 0 {
		return nil
	}
	// Returning an error is the ONLY way to keep this work alive: nil marks the job done and
	// nothing re-drives it — the reconcile scan is read-only by design and only reports.
	p.Warn("项目级联达到单次页数上限，返回可重试错误以便工单重新认领",
		zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID),
		zap.Int("closed", closed), zap.Int("maxPages", cascadeMaxPages))
	return errCascadeIncomplete
}

// deactivateSeatForCascade closes one seat in its own short transaction.
//
// One transaction per project, not one for the walk: holding the lock on every
// project a member belongs to, for the duration of the walk, would block concurrent
// membership writes across all of them. Short transactions also mean a lease
// expiring mid-walk costs at most a repeated no-op rather than a rollback.
func (p *Project) deactivateSeatForCascade(projectID, spaceID, uid, operatorUID, reason string) (bool, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin cascade seat close: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// Re-check the Space seat HERE, in this transaction, holding a shared lock on the
	// space_member row — not just in the outer gate.
	//
	// The outer gate (cleanupSpaceMemberProjects) checks once, outside any transaction, and the
	// job may sit in backoff for minutes after that. If the user rejoins the Space in the
	// window, closing their project seat destroys a membership that is legitimate again, and
	// nothing puts it back. The shared lock means a concurrent rejoin (which takes X on that
	// row) cannot commit inside this transaction's window.
	//
	// Cleanup semantics, not authorization semantics: a banned Space still counts as holding
	// the seat, so a ban does not tear members out of their projects. Same predicate as the
	// outer gate, which is the point — two layers answering different questions is how the
	// outer one short-circuits the inner one.
	//
	// Lock order: space -> project, taken in that order below.
	stillSeated, err := p.db.checkSpaceSeatForCleanupTx(tx, spaceID, uid)
	if err != nil {
		return false, err
	}
	if stillSeated {
		p.Info("级联动手前复核：成员已重新持有 Space 席位，跳过关闭项目席位",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.String("uid", uid))
		return false, nil
	}

	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if row == nil {
		// The project is disbanded (or gone). disbandProject closes every seat in the same
		// transaction, so normally there is nothing here — but close the row anyway rather
		// than returning "nothing done". An active seat on a disbanded project is an I1
		// violation the reconcile scan would report forever, and skipping it would also make
		// the caller's loop see "no progress" and stop with the seat still active.
		//
		// No epoch bump: the project is disbanded, so no consumer is watching its epoch, and
		// disband already moved it.
		changed, err := p.db.deactivateMemberTx(tx, projectID, uid, now)
		if err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("project: commit stale seat close: %w", err)
		}
		if changed {
			p.invalidateProjectMemberCache(projectID, uid)
		}
		return changed, nil
	}

	// KNOWN END STATE, deliberately not resolved here: if the departing member is the
	// project's only owner, this closes their seat and leaves the project active with no
	// owner. Role change and disband are owner-only in P0 and a Space admin has read access
	// only, so such a project cannot be renamed, disbanded or re-owned.
	//
	// Not handled in P0 because the fix is a PRODUCT decision, not a technical gap: auto-
	// promoting a member changes who controls a project without anyone asking, and auto-
	// disbanding destroys data. The brief scopes this step to exactly "deactivate every active
	// row for (space_id, uid) and bump member_epoch when rows were affected"; both of those
	// resolutions are outside it. Recorded as an Open question in the task brief.
	//
	// It is also the SAME end state this repo already accepts one layer down: group's
	// handOverGroupCreator leaves an ownerless group when nobody can inherit, and documents
	// that as consistent with the existing groupExit outcome. So this is not a new class of
	// bad state, and it is not a security one either — no access is widened, nothing leaks;
	// the members simply keep a project none of them can administer until the P2 admin
	// surface can adopt it.
	//
	// The Warn below is the whole P0 treatment: make it visible, decide it with product.
	//
	// Detect-only, read in this transaction so the log line cannot describe a state that had
	// already changed by the time it was written.
	wasSoleOwner := false
	member, err := p.db.queryMemberTx(tx, projectID, uid)
	if err != nil {
		return false, err
	}
	if member != nil && member.Status == MemberStatusActive && member.Role == RoleOwner {
		owners, err := p.db.countActiveOwnersTx(tx, projectID)
		if err != nil {
			return false, err
		}
		wasSoleOwner = owners <= 1
	}

	changed, err := p.db.deactivateMemberTx(tx, projectID, uid, now)
	if err != nil {
		return false, err
	}
	if changed {
		// Only when a row actually changed. The step is re-run on every job retry, so
		// an unconditional bump would inflate the epoch on no-op reruns and break the
		// "a no-op does not change the epoch" rule clients cache against.
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit cascade seat close: %w", err)
	}
	if changed {
		// Invalidate even though this is a background path. The Space gate already
		// closed synchronously when the Space removal committed, so this is not the
		// isolation boundary — but leaving a stale positive role cached would make the
		// project's own membership answer disagree with the database for a full TTL.
		p.invalidateProjectMemberCache(projectID, uid)
	}
	if changed && wasSoleOwner {
		p.Warn("项目唯一 owner 已被移出 Space，项目暂时无人可管理（P0 已知终局，处置待产品决策）",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.String("uid", uid))
	}
	return changed, nil
}
