package project

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCascade invokes the cleanup step directly with the removal the Space module would
// have enqueued. Calling the step rather than driving the whole worker keeps the
// assertions about THIS step; the worker's own contract (lease, backoff, batch
// isolation) is modules/space's and is covered by its tests, plus the external
// non-regression case in project_external_test.go.
func runCascade(t *testing.T, p *Project, spaceID, uid, operator, reason string) error {
	t.Helper()
	return p.cleanupSpaceMemberProjects(testCtx, spacemod.MemberRemoval{
		SpaceID:     spaceID,
		UID:         uid,
		OperatorUID: operator,
		Reason:      reason,
	})
}

// TestCascadeDeactivatesEverySeatAndIsIdempotent covers the reverse half of I1, plus the
// property the whole retry design depends on: rerunning the step produces the same final
// state, no error, and NO second epoch bump.
//
// The epoch part is not incidental. The step is re-executed on every job retry, so an
// unconditional bump would inflate the epoch on no-op reruns and break the "a no-op does
// not change the epoch" rule that every consumer's cache is keyed on.
func TestCascadeDeactivatesEverySeatAndIsIdempotent(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, first := projectWithMembers(t, srv, "leaver")

	// A second project in the same Space, so the cascade is proven to sweep all of them
	// rather than just the first one it finds.
	second := createProjectVia(t, srv, spaceA, ownerTok, "second")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+second.ProjectID+"/members/add",
		ownerTok, addMembersPayload("leaver"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	epochFirstBefore := epochOf(t, first.ProjectID)
	epochSecondBefore := epochOf(t, second.ProjectID)

	// The Space seat is gone; no cleanup job exists (the worker's own enqueue path is
	// modules/space's concern).
	removeSpaceMember(t, spaceA, "leaver")

	require.NoError(t, runCascade(t, p, spaceA, "leaver", "owner1", spacemod.MemberRemoveReasonKicked))

	for _, projectID := range []string{first.ProjectID, second.ProjectID} {
		member, err := p.db.queryMember(projectID, "leaver")
		require.NoError(t, err)
		require.NotNil(t, member)
		assert.Equal(t, MemberStatusRemoved, member.Status, "project %s", projectID)
	}
	epochFirstAfter := epochOf(t, first.ProjectID)
	epochSecondAfter := epochOf(t, second.ProjectID)
	assert.Greater(t, epochFirstAfter, epochFirstBefore)
	assert.Greater(t, epochSecondAfter, epochSecondBefore)

	// Rerun: same final state, no error, and crucially no further epoch movement.
	require.NoError(t, runCascade(t, p, spaceA, "leaver", "owner1", spacemod.MemberRemoveReasonKicked))
	assert.Equal(t, epochFirstAfter, epochOf(t, first.ProjectID),
		"a cascade rerun must not bump the epoch again")
	assert.Equal(t, epochSecondAfter, epochOf(t, second.ProjectID),
		"a cascade rerun must not bump the epoch again")
}

// TestCascadeSkipsBannedSpace is the CheckMembershipForCleanup vs CheckMembership
// distinction, and it is the single most consequential line in the step.
//
// In a banned Space (status=2) the member still holds their seat, so cleanup must SKIP. A
// step written against CheckMembership — which requires status=1 — would deactivate every
// project membership in the Space the moment it was banned, and un-banning would not
// restore them, because nothing re-adds a seat.
func TestCascadeSkipsBannedSpace(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")
	epochBefore := epochOf(t, created.ProjectID)

	setSpaceStatus(t, spaceA, 2)
	require.NoError(t, runCascade(t, p, spaceA, "m1", "owner1", spacemod.MemberRemoveReasonKicked))

	member, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusActive, member.Status,
		"banning a Space must not deactivate any project seat")
	assert.Equal(t, epochBefore, epochOf(t, created.ProjectID),
		"a skipped cascade must not move the epoch")

	// Un-banning needs no repair: the seat was never touched.
	setSpaceStatus(t, spaceA, 1)
	member, err = p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	assert.Equal(t, MemberStatusActive, member.Status)
}

// TestCascadeProceedsForDisbandedSpace pins the other side of that one axis: a DISBANDED
// Space (status=0) does not count as holding a seat, so cleanup proceeds. A surviving
// space_member row there is a join-vs-disband orphan, and skipping would leave the
// project seat alive in a Space that no longer exists.
func TestCascadeProceedsForDisbandedSpace(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")

	// Disband the Space while leaving the space_member row active — exactly the orphan
	// shape the relaxed predicate must still clean up.
	setSpaceStatus(t, spaceA, 0)
	require.NoError(t, runCascade(t, p, spaceA, "m1", "owner1", spacemod.MemberRemoveReasonSpaceDisbanded))

	member, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusRemoved, member.Status,
		"a disbanded Space must not stop the cascade")
}

// TestCascadeSkipsWhenMemberRejoined covers the stale-job case: the job may sit in
// backoff while the member rejoins, and tearing their seats down then would be the real
// fault.
func TestCascadeSkipsWhenMemberRejoined(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")
	epochBefore := epochOf(t, created.ProjectID)

	// The space_member row is still active, i.e. they rejoined before the step ran.
	require.NoError(t, runCascade(t, p, spaceA, "m1", "owner1", spacemod.MemberRemoveReasonKicked))

	member, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	assert.Equal(t, MemberStatusActive, member.Status)
	assert.Equal(t, epochBefore, epochOf(t, created.ProjectID))
}

// TestCascadeIsNoOpWithNothingToDo pins the contract clause "decide nothing to do
// yourself and return nil". Returning an error here would keep the whole shared job being
// re-claimed for a member who has no project seats at all.
func TestCascadeIsNoOpWithNothingToDo(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "ghost")
	// No space_member row at all, and no project seats.
	require.NoError(t, runCascade(t, p, spaceA, "ghost", "someone", spacemod.MemberRemoveReasonKicked))
}

// TestCascadeSkipsDisbandedProjects pins that a disbanded project needs nothing done:
// disbandProject already closed its seats, and the step must not error on it.
func TestCascadeSkipsDisbandedProjects(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")

	w := doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	removeSpaceMember(t, spaceA, "m1")
	require.NoError(t, runCascade(t, p, spaceA, "m1", "owner1", spacemod.MemberRemoveReasonKicked))
}

// TestCascadeStepIsRegisteredUnderItsName pins that constructing the module registers the
// step, and that it is registered under the name the reconcile alerting and the
// non-regression test both refer to.
//
// Registration happens in New() rather than Route(): the very first thing createProject
// does is write an owner seat, so octo_project_member has active rows from the moment the
// module is loaded, and I1's reverse direction has to already exist by then.
func TestCascadeStepIsRegisteredUnderItsName(t *testing.T) {
	require.Equal(t, "project_member", spaceMemberRemovalStepName)

	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "ghost")

	// The registry is name-keyed and latest-wins. Substitute a probe under the step's
	// name, confirm the registry accepts it, then restore the real step so later cases in
	// this package are unaffected.
	probeRan := false
	spacemod.RegisterMemberRemovalCleanupStep(spaceMemberRemovalStepName,
		func(_ *config.Context, _ spacemod.MemberRemoval) error {
			probeRan = true
			return nil
		})
	t.Cleanup(func() {
		spacemod.RegisterMemberRemovalCleanupStep(spaceMemberRemovalStepName, p.cleanupSpaceMemberProjects)
	})

	require.NoError(t, runCascade(t, p, spaceA, "ghost", "someone", spacemod.MemberRemoveReasonKicked))
	assert.False(t, probeRan, "runCascade calls the step directly, so the probe proves "+
		"registration is accepted rather than that the worker dispatched it")
}

// TestSpaceRemovalPreservesOwnerAndClosesAgentRiders exercises the production
// all-Space removal entry point rather than a direct project-seat update. The
// human Owner row is deliberately retained, while a non-Owner bot seated by
// that Owner must still be removed from the Project.
func TestSpaceRemovalPreservesOwnerAndClosesAgentRiders(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "cascade-owner")
	seedSpaceMember(t, spaceA, "cascade-owner", 0, 1)
	seedAgent(t, spaceA, "cascade-owner-bot", "cascade-owner", "octo_hosted")

	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", ownerToken,
		map[string]any{
			"name":       "owner rider cascade",
			"agent_uids": []string{"cascade-owner-bot"},
		})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	created := decodeResp(t, w)
	epochBefore := epochOf(t, created.ProjectID)

	closed, err := spacemod.CloseAllSpaceSeats(
		testCtx, "cascade-owner", "cascade-operator", spacemod.MemberRemoveReasonForceRemoved)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{spaceA}, closed)

	// CloseAllSpaceSeats is the real Space-side entry point. Its asynchronous
	// worker may already have run the project step; the direct call is idempotent
	// and makes this test deterministic about the project-side contract.
	require.NoError(t, runCascade(t, p, spaceA, "cascade-owner", "cascade-operator",
		spacemod.MemberRemoveReasonForceRemoved))
	p.runRemovalCascade()
	epochAfter := epochOf(t, created.ProjectID)
	require.Greater(t, epochAfter, epochBefore,
		"closing the Owner's rider seats must move the project epoch")

	// The delta is not pinned to exactly 1 any more, and that is a real change rather
	// than a loosened assertion.
	//
	// A force-removal now moves the epoch TWICE, at two different times, because two
	// different membership facts change: the Space seat closing is published
	// synchronously inside the Space transaction (bumpEpochsOnSeatTransition — this is
	// the invalidation signal the internal membership endpoints depend on, and an
	// asynchronous-only signal with a terminal abandoned state is no bound at all), and
	// the rider agent seat closing is published by the cascade when it actually closes.
	// Counting those as one would mean a consumer that re-verified between them cached
	// an answer under an epoch that then never moved again.
	//
	// What the test still pins strictly is the property this case exists for: IDEMPOTENCE.
	// A second cascade pass has neither an active rider nor a human seat to close, so it
	// must move nothing — a re-run bump would inflate the epoch on every worker retry and
	// break "a no-op does not change the epoch", which is the rule clients cache against.
	require.NoError(t, runCascade(t, p, spaceA, "cascade-owner", "cascade-operator",
		spacemod.MemberRemoveReasonForceRemoved))
	p.runRemovalCascade()
	require.Equal(t, epochAfter, epochOf(t, created.ProjectID),
		"a second cascade pass has nothing to close and must not move the epoch again")

	owner := memberRow(t, created.ProjectID, "cascade-owner")
	require.NotNil(t, owner)
	assert.Equal(t, MemberStatusActive, owner.Status)
	assert.Zero(t, owner.Removing)
	assert.Equal(t, RoleOwner, owner.Role)

	bot := memberRow(t, created.ProjectID, "cascade-owner-bot")
	require.NotNil(t, bot)
	assert.Equal(t, MemberStatusRemoved, bot.Status)
	assert.Zero(t, bot.Removing)
}
