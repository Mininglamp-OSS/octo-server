package project

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Lock order for every write path in this module, followed without exception:
//
//	space_member -> space -> project -> octo_project_member
//
// Membership writes lock the octo_project row (lockActiveProjectTx) before
// touching octo_project_member, and read the Space-side facts they need before
// that lock is taken. Native group membership is an independent write domain;
// this module never holds group_member locks while taking an exclusive
// octo_project_member lock.
//
// space_member leads space, and that is NOT this module's choice — it is
// modules/space's, recorded at modules/space/db.go:71-88 after an Error 1213 incident:
// both Space-disband paths take `space_member ... FOR UPDATE` and then update `space`,
// so any transaction taking those two in the opposite order closes a cycle with them.
// createProject is the only path here that takes both, and it took them backwards until
// PR #841 round 2 (yujiawei P1-3 / Jerry-Xin B-3); the deadlock was reproduced against
// MySQL 8.0.33 with the OPERATOR'S DISBAND as InnoDB's victim.
//
// Scope of this claim, stated because two earlier versions of this comment overstated it:
//
//   - it has been checked against modules/space's disband and member-removal transactions,
//     not just this module's own. "No cycle within this module" is not the property that
//     matters.
//   - it is a TABLE order, and row order is made explicit separately: every write prepares
//     the requested space_member primary keys before BEGIN, then the locking read probes those
//     exact ids in ascending clustered-key order. Do not replace that with a uid IN query;
//     sorting after a UID-index lookup does not control the order in which InnoDB acquires locks.
//     retryOnLockConflict remains the backstop for transient conflicts not covered by this
//     shared order.
//
// Every membership and role write follows the same three steps inside one
// transaction:
//
//	1. lock the project row
//	2. write the membership row
//	3. IF step 2 affected a row, member_epoch = member_epoch + 1
//
// Step 3's condition is not an optimization. The Space-removal cascade step is
// re-executed on every job retry, so an unconditional bump would inflate the epoch
// on no-op reruns and break the "a no-op write does not change the epoch" rule that
// clients cache against.

// txRetryAttempts bounds the retry budget for transient lock conflicts. Three: enough for the
// pathological interleaving to have passed, few enough that a genuine hot spot surfaces as an
// error rather than as latency.
const txRetryAttempts = 3

// retryOnLockConflict re-runs fn while it fails with a TRANSIENT lock conflict.
//
// Why this exists even though the seat locks are now taken in one statement: three consecutive
// review rounds each found a lock-order cycle that careful reasoning had missed — table order
// vs. modules/space, then row order within space_member. "We reasoned about the order" is
// demonstrably not sufficient on its own, and the cost of being wrong is not a retry, it is
// store_failed (Internal, HTTP 500) — and when InnoDB picks the Space disband as its victim, a
// failed step of the member-removal security cascade.
//
// Only 1213 (deadlock) and 1205 (lock wait timeout) are retried, matching
// modules/common's isRetryableTxErr. Everything else — 1062, every service sentinel — is
// returned verbatim on the first attempt, so callers' errors.Is checks are untouched. fn must
// own its whole transaction: a retry re-runs it from BEGIN, which is only sound because a
// deadlock has already rolled the failed attempt back.
func retryOnLockConflict(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < txRetryAttempts; attempt++ {
		err := fn()
		if err == nil || !isRetryableTxErr(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("project: transaction retries exhausted: %w", lastErr)
}

// isRetryableTxErr reports whether err is a transient InnoDB lock conflict. Spelled out here
// rather than imported from modules/common, whose copy is unexported; the predicate is
// deliberately identical.
func isRetryableTxErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	return false
}

// Sentinel errors the API layer maps onto registered error codes. Returning typed
// errors rather than responding from the service keeps the transaction boundary and
// the wire contract in separate files.
var (
	errQuotaPerSpace       = errors.New("project: per-space project quota reached")
	errQuotaPerCreator     = errors.New("project: per-creator project quota reached")
	errQuotaDailyCreate    = errors.New("project: daily project creation quota reached")
	errQuotaMembers        = errors.New("project: per-project member quota reached")
	errProjectGone         = errors.New("project: project is absent or disbanded")
	errNotSpaceMember      = errors.New("project: target uid is not an active space member")
	errActorNotSpaceMember = fmt.Errorf(
		"project: actor uid is not an active space member: %w", errNotSpaceMember)
	errMemberNotFound        = errors.New("project: target uid is not an active project member")
	errMemberRoleConflict    = errors.New("project: target already has a different active role")
	errMemberRoleInvalid     = errors.New("project: member role must be common or admin")
	errLastOwnerMustTransfer = errors.New("project: the last owner must transfer ownership first")
	errPermissionDenied      = errors.New("project: operation not permitted for this role")
	errNoFieldsToUpdate      = errors.New("project: update names no field")
	errTargetProtected       = errors.New("project: not permitted to act on this member's role")
	errSelfRemovalNotAllowed = errors.New("project: use leave to remove yourself")
)

// ---------- permission matrix ----------

func canUpdateProject(projectRole int) bool    { return projectRole >= RoleAdmin }
func canDisbandProject(projectRole int) bool   { return projectRole == RoleOwner }
func canManageMembers(projectRole int) bool    { return projectRole >= RoleAdmin }
func canChangeMemberRole(projectRole int) bool { return projectRole >= RoleAdmin }
func isProjectMember(projectRole int) bool     { return projectRole >= RoleCommon }

// canAssignMemberRole allows owners and admins to manage common/admin seats.
// Owner is deliberately excluded; ownership changes use the dedicated transfer
// transaction, which demotes the previous owner in the same commit.
func canAssignMemberRole(actorRole, role int) bool {
	if !canChangeMemberRole(actorRole) {
		return false
	}
	return role == RoleCommon || role == RoleAdmin
}

func canViewMembers(projectRole, _ int) bool { return isProjectMember(projectRole) }

func capabilitiesFor(projectRole, spaceRole int) Capabilities {
	_ = spaceRole
	return Capabilities{
		CanUpdate:       canUpdateProject(projectRole),
		CanDisband:      canDisbandProject(projectRole),
		CanManageMember: canManageMembers(projectRole),
		CanChangeRole:   canChangeMemberRole(projectRole),
		CanLeave:        isProjectMember(projectRole) && projectRole != RoleOwner,
		CanViewMembers:  isProjectMember(projectRole),
	}
}

// canActOnTargetRole protects the unique Owner role. Admins may manage peer
// admins, but no normal member mutation may create, remove, or demote Owner.
func canActOnTargetRole(actorRole, targetRole int) bool {
	if actorRole != RoleOwner && actorRole != RoleAdmin {
		return false
	}
	return targetRole != RoleOwner
}

// requireSpaceSeatsTx takes every space_member seat lock a write path needs in
// one resolved statement and maps each absence onto the sentinel its subject
// deserves. refs was prepared from the exact primary keys before BEGIN; the
// locking DAO revalidates those identities under the transaction locks.
//
// actorUID is always first and always required. others are target seats whose
// absence is TARGET-level: the caller is fine, the subject of the operation is
// not. Empty strings in others are ignored.
func (p *Project) requireSpaceSeatsTx(
	tx *dbr.Tx, spaceID string, actorUID string, refs spaceSeatRefs, others ...string,
) error {
	_, err := p.lockSeatsTx(tx, spaceID, actorUID, others, nil, refs)
	return err
}

// lockSeatsTx is requireSpaceSeatsTx plus CANDIDATE seats: locked in the same
// resolved statement, but not refused unless the caller establishes that the
// seat is actually needed.
func (p *Project) lockSeatsTx(
	tx *dbr.Tx, spaceID, actorUID string, required, candidates []string, refs spaceSeatRefs,
) (map[string]bool, error) {
	uids := make([]string, 0, len(required)+len(candidates)+1)
	seen := map[string]bool{}
	add := func(uid string) {
		if uid == "" || seen[uid] {
			return
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	add(actorUID)
	for _, uid := range required {
		add(uid)
	}
	for _, uid := range candidates {
		add(uid)
	}
	held, err := p.db.lockSpaceSeatsTx(tx, spaceID, uids, refs)
	if err != nil {
		return nil, err
	}
	// The ACTOR is checked first, so a caller who has lost their own seat is told that rather
	// than being told something about the target. Their project role may well still be active,
	// because the Space-removal cascade is asynchronous by design.
	if !held[actorUID] {
		return nil, errActorNotSpaceMember
	}
	for _, uid := range required {
		if uid != "" && uid != actorUID && !held[uid] {
			return nil, errNotSpaceMember
		}
	}
	return held, nil
}

// ---------- create ----------

type createInput struct {
	SpaceID         string
	Creator         string
	Name            string
	Description     string
	Logo            string
	Discoverability int
	MaxMembers      int
	// AgentUIDs are the creator's own AI agents to seat alongside them (D2).
	// Eligibility is decided inside the create transaction; one ineligible uid
	// rejects the whole create (D3).
	AgentUIDs []string
}

// createProject runs createProjectOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) createProject(in createInput) (*Model, error) {
	var model *Model
	err := retryOnLockConflict(func() error {
		var e error
		model, e = p.createProjectOnce(in)
		return e
	})
	if err != nil || model == nil {
		return model, err
	}
	return model, nil
}

func createSeatUIDs(in createInput) []string {
	agentUIDs := withoutUID(sanitizeUIDs(in.AgentUIDs), in.Creator)
	uids := make([]string, 0, len(agentUIDs)+1)
	uids = append(uids, in.Creator)
	uids = append(uids, agentUIDs...)
	return uids
}

// createProject inserts a project and its owner seat in ONE transaction.
//
// All three creation quotas are counted inside that transaction. Counting them
// outside would let two concurrent creates both pass the check and both land, which
// is the whole failure mode a quota exists to prevent.
//
// member_epoch stays at its default 0. The acceptance list for "the epoch strictly
// increases" covers add / remove / leave / role change / Space cascade / disband —
// creation is where the roster comes into existence rather than changing, so 0 is
// its initial value and the first real membership change makes it 1.
func (p *Project) createProjectOnce(in createInput) (*Model, error) {
	seatRefs, err := p.db.resolveSpaceSeatIDs(in.SpaceID, createSeatUIDs(in))
	if err != nil {
		return nil, err
	}
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin create: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	model, err := p.createProjectTxWithSeatRefs(tx, in, seatRefs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit create: %w", err)
	}
	p.finishProjectCreate(model, in)
	return model, nil
}

// finishProjectCreate performs post-commit effects shared by every ordinary
// Project creation path. It is deliberately idempotent: retries of a
// provisioning hook must not change the Project transaction's result.
func (p *Project) finishProjectCreate(model *Model, in createInput) {
	if model == nil {
		return
	}
	agentUIDs := withoutUID(sanitizeUIDs(in.AgentUIDs), model.Creator)
	p.invalidateProjectMemberCache(model.ProjectID, model.Creator)
	p.provisionSidebarSection(model.ProjectID, model.SpaceID, model.Creator)
	for _, uid := range agentUIDs {
		p.invalidateProjectMemberCache(model.ProjectID, uid)
		p.provisionSidebarSection(model.ProjectID, model.SpaceID, uid)
	}
	p.provisionAllMemberGroup(model.ProjectID, model.SpaceID, model.Creator, model.Name, agentUIDs)
	if groupNo, err := p.db.queryAllMemberGroupNo(model.ProjectID); err != nil {
		p.Error("查询刚创建项目的全员群失败", zap.Error(err),
			zap.String("projectId", model.ProjectID))
	} else {
		// Provisioning is a post-commit best-effort effect, but when it succeeds
		// synchronously the create response should expose the pointer it just wrote.
		model.AllMemberGroupNo = groupNo
	}
	if p.nudgeProvisioningFn != nil {
		p.nudgeProvisioningFn()
	}
}

// createProjectTxWithSeatRefs writes a Project, its Owner seat, initial agents
// and provisioning rows into the caller's transaction. It does not commit or
// run post-commit hooks.
func (p *Project) createProjectTxWithSeatRefs(
	tx *dbr.Tx, in createInput, seatRefs spaceSeatRefs,
) (*Model, error) {

	now := time.Now().UTC()
	dayFrom, dayTo := p.cfg.dayWindow(now)

	// I1 requires every Space seat used by the create to be revalidated inside
	// this transaction. Prepare the exact primary keys before BEGIN, then lock
	// creator and agents together in ascending clustered-key order, before the
	// exclusive `space` lock below. This matches Space disband's row order and
	// prevents a creator-high/agent-low two-statement cycle.
	//
	// The JOIN-free helper is intentional: joining `space` here would open a
	// consistent-read view before the quota counts. The authoritative active
	// Space check remains the exclusive lock immediately below.
	agentUIDs := withoutUID(sanitizeUIDs(in.AgentUIDs), in.Creator)
	agentSeats, err := p.db.lockSpaceSeatRowsTx(tx, in.SpaceID, createSeatUIDs(in), seatRefs)
	if err != nil {
		return nil, err
	}
	if !agentSeats[in.Creator] {
		return nil, errNotSpaceMember
	}

	// NOW lock the Space row. Two things depend on it, and neither is optional:
	//
	//   1. It serialises every create in this Space, which is what makes the counts below
	//      mean anything. A plain SELECT COUNT(*) is a non-locking consistent read even
	//      inside a transaction, so without this lock two concurrent creates both read 999
	//      and both insert. An earlier version of this function put the counts in a
	//      transaction and claimed that was enough; it was not. Moving the membership check
	//      ahead of this lock does not weaken that serialisation: both creates still queue on
	//      the same `space` row, they just queue one statement later.
	//   2. It confirms the Space is still active at write time. The membership check above
	//      already joins on space.status = 1, but it read that under a shared lock on
	//      space_member only, so a ban or disband could still commit in between — this is the
	//      authoritative recheck, and it is why it stays.
	spaceActive, err := p.db.lockSpaceRowTx(tx, in.SpaceID)
	if err != nil {
		return nil, err
	}
	if !spaceActive {
		// Deliberately the same answer as "you hold no seat here", which renders as
		// actor_not_space_member — literally inaccurate when the SPACE was disbanded, and
		// intentional: distinguishing the two tells a caller whether a Space id they cannot
		// access exists. Same anti-enumeration reasoning as projectMiddleware folding three
		// refusals into one 404 (PR #841 round 3, P2 — noted because the message reads like a
		// bug otherwise).
		return nil, errNotSpaceMember
	}

	count, err := p.db.countActiveInSpaceTx(tx, in.SpaceID)
	if err != nil {
		return nil, err
	}
	if count >= p.cfg.MaxPerSpace {
		return nil, errQuotaPerSpace
	}
	count, err = p.db.countActiveByCreatorTx(tx, in.SpaceID, in.Creator)
	if err != nil {
		return nil, err
	}
	if count >= p.cfg.MaxPerCreator {
		return nil, errQuotaPerCreator
	}
	// The per-day cap is the one quota the Space lock does not fully serialise, and that is
	// worth stating rather than leaving for someone to discover: it is keyed on the creator
	// ACROSS Spaces, while the lock is per Space. A user creating in two Spaces at the same
	// instant can therefore exceed it by the number of Spaces they raced in.
	//
	// Accepted rather than closed, because closing it means locking a creator-wide row —
	// `user` — which is not in this module's declared lock order and would put project
	// creation in contention with profile writes. The consequence is bounded: the two hard
	// caps above ARE serialised, so total project count stays within
	// MaxPerSpace per Space and MaxPerCreator per (Space, creator) regardless. The per-day cap
	// is a rate limit on top of those, not a correctness bound.
	count, err = p.db.countCreatedInWindowTx(tx, in.Creator, dayFrom, dayTo)
	if err != nil {
		return nil, err
	}
	if count >= p.cfg.MaxDailyCreate {
		return nil, errQuotaDailyCreate
	}

	model := &Model{
		ProjectID:       util.GenerUUID(),
		SpaceID:         in.SpaceID,
		Name:            in.Name,
		Description:     in.Description,
		Logo:            in.Logo,
		Creator:         in.Creator,
		Discoverability: in.Discoverability,
		MaxMembers:      in.MaxMembers,
		Status:          StatusNormal,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := p.db.insertProjectTx(tx, model); err != nil {
		// Project names are intentionally not an identity or uniqueness key.
		return nil, err
	}
	// Collaboration roles are descriptive metadata, not authorization. Seed the
	// built-in catalog in the same transaction as the project so a successful
	// create never exposes a partially initialized catalog. The built-in key's
	// unique constraint makes retries and the bounded backfill idempotent.
	if _, err := p.db.seedBuiltinCollaborationRolesTx(tx, model.ProjectID, now); err != nil {
		return nil, err
	}
	if _, err := p.db.admitMemberTx(tx, &MemberModel{
		ProjectID: model.ProjectID,
		UID:       in.Creator,
		SpaceID:   in.SpaceID,
		Role:      RoleOwner,
		InviteUID: in.Creator,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return nil, err
	}

	// D2/D3 — the creator's own agents, seated in the SAME transaction.
	//
	// Same transaction, not a follow-up call: the dialog says "confirm and they join",
	// and a project that exists without the agents the user picked is a state the user
	// never asked for. It also keeps the failure story simple — one ineligible uid means
	// nothing was written, so "fix it and retry" is the whole recovery.
	//
	// The eligibility verdict is computed here rather than in the handler because it
	// depends on the Space seats locked above: a check before the transaction is a read
	// that can expire. The agent seats were revalidated in the same ordered locking read,
	// so classification can safely use that held-seat map.
	if len(agentUIDs) > 0 {
		verdicts, err := p.classifyAgentsTx(tx, in.Creator, agentUIDs, agentSeats)
		if err != nil {
			return nil, err
		}
		if bad := ineligibleAgentUIDs(agentUIDs, verdicts); len(bad) > 0 {
			// The INELIGIBLE SUBSET goes to the caller (they submitted those uids, so
			// echoing them leaks nothing); the REASONS stay in the log. See
			// errAgentNotEligible and agentNotEligibleError.
			p.Warn("建项目：分身不合格，整单拒绝",
				zap.String("spaceId", in.SpaceID), zap.String("creator", in.Creator),
				zap.Strings("ineligible", bad),
				zap.Any("reasons", ineligibleAgentReasons(agentUIDs, verdicts)))
			return nil, &agentNotEligibleError{UIDs: bad}
		}

		// The agent seats count against max_members exactly like human seats: an agent
		// reads the project's messages, so it costs a seat. Counted here, inside the
		// transaction that holds the project row lock, for the same reason the create
		// quotas are — a check outside it lets two concurrent writes both pass.
		//
		// count+1 for the owner seat inserted just above: countActiveMembersTx sees it
		// (same transaction), so this reads the post-owner count and only has to add the
		// agents.
		seated, err := p.db.countActiveMembersTx(tx, model.ProjectID)
		if err != nil {
			return nil, err
		}
		if seated+len(agentUIDs) > p.cfg.effectiveMaxMembers(model.MaxMembers) {
			return nil, errQuotaMembers
		}
		for _, uid := range agentUIDs {
			if _, err := p.db.admitMemberTx(tx, &MemberModel{
				ProjectID: model.ProjectID,
				UID:       uid,
				SpaceID:   in.SpaceID,
				Role:      RoleCommon,
				InviteUID: in.Creator,
				CreatedAt: now,
				UpdatedAt: now,
			}); err != nil {
				return nil, err
			}
		}
	}

	// Subsystem provisioning is enqueued in THIS transaction (D2). That is the only
	// construction under which "the project exists ⟹ its provisioning jobs exist" is
	// true; a Redis queue or a post-commit call can drop the job or orphan it.
	//
	// Fail-closed, and this is a real behavioural change to create: a failure to write
	// the outbox rows aborts the create. It is bounded — the failure modes are a DB
	// error (which would have failed the create anyway) and a crypto/rand failure
	// (see newContainerID, where a fallback would silently reintroduce a derivable
	// container id). It is a no-op with no target enabled, which is the default, so
	// create's availability is unchanged until an operator turns a target on.
	//
	// LAST statement before the commit, which is where the lock order puts it — see
	// enqueueProvisioningTx.
	if err := p.db.enqueueProvisioningTx(tx, model.ProjectID, in.SpaceID, p.cfg.Provisioning.Targets, now); err != nil {
		return nil, err
	}
	return model, nil

}

// ---------- update / disband ----------

// updateProject runs updateProjectOnce through the bounded lock-conflict retry
// (see retryOnLockConflict), then syncs the all-member group's name (D8).
//
// The sync is after the retry loop for the same reason the provisioner is: a
// lock-conflict retry re-runs the transaction, and a rename inside the closure
// would fire once per attempt.
func (p *Project) updateProject(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
	var model *Model
	err := retryOnLockConflict(func() error {
		var e error
		model, e = p.updateProjectOnce(projectID, actorUID, spaceID, req)
		return e
	})
	if err == nil && model != nil && req.Name != nil {
		// D8 — the all-member group's name follows the project's.
		//
		// The name is how a user recognises which group belongs to which project;
		// letting it drift means the group quietly stops being findable as "the
		// project's group". Truncation to the group's own limit is the group
		// side's job — that rule belongs to groups, not to projects.
		//
		// Only when the name actually changed: an update touching only the
		// description has no business bumping the group's version and pushing a
		// member-list refresh to every client.
		//
		// Failure leaves the two names out of step and is logged, not retried.
		// The next rename converges them.
		p.syncAllMemberGroupName(projectID, model.Name)
	}
	return model, err
}

// updateProject applies a partial profile update under the project row lock.
//
// The allow-list is built here, not from the request payload: active_name and
// is_official must never reach a SET clause, and an allow-list is the only form of
// that guarantee which survives someone later adding a field to updateReq.
func (p *Project) updateProjectOnce(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin update: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs); err != nil {
		return nil, err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errProjectGone
	}

	// Re-read the actor's role under the project lock. The handler's check came from the
	// middleware, i.e. from the Redis role cache read before this transaction opened, so an
	// admin demoted in between would still edit the project. Every other privileged write in
	// this file already did this; update and disband were the two that did not.
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, err
	}
	if !canUpdateProject(actorRole) {
		return nil, errPermissionDenied
	}

	set := map[string]interface{}{}
	if req.Name != nil {
		set["name"] = *req.Name
		row.Name = *req.Name
	}
	if req.Description != nil {
		set["description"] = *req.Description
		row.Description = *req.Description
	}
	if req.Logo != nil {
		set["logo"] = *req.Logo
		row.Logo = *req.Logo
	}
	if req.Discoverability != nil {
		set["discoverability"] = *req.Discoverability
		row.Discoverability = *req.Discoverability
	}
	if req.MaxMembers != nil {
		set["max_members"] = *req.MaxMembers
		row.MaxMembers = *req.MaxMembers
	}
	// An update naming no field is rejected rather than quietly succeeding. The previous
	// behaviour wrote nothing to the database (updateProfileTx returns early on an empty set)
	// but still reported `updated_at = now` and emitted an update audit entry — so the response
	// disagreed with the very next GET, and the audit log recorded a change that never
	// happened. Both are worse than a 400.
	if len(set) == 0 {
		return nil, errNoFieldsToUpdate
	}
	if err := p.db.updateProfileTx(tx, projectID, set, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit update: %w", err)
	}
	row.UpdatedAt = now
	return row, nil
}

// disbandProject runs disbandProjectOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) disbandProject(projectID, actorUID, spaceID string) ([]string, error) {
	var uids []string
	err := retryOnLockConflict(func() error {
		var e error
		uids, e = p.disbandProjectOnce(projectID, actorUID, spaceID)
		return e
	})
	return uids, err
}

// disbandProject marks the project disbanded, closes every seat and bumps the epoch
// in one transaction.
//
// The epoch bump is unconditional here — unlike everywhere else — and that is
// correct: disband is not retried by any worker, so there is no rerun to inflate,
// and the acceptance list requires disband to move the epoch even for a project
// whose only member is the departing owner.
//
// Returns the uids whose seats were closed, so the caller can invalidate their
// caches synchronously.
func (p *Project) disbandProjectOnce(projectID, actorUID, spaceID string) ([]string, error) {
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin disband: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs); err != nil {
		return nil, err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errProjectGone
	}

	// Owner role re-read under the project lock, same reason as updateProject: disband is the
	// most destructive operation here, and letting it run on a cached role means a
	// just-demoted ex-owner can still destroy the project.
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, err
	}
	if !canDisbandProject(actorRole) {
		return nil, errPermissionDenied
	}

	// Read the seats before closing them: after the UPDATE the status filter no
	// longer matches, so the cache-invalidation list would come back empty and the
	// removed members would keep their cached role for a full TTL.
	// FOR SHARE, for the reason on countActiveOwnersTx: this transaction's read view opened at
	// its first statement (the JOINing seat check), so a plain SELECT here answers from a
	// snapshot older than the project row lock — while the UPDATE below is a CURRENT read and
	// closes seats this list never saw. Reproduced: the list came back as ["owner1"] while the
	// UPDATE closed ["owner1", "late1"], so nothing invalidated late1's cached role and they
	// kept a positive entry on a DISBANDED project for the full TTL. That is exactly the leak
	// the requirement below exists to prevent, so the read has to see what the UPDATE will.
	var affectedUIDs []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM `octo_project_member` WHERE project_id = ? AND status = ? FOR SHARE",
		projectID, MemberStatusActive,
	).Load(&affectedUIDs); err != nil {
		return nil, fmt.Errorf("project: read seats before disband: %w", err)
	}

	// Bump BEFORE the status flip: bumpMemberEpochTx guards on status=1 (the predicate that
	// keeps a disbanded project's epoch frozen), and disband is exactly the write that must
	// move the epoch — the brief lists it alongside add/remove/leave/role-change/cascade.
	if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
		return nil, err
	}
	if _, err := p.db.disbandProjectTx(tx, projectID, now); err != nil {
		return nil, err
	}
	// D10 — the all-member group stops being one, in the SAME transaction as the
	// status flip.
	//
	// The group itself is untouched here: P1's disband step reverts it to
	// Space-direct with its members intact, along with every other group of the
	// project. What is cleared is only the FACT that it was the all-member group,
	// and that fact stops being true the moment the project does.
	//
	// In the transaction rather than in the disband step, because the step is
	// allowed to fail and P1 deliberately does not roll disband back for it. A
	// disbanded project still pointing at a group would be a state that both the
	// D7 protection predicate and the rebuild predicate would have to reason
	// about; clearing it here means that state does not exist.
	//
	// LOCK ORDER: this runs AFTER disbandProjectTx, whose last statement writes
	// octo_project_provisioning — the final lock in the declared order. Writing
	// octo_project after that would normally be an inversion; it is not one here
	// because disbandProjectTx's FIRST statement already took the X lock on this
	// exact octo_project row, so this UPDATE acquires nothing new. That is a real
	// dependency on a statement in another function, so it is written down: if the
	// status flip ever moves out of disbandProjectTx, this call has to move above
	// it rather than stay where it reads naturally.
	if err := p.db.clearAllMemberGroupNoTx(tx, projectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit disband: %w", err)
	}
	// One Redis round-trip per member, on the request path: at the 500-member default that is
	// 500 sequential DELs. They are synchronous by rule — this is an authorization boundary, so
	// handing invalidation to a worker would leave every member of a disbanded project
	// authorized for up to a full TTL — but sequential is not required by that rule, only by
	// the client: octo-lib's redis.Conn exposes single-key Del and neither a variadic form nor
	// the underlying client, so pipelining them needs a capability added there rather than a
	// second connection opened here. Recorded rather than worked around (PR #841 round 2, P2).
	for _, uid := range affectedUIDs {
		p.invalidateProjectMemberCache(projectID, uid)
	}

	// Run the registered disband steps AFTER the commit. Today that means
	// modules/group reverting this project's groups to Space-direct, members
	// untouched — the product's answer to "what happens to the groups", and the
	// same rule the cascade uses when a group's creator leaves and nobody in it
	// is still in the project.
	//
	// After the commit rather than inside it, because the steps do their own
	// transactional work across another module's tables, and holding the project
	// row's exclusive lock across that would serialize disband against every
	// group write in the project.
	//
	// A step failing does NOT fail the disband: the project IS disbanded, and
	// re-running the handler is not available to the caller. What it leaves is a
	// group whose project_id points at a disbanded project, which is precisely
	// what the I3 reconcile scan reports — so the failure is visible and
	// repairable rather than silent. Recorded here so nobody "fixes" this into a
	// rollback that would leave the project alive with its groups already
	// detached.
	p.runDisbandSteps(ProjectDisband{ProjectID: projectID, SpaceID: spaceID})

	return affectedUIDs, nil
}

// runDisbandSteps executes every registered disband step, logging failures.
//
// byCascade is false for every caller at HEAD, and that is a correction to the
// task brief rather than an omission. The brief states that P0's round-2 review
// made the Space cascade "disband the project when there is no successor", with
// a project_cascade_ownerless_disbands_total metric. Measured at e6a46cf: no
// such metric exists, disbandProject has exactly ONE caller (the handler seam in
// New), and space_member_removal.go says in as many words that leaving an
// ownerless project is "the whole P0 treatment: make it visible, decide it with
// product". So there is no cascade branch to reach.
//
// The ByCascade field is kept anyway because the distinction is real the day
// that branch exists — a project ending because a worker decided so is worth
// telling apart from one a human disbanded — and adding the field later would
// mean changing a registered step's signature across modules.
func (p *Project) runDisbandSteps(disband ProjectDisband) {
	for _, step := range snapshotDisbandSteps() {
		if err := step.fn(p.ctx, disband); err != nil {
			p.Error("项目解散级联步骤失败（项目已解散，遗留状态由 I3 对账扫描报出）",
				zap.String("step", step.name),
				zap.String("project_id", disband.ProjectID),
				zap.Error(err))
		}
	}
}

// ---------- membership ----------

// normalizeMemberAdds trims UIDs, drops exact duplicates, and rejects a
// same-UID role conflict before any transaction begins.
func normalizeMemberAdds(in []memberAdd) ([]memberAdd, error) {
	out := make([]memberAdd, 0, len(in))
	byUID := make(map[string]int, len(in))
	for _, item := range in {
		uid := strings.TrimSpace(item.UID)
		if uid == "" {
			return nil, errMemberNotFound
		}
		if item.Role != RoleCommon && item.Role != RoleAdmin {
			return nil, errMemberRoleInvalid
		}
		if role, ok := byUID[uid]; ok {
			if role != item.Role {
				return nil, errMemberRoleConflict
			}
			continue
		}
		byUID[uid] = item.Role
		out = append(out, memberAdd{UID: uid, Role: item.Role})
	}
	return out, nil
}

// addMembers admits the whole batch in one transaction. Target eligibility,
// current roles, quota, re-admission and epoch changes all share that
// transaction; one rejected target rolls every seat write back.
func (p *Project) addMembers(projectID, spaceID, actorUID string, input []memberAdd) ([]string, error) {
	members, err := normalizeMemberAdds(input)
	if err != nil {
		return nil, err
	}
	var changed []string
	err = retryOnLockConflict(func() error {
		var e error
		changed, e = p.addMembersOnce(projectID, spaceID, actorUID, members)
		return e
	})
	return changed, err
}

func (p *Project) addMembersOnce(projectID, spaceID, actorUID string, members []memberAdd) ([]string, error) {
	now := time.Now().UTC()
	targetUIDs := make([]string, 0, len(members))
	for _, item := range members {
		targetUIDs = append(targetUIDs, item.UID)
	}
	seatUIDs := make([]string, 0, len(targetUIDs)+1)
	seatUIDs = append(seatUIDs, actorUID)
	seatUIDs = append(seatUIDs, targetUIDs...)
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, seatUIDs)
	if err != nil {
		return nil, err
	}
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin add members: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if _, err := p.lockSeatsTx(tx, spaceID, actorUID, targetUIDs, nil, seatRefs); err != nil {
		return nil, err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, err
	}
	if !canManageMembers(actorRole) {
		return nil, errPermissionDenied
	}

	toAdmit := make([]memberAdd, 0, len(members))
	newSeats := 0
	for _, item := range members {
		existing, qerr := p.db.queryMemberTx(tx, projectID, item.UID)
		if qerr != nil {
			return nil, qerr
		}
		if existing != nil && existing.Status == MemberStatusActive && existing.Removing == 0 {
			if existing.Role != item.Role {
				return nil, errMemberRoleConflict
			}
			continue
		}
		toAdmit = append(toAdmit, item)
		// A closing seat is not counted by countActiveMembersTx, but this admission
		// clears removing and makes it effective again. Count it exactly like a removed
		// seat so a pending removal cannot be used as a quota slot.
		if existing == nil || existing.Status != MemberStatusActive || existing.Removing != 0 {
			newSeats++
		}
	}
	count, err := p.db.countActiveMembersTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if count+newSeats > p.cfg.effectiveMaxMembers(row.MaxMembers) {
		return nil, errQuotaMembers
	}

	changed := make([]string, 0, len(toAdmit))
	for _, item := range toAdmit {
		didChange, aerr := p.db.admitMemberTx(tx, &MemberModel{
			ProjectID: projectID,
			UID:       item.UID,
			SpaceID:   spaceID,
			Role:      item.Role,
			InviteUID: actorUID,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if aerr != nil {
			return nil, aerr
		}
		if _, cerr := p.db.cancelPendingRemovalJobsTx(tx, projectID, item.UID, now); cerr != nil {
			return nil, cerr
		}
		if didChange {
			changed = append(changed, item.UID)
		}
	}
	if len(changed) > 0 {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit add members: %w", err)
	}
	for _, uid := range changed {
		p.invalidateProjectMemberCache(projectID, uid)
		p.provisionSidebarSection(row.ProjectID, row.SpaceID, uid)
	}
	return changed, nil
}

// addOneMember remains a narrow seam for create/legacy callers; it uses the
// same atomic role-aware implementation as members/add.
func (p *Project) addOneMember(projectID, spaceID, actorUID, uid string) (bool, error) {
	var changed []string
	err := retryOnLockConflict(func() error {
		var e error
		changed, e = p.addMembersOnce(projectID, spaceID, actorUID, []memberAdd{{UID: uid, Role: RoleCommon}})
		return e
	})
	if err != nil {
		return false, err
	}
	return len(changed) > 0, nil
}

func (p *Project) addOneMemberOnce(projectID, spaceID, actorUID, uid string) (bool, error) {
	changed, err := p.addMembersOnce(projectID, spaceID, actorUID, []memberAdd{{UID: uid, Role: RoleCommon}})
	return len(changed) > 0, err
}

// removeMember runs removeMemberOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) removeMember(projectID, spaceID, actorUID, targetUID string) (bool, error) {
	var removed bool
	err := retryOnLockConflict(func() error {
		var e error
		removed, e = p.removeMemberOnce(projectID, spaceID, actorUID, targetUID)
		return e
	})
	return removed, err
}

// removeMember closes one seat.
//
// from an earlier unlocked read: the transitive-protection rule ("an admin may
// remove a peer admin but not the owner") is only sound if the role it checks
// cannot change between the check and the write.
func (p *Project) removeMemberOnce(projectID, spaceID, actorUID, targetUID string) (bool, error) {
	if targetUID == actorUID {
		// Self-removal goes through leave, which carries the last-owner transfer rule.
		// Allowing it here would let the last owner delete their own seat and leave an
		// ownerless project.
		return false, errSelfRemovalNotAllowed
	}
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID})
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin remove member: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs); err != nil {
		// ACTOR-level, re-tagged for the same reason as the add path: removal is the other
		// batch-driven endpoint, and its handler drives the loop one target at a time — so
		// without this it fell to the default branch, labelled the actor's own expired
		// standing "store_failed" per uid, and kept opening one doomed transaction for every
		// remaining target.
		if errors.Is(err, errNotSpaceMember) {
			return false, errActorNotSpaceMember
		}
		return false, err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, errProjectGone
	}

	// Both roles are read under the project lock. The actor's role is deliberately NOT the
	// one the middleware resolved: that came from the membership cache before this
	// transaction opened, so an actor demoted in between would still act with the old
	// privilege. modules/space re-reads the operator role in-lock for the same operation and
	// the same reason (modules/space/api.go:871).
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, err
	}

	if !canManageMembers(actorRole) {
		return false, errPermissionDenied
	}

	target, err := p.db.queryMemberTx(tx, projectID, targetUID)
	if err != nil {
		return false, err
	}
	// removing = 1 reads as "not a member" here too. Acting on a closing seat is
	// not a partial success to report, it is a target that no longer exists.
	if target == nil || target.Status != MemberStatusActive || target.Removing != 0 {
		return false, errMemberNotFound
	}
	if !canActOnTargetRole(actorRole, target.Role) {
		return false, errTargetProtected
	}
	if target.Role == RoleOwner {
		owners, err := p.db.countActiveOwnersTx(tx, projectID)
		if err != nil {
			return false, err
		}
		if owners <= 1 {
			// Removing the last owner would leave the project unmanageable, with no
			// path back: nothing in P0 can promote a member without an owner.
			return false, errLastOwnerMustTransfer
		}
	}

	// D4 — two-phase close. `removing = 1` goes in with `status` still 1, and the
	// worker flips status only after registered cleanup completes. See db_removal.go
	// for why the order is inverted: authorization stops using the seat immediately
	// while the worker finishes any cleanup owned by another module.
	//
	// member_epoch is bumped HERE, not at the worker's final flip, because from
	// every consumer's point of view the membership already changed: the seat
	// stops granting anything the moment removing is set.
	changed, closedAgents, err := p.beginRemovalWithAgentsTx(tx, projectID, spaceID, actorUID, targetUID,
		removalReasonKicked, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit remove member: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, targetUID)
		// D13 — the agents lost their seats in the same transaction, so their
		// cached roles are stale too. Missing these would leave an agent
		// authorized against the project for a full cache TTL after its seat
		// closed, which is the same leak the member's own invalidation prevents.
		//
		// Audited in the same loop, and for the same reason the seat closure is
		// not silent: these are member removals, so the trail has to carry them.
		// The caller's handler cannot do it — closedAgents does not leave this
		// function — which is why the audit sits in the service here rather than
		// beside the target's own entry, the way space_member_removal.go does it.
		for _, agentUID := range closedAgents {
			p.invalidateProjectMemberCache(projectID, agentUID)
			p.audit(auditMemberRemove, actorUID, agentUID, projectID, spaceID,
				auditReasonAgentFollowsOwner)
		}
	}
	return changed, nil
}

// leaveProject runs leaveProjectOnce through the bounded lock-conflict retry.
func (p *Project) leaveProject(projectID, spaceID, uid string) error {
	return retryOnLockConflict(func() error {
		return p.leaveProjectOnce(projectID, spaceID, uid)
	})
}

// leaveProject closes the caller's own seat.
//
// An Owner cannot leave directly: ownership must first be transferred through
// the dedicated owner-transfer transaction.
func (p *Project) leaveProjectOnce(projectID, spaceID, uid string) error {
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{uid})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return fmt.Errorf("project: begin leave: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, uid, seatRefs); err != nil {
		return err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return err
	}
	if row == nil {
		return errProjectGone
	}
	self, err := p.db.queryMemberTx(tx, projectID, uid)
	if err != nil {
		return err
	}
	if self == nil || self.Status != MemberStatusActive || self.Removing != 0 {
		return errMemberNotFound
	}
	if self.Role == RoleOwner {
		return errLastOwnerMustTransfer
	}

	changed, closedAgents, err := p.beginRemovalWithAgentsTx(tx, projectID, spaceID, uid, uid,
		removalReasonLeft, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("project: commit leave: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, uid)
		for _, agentUID := range closedAgents {
			p.invalidateProjectMemberCache(projectID, agentUID)
			p.audit(auditMemberRemove, uid, agentUID, projectID, spaceID,
				auditReasonAgentFollowsOwner)
		}
	}
	return nil
}

// changeMemberRole runs the role mutation through the bounded lock-conflict retry.
// Owner changes have their own transferProjectOwner transaction.
func (p *Project) changeMemberRole(projectID, spaceID, actorUID, targetUID string, role int) (bool, error) {
	if role != RoleCommon && role != RoleAdmin {
		return false, errMemberRoleInvalid
	}
	var changed bool
	err := retryOnLockConflict(func() error {
		var e error
		changed, e = p.changeMemberRoleOnce(projectID, spaceID, actorUID, targetUID, role)
		return e
	})
	return changed, err
}

func (p *Project) changeMemberRoleOnce(projectID, spaceID, actorUID, targetUID string, role int) (bool, error) {
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID, targetUID})
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin role change: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs, targetUID); err != nil {
		return false, err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, err
	}
	if !canAssignMemberRole(actorRole, role) {
		return false, errPermissionDenied
	}
	target, err := p.db.queryMemberTx(tx, projectID, targetUID)
	if err != nil {
		return false, err
	}
	if target == nil || target.Status != MemberStatusActive || target.Removing != 0 {
		return false, errMemberNotFound
	}
	if !canActOnTargetRole(actorRole, target.Role) {
		return false, errTargetProtected
	}
	changed, err := p.db.updateMemberRoleTx(tx, projectID, targetUID, role, now)
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit role change: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, targetUID)
	}
	return changed, nil
}

// transferProjectOwner atomically assigns Owner to a current project member and
// demotes the former Owner to Admin.
func (p *Project) transferProjectOwner(projectID, spaceID, actorUID, successorUID string) error {
	return retryOnLockConflict(func() error {
		return p.transferProjectOwnerOnce(projectID, spaceID, actorUID, successorUID)
	})
}

func (p *Project) transferProjectOwnerOnce(projectID, spaceID, actorUID, successorUID string) error {
	if strings.TrimSpace(successorUID) == "" || successorUID == actorUID {
		return errMemberNotFound
	}
	seatRefs, err := p.db.resolveSpaceSeatIDs(spaceID, []string{actorUID, successorUID})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return fmt.Errorf("project: begin owner transfer: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, seatRefs, successorUID); err != nil {
		return err
	}
	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return err
	}
	if row == nil {
		return errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return err
	}
	if actorRole != RoleOwner {
		return errPermissionDenied
	}
	target, err := p.db.queryMemberTx(tx, projectID, successorUID)
	if err != nil {
		return err
	}
	if target == nil || target.Status != MemberStatusActive || target.Removing != 0 {
		return errMemberNotFound
	}
	if target.Role == RoleOwner {
		return errMemberRoleConflict
	}
	if _, err := p.db.updateMemberRoleTx(tx, projectID, successorUID, RoleOwner, now); err != nil {
		return err
	}
	if _, err := p.db.updateMemberRoleTx(tx, projectID, actorUID, RoleAdmin, now); err != nil {
		return err
	}
	if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("project: commit owner transfer: %w", err)
	}
	p.invalidateProjectMemberCache(projectID, actorUID)
	p.invalidateProjectMemberCache(projectID, successorUID)
	return nil
}

// ---------- helpers ----------

// actorRoleTx reads the actor's own project role under the caller's transaction, returning
// roleNonMember when they hold no active seat.
//
// Every privileged write goes through it instead of trusting the role projectMiddleware
// resolved. That role came from the Redis membership cache and was read before the
// transaction opened; acting on it is a privilege TOCTOU, and the cost of closing it is one
// indexed point read on a row the transaction is about to lock anyway.
//
// Safe against the obvious deadlock (A removing B while B removes A takes the two
// octo_project_member row locks in opposite orders) because every membership write locks the
// octo_project row first, so no two of them are ever concurrently past that point for the
// same project.
func (p *Project) actorRoleTx(tx *dbr.Tx, projectID, actorUID string) (int, error) {
	actor, err := p.db.queryMemberTx(tx, projectID, actorUID)
	if err != nil {
		return roleNonMember, err
	}
	// A seat at removing = 1 is NOT a member for any authorization purpose — that
	// is the whole contract of the two-phase close, and every other authorization
	// read in the module already carries it (pkg/project's three predicates,
	// countActiveOwnersTx, listMembers). This one deciding otherwise would leave a
	// departing owner holding disband and role-change for the entire cascade
	// window, on the re-read whose only job is to catch a role the middleware's
	// cache got wrong.
	if actor == nil || actor.Status != MemberStatusActive || actor.Removing != 0 {
		return roleNonMember, nil
	}
	return actor.Role, nil
}

// isDuplicateKeyErr reports whether err is a UNIQUE constraint violation.
//
// Typed path first (*mysql.MySQLError.Number == 1062), which is driver-stable and
// the convention elsewhere in this repo (modules/app_bot/db.go,
// modules/bot_api/obo_db.go); substring fallback so a test double emitting
// errors.New("Error 1062: ...") still satisfies the contract.
// withoutUID returns in without every occurrence of drop, preserving order.
// Used to keep the creator out of the agent list before it is classified.
func withoutUID(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, uid := range in {
		if uid != drop {
			out = append(out, uid)
		}
	}
	return out
}

func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Error 1062")
}

// sanitizeUIDs trims, drops blanks and de-duplicates a batch of uids, preserving
// order. De-duplication matters for more than tidiness: the same uid twice in one
// batch would take the project row lock twice and report two outcomes for one seat.
func sanitizeUIDs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, uid := range in {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out
}

// beginRemovalWithCascadeTx performs the seat-closing half of D4's two-phase
// removal inside the caller's transaction: set removing = 1, bump the epoch, and
// enqueue the cascade job.
//
// All three in ONE transaction is the point. A crash between the flag and the
// job would leave a seat at removing = 1 without a worker to finish it. That is
// exactly the failure the outbox pattern exists to prevent, and why this is not
// "flip the flag, then enqueue".
//
// Reports whether anything changed. False means the seat was already closing or
// already closed, so the caller must not bump the epoch again or enqueue a
// second job — the idempotence rule P0 established and nearly broke.
func (p *Project) beginRemovalWithCascadeTx(
	tx *dbr.Tx,
	projectID, spaceID, operatorUID, targetUID, reason string,
	now time.Time,
) (bool, error) {
	changed, _, err := p.beginRemovalWithAgentsTx(
		tx, projectID, spaceID, operatorUID, targetUID, reason, now)
	return changed, err
}

// beginRemovalWithAgentsTx is beginRemovalWithCascadeTx plus D13: the departing
// member's OWN agents lose their seats in the same transaction, each with its own
// cascade job, and the epoch moves exactly once for the whole group.
//
// Returns the agent uids whose seats were closed, so the caller can invalidate
// their membership caches beside the member's own.
//
// # Why the agents have to go
//
// This is not a policy invented here, it is the project-side half of a rule the
// group side has enforced since #354: RemoveGroupMembers takes the leaver's bots
// out of the group with them, matched on robot.creator_uid, with no exception for
// role. P1's detach reuses RemoveGroupMembers, so the moment a person's project
// seat closes, their agents are already being pulled out of that project's groups.
//
// Without this, the agent keeps an ACTIVE project seat while sitting in none of
// the project's groups. That is I4 broken — the all-member group's roster no
// longer equals the project's roster — and nothing repairs it: the seat is
// active, so no cascade will ever look at it again, and the admitter only runs on
// a fresh add. The first member to leave any project would break the invariant
// permanently.
//
// It is also what the word means. An agent runs AS its owner
// (modules/bot_api/obo_fanout.go renders it that way to the model itself); an
// owner who has left the project should not still have a proxy reading it.
//
// # Matched on robot.creator_uid, deliberately the same field as the group side
//
// queryOwnedAgentSeatsTx joins `robot` on creator_uid, exactly as
// QueryBotsInvitedByUIDTx does. Matching on invite_uid instead would be the
// obvious alternative and it is wrong: the two sides would then disagree about
// whose agent something is, and every disagreement is a row that one side removed
// and the other kept.
//
// # Owner transfer is not leaving
//
// Only seat-CLOSING paths reach here (kick, leave, Space cascade). An owner
// handing the project to someone else stays a member, so their agents stay too.
//
// # One epoch bump for the whole set
//
// A member and their agents leaving is one membership change from a consumer's
// point of view, and the rule is that the epoch only ever moves by +1 per write.
// Bumping per uid would move it by 1+N and break the "+1 only" assertion P0
// established.
func (p *Project) beginRemovalWithAgentsTx(
	tx *dbr.Tx,
	projectID, spaceID, operatorUID, targetUID, reason string,
	now time.Time,
) (bool, []string, error) {
	changed, err := p.db.beginMemberRemovalTx(tx, projectID, targetUID, now)
	if err != nil {
		return false, nil, err
	}
	if !changed {
		return false, nil, nil
	}

	// Read the agents BEFORE their seats are touched: queryOwnedAgentSeatsTx
	// filters on status = 1 AND removing = 0, so reading after would return a
	// shorter list on a retry and silently leave seats open.
	agents, err := p.db.queryOwnedAgentSeatsTx(tx, projectID, targetUID)
	if err != nil {
		return false, nil, err
	}
	closedAgents := make([]string, 0, len(agents))
	for _, agentUID := range agents {
		if agentUID == "" || agentUID == targetUID {
			continue
		}
		agentChanged, err := p.db.beginMemberRemovalTx(tx, projectID, agentUID, now)
		if err != nil {
			return false, nil, err
		}
		if !agentChanged {
			continue
		}
		// Its own job, not a rider on the owner's: the cascade worker is keyed
		// (project_id, uid) and re-reads THAT row under lock before each batch.
		// A job that claimed to cover two uids could not be cancelled for one of
		// them, and re-admission cancels per uid (D4).
		//
		// The operator is whoever triggered the owner's removal, and the reason
		// is the owner's reason: the agent is not being kicked on its own
		// account, and a distinct reason would have to be added to the group
		// side's suppression logic to render sensibly.
		if err := p.db.enqueueRemovalJobTx(tx, RemovalJob{
			ProjectID:   projectID,
			UID:         agentUID,
			SpaceID:     spaceID,
			OperatorUID: operatorUID,
			Reason:      reason,
		}, now); err != nil {
			return false, nil, err
		}
		closedAgents = append(closedAgents, agentUID)
	}
	closingUIDs := make([]string, 0, 1+len(closedAgents))
	closingUIDs = append(closingUIDs, targetUID)
	closingUIDs = append(closingUIDs, closedAgents...)
	rolesCleared, err := p.db.deleteMemberCollaborationRolesTx(tx, projectID, closingUIDs)
	if err != nil {
		return false, nil, err
	}
	if rolesCleared {
		if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
			return false, nil, err
		}
	}

	if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
		return false, nil, err
	}
	if err := p.db.enqueueRemovalJobTx(tx, RemovalJob{
		ProjectID:   projectID,
		UID:         targetUID,
		SpaceID:     spaceID,
		OperatorUID: operatorUID,
		Reason:      reason,
	}, now); err != nil {
		return false, nil, err
	}
	return true, closedAgents, nil
}
