package project

// Direct tests for the exported cross-module principal read seam
// (principal_read.go). They call the seam itself rather than going through the
// Bot API handler, because the seam is an authorization API in its own right:
// modules/bot_api is only its first caller, and the handler happens to pass the
// caller's own uid (which the Space gate already cleared) ahead of the owner's.
//
// The gate the seam must hold on its own: a uid that is NOT the caller authorizes
// nothing unless it clears the live Space gate right now, even when it is the
// ONLY uid in the set.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	principalSeamCaller   = "principal-seam-caller"
	principalSeamDelegate = "principal-seam-delegate"
)

func TestReadProjectsForPrincipalRequiresLiveSpaceAccessForSingletonDelegateUID(t *testing.T) {
	_, p := setup(t)
	seedUser(t, principalSeamCaller)
	seedUser(t, principalSeamDelegate)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, principalSeamCaller, 0, 1)
	seedSpaceMember(t, spaceA, principalSeamDelegate, 0, 1)

	created, err := p.createProject(createInput{
		SpaceID: spaceA, Creator: principalSeamDelegate, Name: "principal seam alpha",
	})
	require.NoError(t, err)
	require.NotNil(t, created)

	list := func(seatUIDs []string) []*Resp {
		t.Helper()
		rows, _, err := ReadProjectsForPrincipal(testCtx, spaceA, principalSeamCaller, "", seatUIDs, 1, 50)
		require.NoError(t, err, "seatUIDs=%v", seatUIDs)
		return rows
	}

	// A live delegate authorizes through its own active Project seat.
	live := list([]string{principalSeamDelegate})
	require.Len(t, live, 1)
	assert.Equal(t, created.ProjectID, live[0].ProjectID)

	// Space removal leaves the Project seat behind: seat closure is an async
	// cascade, so this window is real state, not a synthetic one.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceA, principalSeamDelegate,
	).Exec()
	require.NoError(t, err)
	assert.Empty(t, list([]string{principalSeamDelegate}),
		"a singleton delegated uid without a live Space seat must not authorize")
	detail, err := ReadProjectForPrincipal(testCtx, created.ProjectID, principalSeamCaller, []string{principalSeamDelegate})
	require.ErrorIs(t, err, ErrProjectReadNotFound)
	assert.Nil(t, detail)

	// Restoring the Space seat restores the delegation...
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 1 WHERE space_id = ? AND uid = ?", spaceA, principalSeamDelegate,
	).Exec()
	require.NoError(t, err)
	require.Len(t, list([]string{principalSeamDelegate}), 1)

	// ...and destroying the account revokes it again with the Space seat intact:
	// nothing in modules/user closes a `space_member` row or a Project seat.
	_, err = testCtx.DB().UpdateBySql("UPDATE `user` SET is_destroy = 2 WHERE uid = ?", principalSeamDelegate).Exec()
	require.NoError(t, err)
	assert.Empty(t, list([]string{principalSeamDelegate}),
		"a singleton delegated uid with a destroyed account must not authorize")
	detail, err = ReadProjectForPrincipal(testCtx, created.ProjectID, principalSeamCaller, []string{principalSeamDelegate})
	require.ErrorIs(t, err, ErrProjectReadNotFound)
	assert.Nil(t, detail)

	// The caller's own singleton set is unaffected: its Space access is what the
	// entry gate already proved, so a caller with no Project seat reads an empty
	// list instead of an error.
	assert.Empty(t, list([]string{principalSeamCaller}))
}
