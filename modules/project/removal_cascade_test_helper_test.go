package project

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drainRemovalCascade runs the project-side removal cascade to completion,
// synchronously, so a test can assert the END state of a removal.
//
// # Why existing tests needed this
//
// P1 makes member removal two-phase (D4). The request transaction sets
// `removing = 1` and leaves `status` at 1; the worker detaches the member from
// the project's groups and only then flips status to 0. Every authorization read
// treats removing = 1 as a non-member, so nothing is weaker in between — but a
// test that reads octo_project_member.status straight after the request now sees
// 1, where P0's single-phase removal wrote 0 inline.
//
// Six P0 assertions read that column directly. Rather than weakening them to
// "status is still 1, removing is 1" — which would stop proving that the seat
// ever actually closes — each now drives the cascade first and asserts the same
// end state it always did. The assertion is unchanged; only the point in time it
// is taken at moved, and the machinery that moves it is now covered too.
//
// The loop bound is a safety net, not a schedule: one pass claims a batch and
// works it, and a job that reschedules itself with backoff will not be picked up
// again within the same test. If the queue is still not empty after ten passes
// the cascade is not converging, and failing here is far more useful than the
// assertion that follows failing for an unrelated-looking reason.
func drainRemovalCascade(t *testing.T, p *Project) {
	t.Helper()
	for i := 0; i < 10; i++ {
		pending, err := p.db.countPendingRemovalJobs()
		require.NoError(t, err)
		if pending == 0 {
			return
		}
		p.runRemovalCascade()
		// claimRemovalJobs filters on next_attempt_at <= now, and enqueue writes
		// now. Nudging the clock forward is not needed for the first pass, but a
		// rescheduled job would otherwise make this loop spin without progress.
		time.Sleep(time.Millisecond)
	}
	pending, err := p.db.countPendingRemovalJobs()
	require.NoError(t, err)
	require.Zero(t, pending, "removal cascade did not drain: %d job(s) still pending", pending)
}
