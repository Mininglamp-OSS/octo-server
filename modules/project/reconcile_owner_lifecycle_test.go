package project

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileOwnerLifecycleEligibility keeps the Owner exemption scoped to a live Project.
// Space removal preserves the Owner row so a later rejoin can transfer ownership, but that
// retained identity is not a usable seat once the Project itself is gone.
func TestReconcileOwnerLifecycleEligibility(t *testing.T) {
	t.Run("normal project retains owner exemption while ordinary member is reported", func(t *testing.T) {
		srv, p := setup(t)
		_, _, _ = projectWithMembers(t, srv, "ordinary")

		// Model the state after Space revocation before any cleanup worker runs: both Project
		// rows remain active, but neither uid has an eligible Space seat.
		removeSpaceMember(t, spaceA, "owner1")
		removeSpaceMember(t, spaceA, "ordinary")

		assert.ElementsMatch(t, []string{"ordinary"}, reconcileI1ViolationUIDs(t, p),
			"a normal Project retains its Owner identity, but an ordinary orphan seat is a leak")

		enqueueCleanupJob(t, spaceA, "owner1", cleanupStatusAbandoned)
		enqueueCleanupJob(t, spaceA, "ordinary", cleanupStatusAbandoned)
		assert.ElementsMatch(t, []string{"ordinary"}, reconcileAbandonedViolationUIDs(t, p),
			"an abandoned cleanup on a normal Project still exempts the retained Owner")
		assert.Equal(t, 1, countAbandonedLeak(t, p),
			"the production abandoned scan must report the ordinary leaked seat only")
	})

	for _, tc := range []struct {
		name string
		gone func(t *testing.T, projectID string)
	}{
		{
			name: "disbanded project",
			gone: func(t *testing.T, projectID string) {
				t.Helper()
				_, err := testCtx.DB().UpdateBySql(
					"UPDATE `octo_project` SET status = ? WHERE project_id = ?",
					StatusDisbanded, projectID).Exec()
				require.NoError(t, err)
			},
		},
		{
			name: "missing project",
			gone: func(t *testing.T, projectID string) {
				t.Helper()
				_, err := testCtx.DB().DeleteFrom("octo_project").
					Where("project_id = ?", projectID).Exec()
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name+" reports an active owner and ordinary member", func(t *testing.T) {
			srv, p := setup(t)
			_, _, created := projectWithMembers(t, srv, "ordinary")

			// Keep both Project seats ACTIVE, matching the stale window handled by
			// deactivateStaleMemberTx, while revoking both Space seats.
			removeSpaceMember(t, spaceA, "owner1")
			removeSpaceMember(t, spaceA, "ordinary")
			tc.gone(t, created.ProjectID)

			assert.ElementsMatch(t, []string{"owner1", "ordinary"}, reconcileI1ViolationUIDs(t, p),
				"an Owner is exempt only while its Project is normal; ordinary members remain violations")

			enqueueCleanupJob(t, spaceA, "owner1", cleanupStatusAbandoned)
			enqueueCleanupJob(t, spaceA, "ordinary", cleanupStatusAbandoned)
			assert.ElementsMatch(t, []string{"owner1", "ordinary"}, reconcileAbandonedViolationUIDs(t, p),
				"abandoned cleanup must report the stale Owner and ordinary seat when the Project is gone")
			assert.Equal(t, 2, countAbandonedLeak(t, p),
				"the production abandoned scan must count both leaked seats")
		})
	}
}

func reconcileI1ViolationUIDs(t *testing.T, p *Project) []string {
	t.Helper()
	rows, err := p.queryI1ViolationPage("", "", p.cfg.ReconcileLimit)
	require.NoError(t, err)
	return violationUIDs(violatingI1Rows(rows))
}

func reconcileAbandonedViolationUIDs(t *testing.T, p *Project) []string {
	t.Helper()
	rows, err := p.queryAbandonedLeakPage("", "", p.cfg.ReconcileLimit)
	require.NoError(t, err)
	return violationUIDs(violatingI1Rows(rows))
}

func violationUIDs(rows []*i1Row) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.UID)
	}
	return out
}
