package project

import (
	"errors"
	"net/http"
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engine coverage for pkg/project's two batch statements, driven from the module
// that owns the tables.
//
// pkg/project has no database harness of its own — it is a predicate package over
// a *dbr.Session — and its statements had zero engine coverage in any lane while
// the PR body claimed "the queries themselves are exercised by CI". This file is
// that lane: real schema, real rows, the same statements the peer endpoints run.
//
// It lives here rather than in modules/internal_membership because the fixtures
// (Space, Space member, project, project member, the real create handler) are
// here, and because the states being modelled are this module's states.

// TestProjectMembershipsDeniesAUserRemovedFromTheSpace is the P1-C blocker,
// reproduced against the engine.
//
// The Space→project cascade is asynchronous. Between a Space removal committing
// and the cleanup job running — and PERMANENTLY, if the job exhausts its retry
// cap, since abandoned rows are kept but never re-claimed — the project seat is
// still `status = 1 AND removing = 0`. Before the conjunction, this function
// reported that user as a member with a role, to a peer control plane that has no
// way to check the Space half itself.
//
// The removal here flips space_member.status WITHOUT running the cascade, which
// is exactly the window: the state is reachable in production for an unbounded
// time, not a test-only contrivance.
func TestProjectMembershipsDeniesAUserRemovedFromTheSpace(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "conjOwner")
	seedSpaceMember(t, spaceA, "conjOwner", 0, 1)
	seedUser(t, "conjMember")
	seedSpaceMember(t, spaceA, "conjMember", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "space-conjunction")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerToken, map[string]any{"uids": []string{"conjMember"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Baseline: both hold seats and both are in the Space.
	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjOwner", "conjMember"})
	require.NoError(t, err)
	require.Contains(t, roles, "conjOwner")
	require.Contains(t, roles, "conjMember")

	// The window: out of the Space, cascade not yet run.
	removeSpaceMember(t, spaceA, "conjMember")

	seat, err := testDB.queryMember(created.ProjectID, "conjMember")
	require.NoError(t, err)
	require.NotNil(t, seat, "the project seat must still exist — that IS the window")

	_, roles, err = projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjOwner", "conjMember"})
	require.NoError(t, err)
	assert.NotContains(t, roles, "conjMember",
		"a user removed from the Space must not be reported as a project member; the seat "+
			"outlives the removal by a window that is unbounded when the cascade gives up")
	assert.Contains(t, roles, "conjOwner",
		"the conjunction must narrow only the removed user, not empty the answer")
}

// TestProjectMembershipsDeniesEveryoneInAnInactiveSpace covers the other half of
// the predicate space.ActiveMembers carries: `space.status = 1`.
//
// A banned or disbanded Space must not pass an authorization gate — pkg/space's
// own comment says so, and the distinction is why CheckMembershipForCleanup
// exists as a SEPARATE predicate rather than a relaxation of this one.
func TestProjectMembershipsDeniesEveryoneInAnInactiveSpace(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "conjBanOwner")
	seedSpaceMember(t, spaceA, "conjBanOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "space-banned")

	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `space` SET status = 2 WHERE space_id = ?", spaceA).Exec()
	require.NoError(t, err)

	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjBanOwner"})
	require.NoError(t, err)
	assert.Empty(t, roles,
		"a banned Space must fail the authorization gate, even for a seat holder")
}

// TestProjectMembershipsAndEpochsFoldTheAbsentCases pins the three answers the
// contract deliberately makes indistinguishable, against the engine.
//
// Telling them apart would let a caller probe which Space a project id lives in,
// which is the fact the folding protects.
func TestProjectMembershipsAndEpochsFoldTheAbsentCases(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	token := seedUser(t, "conjFold")
	seedSpaceMember(t, spaceA, "conjFold", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "space-fold")

	cases := []struct {
		name      string
		spaceID   string
		projectID string
	}{
		{"a project id that does not exist", spaceA, "no-such-project"},
		{"a project that lives in another Space", spaceB, created.ProjectID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), tc.spaceID, []string{tc.projectID})
			require.NoError(t, err)
			assert.NotContains(t, epochs, tc.projectID,
				"absent from the map is what the caller turns into epoch 0")

			epoch, roles, err := projectpkg.ProjectMemberships(
				testCtx.DB(), tc.spaceID, tc.projectID, []string{"conjFold"})
			require.NoError(t, err)
			assert.Zero(t, epoch)
			assert.Empty(t, roles)
		})
	}

	// A DISBANDED project folds into the same answer, and that is what makes
	// disband converge without a dedicated event.
	forceStatus(t, created.ProjectID, StatusDisbanded)
	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.NotContains(t, epochs, created.ProjectID)
	epoch, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjFold"})
	require.NoError(t, err)
	assert.Zero(t, epoch)
	assert.Empty(t, roles)
}

// TestRolloutSentinelIsRefusedRatherThanServed is the reproduction of the
// stale-grant path that a rollout- or rollback-created row opens, and the proof
// that the read layer now closes it.
//
// The steps are the ones a deployment actually walks, not a contrivance:
//
//  1. a not-yet-upgraded pod (or a rolled-back binary) creates P at the column
//     default: member_epoch 0, status 1;
//  2. the peer verifies a user in P and is told "member, epoch 0", so it caches
//     a positive grant keyed on 0;
//  3. P is disbanded before the reconcile scan reaches it. Disband bumps the
//     epoch and THEN flips status, so the row ends at epoch 1, status 0;
//  4. the peer re-reads the epoch. The status filter drops P and the answer is
//     the contract's absent sentinel, 0;
//  5. 0 == 0. The staleness check agrees and the grant never expires.
//
// Measured before the fix, against this engine: step 2 served epoch 0 with
// member true, step 4 answered 0, and step 5 agreed. The repair scan cannot
// undo it after step 3 — its predicate needs status = 1, and the row is
// disbanded forever.
//
// The fix refuses the sentinel at step 2 instead. That costs availability for
// the length of one scan rotation, which is the direction this module trades in
// everywhere else.
func TestRolloutSentinelIsRefusedRatherThanServed(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "sentinelOwner")
	seedSpaceMember(t, spaceA, "sentinelOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "rollout-sentinel")
	forceAbsentSentinelEpoch(t, created.ProjectID)

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.EqualValues(t, 0, row.MemberEpoch, "the hazard must actually be staged")
	require.Equal(t, StatusNormal, row.Status, "and the project must still be ACTIVE — that is the point")

	// Step 2 must not produce a servable answer at all.
	_, _, err = projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"sentinelOwner"})
	require.Error(t, err, "an ACTIVE project on the reserved sentinel must not be served")
	assert.True(t, errors.Is(err, projectpkg.ErrLiveProjectOnAbsentSentinel),
		"the refusal must be the distinguished error, so the handler can log which project it was: %v", err)

	_, err = projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.Error(t, err, "the epochs endpoint must refuse for the same row")
	assert.True(t, errors.Is(err, projectpkg.ErrLiveProjectOnAbsentSentinel))

	// One anomalous row poisons the whole batch, deliberately: the response has
	// no per-key error channel, so answering the healthy ids and omitting this
	// one would hand the caller the sentinel through the absent-key path — the
	// same collision by another route.
	healthy := createProjectVia(t, srv, spaceA, token, "rollout-sentinel-healthy")
	_, err = projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA,
		[]string{healthy.ProjectID, created.ProjectID})
	require.Error(t, err, "a batch containing an anomalous row must fail, not answer partially")

	// And once the reconcile scan has repaired it, service resumes — the refusal
	// is a window, not a latch.
	repaired, err := testDB.repairAbsentSentinelEpoch(created.ProjectID)
	require.NoError(t, err)
	require.EqualValues(t, 1, repaired)
	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err, "after the repair the project must be servable again")
	assert.EqualValues(t, 1, epochs[created.ProjectID])
}
