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

// withRemoveSeam drives removeOneMemberForTest so every target before failOn commits for
// real and failOn itself is told inject happened.
func withRemoveSeam(t *testing.T, failOn string, inject error, calls *int) {
	t.Helper()
	orig := removeOneMemberForTest
	removeOneMemberForTest = func(pr *Project, projectID, actorUID, targetUID string) (bool, error) {
		*calls++
		if targetUID == failOn {
			return false, inject
		}
		return orig(pr, projectID, actorUID, targetUID)
	}
	t.Cleanup(func() { removeOneMemberForTest = orig })
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
	withRemoveSeam(t, "r2", errProjectGone, &calls)

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
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "x1")
	r := mountProject(t, New(testCtx))

	calls := 0
	withRemoveSeam(t, "x1", errProjectGone, &calls)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"x1"}})
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	assert.Equal(t, 1, calls)
}
