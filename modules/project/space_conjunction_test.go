package project

import (
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
