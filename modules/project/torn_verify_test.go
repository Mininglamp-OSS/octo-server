package project

// What this file pins:
//
//	A `_verify` answer must never be a DENIAL stamped with a LIVE epoch.
//
// # The defect
//
// ProjectMemberships ran its four narrowing reads on a plain autocommit session, so the
// steps shared no snapshot. Measured on MySQL 8.0.33 (default REPEATABLE-READ): a read
// inside a transaction keeps the view established by its first read, and a read outside
// one does not.
//
// So a Space ban committing mid-request tore the answer:
//
//  1. Step 1 stamps epoch E through ProjectEpochsInSpace, whose IsActiveSpace fold sees
//     the Space as active.
//  2. The ban commits. Its ONLY write is one UPDATE of the `space` row
//     (modules/space.updateSpaceStatus) — no seat write, no epoch bump.
//  3. Step 3's space half INNER JOINs `space ... AND s.status = 1`, observes the ban,
//     and drops every seated uid.
//
// The response is `member:false` for everyone beside `member_epoch: E`, and the peer
// keys its cached DENIAL on E. Unbanning moves only the `space` row, so the epoch
// channel answers E again and the peer's staleness check agrees with its own denial.
// Everyone queried inside that window stays locked out until some unrelated membership
// change happens to bump that project — unbounded in a quiet project.
//
// # Why the fold did not already cover it
//
// Round 5 closed the STEADY-state directions: a banned Space folds to the absent
// sentinel, so `0 != E` breaks agreement, and unbanning gives `E != 0`. That holds for
// answers computed while the fold can see the ban. An answer torn ACROSS the ban commit
// carries the denial beside the live epoch, and the unban restores exactly that key.
//
// Step 3's own comment said reading the Space last "can at worst deny someone who was
// re-admitted microseconds ago, and they re-verify". True on the removal axis, because
// re-admission bumps the epoch and that is what fires the re-verify. False on the
// ban/unban axis: unban bumps nothing, so no trigger ever fires.
//
// # Why the test drives the tear through a hook
//
// The interleaving is a race. Reproducing it by timing would make this test
// probabilistic, and a probabilistic test for a defect this branch has already shipped
// three vacuous guards against is worth very little. The hook commits the ban at exactly
// the point the old code tore, from a SEPARATE connection so the commit is real.

import (
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyNeverStampsADenialWithALiveEpoch stages a Space ban between the epoch read
// and the narrowing reads, and asserts the answer that comes out is internally
// consistent.
//
// The assertion is deliberately NOT "member must be true". What must hold is that the
// answer and the epoch describe the same instant: either the answer still reflects the
// Space the epoch was read from (member:true at E — the peer's next `epochs` poll sees
// the ban through the fold, breaks agreement, and re-verifies), or the epoch is the
// absent sentinel. A denial at a live epoch is the one combination with no bound on it.
func TestVerifyNeverStampsADenialWithALiveEpoch(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "tornOwner")
	seedSpaceMember(t, spaceA, "tornOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "torn-verify")

	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	live := epochs[projectpkg.FoldID(created.ProjectID)]
	require.NotZero(t, live, "baseline: an active project in an active Space has a real epoch")

	// The ban lands exactly where the answer used to tear, committed on its own
	// connection so it is visible to anything not holding an earlier snapshot.
	banned := false
	restore, err := projectpkg.SetMembershipTearHookForTest(func() {
		if banned {
			return
		}
		banned = true
		_, execErr := testCtx.DB().Exec(
			"UPDATE `space` SET status = 0, updated_at = NOW() WHERE space_id = ?", spaceA)
		require.NoError(t, execErr)
	})
	require.NoError(t, err)
	defer restore()

	epoch, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"tornOwner"})
	require.NoError(t, err)
	require.True(t, banned, "the hook must have run, or this case proves nothing")

	_, isMember := roles[projectpkg.FoldID("tornOwner")]
	denialAtLiveEpoch := !isMember && epoch == live
	assert.False(t, denialAtLiveEpoch,
		"a DENIAL must never be stamped with the epoch that was read while the Space was "+
			"still active (got member=%v, epoch=%d, live=%d).\n\n"+
			"The peer keys its cached decision on (uid, project, member_epoch). Unbanning "+
			"writes only the `space` row — no seat, no bump — so the epoch channel answers "+
			"this same value again and the peer's staleness check agrees with its own cached "+
			"denial. Every uid queried inside the torn window stays locked out until some "+
			"unrelated membership change happens to bump that project, which in a quiet "+
			"project is never.",
		isMember, epoch, live)
}

// TestVerifyStepsShareOneSnapshot is the mechanism the assertion above rests on.
//
// Stated separately because the test above would also pass if ProjectMemberships had
// simply stopped reading the Space — which would be a much worse fix. This one shows
// the answer is consistent BECAUSE the steps see one instant, not because a step was
// removed: the seat is still reported, and it is reported through the same space-half
// read that observes bans in the steady state.
func TestVerifyStepsShareOneSnapshot(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "snapOwner")
	seedSpaceMember(t, spaceA, "snapOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "torn-snapshot")

	restore, err := projectpkg.SetMembershipTearHookForTest(func() {
		_, execErr := testCtx.DB().Exec(
			"UPDATE `space` SET status = 0, updated_at = NOW() WHERE space_id = ?", spaceA)
		require.NoError(t, execErr)
	})
	require.NoError(t, err)
	defer restore()

	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"snapOwner"})
	require.NoError(t, err)
	assert.Contains(t, roles, projectpkg.FoldID("snapOwner"),
		"the narrowing reads must see the Space the epoch was read from. A ban that commits "+
			"after the first read belongs to the NEXT answer, not to this one — and the peer "+
			"learns about it from the epoch channel, whose IsActiveSpace fold turns the "+
			"project absent and breaks agreement.")

	// And the steady state still denies, so the space half is genuinely still consulted.
	_, roles, err = projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"snapOwner"})
	require.NoError(t, err)
	assert.NotContains(t, roles, projectpkg.FoldID("snapOwner"),
		"a call that STARTS after the ban must deny — otherwise the snapshot fix would have "+
			"been a removal of the Space conjunction rather than a linearisation of it")
}
