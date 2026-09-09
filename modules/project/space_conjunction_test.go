package project

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
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

	// Baseline: both hold seats and both are in the Space. The roles map is keyed by
	// projectpkg.FoldID (see FoldID): the SQL matched these uids under a
	// case-INSENSITIVE collation, so an exact-match Go key would drop a member whose
	// rows are spelled differently from the request.
	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjOwner", "conjMember"})
	require.NoError(t, err)
	require.Contains(t, roles, projectpkg.FoldID("conjOwner"))
	require.Contains(t, roles, projectpkg.FoldID("conjMember"))

	// The window: out of the Space, cascade not yet run.
	removeSpaceMember(t, spaceA, "conjMember")

	seat, err := testDB.queryMember(created.ProjectID, "conjMember")
	require.NoError(t, err)
	require.NotNil(t, seat, "the project seat must still exist — that IS the window")

	_, roles, err = projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"conjOwner", "conjMember"})
	require.NoError(t, err)
	assert.NotContains(t, roles, projectpkg.FoldID("conjMember"),
		"a user removed from the Space must not be reported as a project member; the seat "+
			"outlives the removal by a window that is unbounded when the cascade gives up")
	assert.Contains(t, roles, projectpkg.FoldID("conjOwner"),
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

// TestSpaceBanMovesTheEpochChannelToo is the blocker two reviewers converged on,
// and the half I broke by fixing the other half.
//
// Before the Space conjunction landed in ProjectMemberships, BOTH predicates
// ignored Space status. They were wrong together, which at least kept them
// consistent. Conjoining one and not the other put the disagreement on the one
// channel the peer uses to invalidate an authorization cache:
//
//	ban   — memberships flip to member:false while the epoch stays E, so the
//	        peer's check agrees with its own cached grant and the grant survives
//	        the ban, unbounded;
//	unban — a peer that re-verified during the ban cached member:false under E;
//	        the unban restores the answer, the epoch is still E, the check agrees
//	        again, and everyone stays denied.
//
// The fix folds an inactive parent Space into the absent answer, so the epoch
// moves in BOTH directions: E -> 0 on ban, 0 -> E on unban. Both mismatches make
// the peer re-verify, which is the only thing the contract needs.
// The ban is staged through setSpaceStatus (api_test.go), which writes the
// `space` row and nothing else — exactly what modules/space's updateSpaceStatus
// does. No project row moves and no epoch bumps, which is the premise.
func TestSpaceBanMovesTheEpochChannelToo(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "banOwner")
	seedSpaceMember(t, spaceA, "banOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "space-ban")

	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	cached := epochs[created.ProjectID]
	require.NotZero(t, cached, "baseline: an active project in an active Space has a real epoch")

	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"banOwner"})
	require.NoError(t, err)
	require.Contains(t, roles, projectpkg.FoldID("banOwner"),
		"baseline: the owner is a member (keyed by the fold — see FoldID)")

	// ---- ban ----
	setSpaceStatus(t, spaceA, 2)

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.Equal(t, StatusNormal, row.Status, "a ban must not touch the project row — that is the point")
	require.EqualValues(t, cached, row.MemberEpoch, "and it must not bump the epoch either")

	_, banRoles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"banOwner"})
	require.NoError(t, err)
	require.Empty(t, banRoles, "a banned Space must fail the authorization gate")

	banEpochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.NotContains(t, banEpochs, created.ProjectID,
		"the epoch channel must report the change too; absent is what the caller turns into 0")
	assert.NotEqual(t, cached, banEpochs[created.ProjectID],
		"a cached grant keyed on %d must stop matching, or it survives the ban forever", cached)

	// ---- unban ----
	setSpaceStatus(t, spaceA, 1)

	unbanEpochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.EqualValues(t, cached, unbanEpochs[created.ProjectID],
		"the unban must restore the epoch")
	assert.NotEqual(t, banEpochs[created.ProjectID], unbanEpochs[created.ProjectID],
		"a denial cached during the ban (under 0) must stop matching, or every member of "+
			"every project in this Space stays denied after the unban")

	_, backRoles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"banOwner"})
	require.NoError(t, err)
	assert.Contains(t, backRoles, projectpkg.FoldID("banOwner"),
		"and the answer itself must come back")
}

// TestDisbandedSpaceProjectsReadAsAbsent covers the permanent case.
//
// Nothing disbands the projects of a disbanded Space — this repository says so
// in its own words in modules/project/reconcile.go, where scanOrphanProjects
// only LOGS the state. So octo_project.status stays 1 forever, and without the
// parent check the endpoint would keep reporting a live epoch, the contract's
// "exists and is visible" value, for a project whose Space is gone.
func TestDisbandedSpaceProjectsReadAsAbsent(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "goneSpaceOwner")
	seedSpaceMember(t, spaceA, "goneSpaceOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "space-disbanded")
	setSpaceStatus(t, spaceA, 0)

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.Equal(t, StatusNormal, row.Status,
		"nothing disbands the projects of a disbanded Space — the row stays active, which "+
			"is exactly why the parent has to be part of the predicate")

	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.NotContains(t, epochs, created.ProjectID,
		"a project whose Space is disbanded must read as absent, i.e. epoch 0")

	epoch, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"goneSpaceOwner"})
	require.NoError(t, err)
	assert.Zero(t, epoch)
	assert.Empty(t, roles)
}

// TestSpaceRemovalMovesTheEpochAtCommit is the P1 both round-6 reviewers filed,
// and it is the half the Space conjunction did NOT close.
//
// The conjunction fixes FRESH answers: `_verify` denies a removed user
// immediately. It does nothing for a grant the peer cached BEFORE the removal,
// because that peer re-checks staleness by re-reading member_epoch — and nothing
// used to move it at removal time. The seat closes, and only then bumps, when the
// async cleanup job reaches it; that job can sit in backoff for minutes and has a
// terminal abandoned state after which nothing re-claims it. So the revocation
// survived for minutes normally and forever when the job gave up, while the
// peer's own check kept agreeing the cached answer was current.
//
// What makes that this PR's defect rather than a pre-existing one: before it, the
// stale seat granted nothing outside this repository. This PR publishes it as an
// authorization fact to a peer, under a contract that says epoch agreement is
// sufficient.
//
// The removal here goes through the real Space handler path, so the assertion is
// about the transaction that actually ships — not about a helper called directly.
func TestSpaceRemovalMovesTheEpochAtCommit(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "epochRemOwner")
	seedSpaceMember(t, spaceA, "epochRemOwner", 0, 1)
	seedUser(t, "epochRemTarget")
	seedSpaceMember(t, spaceA, "epochRemTarget", 0, 1)

	// Two projects the target is in, and one they are not — the third is the
	// bound: a removal must not churn epochs of projects it does not touch.
	inA := createProjectVia(t, srv, spaceA, ownerToken, "space-removal-a")
	inB := createProjectVia(t, srv, spaceA, ownerToken, "space-removal-b")
	untouched := createProjectVia(t, srv, spaceA, ownerToken, "space-removal-untouched")
	for _, p := range []string{inA.ProjectID, inB.ProjectID} {
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+p+"/members/add",
			ownerToken, map[string]any{"uids": []string{"epochRemTarget"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	before := map[string]int64{}
	for _, p := range []string{inA.ProjectID, inB.ProjectID, untouched.ProjectID} {
		before[p] = epochOf(t, p)
	}

	// The removal transaction: the Space seat closes and the registered tx step
	// runs, in one commit. Seats in octo_project_member are NOT closed here —
	// that is the cascade's job and it stays asynchronous on purpose.
	//
	// Driven through the step this module registers rather than through
	// modules/space's handler, because the two halves are tested where they live:
	// that modules/space actually RUNS a registered step inside the removal
	// transaction (and rolls back when it fails) is pinned by
	// TestSeatTransitionTxStepRunsInsideTheTransaction over there.
	tx, err := testCtx.DB().Begin()
	require.NoError(t, err)
	_, err = tx.UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?",
		spaceA, "epochRemTarget").Exec()
	require.NoError(t, err)
	seatRef, err := spacemod.SeatRefForTest(spaceA, "epochRemTarget")
	require.NoError(t, err)
	require.NoError(t, p.bumpEpochsOnSeatTransition(tx, spacemod.SeatTransition{Seat: seatRef}))
	require.NoError(t, tx.Commit())

	seat, err := testDB.queryMember(inA.ProjectID, "epochRemTarget")
	require.NoError(t, err)
	require.NotNil(t, seat, "the project seat must still be open — the cascade has not run, "+
		"which is exactly the window this test is about")

	for _, p := range []string{inA.ProjectID, inB.ProjectID} {
		assert.Greater(t, epochOf(t, p), before[p],
			"project %s: the epoch must move AT REMOVAL COMMIT. Left to the cascade it moves "+
				"minutes later, or never once the job is abandoned — and until it does, a peer "+
				"re-reading the epoch gets the same value and keeps a grant the removal revoked", p)
	}
	assert.Equal(t, before[untouched.ProjectID], epochOf(t, untouched.ProjectID),
		"a project the removed member was never in must not be churned: every bump costs "+
			"every consumer of that project a re-verify")
}

// TestSpaceMemberRemovalRegistersBothHalves pins the WIRING, which no behavioural
// test in this package can see.
//
// TestSpaceRemovalMovesTheEpochAtCommit calls the step directly, so it proves the
// STATEMENT is right and says nothing about whether anything ever calls it —
// deleting the registration leaves that test green. Registration happens in an
// init-time reverse hook into modules/space, and its absence is silent: removals
// would simply stop moving the epoch, and the only symptom is a peer holding a
// grant it cannot detect as stale.
//
// Both halves must be there. The async step closes the seats; the sync step moves
// the invalidation signal. Dropping either one is a different, and equally quiet,
// regression.
func TestSpaceMemberRemovalRegistersBothHalves(t *testing.T) {
	body := funcBody(t, readLinesWithoutComments(t, "space_member_removal.go"),
		"func (p *Project) registerSpaceMemberRemovalCleanup(")

	if !strings.Contains(body, "RegisterMemberRemovalCleanupStep(") {
		t.Error("the ASYNC cleanup step must stay registered: nothing else closes the project " +
			"seats of a removed Space member")
	}
	if !strings.Contains(body, "RegisterSeatTransitionTxStep(") {
		t.Fatal("the SYNCHRONOUS tx step must be registered, or member_epoch never moves at " +
			"Space-removal commit and a peer keeps a revoked grant riding epoch agreement — " +
			"for minutes on the normal path, forever once the cleanup job is abandoned")
	}
	if !strings.Contains(body, "bumpEpochsOnSeatTransition") {
		t.Error("the tx step must be the epoch bump; registering something else here would " +
			"satisfy the check above while leaving the signal unmoved")
	}
}
