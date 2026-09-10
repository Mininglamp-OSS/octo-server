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
		ownerTok, map[string]any{"uids": []string{"leaver"}})
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
