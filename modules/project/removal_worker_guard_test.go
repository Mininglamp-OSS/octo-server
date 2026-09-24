package project

import (
	"errors"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestRemovalWorkerClosesProjectSeat checks the worker's own seat transition.
func TestRemovalWorkerClosesProjectSeat(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	p.workRemovalJob(job, "worker-a")

	status, removing := seatState(t, job.ProjectID, job.UID)
	require.Equal(t, MemberStatusRemoved, status, "the worker must close the Project seat")
	require.Zero(t, removing)

	jobStatus, _ := jobRow(t, job.ID)
	require.Equal(t, removalJobDone, jobStatus)
}

// TestAnAbandonedJobIsCounted.
//
// The brief asks for backlog AND abandoned counts and only backlog was built.
// The two answer different questions, and this is the one that pages: an
// abandoned job is terminal and leaves a seat at removing = 1 with group rows in
// place, which nothing else repairs. The stall gauge notices the same state half
// an hour later, once last_error is no longer the first thing an operator sees.
func TestAnAbandonedJobIsCounted(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	before := promtestutil.ToFloat64(removalAbandoned)
	job.Attempts = removalMaxAttempts
	p.rescheduleAfterFailure(job, "worker-a", errors.New("boom"))

	require.Equal(t, before+1, promtestutil.ToFloat64(removalAbandoned),
		"exhausting the attempt budget must move the counter, not only write a log line")

	jobStatus, _ := jobRow(t, job.ID)
	require.Equal(t, removalJobAbandoned, jobStatus)
}

// TestACancelledJobIsNotCountedAsAbandoned pins the other half of that counter:
// a cascade retired by re-admission is D4 working, not a failure.
func TestACancelledJobIsNotCountedAsAbandoned(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	before := promtestutil.ToFloat64(removalAbandoned)
	held, err := p.db.completeRemovalJob(job.ID, "worker-a", removalJobCancelled, "re-admitted",
		time.Now().UTC())
	require.NoError(t, err)
	require.True(t, held)

	require.Equal(t, before, promtestutil.ToFloat64(removalAbandoned),
		"a cancelled cascade must not read as an abandoned one")
}
