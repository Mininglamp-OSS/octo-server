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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	principalSeamCaller    = "principal-seam-caller"
	principalSeamDelegate  = "principal-seam-delegate"
	principalSeamAlpha     = "principal-seam-alpha"
	principalSeamBeta      = "principal-seam-beta"
	principalSeamPaddingID = "principal-seam-padding"
)

// seedPrincipalSeamProject inserts a Project with one active Owner seat, WITHOUT
// going through createProject.
//
// The create path is asynchronous — it builds the all-member group and enqueues
// provisioning work that keeps writing to MySQL after the test returns — and a
// case that only needs a readable Project should not leave work in flight for
// the next case's CleanAllTables to collide with.
func seedPrincipalSeamProject(t *testing.T, projectID, ownerUID string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, NOW(3), NOW(3))",
		projectID, spaceA, "principal seam "+projectID, ownerUID, StatusNormal,
	).Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` "+
			"(project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, '', NOW(3), NOW(3))",
		projectID, ownerUID, spaceA, RoleOwner, MemberStatusActive,
	).Exec()
	require.NoError(t, err)
}

func TestReadProjectsForPrincipalRequiresLiveSpaceAccessForSingletonDelegateUID(t *testing.T) {
	setup(t)
	seedUser(t, principalSeamCaller)
	seedUser(t, principalSeamDelegate)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, principalSeamCaller, 0, 1)
	seedSpaceMember(t, spaceA, principalSeamDelegate, 0, 1)
	seedPrincipalSeamProject(t, principalSeamAlpha, principalSeamDelegate)

	list := func(seatUIDs []string) []*Resp {
		t.Helper()
		rows, _, err := ReadProjectsForPrincipal(testCtx, spaceA, principalSeamCaller, "", seatUIDs, 1, 50)
		require.NoError(t, err, "seatUIDs=%v", seatUIDs)
		return rows
	}

	// A live delegate authorizes through its own active Project seat.
	live := list([]string{principalSeamDelegate})
	require.Len(t, live, 1)
	assert.Equal(t, principalSeamAlpha, live[0].ProjectID)

	// Space removal leaves the Project seat behind: seat closure is an async
	// cascade, so this window is real state, not a synthetic one.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceA, principalSeamDelegate,
	).Exec()
	require.NoError(t, err)
	assert.Empty(t, list([]string{principalSeamDelegate}),
		"a singleton delegated uid without a live Space seat must not authorize")
	detail, err := ReadProjectForPrincipal(testCtx, principalSeamAlpha, principalSeamCaller, []string{principalSeamDelegate})
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
	detail, err = ReadProjectForPrincipal(testCtx, principalSeamAlpha, principalSeamCaller, []string{principalSeamDelegate})
	require.ErrorIs(t, err, ErrProjectReadNotFound)
	assert.Nil(t, detail)

	// The caller's own singleton set is unaffected: its Space access is what the
	// entry gate already proved, so a caller with no Project seat reads an empty
	// list instead of an error.
	assert.Empty(t, list([]string{principalSeamCaller}))
}

// TestReadSeamsRejectAnOversizedPrincipalSet pins the exported contract's bound:
// each uid costs a join and a Space-gate point read, so a set larger than
// principalMaxSeatUIDs is denied instead of being served (or silently truncated).
func TestReadSeamsRejectAnOversizedPrincipalSet(t *testing.T) {
	setup(t)
	seedUser(t, principalSeamCaller)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, principalSeamCaller, 0, 1)
	seedPrincipalSeamProject(t, principalSeamBeta, principalSeamCaller)

	callerFirst := []string{principalSeamCaller}
	for i := 0; i <= principalMaxSeatUIDs; i++ {
		callerFirst = append(callerFirst, fmt.Sprintf("%s-%d", principalSeamPaddingID, i))
	}
	require.Greater(t, len(callerFirst), principalMaxSeatUIDs)

	rows, total, err := ReadProjectsForPrincipal(testCtx, spaceA, principalSeamCaller, "", callerFirst, 1, 50)
	require.ErrorIs(t, err, ErrProjectReadForbidden)
	assert.Nil(t, rows)
	assert.Zero(t, total)

	detail, err := ReadProjectForPrincipal(testCtx, principalSeamBeta, principalSeamCaller, callerFirst)
	require.ErrorIs(t, err, ErrProjectReadNotFound)
	assert.Nil(t, detail)

	// Exactly at the bound is still served: the padding uids simply hold no seat,
	// so the answer is the caller's own visible set rather than a refusal.
	atBound := callerFirst[:principalMaxSeatUIDs]
	rows, total, err = ReadProjectsForPrincipal(testCtx, spaceA, principalSeamCaller, "", atBound, 1, 50)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	assert.Equal(t, principalSeamBeta, rows[0].ProjectID)
}
