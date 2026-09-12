package project

// The removal worker's two fail-closed guards, and the counter an operator
// pages off.
//
// PR #846's review measured that three of round 2's fixes were pinned by
// nothing: disabling the empty-registry guard kept the whole modules/project
// suite green. A fix this repo's own doctrine calls "guards over intent" is not
// finished while a mutation of it passes.

import (
	"errors"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// withNoRemovalSteps empties the reverse-registration registry for the duration
// of one case and restores it afterwards. The project worker owns this registry
// even when no optional project-side cleanup callback is installed.
func withNoRemovalSteps(t *testing.T) {
	t.Helper()
	cascadeMu.Lock()
	saved := memberRemovalSteps
	memberRemovalSteps = nil
	cascadeMu.Unlock()
	t.Cleanup(func() {
		cascadeMu.Lock()
		memberRemovalSteps = saved
		cascadeMu.Unlock()
	})
}

// TestAnEmptyStepRegistryClosesTheProjectSeat.
//
// Group-owned native membership cleanup is no longer registered here: Project removal must
// still complete its own seat closure rather than retrying forever on an intentionally empty
// registry. The absence of project cleanup steps means there is no local side effect to wait
// for; native group membership remains owned by the group module.
func TestAnEmptyStepRegistryClosesTheProjectSeat(t *testing.T) {
	p, job := claimedJob(t, "worker-a")
	withNoRemovalSteps(t)

	p.workRemovalJob(job, "worker-a")

	status, removing := seatState(t, job.ProjectID, job.UID)
	require.Equal(t, MemberStatusRemoved, status,
		"with no project-side cleanup steps, the Project seat must still close")
	require.Zero(t, removing)

	jobStatus, _ := jobRow(t, job.ID)
	require.Equal(t, removalJobDone, jobStatus,
		"an intentionally empty cleanup registry must not leave the job pending")
}

// TestAFullyRegisteredCascadeDoesCloseTheSeat is the control. Without it,
// breaking the worker outright would satisfy the empty-registry case above.
func TestAFullyRegisteredCascadeDoesCloseTheSeat(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	p.workRemovalJob(job, "worker-a")

	status, removing := seatState(t, job.ProjectID, job.UID)
	require.Equal(t, MemberStatusRemoved, status, "the real registry must close the seat")
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
