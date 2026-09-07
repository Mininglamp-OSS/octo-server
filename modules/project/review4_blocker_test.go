package project

// TDD reproducers for PR #841 review round 1. Each case follows the RED -> GREEN cycle; the
// RED run is recorded in verification.md before any production code changes.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Blocker 1 (Jerry-Xin): a partially committed removal batch collapses to a bare 404
// when the project is disbanded mid-batch. The add path handles this; the remove path's
// errProjectGone branch responds unconditionally.
// ---------------------------------------------------------------------------

// withRemoveSeam swaps the remove seam on p's instance so every target before failOn
// commits for real and failOn itself is told inject happened.
func withRemoveSeam(t *testing.T, p *Project, failOn string, inject error, calls *int) {
	t.Helper()
	orig := p.removeOneFn
	p.removeOneFn = func(projectID, spaceID, actorUID, targetUID string) (bool, error) {
		*calls++
		if targetUID == failOn {
			return false, inject
		}
		return orig(projectID, spaceID, actorUID, targetUID)
	}
	t.Cleanup(func() { p.removeOneFn = orig })
}

// TestRemoveBatchReportsPartialWhenProjectDisbandsMidBatch is the RED reproducer.
//
// Target 1's removal commits for real; target 2 hits errProjectGone. The handler must report
// per-target results (r1 ok, r2 project_disbanded, r3 not_attempted) instead of a detail-free
// 404 implying nothing happened — the exact contract the add path already implements and the
// round-3 fix established for the permission branch.
func TestRemoveBatchReportsPartialWhenProjectDisbandsMidBatch(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "r1", "r2", "r3")
	r := mountProject(t, p)

	calls := 0
	withRemoveSeam(t, p, "r2", errProjectGone, &calls)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"r1", "r2", "r3"}})
	require.Equal(t, http.StatusOK, w.Code,
		"r1 committed, so the response must be a per-target report, not a bare 404: %s",
		w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "every uid must be accounted for: %s", w.Body.String())
	assert.True(t, outcomes[0].OK, "r1 committed")
	assert.Equal(t, reasonProjectDisbanded, outcomes[1].Reason)
	assert.Equal(t, outcomeNotAttempted, outcomes[2].Reason)

	// And the committed removal really is committed.
	m, err := p.db.queryMember(created.ProjectID, "r1")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, MemberStatusRemoved, m.Status)
}

// TestRemoveBatchBare404WhenNothingCommitted pins the other half of the contract: with
// nothing committed, the single status code IS the honest answer.
func TestRemoveBatchBare404WhenNothingCommitted(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "x1")
	r := mountProject(t, p)

	calls := 0
	withRemoveSeam(t, p, "x1", errProjectGone, &calls)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"x1"}})
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	assert.Equal(t, 1, calls)
}

// TestNoOpBatchWithActorFailureStaysOneStatusCode pins Jerry-Xin's companion finding at the
// BEHAVIOR level (committed is deliberately off the wire, so it cannot be asserted from a
// response): a batch whose only "successes" are idempotent no-ops (re-adding existing
// members, OK=true, nothing written) followed by an actor-level failure must still answer
// with the single status code — "nothing committed yet" is TRUE for that batch.
//
// Mutated-check: with anyApplied keyed on OK instead of committed, this test returns 200
// with a partial report and FAILS — verified before the fix landed.
func TestNoOpBatchWithActorFailureStaysOneStatusCode(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "already1")

	// already1 holds a seat: re-adding is a no-op. The seam makes the SECOND target hit an
	// actor-level failure after the no-op "succeeded".
	calls := 0
	orig := p.addOneFn
	p.addOneFn = func(projectID, spaceID, actorUID, uid string) (bool, error) {
		calls++
		if uid == "blocked1" {
			return false, errPermissionDenied
		}
		return orig(projectID, spaceID, actorUID, uid)
	}
	t.Cleanup(func() { p.addOneFn = orig })

	seedUser(t, "blocked1")
	seedSpaceMember(t, spaceA, "blocked1", 0, 1)

	r := mountProject(t, p)
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"already1", "blocked1"}})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")
	assert.Equal(t, 2, calls)
}
