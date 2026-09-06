package project

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// Lock order for every write path in this module, followed without exception:
//
//	space  ->  project  ->  group  ->  group_member  ->  octo_project_member
//
// Concretely: a membership write locks the octo_project row (lockActiveProjectTx)
// BEFORE it touches octo_project_member, and the Space-side facts it needs
// (CheckMembership, MemberRole) are read before that lock is taken. P0 touches no
// group table at all, so the two middle positions are reserved for P1's group
// admission; recording them now is what keeps P1 from choosing a different order
// and deadlocking against this code.
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

// Sentinel errors the API layer maps onto registered error codes. Returning typed
// errors rather than responding from the service keeps the transaction boundary and
// the wire contract in separate files.
var (
	errQuotaPerSpace         = errors.New("project: per-space project quota reached")
	errQuotaPerCreator       = errors.New("project: per-creator project quota reached")
	errQuotaDailyCreate      = errors.New("project: daily project creation quota reached")
	errQuotaMembers          = errors.New("project: per-project member quota reached")
	errNameDuplicated        = errors.New("project: active project name already used in this space")
	errProjectGone           = errors.New("project: project is absent or disbanded")
	errNotSpaceMember        = errors.New("project: target uid is not an active space member")
	errMemberNotFound        = errors.New("project: target uid is not an active project member")
	errLastOwnerMustTransfer = errors.New("project: the last owner must transfer ownership first")
	// errPermissionDenied is ACTOR-level: the caller does not hold the role this operation
	// needs. A batch endpoint must surface it as one top-level 403, because no target in the
	// batch could succeed either.
	errPermissionDenied = errors.New("project: operation not permitted for this role")
	// errNoFieldsToUpdate marks an update request that names no field. Rejected rather than
	// treated as a success, so the response and the audit log cannot describe a write that
	// never reached the database.
	errNoFieldsToUpdate = errors.New("project: update names no field")
	// errTargetProtected is TARGET-level: the caller is authorized in general, but not against
	// this particular member (the transitive-protection rule — an admin may not remove or
	// demote another admin or the owner). A batch endpoint reports it per uid, because the
	// other targets may well be fine.
	//
	// Keeping the two apart is a wire-contract matter, not tidiness: folding them together
	// turned "you are no longer an admin" into a 200 with a per-uid note, which tells the
	// client the wrong thing about what went wrong.
	errTargetProtected = errors.New("project: not permitted to act on this member's role")
	// errSelfRemovalNotAllowed steers self-removal to the leave endpoint, which carries the
	// last-owner transfer rule. Target-level: the rest of a batch is unaffected.
	errSelfRemovalNotAllowed = errors.New("project: use leave to remove yourself")
)

// ---------- permission matrix ----------
//
// Space admins get READ widening only (they can see unlisted projects and rosters,
// which is what discoverability being "not a security boundary" means). They do NOT
// get project management: the admin-facing surface — the is_official badge and the
// rest — is P2, and quietly granting Space admins write access here would make that
// P2 design retroactively load-bearing.

func canUpdateProject(projectRole int) bool    { return projectRole >= RoleAdmin }
func canDisbandProject(projectRole int) bool   { return projectRole == RoleOwner }
func canManageMembers(projectRole int) bool    { return projectRole >= RoleAdmin }
func canChangeMemberRole(projectRole int) bool { return projectRole == RoleOwner }
func isProjectMember(projectRole int) bool     { return projectRole >= RoleCommon }
func canViewMembers(projectRole, spaceRole int) bool {
	return isProjectMember(projectRole) || spaceRole >= spacepkg.MemberRoleAdmin
}

// capabilitiesFor renders the caller's permissions as explicit booleans so a client
// never re-derives them from the role number. A client-side copy of this matrix
// drifts from the server the first time the matrix changes.
func capabilitiesFor(projectRole, spaceRole int) Capabilities {
	return Capabilities{
		CanUpdate:       canUpdateProject(projectRole),
		CanDisband:      canDisbandProject(projectRole),
		CanManageMember: canManageMembers(projectRole),
		CanChangeRole:   canChangeMemberRole(projectRole),
		CanLeave:        isProjectMember(projectRole),
		CanViewMembers:  canViewMembers(projectRole, spaceRole),
	}
}

// canActOnTargetRole implements the transitive protection: an admin may not remove
// or demote another admin or the owner.
//
// Without it "admin" is effectively "owner": one admin demotes every peer and the
// owner, and the project has a new sole controller. Only an owner may act on a
// role at or above admin.
func canActOnTargetRole(actorRole, targetRole int) bool {
	if actorRole == RoleOwner {
		return true
	}
	if actorRole == RoleAdmin {
		return targetRole == RoleCommon
	}
	return false
}

// ---------- create ----------

type createInput struct {
	SpaceID         string
	Creator         string
	Name            string
	Description     string
	Logo            string
	Discoverability int
	JoinMode        int
	MaxMembers      int
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
func (p *Project) createProject(in createInput) (*Model, error) {
	now := time.Now().UTC()
	dayFrom, dayTo := p.cfg.dayWindow(now)

	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin create: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// Lock the Space row FIRST. Two things depend on it, and neither is optional:
	//
	//   1. It serialises every create in this Space, which is what makes the counts below
	//      mean anything. A plain SELECT COUNT(*) is a non-locking consistent read even
	//      inside a transaction, so without this lock two concurrent creates both read 999
	//      and both insert. An earlier version of this function put the counts in a
	//      transaction and claimed that was enough; it was not.
	//   2. It confirms the Space is still active at write time, not just when the middleware
	//      looked.
	//
	// It is also the first position in the module's lock order, so nothing else may be held yet.
	spaceActive, err := p.db.lockSpaceRowTx(tx, in.SpaceID)
	if err != nil {
		return nil, err
	}
	if !spaceActive {
		return nil, errNotSpaceMember
	}

	// I1 for the owner seat, inside this transaction and holding a shared lock on the
	// creator's space_member row.
	//
	// createProject writes a membership row like any other write path, so it owes the same
	// invariant — and it was the one path that did not check. The middleware's check does not
	// substitute: it ran before this transaction, against a 60s Redis cache, so a Space
	// removal committing in between left a permanent owner seat with no Space seat, on the one
	// project nobody could then clean up (the cascade closes seats, and an ownerless project
	// cannot be disbanded).
	creatorIsMember, err := p.db.checkSpaceMembershipForWriteTx(tx, in.SpaceID, in.Creator)
	if err != nil {
		return nil, err
	}
	if !creatorIsMember {
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
		JoinMode:        in.JoinMode,
		MaxMembers:      in.MaxMembers,
		Status:          StatusNormal,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := p.db.insertProjectTx(tx, model); err != nil {
		// A duplicate ACTIVE name is caught by the unique index rather than by a
		// pre-check, so two concurrent creates of the same name cannot both win.
		if isDuplicateKeyErr(err) {
			return nil, errNameDuplicated
		}
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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit create: %w", err)
	}
	p.invalidateProjectMemberCache(model.ProjectID, in.Creator)
	return model, nil
}

// ---------- update / disband ----------

// updateProject applies a partial profile update under the project row lock.
//
// The allow-list is built here, not from the request payload: active_name and
// is_official must never reach a SET clause, and an allow-list is the only form of
// that guarantee which survives someone later adding a field to updateReq.
func (p *Project) updateProject(projectID, actorUID string, req updateReq) (*Model, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin update: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

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
	if req.JoinMode != nil {
		set["join_mode"] = *req.JoinMode
		row.JoinMode = *req.JoinMode
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
		if isDuplicateKeyErr(err) {
			return nil, errNameDuplicated
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit update: %w", err)
	}
	row.UpdatedAt = now
	return row, nil
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
func (p *Project) disbandProject(projectID, actorUID string) ([]string, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin disband: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

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
	var affectedUIDs []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM `octo_project_member` WHERE project_id = ? AND status = ?",
		projectID, MemberStatusActive,
	).Load(&affectedUIDs); err != nil {
		return nil, fmt.Errorf("project: read seats before disband: %w", err)
	}

	if _, err := p.db.disbandProjectTx(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit disband: %w", err)
	}
	for _, uid := range affectedUIDs {
		p.invalidateProjectMemberCache(projectID, uid)
	}
	return affectedUIDs, nil
}

// ---------- membership ----------

// addMemberResult reports one target's outcome so a batch add can be partially
// successful without the caller having to guess which uids landed.
type addMemberResult struct {
	UID      string
	Admitted bool
	Err      error
}

// addMembers admits each target in its own transaction.
//
// One transaction per target rather than one for the batch: a single rejected uid
// must not roll back the ones that were legitimately admitted, and holding the
// project row lock across a 200-uid batch would block every concurrent membership
// write on that project for the whole batch.
//
// I1 is enforced INSIDE each transaction with pkg/space.CheckMembership
// (space_member.status=1 AND space.status=1), so a non-member can never be
// admitted — not even by a caller who raced a Space removal. Checking before the
// transaction would leave exactly that window open.
func (p *Project) addMembers(projectID, spaceID, actorUID string, uids []string) ([]addMemberResult, error) {
	results := make([]addMemberResult, 0, len(uids))
	for _, uid := range uids {
		admitted, err := addOneMemberForTest(p, projectID, spaceID, actorUID, uid)
		results = append(results, addMemberResult{UID: uid, Admitted: admitted, Err: err})
		// An ACTOR-level or project-level failure ends the batch HERE, not in the handler.
		//
		// The handler used to be the only one to stop: it reported the remaining uids as
		// "not_attempted" while this loop had already run every one of them — and some of
		// those later targets had COMMITTED (their transactions were fine; it was the actor's
		// rights that expired). So a committed add was reported as never tried, its audit entry
		// never written, and a client trusting the report would retry it. Worse, the removal
		// batch — which the handler drives one target at a time — made the same label mean the
		// truth, so the two paths disagreed about what "not_attempted" claims.
		//
		// Stopping here makes the label honest: everything before it really ran, everything
		// after it really did not. Continuing would also be pure waste — each remaining uid
		// opens a transaction only to be refused by the same in-lock recheck.
		if errors.Is(err, errPermissionDenied) || errors.Is(err, errProjectGone) {
			break
		}
	}
	return results, nil
}

func (p *Project) addOneMember(projectID, spaceID, actorUID, uid string) (bool, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin add member: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// I1 first, and INSIDE this transaction: it takes a shared lock on the target's
	// space_member row, so a Space removal cannot commit between the check and the write.
	// Before lockActiveProjectTx, so the declared lock order (space -> project) holds.
	isSpaceMember, err := p.db.checkSpaceMembershipForWriteTx(tx, spaceID, uid)
	if err != nil {
		return false, err
	}
	if !isSpaceMember {
		return false, errNotSpaceMember
	}

	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, errProjectGone
	}

	// Re-read the ACTOR's role under the project lock rather than trusting the role the
	// middleware resolved (which came from a cache, before this transaction). modules/space
	// added exactly this re-read to its own member removal for the same reason
	// (modules/space/api.go:871, PR #339 review): a pre-transaction role check plus a
	// conditional write is a privilege TOCTOU.
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, err
	}
	if !canManageMembers(actorRole) {
		return false, errPermissionDenied
	}

	existing, err := p.db.queryMemberTx(tx, projectID, uid)
	if err != nil {
		return false, err
	}
	if existing != nil && existing.Status == MemberStatusActive {
		// Already a member: a no-op. Not an error — a batch add of a roster that
		// partially overlaps must be idempotent — and specifically not an epoch bump.
		return false, nil
	}

	count, err := p.db.countActiveMembersTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if count >= p.cfg.effectiveMaxMembers(row.MaxMembers) {
		return false, errQuotaMembers
	}

	changed, err := p.db.admitMemberTx(tx, &MemberModel{
		ProjectID: projectID,
		UID:       uid,
		SpaceID:   spaceID,
		Role:      RoleCommon,
		InviteUID: actorUID,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit add member: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, uid)
	}
	return changed, nil
}

// removeMember closes one seat.
//
// The target's role is re-read under a row lock inside the transaction, not taken
// from an earlier unlocked read: the transitive-protection rule ("an admin may not
// remove an admin or the owner") is only sound if the role it checks cannot change
// between the check and the write.
func (p *Project) removeMember(projectID, actorUID, targetUID string) (bool, error) {
	if targetUID == actorUID {
		// Self-removal goes through leave, which carries the last-owner transfer rule.
		// Allowing it here would let the last owner delete their own seat and leave an
		// ownerless project.
		return false, errSelfRemovalNotAllowed
	}
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin remove member: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

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
	if target == nil || target.Status != MemberStatusActive {
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

	changed, err := p.db.deactivateMemberTx(tx, projectID, targetUID, now)
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit remove member: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, targetUID)
	}
	return changed, nil
}

// leaveProject closes the caller's own seat, transferring ownership first when the
// caller is the last owner.
//
// The transfer and the departure are one transaction. Two transactions would leave
// a window with two owners (if the transfer commits first) or none (if the departure
// does), and the second of those is unrecoverable in P0.
func (p *Project) leaveProject(projectID, uid, transferTo string) (string, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return "", fmt.Errorf("project: begin leave: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", errProjectGone
	}

	self, err := p.db.queryMemberTx(tx, projectID, uid)
	if err != nil {
		return "", err
	}
	if self == nil || self.Status != MemberStatusActive {
		return "", errMemberNotFound
	}

	successorPromoted := ""
	if self.Role == RoleOwner {
		owners, err := p.db.countActiveOwnersTx(tx, projectID)
		if err != nil {
			return "", err
		}
		if owners <= 1 {
			if err := p.promoteSuccessorTx(tx, projectID, row.SpaceID, transferTo, uid, now); err != nil {
				return "", err
			}
			successorPromoted = transferTo
		}
	}

	changed, err := p.db.deactivateMemberTx(tx, projectID, uid, now)
	if err != nil {
		return "", err
	}
	if changed {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("project: commit leave: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, uid)
	}
	if successorPromoted != "" {
		p.invalidateProjectMemberCache(projectID, successorPromoted)
	}
	return successorPromoted, nil
}

// changeMemberRole sets one member's role, handling the last-owner demotion via the
// same atomic transfer as leaveProject.
func (p *Project) changeMemberRole(projectID, actorUID, targetUID string, role int, transferTo string) (bool, string, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, "", fmt.Errorf("project: begin role change: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	row, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, "", err
	}
	if row == nil {
		return false, "", errProjectGone
	}

	// actorUID was previously accepted and never read — an unused parameter on an
	// authorization-relevant function, which reads exactly like a check that was intended
	// and dropped. The handler's owner-only gate gets its role from the middleware cache, so
	// re-read it here under the project lock.
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, "", err
	}
	if !canChangeMemberRole(actorRole) {
		return false, "", errPermissionDenied
	}

	target, err := p.db.queryMemberTx(tx, projectID, targetUID)
	if err != nil {
		return false, "", err
	}
	if target == nil || target.Status != MemberStatusActive {
		return false, "", errMemberNotFound
	}
	if !canActOnTargetRole(actorRole, target.Role) {
		return false, "", errTargetProtected
	}

	successorPromoted := ""
	if target.Role == RoleOwner && role != RoleOwner {
		owners, err := p.db.countActiveOwnersTx(tx, projectID)
		if err != nil {
			return false, "", err
		}
		if owners <= 1 {
			if err := p.promoteSuccessorTx(tx, projectID, row.SpaceID, transferTo, targetUID, now); err != nil {
				return false, "", err
			}
			successorPromoted = transferTo
		}
	}

	// A PROMOTION must re-check the target's Space seat, in this transaction.
	//
	// promoteSuccessorTx already does this for the transfer path, but a direct role change went
	// straight to the UPDATE. The gap: a target removed from the Space whose asynchronous cascade
	// has not yet closed their Project seat could be promoted — and promoting them to owner makes
	// the owner count 2, which lets the original owner leave with no transfer; the cascade then
	// closes the new owner and the project is left with nobody who can administer it. Exactly the
	// end state the transfer path was hardened against, reached through the other door.
	//
	// Only promotions are gated. A DEMOTION narrows privilege, so refusing it because the target
	// is already on their way out would block the operator from doing the safe thing.
	if role > target.Role && role >= RoleAdmin {
		stillMember, err := p.db.checkSpaceMembershipForWriteTx(tx, row.SpaceID, targetUID)
		if err != nil {
			return false, "", err
		}
		if !stillMember {
			return false, "", errNotSpaceMember
		}
	}

	changed, err := p.db.updateMemberRoleTx(tx, projectID, targetUID, role, now)
	if err != nil {
		return false, "", err
	}
	if changed || successorPromoted != "" {
		if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
			return false, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("project: commit role change: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, targetUID)
	}
	if successorPromoted != "" {
		p.invalidateProjectMemberCache(projectID, successorPromoted)
	}
	if changed || successorPromoted != "" {
		return true, successorPromoted, nil
	}
	return false, successorPromoted, nil
}

// promoteSuccessorTx validates and promotes a named successor to owner, inside the caller's
// transaction.
//
// The Space-seat check is the point. Checking only for an active PROJECT seat is not enough:
// the Space-removal cascade is asynchronous, so a user removed from the Space keeps their
// project seat until the cleanup job runs. Promoting them hands ownership to a seat that is
// already scheduled for closure — and once the cascade closes it the project has no owner at
// all, which is unrecoverable in P0 (role change and disband are owner-only, and a Space admin
// has read access only). The predicate is the authorization one, so a banned Space does not
// qualify a successor either.
//
// Lock order is preserved: the Space seat is taken (FOR SHARE) while the project row is
// already held, which is space -> project read in the other direction. That is safe here and
// only here because the Space-side lock is SHARED and the removal path takes its own
// project-row lock only from the cascade, after its Space transaction has committed — so no
// cycle exists. Any future writer that needs an EXCLUSIVE space_member lock while holding a
// project row must take it before the project row instead.
func (p *Project) promoteSuccessorTx(tx *dbr.Tx, projectID, spaceID, successorUID, departingUID string, now time.Time) error {
	if successorUID == "" || successorUID == departingUID {
		return errLastOwnerMustTransfer
	}
	successor, err := p.db.queryMemberTx(tx, projectID, successorUID)
	if err != nil {
		return err
	}
	if successor == nil || successor.Status != MemberStatusActive {
		return errMemberNotFound
	}
	stillInSpace, err := p.db.checkSpaceMembershipForWriteTx(tx, spaceID, successorUID)
	if err != nil {
		return err
	}
	if !stillInSpace {
		// Reported as "not a Space member" rather than "not found": the caller named a real
		// project member, and telling them the seat is missing would send them looking in the
		// wrong place.
		return errNotSpaceMember
	}
	if _, err := p.db.updateMemberRoleTx(tx, projectID, successorUID, RoleOwner, now); err != nil {
		return err
	}
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
	if actor == nil || actor.Status != MemberStatusActive {
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
