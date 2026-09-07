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
// of one case and restores it afterwards.
//
// The registry is package state, which is exactly why this test can reach it:
// modules/group registers its detach step into this package at construction, so
// there is no way to build a "no steps registered" world other than to make one.
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

// TestAnEmptyStepRegistryDoesNotCloseTheSeat.
//
// Falling through an empty step list reads as "every step succeeded", and the
// worker then closes the seat with the member's group_member rows never
// detached — the I2 violation the two-phase close exists to avoid, produced
// silently, with the job marked done. Failing instead leaves the seat at
// removing = 1, where the member is already a non-member for every
// authorization read, and surfaces as backlog plus the stall alert.
func TestAnEmptyStepRegistryDoesNotCloseTheSeat(t *testing.T) {
	p, job := claimedJob(t, "worker-a")
	withNoRemovalSteps(t)

	p.workRemovalJob(job, "worker-a")

	status, removing := seatState(t, job.ProjectID, job.UID)
	require.Equal(t, MemberStatusActive, status,
		"with no cascade registered the seat must NOT close: the group rows are still there")
	require.Equal(t, 1, removing, "and it must stay in the closing state, visible to the stall scan")

	jobStatus, _ := jobRow(t, job.ID)
	require.Equal(t, removalJobPending, jobStatus,
		"the job must stay pending so the backlog shows it, rather than reading as done")
}

// TestAFullyRegisteredCascadeDoesCloseTheSeat is the control. Without it,
// breaking the worker outright would satisfy the case above.
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
