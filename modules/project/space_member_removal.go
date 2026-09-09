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

// allMemberGroupOwnerFinalizerName is the finalizer's name, which also prefixes the
// job's last_error.
const allMemberGroupOwnerFinalizerName = "project_all_member_group_owner"

// registerAllMemberGroupOwnerFinalizer registers "make every all-member group this
// member touched end up owned by an active project owner" as a post-steps finalizer.
//
// # Why this exists at all
//
// A project owner can lose their seat by four routes: kicked, left, role changed, and
// this one — removed from the Space, which cascades into every project in it. The first
// three call syncAllMemberGroupOwner directly (service.go). This one did not, and the
// hole it left is the exact state the leave path's comment says that sync was added to
// prevent: the group side still hands the group over on its way out
// (handOverGroupCreator), and it picks the second-oldest non-bot GROUP member with no
// project-role filter at all. That can land on an ordinary project member — and D7 then
// forbids that person from transferring, leaving, disbanding or blacklisting the group.
// Nobody can move it, and no scan reports it: I4's two scans compare MEMBER SETS, not
// creator-versus-owner. A quiet project keeps that state forever.
//
// # Why a finalizer and not a step, and not a call inside deactivateSeatForCascade
//
// The convergence needs BOTH halves to have happened: the group cascade must have done
// its handover, and the project cascade must have closed the departing owner's seat.
// Steps run in registration order, and registration order is import order, which is
// declared nowhere — so a step (or a call inside deactivateSeatForCascade) computes its
// answer against whichever intermediate state that ordering happens to produce:
//
//   - Group step first: the departing owner's project seat is still ACTIVE, so
//     PickActiveOwner can return the person who is on their way out. Promoting them
//     fails (they are no longer a group member) and the group keeps the non-owner the
//     handover picked.
//   - Project step first: the group still has the departing owner as its creator, so the
//     sync sees a creator who is still a project member and changes nothing; the handover
//     then installs the non-owner afterwards.
//
// Both orders leave the defect. A finalizer runs after every step has SUCCEEDED, so it
// is the only placement whose input is a settled state rather than an ordering artifact.
func (p *Project) registerAllMemberGroupOwnerFinalizer() {
	spacemod.RegisterMemberRemovalCleanupFinalizer(
		allMemberGroupOwnerFinalizerName, p.convergeAllMemberGroupOwners)
}

// convergeAllMemberGroupOwners re-runs the idempotent D6 owner sync for every project in
// this Space that the removed member had a row in.
//
// Contract compliance (modules/space/member_removal.go):
//
//   - Idempotent. The sync is self-deciding on the group side: it reads who the group's
//     creators are and who the project's active owners are, and writes only when they
//     disagree. Running it on a project that is already correct is a read and nothing else.
//   - Decides "nothing to do" itself: no member rows, no all-member group pointer, or no
//     active project owner all return nil rather than an error.
//   - Assumes nothing about which steps ran — only that they all succeeded, which is the
//     finalizer contract.
//
// The set is "every project in this Space with a row for this uid, any status", not
// "every project whose seat we just closed" and not "every project where they were an
// owner"; queryProjectIDsForSpaceMemberPage's comment carries why both of the narrower
// sets are wrong. Paged with the cascade's own budget so one member of a thousand
// projects cannot hold the lease for the whole walk. A spent budget does NOT ask for a
// retry — unlike the cascade, whose retries shrink their own input; see the note at the
// end of this function for why a retry here could not make progress.
func (p *Project) convergeAllMemberGroupOwners(_ *config.Context, removal spacemod.MemberRemoval) error {
	if removal.SpaceID == "" || removal.UID == "" {
		return nil
	}
	after := ""
	synced, failed := 0, 0
	var firstErr error
	for page := 0; page < cascadeMaxPages; page++ {
		ids, err := p.db.queryProjectIDsForSpaceMemberPage(
			removal.SpaceID, removal.UID, after, cascadePageSize)
		if err != nil {
			return fmt.Errorf("project: list projects for owner convergence: %w", err)
		}
		for _, projectID := range ids {
			// One failure does not stop the walk: the projects are independent, and
			// stopping would make the first broken one starve every project after it in
			// project_id order — the same isolation rule the step loop follows. The first
			// error is what the job reports; each failure logs on its own.
			if err := p.syncAllMemberGroupOwnerE(projectID); err != nil {
				failed++
				p.Error("全员群群主收敛失败（Space 级联路径）",
					zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID),
					zap.String("projectId", projectID), zap.Error(err))
				observeAllMemberGroupSyncFailure(reasonSyncOwner)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			synced++
		}
		if len(ids) < cascadePageSize {
			if firstErr != nil {
				return fmt.Errorf("project: converge all-member group owner (%d of %d failed): %w",
					failed, synced+failed, firstErr)
			}
			return nil
		}
		after = ids[len(ids)-1]
	}
	if firstErr != nil {
		return fmt.Errorf("project: converge all-member group owner (%d of %d failed): %w",
			failed, synced+failed, firstErr)
	}
	// 预算在一个满页上用完了。确认真的还有项目没走到——行数正好是页大小整数倍时
	// 也会走到这里，而那种情况其实已经走完了。
	remaining, err := p.db.queryProjectIDsForSpaceMemberPage(removal.SpaceID, removal.UID, after, 1)
	if err != nil {
		return fmt.Errorf("project: confirm remaining projects after convergence budget: %w", err)
	}
	if len(remaining) == 0 {
		return nil
	}

	// 走到这里**不要求重试**，而级联的同一处是要求的。上一版这里返回
	// errCascadeIncomplete，并在注释里说"与级联同一条可重试错误"——那句话是错的，
	// 第十一轮 review 的 P2-3 拆穿了它，而且拆穿的正是让重试有意义的那半：
	//
	// 级联查的是**活跃**席位，它关掉的行会离开自己的结果集，所以每一次重试都从一个更小
	// 的集合开始，最终收敛。这里的查询**故意不带 status 过滤**（带了就返回空集、什么都
	// 收敛不了，见 queryProjectIDsForSpaceMemberPage），于是重试从 project_id > '' 重新
	// 走同样的前 5000 行，永远走不到第 5001 行——直到工单把尝试次数烧光、置为 abandoned。
	// 那是一次注定失败的重试，代价是整条工单（含已经成功的每个步骤）跟着重跑二十遍。
	//
	// 可达性不是假设的：octo_project_member 的行从不删除（重新加入是 UPDATE，见
	// 20260904000001_project_core.sql），而每 Space 的项目配额只按**活跃**项目算，所以
	// 同一个 (space_id, uid) 的历史行数没有上限。
	//
	// 放弃的是什么，说清楚——上一版只说了"这一次不收敛，等它们自己的下一次成员变动"，
	// 那是**处置**，不是**残留状态**。PR #868 第二轮 review 的 P2-2 要求把后者点名，
	// 因为它比"延后"重：
	//
	//   - 留下的是一次 D6 违反。群侧级联挑继任者用的是 querySuccessorForProjectGroupTx
	//     （group/db.go），按资历、以 I2 收窄——条件是"项目的活跃成员"，**不是**项目
	//     owner。这个错配正是本 finalizer 存在的理由（见本文件开头）。所以没被访问到的
	//     项目，可能挂着一个群主不是 owner 的全员群。
	//   - 人工修不了。群主转让是 D7 拦住的六个入口之一，而这个群仍然项目直属，
	//     IsAllMemberGroup 为真，守卫照常生效。
	//   - 没有任何扫描报它。五条不受开关控制的扫描是 ownerless / epoch / I2 / I3 /
	//     removing_stall，没有一条查 D6 的群主正确性。
	//
	// 也就是说，上面那个 counter 不是锦上添花，它是这个状态**唯一**的信号。
	//
	// 为什么仍然只算 P2 而不是拦路：可达性极低（cascadeMaxPages × cascadePageSize =
	// 单个 (space_id, uid) 十万行），而且这些项目在旧代码下同样从来没被收敛过——重试
	// 走不过第一页——所以这不是回退，只是把一个既有残留说清楚。
	//
	// 要真正做到"这一次就走完"，需要把游标持久化到工单行上，而那张表属于 modules/space，
	// 为一个 project 侧收敛动作加列是把分层反过来——真需要时单独立项，和一条 D6 群主
	// 正确性扫描一起。
	observeAllMemberGroupConvergenceIncomplete()
	p.Warn("全员群群主收敛用尽单次页数预算，剩余项目留给它们各自的下一次成员变动",
		zap.String("spaceId", removal.SpaceID), zap.String("uid", removal.UID),
		zap.Int("synced", synced), zap.Int("maxPages", cascadeMaxPages),
		zap.Int("pageSize", cascadePageSize), zap.String("resumeAfterProjectId", after))
	return nil
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

	// D13 — the departing member's OWN agents lose their seats with them, read
	// BEFORE the member's row is touched (queryOwnedAgentSeatsTx filters on
	// status = 1 AND removing = 0, so reading after would come back short).
	//
	// The HUMAN's seat closes directly (status = 0) on this path, because the
	// Space removal that drove this job also runs modules/group's
	// cleanupSpaceMemberGroups and the group side is therefore already covered
	// for them. THE AGENTS GET THE TWO-PHASE CLOSE INSTEAD, and the difference is
	// the whole point.
	//
	// An earlier version of this comment claimed the group side covered the agents
	// too, "because RemoveGroupMembers pulls the leaver's bots out of every group
	// with them (#354)". That is true only of the groups the LEAVER is in:
	// cleanupSpaceMemberGroups enumerates queryGroupsWithMemberUIDAndSpaceID for
	// the departing person, and RemoveGroupMembers then cascades their bots WITHIN
	// those groups. An agent sitting in a project group its owner is not a member
	// of — which D15 makes ordinary, since any member may seat their own agent and
	// any member may create a project group — is never visited.
	//
	// The end state that produced was an I2 violation nothing repairs: the agent
	// loses its project seat and stays an active member of that group, its own
	// space_member row was never touched so no Space cascade revisits it, no
	// project-side job was ever enqueued, and the I2 scan is report-only. Its
	// owner, now outside the Space, keeps a proxy reading a project group.
	//
	// So the agents go through beginMemberRemovalTx + enqueueRemovalJobTx exactly
	// as the kick and leave paths do (beginRemovalWithAgentsTx), and P1's detach
	// step then removes that uid from EVERY group of the project rather than from
	// the subset its owner happened to share. Direct-closing them instead would
	// also have made the job a no-op even if it were enqueued: removalCancelled
	// retires any job whose member reads removing = 0, which a directly-closed
	// seat does.
	agents, err := p.db.queryOwnedAgentSeatsTx(tx, projectID, uid)
	if err != nil {
		return false, err
	}

	changed, err := p.db.deactivateMemberTx(tx, projectID, uid, now)
	if err != nil {
		return false, err
	}
	removingAgents := make([]string, 0, len(agents))
	if changed {
		for _, agentUID := range agents {
			if agentUID == "" || agentUID == uid {
				continue
			}
			agentChanged, err := p.db.beginMemberRemovalTx(tx, projectID, agentUID, now)
			if err != nil {
				return false, err
			}
			if !agentChanged {
				continue
			}
			// Its own job, keyed (project_id, uid), for the reason
			// beginRemovalWithAgentsTx gives: the worker re-reads THAT row under
			// lock before each batch, and re-admission cancels per uid, so a job
			// claiming to cover two uids could not be cancelled for one of them.
			//
			// The operator and reason are the Space removal's, not a distinct
			// agent reason: the agent is not being removed on its own account, and
			// a new reason would have to be taught to the group side's system
			// message suppression before it rendered sensibly.
			if err := p.db.enqueueRemovalJobTx(tx, RemovalJob{
				ProjectID:   projectID,
				UID:         agentUID,
				SpaceID:     spaceID,
				OperatorUID: operatorUID,
				Reason:      reason,
			}, now); err != nil {
				return false, err
			}
			removingAgents = append(removingAgents, agentUID)
		}
		closingUIDs := make([]string, 0, 1+len(removingAgents))
		closingUIDs = append(closingUIDs, uid)
		closingUIDs = append(closingUIDs, removingAgents...)
		rolesCleared, err := p.db.deleteMemberCollaborationRolesTx(tx, projectID, closingUIDs)
		if err != nil {
			return false, err
		}
		if rolesCleared {
			if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
				return false, err
			}
		}
		// Only when a row actually changed. The step is re-run on every job retry, so
		// an unconditional bump would inflate the epoch on no-op reruns and break the
		// "a no-op does not change the epoch" rule clients cache against.
		//
		// ONE bump for the member and every agent that went with them: a member
		// leaving with their agents is one membership change, and the epoch is
		// asserted to move by exactly +1 per write.
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
		// removing = 1 already makes the agent a non-member for every authorization
		// read, so the cached role is stale from this commit, not from the worker's
		// later close.
		//
		// Audited here rather than by the caller, which sees only a bool and audits
		// the human alone: an agent losing its seat is a membership write and the
		// trail has to carry it. Same split, and the same reason, as the kick and
		// leave paths.
		for _, agentUID := range removingAgents {
			p.invalidateProjectMemberCache(projectID, agentUID)
			p.audit(auditCascade, operatorUID, agentUID, projectID, spaceID,
				auditReasonAgentFollowsOwner)
		}
	}
	if changed && wasSoleOwner {
		p.Warn("项目唯一 owner 已被移出 Space，项目暂时无人可管理（P0 已知终局，处置待产品决策）",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.String("uid", uid))
	}
	return changed, nil
}
