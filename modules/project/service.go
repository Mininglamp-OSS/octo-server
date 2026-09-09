package project

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	dbpkg "github.com/Mininglamp-OSS/octo-server/pkg/db"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	userpkg "github.com/Mininglamp-OSS/octo-server/pkg/user"
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
// group table at all, so the two middle positions were reserved for P1's group
// admission; recording them was meant to keep P1 from choosing a different order.
//
// P1 chose a different order anyway, and the reservation above is NOT what its
// admission path does — see the corrected account in
// pkg/project.AssertMembersInProjectTx. The funnel takes octo_project_member
// (SHARED) before any group_member row; the group-side handover takes it after.
// A three-way cycle across those two plus this module's exclusive seat writes is
// reachable and was reproduced on MySQL 8.0.46 in PR #846's review.
//
// What holds instead, and what every path here must keep holding: this module
// takes its EXCLUSIVE octo_project_member locks while holding NO group_member
// lock — it holds no group locks at all. That is the half of the invariant that
// lives on this side of the import edge.
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

// txRetryAttempts and the two helpers below are thin aliases over pkg/db, which now
// owns the canonical copy. Kept as package-local names because every call site in this
// file reads `retryOnLockConflict(...)` and the tests assert on `txRetryAttempts`; the
// reasoning that justifies retrying at all moved with the implementation.
const txRetryAttempts = dbpkg.LockRetryAttempts

// retryOnLockConflict re-runs fn while it fails with a TRANSIENT lock conflict.
//
// See pkg/db.RetryOnLockConflict. fn must own its whole transaction: a retry re-runs it
// from BEGIN, which is only sound because a deadlock has already rolled the failed
// attempt back.
func retryOnLockConflict(fn func() error) error { return dbpkg.RetryOnLockConflict(fn) }

// isRetryableTxErr reports whether err is a transient InnoDB lock conflict (1213/1205).
func isRetryableTxErr(err error) bool { return dbpkg.IsRetryableLockErr(err) }

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
		// D15 — an ordinary member holds this and holds nothing else on the
		// member endpoints. Emitted explicitly rather than left for the client to
		// infer from the role number, like every other capability here: a client
		// that re-derives the matrix drifts from the server the first time the
		// matrix changes, and this row is new, so every existing client would
		// derive it wrong.
		CanManageOwnAgents: canManageOwnAgents(projectRole),
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

	// The ACTOR's ACCOUNT liveness, as a SEPARATE read rather than a third join in the
	// statement above.
	//
	// A Space seat does not imply a live account: a super-admin ban writes only the `user` row
	// (modules/user.liftBanUser) and account destroy cascades no membership removal, so a banned
	// or destroyed uid keeps its seat and could keep administering projects.
	//
	// Why not in the locking statement, which is the obvious shape and what both existing
	// precedents do: joining `user` there hands the optimizer the driving table. Measured on
	// 8.0.33, `user` can drive, which locks space_member rows one eq_ref at a time in user-PK
	// order instead of letting InnoDB pick its own order for one IN predicate — the exact
	// property lockSpaceSeatsTx's own comment relies on. The EXPLAIN transcript is there.
	//
	// # The ACTOR only, and this scope is load-bearing
	//
	// The TARGETS' liveness is deliberately NOT enforced here, because each caller already owns
	// that fact and renders it in the shape its own contract requires:
	//
	//   - an AGENT target: classifyAgentsTx carries AccountUsable, spelled with the same
	//     predicate (db_agent.go), and folds it into the single agent refusal D2 requires —
	//     the six ineligibility reasons must not be distinguishable on the wire. Refusing a
	//     deactivated agent here instead would render one of those six as
	//     errNotSpaceMember, i.e. exactly the distinction D2 exists to prevent.
	//   - a HUMAN target: addOneMemberOnce refuses a non-live account on its own, after the
	//     agent classification, so the two branches stay symmetric.
	//
	// Enforcing it for everyone here looked like the tighter choice and was the wrong one: it
	// pre-empted a more specific refusal with a less specific one, and it duplicated a
	// predicate that already exists — which is how two copies of one fact drift.
	// FOLDED on both sides — and that includes `held`, which was exact-match before this
	// and is the half a review looking only at the new liveness code would miss. `held` is
	// keyed by
	// the spelling `space_member` returned and `liveActor` by the spelling `user`
	// returned, and `uid` compares case-insensitively under either collation — so an
	// exact-string intersection can drop a live, seated actor whose two rows differ in
	// case. Fail-closed, but a real caller refused: the same shape this branch treated as
	// a blocker on the read path. pkg/user.ActiveAccounts' own comment prescribes exactly
	// this, and this path was not doing it.
	liveAccounts, err := userpkg.ActiveAccounts(p.db.session, []string{actorUID})
	if err != nil {
		return nil, fmt.Errorf("project: check actor account liveness: %w", err)
	}
	liveActor := make(map[string]bool, len(liveAccounts))
	for uid, ok := range liveAccounts {
		if ok {
			liveActor[projectpkg.FoldID(uid)] = true
		}
	}

	// The ACTOR is checked first, so a caller who has lost their own seat is told that rather
	// than being told something about the target. Their project role may well still be active,
	// because the Space-removal cascade is asynchronous by design.
	//
	// A banned actor lands in the same answer rather than a distinct sentinel: a caller learning
	// "your account is banned" from a project endpoint is an enumeration answer, and the ban is
	// already reported on the paths that own it.
	if !projectpkg.FoldedHas(held, actorUID) || !liveActor[projectpkg.FoldID(actorUID)] {
		return nil, errActorNotSpaceMember
	}
	for _, uid := range required {
		if uid != "" && uid != actorUID && !projectpkg.FoldedHas(held, uid) {
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

	// Provision the all-member group AFTER the transaction commits, and after the
	// retry loop rather than inside it: a lock-conflict retry re-runs
	// createProjectOnce, and a provisioner call inside the closure would run once
	// per attempt, each attempt building a group for a project row that was then
	// rolled back.
	//
	// Failure here does NOT fail the create (D4). The project is the real entity;
	// the group is its attachment, and rolling a committed project back across a
	// module boundary — after the group side may already have created an IM
	// channel — is a worse failure than the one being handled. P1 made the same
	// call for its disband steps, and this keeps the two consistent.
	//
	// The seeded members are the agents only: the creator is added by CreateGroup
	// itself as the group's owner.
	p.provisionAllMemberGroup(model.ProjectID, model.SpaceID, model.Creator, model.Name,
		withoutUID(sanitizeUIDs(in.AgentUIDs), model.Creator))

	// Re-read so the response carries the group number the provisioner just wrote.
	// One extra point read on the create path, and it is what lets the client open
	// the group immediately instead of polling for it.
	if fresh, ferr := p.db.queryByProjectID(model.ProjectID); ferr == nil && fresh != nil {
		model = fresh
	}
	return model, nil
}

// createProject inserts a project and its owner seat in ONE transaction.
//
// All three creation quotas are counted inside that transaction. Counting them
// outside would let two concurrent creates both pass the check and both land, which
// is the whole failure mode a quota exists to prevent.
//
// member_epoch is BUMPED by creation, so a new project lands on 1 rather than on
// the column default of 0.
//
// Creation used to be the one membership write exempted from the bump, on the
// reasoning that it is where the roster comes into existence rather than
// changes. That exemption is what put a real project on a reserved value: the
// integration contract that consumes this column defines 0 as "the project does
// not exist or is not visible", so a solo project nobody had yet added to was
// active, visible, had a real member, and reported the same epoch as a project
// that had been disbanded. A consumer caching an authorization answer under
// epoch 0 kept it forever, because the disbanded project answers 0 too and the
// staleness check therefore agreed. See migration 20260908000002.
//
// Removing the exemption is also the more honest reading of the rule: creation
// writes the owner seat into octo_project_member, which IS a membership write,
// and every membership write bumps the epoch. It is done with the same
// bumpMemberEpochTx every other path uses rather than by seeding the column at
// insert, because member_epoch may only ever be written as member_epoch + 1 —
// a property TestIsOfficialHasNoWriter and TestMemberEpochOnlyEverIncrements
// enforce between them, and one this change deliberately does not weaken.
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
	storedCreator, creatorIsMember, err := p.db.lockSpaceSeatRowTx(tx, in.SpaceID, in.Creator)
	if err != nil {
		return nil, err
	}
	if !creatorIsMember {
		return nil, errNotSpaceMember
	}
	// The creator's uid is rebound to the spelling `space_member` stores, because every
	// octo_* row below denormalises it: octo_project.creator, the owner seat's
	// octo_project_member.uid, and the invite_uid on the creator's agent seats. The epoch
	// step matches that column under utf8mb4_general_ci while this lock resolved it under
	// space_member's looser collation — the same one-hop gap the seat funnel closes on the
	// removal side, here on the admission side.
	in.Creator = storedCreator

	// The agents' Space seats, locked in the SAME position in the lock order as the
	// creator's — before the `space` row, never after (see the deadlock argument above;
	// the order is space_member -> space -> project -> ... -> octo_project_member).
	//
	// lockSpaceSeatRowsTx, NOT lockSpaceSeatsTx. The latter joins `space`, and a table
	// outside its `FOR SHARE OF` list is read as a consistent read that OPENS this
	// transaction's read view — before the `space` lock below, so every quota counted
	// after it would answer from a stale snapshot. That is the defect
	// lockSpaceSeatRowTx exists to avoid and TestCreateDoesNotTakeItsSpaceSeatLockThroughAJoin
	// pins; the first version of this block reopened it, and the guard caught it.
	//
	// One statement rather than one lockSpaceSeatRowTx per uid: the round-trips inside
	// the transaction do not grow with the number of agents, and the rows are taken as a
	// single deterministic set rather than one at a time in request order (which is
	// caller-controlled and therefore a deadlock shape).
	//
	// I1 applies to an agent exactly as it does to a person: an agent with no active
	// Space seat cannot hold a project seat. botfather writes space_member when it mints
	// a bot, so this normally passes; failing it means the bot belongs to another Space,
	// or D14's deletion path has already closed its seat.
	var agentSeats map[string]bool
	agentUIDs := sanitizeUIDs(in.AgentUIDs)
	// Drop the creator BEFORE anything else looks at the list. Naming yourself in
	// agent_uids is nonsense rather than an attack, and leaving it in would make
	// classifyAgentsTx refuse the whole create with reason "not_a_bot" — a refusal
	// whose message would be actively misleading.
	agentUIDs = withoutUID(agentUIDs, in.Creator)
	if len(agentUIDs) > 0 {
		agentSeats, err = p.db.lockSpaceSeatRowsTx(tx, in.SpaceID, agentUIDs)
		if err != nil {
			return nil, err
		}
	}

	// The ACCOUNT half, as a SEPARATE single-table read rather than a join added to either
	// seat-lock statement above.
	//
	// A Space seat does not imply a live account: a super-admin ban writes only the `user`
	// row (modules/user.liftBanUser) and account destroy cascades no membership removal, so
	// a banned or destroyed uid keeps its seat and could create projects indefinitely.
	//
	// Why not join `user` into lockSpaceSeatRowTx / lockSpaceSeatRowsTx, which is the
	// obvious shape: those helpers are JOIN-FREE on purpose. A table outside `FOR SHARE OF`
	// is a consistency read, it would assign this transaction read view here, and all three
	// quota counts below would then answer from a snapshot older than the `space` lock —
	// six concurrent creates all passed MaxPerSpace=1 when that regressed. This is a plain
	// single-table SELECT that adds no table to either locking statement.
	// TestCreateQuotaStillHoldsUnderConcurrency is the regression net.
	//
	// The CREATOR only. The agents' account liveness is NOT checked here, deliberately:
	// classifyAgentsTx below already carries it as AccountUsable, spelled with the same
	// predicate (`u.status = 1 AND COALESCE(u.is_destroy, 0) <> 2`, db_agent.go), and it
	// folds it into the single agent refusal that D2 requires — the six ineligibility
	// reasons are not distinguishable on the wire. Checking it again up here would refuse
	// a deactivated agent as errNotSpaceMember instead, i.e. render one of those six
	// reasons differently from the other five, which is the distinction D2 exists to
	// prevent. Two predicates for one fact is also how they drift.
	//
	// Refused as errNotSpaceMember, not a distinct sentinel: a caller learning "your account
	// is banned" from a project endpoint is an enumeration answer, and the ban is already
	// reported on the paths that own it.
	liveCreator, err := userpkg.ActiveAccounts(p.db.session, []string{in.Creator})
	if err != nil {
		return nil, fmt.Errorf("project: check creator account liveness: %w", err)
	}
	// Folded: the map is keyed by the spelling `user` returned, which the
	// case-insensitive collation lets differ from the one the caller sent.
	if !projectpkg.FoldedHas(liveCreator, in.Creator) {
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
	storedSpaceID, spaceActive, err := p.db.lockSpaceRowTx(tx, in.SpaceID)
	if err != nil {
		return nil, err
	}
	// Every octo_* row below is denormalised with the SPACE row's spelling, not the
	// request's. The epoch step matches octo_project_member on (space_id, uid) under a
	// stricter collation than the one that resolved this lock, so bytes that differ
	// here are bytes the invalidation signal cannot find later.
	if spaceActive {
		in.SpaceID = storedSpaceID
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
	// that can expire, which is the same reason P0 moved the creator's own I1 check in
	// here (see the comment on lockSpaceSeatRowTx above).
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

	// AFTER EVERY roster write in this transaction: the owner seat above and the
	// creator's agent seats with it. The epoch is the invalidation channel for the
	// project's MEMBER SET, so it must be bumped once that set is final. Bumping it
	// before the agent inserts would publish an epoch a consumer could cache against a
	// roster still growing inside this transaction.
	//
	// It runs after the project INSERT rather than before because bumpMemberEpochTx is
	// guarded on status = StatusNormal and would otherwise match no row.
	//
	// And BEFORE the provisioning enqueue below, which the lock order requires to be the
	// last statement in this transaction. This bump takes no new lock — octo_project is
	// already held from the insert above — so it cannot affect that ordering.
	//
	// The affected-row count is CHECKED here and ignored everywhere else, because this is
	// the one call site where a silent no-op is a security state rather than the intended
	// behaviour: it would leave a fresh project on the reserved absent-epoch sentinel
	// while the response below reports 1 — the exact state migration 20260908000002
	// exists to remove. It holds today because the insert above writes StatusNormal in
	// this same transaction; it stops holding the moment a two-phase create (O6) gives a
	// project a non-normal initial status, and this turns that from a silent wrong answer
	// into a failed create.
	bumped, err := p.db.bumpMemberEpochTx(tx, model.ProjectID, now)
	if err != nil {
		return nil, err
	}
	if bumped == 0 {
		return nil, fmt.Errorf(
			"project: create bumped no epoch row for %s; the project would ship on the "+
				"reserved absent-epoch sentinel while reporting 1", model.ProjectID)
	}
	model.MemberEpoch++

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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit create: %w", err)
	}
	p.invalidateProjectMemberCache(model.ProjectID, in.Creator)
	for _, uid := range agentUIDs {
		p.invalidateProjectMemberCache(model.ProjectID, uid)
	}
	// member_epoch stays at 0 (D11): the agents are part of the roster coming into
	// existence, not a change to it. The first real membership write makes it 1.

	// Off the request path, and only after the commit: "eager" should mean seconds,
	// not up to a full interval tick. The interval remains the guarantee — this is
	// just the nudge.
	if p.nudgeProvisioningFn != nil {
		p.nudgeProvisioningFn()
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
	if _, err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
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
	// D4(c) — the rebuild point. If provisioning failed when the project was
	// created, this is where it gets another go, under the CAS lease that keeps
	// two concurrent adds from each building a group.
	//
	// Once per batch, not once per uid: the lease would make the repeats harmless
	// but they would still be N failed CAS round-trips on a 200-uid batch.
	//
	// Before the loop, so that the members added below have a group to be admitted
	// into. It is best-effort — a batch add must not fail because the group could
	// not be built — so the admissions below tolerate its absence.
	//
	// # The latency this puts on the request, and who can now trigger it
	//
	// Synchronous, inside the HTTP request. D15 widened this handler's pre-check
	// from canManageMembers to canManageOwnAgents, so an ORDINARY member adding
	// their own agent can reach it — the trade was argued for project creation,
	// and the caller set got wider in the same change. PR #855s fifth review, Q3.
	//
	// Worst case, once per project and only while it has no usable group: one CAS
	// UPDATE, one roster read of at most max_members + 1 rows, one batched
	// Space-active read over those uids, then CreateGroup — one transaction
	// inserting up to max_members member rows with a GenSeq (Redis) per member,
	// plus one blocking IM channel call — then one write-back UPDATE. At the
	// default cap of 500 that is a few hundred Redis round-trips and one IM call.
	//
	// The budget is the HTTP request: this has to stay inside it with room to
	// spare, and if it ever does not, the answer is to move the rebuild onto the
	// reconcile worker rather than to cap the roster (a capped rebuild is the
	// incomplete-group defect the third round fixed). The 2-minute lease is NOT
	// the budget — it is sized for a process dying mid-provision, so that the next
	// write path can reclaim it; reading it as a latency allowance would be
	// reading a crash timeout as a target.
	//
	// What keeps it bounded meanwhile: it is reachable only for a project whose
	// provisioning already failed or whose group was detached, the lease
	// serialises concurrent triggers so only one caller pays, and a failure does
	// not fail the add. It has not been measured at a full 500-member roster;
	// open_verification carries that.
	allMemberGroupNo := p.ensureAllMemberGroup(projectID, spaceID)
	if allMemberGroupNo == "" {
		// 整批人都不会进群。补建自己失败时那一路已经记过原因；这里补的是它**没跑**
		// 的那种情况——另一个写路径握着租约，认领 CAS 影响 0 行就直接返回，此前
		// 什么都不记。一条批次级的日志，而不是逐 uid 一条。
		p.Warn("项目此刻没有全员群，本批加人不会进群（补建失败或正被他人认领），由 I4 扫描 B 报出",
			zap.String("projectId", projectID), zap.String("spaceId", spaceID),
			zap.Int("uids", len(uids)))
	}

	results := make([]addMemberResult, 0, len(uids))
	for _, uid := range uids {
		admitted, err := p.addOneFn(projectID, spaceID, actorUID, uid)
		results = append(results, addMemberResult{UID: uid, Admitted: admitted, Err: err})
		if err == nil {
			// D12 — the seat is committed, so put them in the all-member group.
			//
			// AFTER the per-target transaction commits, never inside it: the
			// admitter opens its own transaction in modules/group and then makes a
			// blocking IM call. Holding the project seat's transaction across that
			// would put a network round-trip inside a row lock.
			//
			// Best-effort by design (see admitAllMemberGroup): the seat is the
			// authorization fact and the group is its projection. Failing the add
			// because WuKongIM hiccuped would report failure for something that
			// actually succeeded.
			//
			// # On err == nil rather than admitted && err == nil
			//
			// The only way to get (false, nil) out of addOneMemberOnce is "already
			// an active member" — an idempotent no-op on the SEAT. It is not a
			// no-op on the projection, and gating the admission on `admitted` made
			// it one: an I4 scan-B gap (seat present, group row missing) is
			// precisely the state where the seat write has nothing to do, so the
			// repair the brief documents for that gauge — an admin re-adds the
			// member — did nothing at all, quietly, and the gauge stayed red.
			//
			// The cost is one idempotent admitter round-trip per already-member in
			// a batch. That is the price of the projection being self-healing, and
			// it is paid only on a roster that overlaps what is already there.
			p.admitAllMemberGroup(projectID, spaceID, allMemberGroupNo, uid)
		}
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
	held, err := p.lockSeatsTx(tx, spaceID, actorUID, []string{uid}, nil)
	if err != nil {
		return false, err
	}
	// The uid that gets WRITTEN is the one `space_member` stores, not the one the
	// caller sent. `held` is keyed by the database's spelling, and the two can differ
	// under space_member's case- and accent-insensitive collation.
	//
	// This is the admission half of the identity rule modules/space/seatref.go states
	// for the removal half: both tables must end up holding the same bytes, because
	// `octo_project_member` is pinned to a STRICTER collation than `space_member` and
	// a seat written under one spelling can be unreachable from a query that resolved
	// the person through the other. Relying on general_ci's case-insensitivity to
	// bridge them works today only because FoldID is ASCII-only and refuses every
	// spelling that actually diverges — an argument that spans two packages and breaks
	// silently if FoldID is ever widened.
	//
	// The lookup cannot miss here: lockSeatsTx already refused unless it hit.
	seatUID, ok := projectpkg.FoldedLookup(held, uid)
	if !ok {
		return false, errNotSpaceMember
	}
	uid = seatUID

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

	// D15 — an AI agent is admitted under DIFFERENT rules from a person.
	//
	// Before this, members/add had no rule about bots at all: any admin could seat
	// any bot that happened to hold a Space seat, including one belonging to
	// somebody who is not in the project, while an ordinary member had no way to
	// bring their own. Both halves contradict what the create dialog promises
	// ("only your own agents, they join on confirm"), and the second half is what
	// makes D13 lopsided — agents follow their owner OUT with no matching way in.
	//
	// So: an agent may be seated only by its OWN owner, and only while that owner
	// is an active member of this project. An admin cannot do it on someone
	// else's behalf — the person being represented neither agreed nor knows.
	//
	// canManageOwnAgents is what an ordinary member holds here. It is deliberately
	// narrower than canManageMembers: it authorizes exactly "my own agents".
	// ORDER MATTERS, and getting it wrong builds an oracle.
	//
	// The first version asked "is this an agent, and may the actor seat it?" BEFORE
	// checking permission at all. An ordinary member could then tell a live bot
	// owned by someone else (200 with a per-uid agent_not_eligible) from a human or
	// an unknown uid (actor-level permission_denied) — which is exactly the
	// distinction ErrProjectAgentNotEligible was registered to hide, handed to the
	// least privileged caller there is.
	//
	// So: decide OWNERSHIP first, because "my own agent" is the only thing an
	// ordinary member may add; then apply the permission gate; and only for a
	// caller who passed it does the agent-specific refusal become visible.
	// The predicate is D2's, not a looser one. PR #855's review found that this
	// path decided on creator_uid alone while create ran the full rule, and the two
	// disagreements were not symmetric:
	//
	//   - a SELF-HOSTED agent that create refuses was accepted here, from the same
	//     user. Product inconsistency: the brief's reason for excluding it is that a
	//     user cannot see it in the picker, and "cannot see it but can name it in a
	//     request" is the gap that sentence is about.
	//   - a DISABLED or ORPHANED bot (user.robot = 1 with robot.status = 0, or no
	//     robot row) read as "not an agent" and fell through to the HUMAN branch, so
	//     an admin could seat it as an ordinary member. That one is not cosmetic: it
	//     is then permanently outside D13, because queryOwnedAgentSeatsTx requires
	//     r.status = 1, so no owner's departure ever reclaims the seat and no cascade
	//     revisits an active one.
	//
	// So: `IsBot` decides WHICH BRANCH (it is the `user`.robot bit, independent of
	// the robot row), and the full eligibility rule decides whether the own-agent
	// branch applies.
	class, err := p.db.queryAgentClassTx(tx, uid)
	if err != nil {
		return false, err
	}
	isAgentTarget := class.IsBot || spacepkg.IsSystemBot(uid)
	isOwnAgent := class.IsBot &&
		class.AccountUsable &&
		!spacepkg.IsSystemBot(uid) &&
		class.OwnerUID != "" &&
		class.OwnerUID == actorUID &&
		class.Hosting != agentHostingSelfHosted
	if isOwnAgent {
		// D15 — the narrow capability, held by any active project member.
		if !canManageOwnAgents(actorRole) {
			return false, errPermissionDenied
		}
	} else {
		// Everything else — a person, an unknown uid, somebody else's agent, or an
		// agent of the actor's that D2 refuses — needs the ordinary
		// member-management right. A caller without it gets the SAME answer for all
		// of them, so nothing is learned about the target. That uniformity is why
		// the ineligible-own-agent case lands here rather than getting its own
		// refusal above the gate.
		if !canManageMembers(actorRole) {
			return false, errPermissionDenied
		}
		if isAgentTarget {
			// A privileged caller naming a bot that is not their own eligible agent:
			// somebody else's, a disabled or orphaned one, a self-hosted one, or a
			// system bot (exempt from project membership by design, so a seat for it
			// is meaningless and collides with that exemption).
			//
			// One refusal for all of them, matching create. Distinguishable from a
			// human only by someone who could already enumerate the roster and add
			// arbitrary members, so it is not the oracle the ordering above avoids.
			p.Warn("加成员：目标不是调用方的合格分身，拒绝",
				zap.String("projectId", projectID), zap.String("actor", actorUID),
				zap.String("target", uid), zap.String("owner", class.OwnerUID),
				zap.String("hosting", class.Hosting),
				zap.Bool("accountUsable", class.AccountUsable),
				zap.Bool("systemBot", spacepkg.IsSystemBot(uid)))
			return false, errAgentNotEligible
		}
	}

	// A HUMAN target's account liveness. The agent branch above already covered bots
	// through classifyAgentsTx's AccountUsable — same predicate, but folded into the
	// single agent refusal D2 requires — so this is the other half of that split, and
	// it keeps the two branches symmetric rather than leaving humans ungated.
	//
	// Why it matters even though the READ path already conjoins liveness: an admission
	// bumps member_epoch, so a peer re-verifies and is served the answer as a FRESH one.
	// Without this, the read gate holds only until someone re-adds a banned account.
	//
	// AFTER the permission gate and the agent classification, so a caller without
	// member-management rights learns nothing about the target — the same ordering
	// argument the agent branch above makes.
	//
	// errNotSpaceMember, the target-level sentinel: from the caller's side "this uid
	// cannot be seated here" is the whole truth, and naming the ban would be an
	// enumeration answer about somebody else's account.
	liveTarget, err := userpkg.ActiveAccounts(p.db.session, []string{uid})
	if err != nil {
		return false, fmt.Errorf("project: check target account liveness: %w", err)
	}
	// Folded, same reason as the actor check in lockSeatsTx.
	if !projectpkg.FoldedHas(liveTarget, uid) {
		p.Warn("加成员：目标账号不可用（已禁用 / 已注销），拒绝",
			zap.String("projectId", projectID), zap.String("actor", actorUID),
			zap.String("target", uid))
		return false, errNotSpaceMember
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
		// row.SpaceID, not the request's: the seat belongs to this project, so its
		// denormalised space_id has to be the project row's own bytes or the epoch
		// step's `WHERE space_id = ?` enumeration cannot reach it.
		SpaceID:   row.SpaceID,
		Role:      RoleCommon,
		InviteUID: actorUID,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return false, err
	}
	if changed {
		if _, err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
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

	// D15 — removing an AI agent is authorized differently from removing a person.
	//
	// Symmetric with the admission rule, and it has to be: an ordinary member who
	// can bring their own agent in must be able to take it back out, or the only
	// way to undo their own action is to ask an admin. An admin keeps the wider
	// power — they may remove ANY agent, exactly as they may remove any member —
	// so this only ever widens, never narrows.
	//
	// Only the WIDENING half is asked here, so the predicate is deliberately the
	// narrow one: an active robot row owned by the actor. A person, an unknown uid
	// and a bot whose ROBOT ROW is disabled all read as "not the actor's agent" and
	// fall through to the unchanged canManageMembers gate — which is the right
	// answer for removal. Widening on a disabled robot row would let an ordinary
	// member act on a seat D2 says they could never have created; leaving it to an
	// admin costs nothing, because removal is always available to one.
	//
	// A DEACTIVATED OR DESTROYED USER ACCOUNT is the one case where this path
	// deliberately differs from the add path. `account_usable` gates the add
	// (D2 keeps eligibility in step with the directory); it is not asked here, so
	// an ordinary member can still take their own agent's seat back after the
	// account is gone. That asymmetry is the point — the seat is the thing being
	// cleaned up, and requiring an admin for it would strand exactly the seats
	// nobody can see any more. The previous comment claimed a symmetry across all
	// three cases, which stopped being true when account_usable was added.
	// PR #855's fifth review, Q5.
	class, err := p.db.queryAgentClassTx(tx, targetUID)
	if err != nil {
		return false, err
	}
	ownsTargetAgent := class.IsBot && class.OwnerUID != "" && class.OwnerUID == actorUID &&
		!spacepkg.IsSystemBot(targetUID)
	if ownsTargetAgent {
		if !canManageOwnAgents(actorRole) {
			return false, errPermissionDenied
		}
	} else if !canManageMembers(actorRole) {
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
	// canActOnTargetRole implements the transitive protection between PEOPLE: an
	// admin may not act on a peer admin or the owner, and an ordinary member may
	// not act on anyone. That last clause would also block D15's own-agent
	// removal, since an ordinary member is exactly who owns an agent — so the
	// own-agent case bypasses it.
	//
	// Narrowed to RoleCommon deliberately. Nothing seats an agent above
	// RoleCommon today, but changeMemberRole takes a uid and does not ask whether
	// it is a bot; if an agent ever ends up holding admin, "it is mine" must stop
	// being sufficient — otherwise an ordinary member could remove a project
	// administrator by virtue of having minted it.
	ownAgentBypass := ownsTargetAgent && target.Role == RoleCommon
	if !ownAgentBypass && !canActOnTargetRole(actorRole, target.Role) {
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
		// D6 — removing a member can remove an OWNER (an admin may not, but an
		// owner may remove a co-owner), and if that owner held the all-member
		// group, the group is now owned by somebody who is not a project owner.
		// Under D7 that person can neither transfer, leave nor disband it, so the
		// group would be stuck with an owner nobody can change.
		//
		// The hook is self-deciding and idempotent, so it is called on every
		// successful removal rather than only when the target was an owner:
		// establishing "was this an owner" here would mean re-reading a role the
		// transaction already discarded.
		p.syncAllMemberGroupOwner(projectID)
	}
	return changed, nil
}

// leaveProject runs leaveProjectOnce through the bounded lock-conflict retry, then
// syncs the all-member group owner when the leave promoted a successor (D6).
func (p *Project) leaveProject(projectID, spaceID, uid, transferTo string) (string, error) {
	var successor string
	err := retryOnLockConflict(func() error {
		var e error
		successor, e = p.leaveProjectOnce(projectID, spaceID, uid, transferTo)
		return e
	})
	// On EVERY successful leave, not only when a successor was promoted.
	//
	// The narrower version missed the common case: an owner who is not the last
	// one leaves, so no transfer is needed and no successor is named — but if they
	// held the all-member group, it is now owned by an ex-member. P1's cascade
	// does hand the group over on its way out, and that is exactly the problem:
	// it picks by GROUP seniority, which can land on an ordinary project member,
	// and D7 then forbids that person from transferring, leaving or disbanding it.
	//
	// Running this as well is not a racing second answer, because the two do not
	// answer the same question: the cascade picks a group member, this picks a
	// project OWNER, and this one is idempotent and self-deciding, so whichever
	// runs last converges on "the creator is an active project owner".
	if err == nil {
		p.syncAllMemberGroupOwner(projectID)
	}
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
	// A member already on their way out cannot leave a second time; the first
	// removal owns the seat and its cascade.
	if self == nil || self.Status != MemberStatusActive || self.Removing != 0 {
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
			if transferTo != "" && !projectpkg.FoldedHas(heldSeats, transferTo) {
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
	changed, closedAgents, err := p.beginRemovalWithAgentsTx(tx, projectID, spaceID, uid, uid,
		removalReasonLeft, now)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("project: commit leave: %w", err)
	}
	if changed {
		p.invalidateProjectMemberCache(projectID, uid)
		// D13 — see removeMemberOnce. The actor is the leaver: they closed their
		// own seat, and the agents followed.
		for _, agentUID := range closedAgents {
			p.invalidateProjectMemberCache(projectID, agentUID)
			p.audit(auditMemberRemove, uid, agentUID, projectID, spaceID,
				auditReasonAgentFollowsOwner)
		}
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
	// D6 — a role change can move ownership, so re-check that the all-member
	// group is still owned by an active project owner.
	//
	// Called on any successful change rather than only on promotions to owner,
	// because a DEMOTION is the dangerous direction: the group creator being
	// demoted out of owner is exactly how the group ends up with an owner who,
	// under D7, can neither disband it, leave it, nor hand it on. The hook
	// decides for itself and no-ops when nothing is wrong, so calling it
	// unconditionally costs one point read.
	//
	// After the retry loop: a lock-conflict retry re-runs the transaction, and a
	// transfer inside the closure would fire once per attempt.
	if err == nil && (changed || successor != "") {
		p.syncAllMemberGroupOwner(projectID)
	}
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
	// See actorRoleTx: a closing seat is not a member, so it cannot be granted or
	// stripped of a role either.
	if target == nil || target.Status != MemberStatusActive || target.Removing != 0 {
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
			if transferTo != "" && !projectpkg.FoldedHas(heldSeats, transferTo) {
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
		if _, err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
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
	// The successor must not be a seat that is CLOSING, and this is the sharpest
	// case of the rule rather than another instance of it. countActiveOwnersTx
	// already excludes removing = 1, so promoting one satisfies the last-owner
	// guard while leaving the project with zero owners the moment the cascade
	// finishes — the exact outcome that guard exists to prevent, reached through
	// the guard itself. Nothing in P0 or P1 can promote a member without an owner,
	// so the project would be unmanageable with no path back.
	if successor == nil || successor.Status != MemberStatusActive || successor.Removing != 0 {
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

	// bumpMemberEpochTx returns the affected-row count now; only createProject checks it (a silent no-op there would ship a project on the absent sentinel). Here the seat write above already established the row exists.
	if _, err := p.db.bumpMemberEpochTx(tx, projectID, now); err != nil {
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
