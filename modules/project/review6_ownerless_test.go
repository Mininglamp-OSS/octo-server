package project

// PR #841 review round 3, suggested direction #3: whatever fixes the concurrency hole, an
// ownerless project is invisible to every existing signal. None of the four reconcile scans
// looks for an active project with zero active owners, and P0 cannot repair the state — role
// change and disband are owner-only, and a Space admin has read access only.
//
// Two reachable routes exist: the concurrency hole (now fixed and pinned) and the sole owner
// being removed from the Space, which is a filed product decision. A detection signal is what
// makes the second one answerable with data rather than a guess.

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileFlagsAnActiveProjectWithNoOwner drives the real scan against a project whose
// only owner's seat has been closed without a successor — exactly what the cascade leaves
// behind when a sole owner is removed from the Space.
func TestReconcileFlagsAnActiveProjectWithNoOwner(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "keep1")
	pid := created.ProjectID

	// Close the owner's seat directly: the roster keeps a member, the project stays active,
	// and nobody holds RoleOwner.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET status = ?, updated_at = ? "+
			"WHERE project_id = ? AND uid = ?",
		MemberStatusRemoved, time.Now().UTC(), pid, "owner1").Exec()
	require.NoError(t, err)
	require.Equal(t, 0, activeOwnerCount(t, pid), "precondition: no owner left")
	require.Equal(t, 1, activeMemberCount(t, pid), "precondition: the project still has a member")

	cursors.idSave(&cursors.ownerless, &cursors.ownerlessRun, 0, 0, true)
	t.Cleanup(func() { cursors.idSave(&cursors.ownerless, &cursors.ownerlessRun, 0, 0, true) })

	p.scanOwnerlessProjects()

	assert.Equal(t, float64(1), testutil.ToFloat64(ownerlessProjects),
		"an active project with zero active owners must be reported; it is unrecoverable in P0")
}

// TestReconcileDoesNotFlagAHealthyOrDisbandedProject pins both directions, so the gauge cannot
// be satisfied by a scan that flags everything.
func TestReconcileDoesNotFlagAHealthyOrDisbandedProject(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, healthy := projectWithMembers(t, srv, "keep2")

	// A disbanded project has no owner either, and that is correct rather than a defect.
	disbanded := createProjectVia(t, srv, spaceA, ownerTok, "to-disband")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET status = ? WHERE project_id = ?",
		StatusDisbanded, disbanded.ProjectID).Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET status = ? WHERE project_id = ?",
		MemberStatusRemoved, disbanded.ProjectID).Exec()
	require.NoError(t, err)

	require.Equal(t, 1, activeOwnerCount(t, healthy.ProjectID), "precondition: healthy has an owner")

	cursors.idSave(&cursors.ownerless, &cursors.ownerlessRun, 0, 0, true)
	t.Cleanup(func() { cursors.idSave(&cursors.ownerless, &cursors.ownerlessRun, 0, 0, true) })

	p.scanOwnerlessProjects()

	assert.Equal(t, float64(0), testutil.ToFloat64(ownerlessProjects),
		"neither a healthy project nor a disbanded one is ownerless in the sense that matters")
}
