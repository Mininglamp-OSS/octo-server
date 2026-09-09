package project

// Q8 smaller items, each with its reproducer.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileFlagsProjectsOfDisbandedSpace covers the gap yujiawei flagged: a Space that
// is DISBANDED (row present, status=0) leaves its projects permanently active and
// permanently invisible — the cascade closes every seat and the middleware refuses access,
// so it is not a security hole, but scanOrphanProjects only flagged projects whose space
// ROW is absent, so no scan ever reported these and no path could ever disband them.
func TestReconcileFlagsProjectsOfDisbandedSpace(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")
	resetCursorsForTest()

	// Disband (not delete) the Space: the row stays with status=0.
	setSpaceStatus(t, spaceA, 0)

	rows, err := p.queryInspectedProjectPage(0, p.cfg.ReconcileLimit)
	require.NoError(t, err)
	orphans := make([]*orphanRow, 0, 1)
	for _, r := range rows {
		if r.Violating {
			orphans = append(orphans, r)
		}
	}
	require.Len(t, orphans, 1, "a project of a disbanded Space must be flagged as orphaned")
	assert.Equal(t, created.ProjectID, orphans[0].ProjectID)

	// And a BANNED Space is still NOT an orphan: a ban is recoverable.
	setSpaceStatus(t, spaceA, 2)
	rows, err = p.queryInspectedProjectPage(0, p.cfg.ReconcileLimit)
	require.NoError(t, err)
	orphans = orphans[:0]
	for _, r := range rows {
		if r.Violating {
			orphans = append(orphans, r)
		}
	}
	assert.Empty(t, orphans, "a banned Space is recoverable, not orphaned")
}

// TestMembershipWriteDoesNotTouchProjectUpdatedAt pins the profile/roster clock split.
//
// bumpMemberEpochTx also wrote updated_at, so every roster edit churned the project's
// updated_at — the one field a client can diff to decide whether the PROFILE changed, and
// member_epoch already carries the roster signal.
func TestMembershipWriteDoesNotTouchProjectUpdatedAt(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "fresh1")
	seedSpaceMember(t, spaceA, "fresh1", 0, 1)
	before := updatedAtOf(t, created.ProjectID)

	// A REAL add (fresh uid), not a no-op re-add: the epoch must move while updated_at must
	// not — the two signals are now cleanly separated.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"fresh1"}})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	require.True(t, outcomes[0].OK, "seeding add failed: %s", outcomes[0].Reason)
	// committed itself is deliberately off the wire; its BEHAVIOR is pinned by
	// TestNoOpBatchWithActorFailureStaysOneStatusCode in review4_blocker_test.go. Here the
	// observable consequences are asserted below: the DB row exists, updated_at is unmoved,
	// the epoch moved.

	after := updatedAtOf(t, created.ProjectID)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt,
		"a membership write must not churn the project's updated_at")
	assert.Equal(t, before.Epoch+1, after.Epoch,
		"the epoch is the roster signal and must still move")
}

// TestBumpMemberEpochSkipsDisbandedProjects pins the DAO's status predicate: the only
// current callers are guarded, but a method that happily bumps a disbanded project is safe
// only by convention.
func TestBumpMemberEpochSkipsDisbandedProjects(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m2")
	w := doJSON(t, srv, "DELETE", "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	disbandedEpoch := epochOf(t, created.ProjectID)

	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	require.NoError(t, err)
	require.NoError(t, p.db.bumpMemberEpochTx(tx, created.ProjectID, now))
	require.NoError(t, tx.Commit())

	assert.Equal(t, disbandedEpoch, epochOf(t, created.ProjectID),
		"a disbanded project's epoch must not move")
}

type updatedAtWithEpoch struct {
	UpdatedAt time.Time `db:"updated_at"`
	Epoch     int64     `db:"member_epoch"`
}

func updatedAtOf(t *testing.T, projectID string) updatedAtWithEpoch {
	t.Helper()
	var v updatedAtWithEpoch
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT updated_at, member_epoch FROM `octo_project` WHERE project_id = ?",
		projectID).LoadOne(&v))
	return v
}
