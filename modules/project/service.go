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
	"go.uber.org/zap"
)

// Lock order for every write path in this module, followed without exception:
//
//	space_member  ->  space  ->  project  ->  group  ->  group_member  ->  octo_project_member
//
// Concretely: a membership write locks the octo_project row (lockActiveProjectTx)
// BEFORE it touches octo_project_member, and the Space-side facts it needs
// (CheckMembership, MemberRole) are read before that lock is taken. P0 touches no
// group table at all, so the two middle positions are reserved for P1's group
// admission; recording them now is what keeps P1 from choosing a different order
// and deadlocking against this code.
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
//   - it is a TABLE order, and a table order cannot express ordering among the ROWS of one
//     table. That is where round 3's remaining cycle lived: several paths locked two or three
//     space_member rows in sequence while modules/space's disband scan locks them row by row
//     in id order. Rows are therefore not ordered by convention at all — every path takes all
//     of its space_member locks in ONE statement (requireSpaceSeatsTx) and lets InnoDB choose,
//     and retryOnLockConflict is the backstop for whatever this reasoning still misses.
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
	errQuotaPerSpace    = errors.New("project: per-space project quota reached")
	errQuotaPerCreator  = errors.New("project: per-creator project quota reached")
	errQuotaDailyCreate = errors.New("project: daily project creation quota reached")
	errQuotaMembers     = errors.New("project: per-project member quota reached")
	errNameDuplicated   = errors.New("project: active project name already used in this space")
	errProjectGone      = errors.New("project: project is absent or disbanded")
	errNotSpaceMember   = errors.New("project: target uid is not an active space member")
	// errActorNotSpaceMember is the ACTOR-level counterpart of errNotSpaceMember: the CALLER
	// no longer holds an active seat in the Space (removal, or the Space itself went
	// inactive). It exists because the two are indistinguishable to a BATCH endpoint
	// otherwise, and they demand opposite handling: a target without a Space seat is one
	// rejected uid among many and the batch continues, while an ACTOR without one means no
	// remaining target can succeed, so the batch must stop and the caller must be told it is
	// their own standing that failed. Folding them, as the first version of the add path did,
	// made an actor-level refusal look like a per-uid note and opened one doomed transaction
	// per remaining uid (PR #841 round 2, Jerry-Xin N-1).
	//
	// It WRAPS errNotSpaceMember rather than standing beside it, so this is a refinement and
	// not a reclassification: every existing errors.Is(err, errNotSpaceMember) keeps holding,
	// including the acceptance guard that drives all six privileged writes. Only code that
	// needs to tell actor from target asks the narrower question. The consequence for callers
	// is a switch-order rule — a `case errors.Is(err, errNotSpaceMember)` arm would swallow
	// this one, so the actor arm MUST come first (see addMembersHandler).
	errActorNotSpaceMember = fmt.Errorf(
		"project: actor uid is not an active space member: %w", errNotSpaceMember)
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

// canViewMembers is the one place the Space-admin read widening applies, and the split
// between this and listVisibleInSpace (which grants Space admins nothing) is deliberate
// rather than an inconsistency.
//
// The rule the module follows: a Space admin may read a project they can already NAME, and
// may not DISCOVER one. The brief grants them the detail route; the roster hangs off a
// project id they must already hold, so it travels with the detail route. The list route is
// discovery, so it stays closed — widening it would make "unlisted" stop meaning what it says
// on the one route where users read it, and would ship a slice of the P2 admin surface early.
//
// Project membership is not derivable from Space membership either way, which is why this is
// written down once instead of being re-derived per endpoint (PR #841 round 2, P2).
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

// requireSpaceSeatsTx takes every space_member seat lock a write path needs in ONE statement,
// and maps each absence onto the sentinel its subject deserves.
//
// It is the ONLY way this module takes a space_member lock on a write path, which is what lets
// the guard test count call sites per function. Two reasons it revalidates seats at all, both
// established by earlier review rounds:
//
//   - the ACTOR earns the same structural guarantee as the target. The middleware checks Space
//     membership before the transaction, through the shared space:member cache, so that
//     guarantee otherwise depends on another module's cache hygiene — and on a cache whose
//     DEL-failure fallback is best-effort.
//   - the TARGET's absence must be observable inside the transaction, or a Space removal can
//     commit between the check and the write.
//
// One statement, because two sequential row locks on space_member reopen the Error 1213 cycle
// with modules/space's disband scan — see lockSpaceSeatsTx for the mechanism and the
// reproduction. Paths that need only their own seat still go through here, so there is exactly
// one way to take these locks and the guard test can count call sites.
//
// actorUID is always first and always required. others are the target / successor, and their
// absence is TARGET-level: the caller is fine, the subject of the operation is not. Empty
// strings in others are ignored, which is what lets the optional transfer_to be passed
// unconditionally.
func (p *Project) requireSpaceSeatsTx(tx *dbr.Tx, spaceID, actorUID string, others ...string) error {
	_, err := p.lockSeatsTx(tx, spaceID, actorUID, others, nil)
	return err
}

// lockSeatsTx is requireSpaceSeatsTx plus CANDIDATE seats: locked in the same statement, but not
// refused unless the caller establishes the seat is actually needed.
//
// The distinction exists because `transfer_to` is optional. Passing it as a required seat meant
// an ordinary member — or an owner who is not the last one — was refused for naming a successor
// who had left the Space, even though no transfer was going to happen (PR #841 round 4, P2-4).
// Simply moving the check later is not available: the seat lock has to precede the project row
// lock (see lockSpaceSeatsTx), while "is a transfer needed" is only knowable under that lock. So
// the lock happens once, up front, and the REFUSAL happens where the need is established — the
// returned map is how the caller asks later without taking a second lock.
func (p *Project) lockSeatsTx(
	tx *dbr.Tx, spaceID, actorUID string, required, candidates []string,
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
	held, err := p.db.lockSpaceSeatsTx(tx, spaceID, uids)
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
}

// createProject runs createProjectOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) createProject(in createInput) (*Model, error) {
	var model *Model
	err := retryOnLockConflict(func() error {
		var e error
		model, e = p.createProjectOnce(in)
		return e
	})
	return model, err
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
	now := time.Now().UTC()
	dayFrom, dayTo := p.cfg.dayWindow(now)

	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin create: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// I1 for the owner seat, inside this transaction, holding a SHARED lock on the creator's
	// space_member row — and BEFORE the exclusive lock on `space` below. The order of these
	// two is load-bearing in both directions:
	//
	//   * Correctness: createProject writes a membership row like any other write path, so it
	//     owes the same invariant. The middleware's check does not substitute — it ran before
	//     this transaction, against a 60s Redis cache, so a Space removal committing in
	//     between left a permanent owner seat with no Space seat, on the one project nobody
	//     could then clean up (the cascade closes seats, and an ownerless project cannot be
	//     disbanded).
	//
	//   * Deadlock: this used to come SECOND, and that is the exact reversed order
	//     modules/space/db.go:71-88 records as a prior Error 1213 incident. Both Space-disband
	//     paths (disbandSpace, forceDisbandSpace) take `space_member ... FOR UPDATE`
	//     (lockActiveMemberUIDsTx) and THEN update `space`. Holding X(`space`) while waiting
	//     for S(`space_member`) closes the cycle, and unlike the gap-lock case that comment
	//     analyses these are record locks on rows that exist, so the cycle is real. It was
	//     reproduced against MySQL 8.0.33: InnoDB chose the OPERATOR'S DISBAND as the victim
	//     while this create committed — and disband is a step of the member-removal security
	//     cascade, so that is the worse of the two outcomes the comment warns about
	//     (TestCreateProjectDoesNotDeadlockAgainstTheSpaceDisbandLockOrder pins it).
	//
	//     Taking the shared lock first makes both transactions acquire `space_member` before
	//     `space`, so there is no cycle: whichever arrives second simply waits.
	//     lockSpaceSeatRowTx, not lockSpaceSeatsTx: the latter JOINs `space`,
	//     and a table outside the `FOR SHARE OF` list is read as a CONSISTENT read, which
	//     opens the transaction's read view. As the FIRST statement that would freeze the
	//     snapshot before the `space` lock below, and every quota count after it would be
	//     answered from it — six concurrent creates all passed MaxPerSpace=1 that way. The
	//     Space's activeness is rechecked under the exclusive lock immediately below, so
	//     nothing is lost. See lockSpaceSeatRowTx.
	creatorIsMember, err := p.db.lockSpaceSeatRowTx(tx, in.SpaceID, in.Creator)
	if err != nil {
		return nil, err
	}
	if !creatorIsMember {
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

// updateProject runs updateProjectOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) updateProject(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
	var model *Model
	err := retryOnLockConflict(func() error {
		var e error
		model, e = p.updateProjectOnce(projectID, actorUID, spaceID, req)
		return e
	})
	return model, err
}

// updateProject applies a partial profile update under the project row lock.
//
// The allow-list is built here, not from the request payload: active_name and
// is_official must never reach a SET clause, and an allow-list is the only form of
// that guarantee which survives someone later adding a field to updateReq.
func (p *Project) updateProjectOnce(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin update: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
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
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin disband: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
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
		admitted, err := p.addOneFn(projectID, spaceID, actorUID, uid)
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
		if errors.Is(err, errPermissionDenied) || errors.Is(err, errProjectGone) ||
			errors.Is(err, errActorNotSpaceMember) {
			break
		}
	}
	return results, nil
}

// addOneMember runs addOneMemberOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) addOneMember(projectID, spaceID, actorUID, uid string) (bool, error) {
	var admitted bool
	err := retryOnLockConflict(func() error {
		var e error
		admitted, e = p.addOneMemberOnce(projectID, spaceID, actorUID, uid)
		return e
	})
	return admitted, err
}

func (p *Project) addOneMemberOnce(projectID, spaceID, actorUID, uid string) (bool, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin add member: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// The ACTOR's own Space seat, revalidated in-transaction like every other privileged
	// write (see requireSpaceSeatsTx). This path was the one omission, and its exposure
	// is the widest of the six: addMembers runs ONE TRANSACTION PER TARGET and breaks the
	// batch only on errPermissionDenied / errProjectGone, so an actor whose Space seat closes
	// mid-batch would otherwise have every remaining uid of a 200-uid batch admitted and
	// audited under them — the actor's project role stays active because the Space-removal
	// cascade is asynchronous by design.
	// Both seats — the actor's and the target's — in ONE statement, before lockActiveProjectTx.
	// I1 for the target is enforced here rather than before the transaction so a Space removal
	// cannot commit between the check and the write; the actor gets the same guarantee (see
	// requireSpaceSeatsTx), and taking them together is what keeps this path out of the
	// row-level 1213 cycle with the disband scan.
	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID, uid); err != nil {
		return false, err
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
	if existing != nil && existing.Status == MemberStatusActive && existing.Removing == 0 {
		// Already a member: a no-op. Not an error — a batch add of a roster that
		// partially overlaps must be idempotent — and specifically not an epoch bump.
		return false, nil
	}
	// `existing.Removing == 1` deliberately does NOT take the branch above, and
	// getting that wrong is silent.
	//
	// A seat being closed still has status = 1, so the plain status check treated
	// a re-add during the removal window as "already a member" and returned a
	// no-op — never reaching the upsert that clears `removing`, never cancelling
	// the outbox job. The cascade then went ahead and removed a member an admin
	// had just put back, and nothing reported it: the API said OK.
	//
	// D4's rule is that re-admission CANCELS an in-flight cascade, so this path
	// has to run to completion for such a seat: the upsert clears removing, the
	// job is retired, and the epoch moves again because the seat went from
	// closing back to active, which is a membership change from every consumer's
	// point of view.

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
	// D4 — re-admission CANCELS an in-flight cascade rather than being rejected.
	//
	// admitMemberTx already cleared `removing` in the same statement; this
	// retires the outbox job that was going to act on it. Both halves are needed
	// and both are in this transaction: clearing the flag without cancelling the
	// job leaves the worker to pick it up, re-read, find removing = 0 and drop it
	// — burning a lease and an attempt each round and making a cancelled cascade
	// indistinguishable from a stalled one in the queue.
	//
	// Unconditional rather than guarded on `changed`: a re-add that changed
	// nothing (the member was already fully active) can still coexist with a
	// stale pending job from an earlier remove/re-add cycle, and retiring it
	// costs one bounded UPDATE on an indexed key.
	//
	// Why cancel and not reject: the cascade can legitimately run long, and a
	// rejection would make an unrelated admin action fail for as long as it does,
	// with no self-service remedy.
	if _, err := p.db.cancelPendingRemovalJobsTx(tx, projectID, uid, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit add member: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, uid)
	}
	return changed, nil
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
// The target's role is re-read under a row lock inside the transaction, not taken
// from an earlier unlocked read: the transitive-protection rule ("an admin may not
// remove an admin or the owner") is only sound if the role it checks cannot change
// between the check and the write.
func (p *Project) removeMemberOnce(projectID, spaceID, actorUID, targetUID string) (bool, error) {
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

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
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

	// D4 — two-phase close. `removing = 1` goes in with `status` still 1, and the
	// worker flips status only after the groups are detached. See db_removal.go
	// for why the order is inverted: closing the seat first would leave a window
	// where the member is not a member and their group_member rows still exist,
	// which is I2 violated by the removal itself, every time.
	//
	// member_epoch is bumped HERE, not at the worker's final flip, because from
	// every consumer's point of view the membership already changed: the seat
	// stops granting anything the moment removing is set.
	changed, err := p.beginRemovalWithCascadeTx(tx, projectID, spaceID, actorUID, targetUID,
		removalReasonKicked, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit remove member: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, targetUID)
	}
	return changed, nil
}

// leaveProject runs leaveProjectOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) leaveProject(projectID, spaceID, uid, transferTo string) (string, error) {
	var successor string
	err := retryOnLockConflict(func() error {
		var e error
		successor, e = p.leaveProjectOnce(projectID, spaceID, uid, transferTo)
		return e
	})
	return successor, err
}

// leaveProject closes the caller's own seat, transferring ownership first when the
// caller is the last owner.
//
// The transfer and the departure are one transaction. Two transactions would leave
// a window with two owners (if the transfer commits first) or none (if the departure
// does), and the second of those is unrecoverable in P0.
func (p *Project) leaveProjectOnce(projectID, spaceID, uid, transferTo string) (string, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return "", fmt.Errorf("project: begin leave: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// The leaver's seat and, when named, the successor's — in ONE statement, before the project
	// row. Together rather than in sequence: two row locks on space_member reopen the 1213 cycle
	// with the disband scan (see lockSpaceSeatsTx). transferTo is passed unconditionally because
	// the helper ignores the empty string.
	heldSeats, err := p.lockSeatsTx(tx, spaceID, uid, nil, []string{transferTo})
	if err != nil {
		return "", err
	}
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
			// NOW the successor's Space seat matters, and not before: this is the point at which
			// the transfer is established as necessary. The seat was already locked up front
			// (one statement, ahead of the project row), so this is a map lookup rather than a
			// second lock.
			if transferTo != "" && !heldSeats[transferTo] {
				return "", errNotSpaceMember
			}
			if err := p.promoteSuccessorTx(tx, projectID, transferTo, uid, now); err != nil {
				return "", err
			}
			successorPromoted = transferTo
		}
	}

	// Same two-phase close as the remove path (D4). Leaving is self-service, so
	// the operator is the member themself.
	changed, err := p.beginRemovalWithCascadeTx(tx, projectID, spaceID, uid, uid,
		removalReasonLeft, now)
	if err != nil {
		return "", err
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

// changeMemberRole runs changeMemberRoleOnce through the bounded lock-conflict retry; see retryOnLockConflict.
func (p *Project) changeMemberRole(projectID, spaceID, actorUID, targetUID string, role int, transferTo string) (bool, string, error) {
	var changed bool
	var successor string
	err := retryOnLockConflict(func() error {
		var e error
		changed, successor, e = p.changeMemberRoleOnce(projectID, spaceID, actorUID, targetUID, role, transferTo)
		return e
	})
	return changed, successor, err
}

// changeMemberRole sets one member's role, handling the last-owner demotion via the
// same atomic transfer as leaveProject.
func (p *Project) changeMemberRoleOnce(projectID, spaceID, actorUID, targetUID string, role int, transferTo string) (bool, string, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, "", fmt.Errorf("project: begin role change: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	// Every seat this operation depends on, in ONE statement, before the project row.
	//
	// Which seats those are depends on the REQUESTED role, and that condition is the same one
	// the sequential version used: a request that would raise the target to admin or owner is a
	// GRANT, and a grant needs the authorization predicate in-transaction. Only a demotion to
	// RoleCommon skips the target's seat — the operator's escape hatch for a member already on
	// their way out of the Space. The requested role is knowable before any lock; whether the
	// change is really a promotion is not, since that needs the target's current role under the
	// project lock. Pinned by TestPromotionRequiresTargetStillInSpace.
	//
	// One statement rather than two or three: see lockSpaceSeatsTx for the 1213 cycle that
	// sequential seat locks reopen against modules/space's disband scan.
	//
	// The target is REQUIRED when the request grants (it is the subject of the grant); the
	// successor is a CANDIDATE, refused only if the last-owner transfer turns out to be needed —
	// see lockSeatsTx for why naming an irrelevant successor must not refuse the request.
	var required []string
	if role >= RoleAdmin {
		required = append(required, targetUID)
	}
	heldSeats, err := p.lockSeatsTx(tx, spaceID, actorUID, required, []string{transferTo})
	if err != nil {
		return false, "", err
	}
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
			// The successor's Space seat becomes relevant exactly here — see leaveProjectOnce.
			// Already locked in the single up-front statement, so this is a map lookup.
			if transferTo != "" && !heldSeats[transferTo] {
				return false, "", errNotSpaceMember
			}
			if err := p.promoteSuccessorTx(tx, projectID, transferTo, targetUID, now); err != nil {
				return false, "", err
			}
			successorPromoted = transferTo
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

// promoteSuccessorTx promotes a successor whose Space seat was ALREADY verified and locked
// by the caller (requireSpaceSeatsTx, before lockActiveProjectTx) to owner.
//
// The Space-seat check is the point. Checking only for an active PROJECT seat is not enough:
// the Space-removal cascade is asynchronous, so a user removed from the Space keeps their
// project seat until the cleanup job runs. Promoting them hands ownership to a seat that is
// already scheduled for closure — and once the cascade closes it the project has no owner at
// all, which is unrecoverable in P0 (role change and disband are owner-only, and a Space admin
// has read access only). The predicate is the authorization one, so a banned Space does not
// qualify a successor either.
//
// The check itself lives in requireSpaceSeatsTx so the shared space_member lock is
// taken BEFORE the project row — restoring the declared space -> project order on every path
// that needs it. The earlier placement (shared lock taken while holding the project row, with
// a no-cycle argument relying on the removal path taking its project lock only in a LATER
// transaction) was incomplete: InnoDB queues a shared-lock request BEHIND an already-waiting
// exclusive request on the same row, and modules/space does take exclusive space_member locks
// (disband takes a range). A three-way cycle was reachable; this placement removes it instead
// of arguing about it. (yujiawei Q2, PR #841 round 1.)
func (p *Project) promoteSuccessorTx(tx *dbr.Tx, projectID, successorUID, departingUID string, now time.Time) error {
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

// beginRemovalWithCascadeTx performs the seat-closing half of D4's two-phase
// removal inside the caller's transaction: set removing = 1, bump the epoch, and
// enqueue the cascade job.
//
// All three in ONE transaction is the point. A crash between the flag and the
// job would leave a member who is not a member of record and whose group rows
// nobody is coming to clean up — an I2 violation with no worker behind it. That
// is exactly the failure the outbox pattern exists to prevent, and why this is
// not "flip the flag, then enqueue".
//
// Reports whether anything changed. False means the seat was already closing or
// already closed, so the caller must not bump the epoch again or enqueue a
// second job — the idempotence rule P0 established and nearly broke.
func (p *Project) beginRemovalWithCascadeTx(
	tx *dbr.Tx,
	projectID, spaceID, operatorUID, targetUID, reason string,
	now time.Time,
) (bool, error) {
	changed, err := p.db.beginMemberRemovalTx(tx, projectID, targetUID, now)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
		return false, err
	}
	if err := p.db.enqueueRemovalJobTx(tx, RemovalJob{
		ProjectID:   projectID,
		UID:         targetUID,
		SpaceID:     spaceID,
		OperatorUID: operatorUID,
		Reason:      reason,
	}, now); err != nil {
		return false, err
	}
	return true, nil
}
