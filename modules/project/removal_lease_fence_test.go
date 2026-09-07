package project

// The removal outbox's terminal writes are fenced on (status, lease_owner).
//
// Every case here was observed RED against the shipped PR #844 code, whose
// completeRemovalJob and rescheduleRemovalJob wrote with a bare `WHERE id = ?`:
// a worker that had lost its lease could retire a job another worker was
// running, reschedule a row that was in flight, and overwrite the CANCELLED
// state that re-admission writes.
//
// The fence is what modules/space's sibling table already carries
// (finishMemberRemovalCleanup / releaseMemberRemovalCleanup); this file is the
// assertion that the copy finally has it.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// claimedJob puts one seat into the closing state and claims its job as `owner`,
// which is the state every case below starts from.
func claimedJob(t *testing.T, owner string) (*Project, RemovalJob) {
	t.Helper()
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")
	beginRemovalWithoutDraining(t, p, created.ProjectID, "member1")

	jobs, err := p.db.claimRemovalJobs(owner, removalBatch, time.Now().UTC(), removalLease)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "phase one must have enqueued exactly one cleanup job")
	return p, jobs[0]
}

// jobRow reads the columns the fence is made of.
func jobRow(t *testing.T, id int64) (status int, leaseOwner string) {
	t.Helper()
	type row struct {
		Status     int    `db:"status"`
		LeaseOwner string `db:"lease_owner"`
	}
	var rows []row
	_, err := testCtx.DB().SelectBySql(
		"SELECT status, lease_owner FROM `octo_project_member_removal_cleanup` WHERE id = ?",
		id).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].Status, rows[0].LeaseOwner
}

// TestARetiredJobRequiresTheLeaseItWasClaimedUnder.
//
// The damaging shape: worker A stalls past its lease, worker B claims the same
// job and starts the cascade, then A wakes and reports done. Without the fence
// the row reads terminal while B is still detaching groups — the concurrent
// cascade the heartbeat exists to prevent, now invisible in the queue.
func TestARetiredJobRequiresTheLeaseItWasClaimedUnder(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	// Worker B takes the job over. Its claim rewrites lease_owner.
	stolen, err := p.db.claimRemovalJobs("worker-b", removalBatch,
		time.Now().UTC().Add(removalLease+time.Second), removalLease)
	require.NoError(t, err)
	require.Len(t, stolen, 1)
	require.Equal(t, job.ID, stolen[0].ID)

	// A now tries to retire it.
	held, err := p.db.completeRemovalJob(job.ID, "worker-a", removalJobDone, "", time.Now().UTC())
	require.NoError(t, err, "a lost lease is not an error, it is a false")
	require.False(t, held, "worker-a no longer holds the lease and must not write a terminal state")

	status, owner := jobRow(t, job.ID)
	require.Equal(t, removalJobPending, status, "the job must still be pending for worker-b")
	require.Equal(t, "worker-b", owner, "worker-a must not have cleared worker-b's lease")

	// And the holder of the lease still can.
	held, err = p.db.completeRemovalJob(job.ID, "worker-b", removalJobDone, "", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, held)
	status, owner = jobRow(t, job.ID)
	require.Equal(t, removalJobDone, status)
	require.Empty(t, owner, "retiring a job releases its lease")
}

// TestARescheduledJobRequiresTheLeaseItWasClaimedUnder.
//
// The mirror case, and the one that loses work rather than duplicating it: a
// stale reschedule on a row another worker owns clears that worker's lease and
// arms next_attempt_at, so a third claimer can pick up a cascade that is still
// running.
func TestARescheduledJobRequiresTheLeaseItWasClaimedUnder(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	held, err := p.db.rescheduleRemovalJob(job.ID, "worker-b", time.Now().UTC().Add(time.Minute), "boom")
	require.NoError(t, err)
	require.False(t, held, "worker-b never held this lease and must not release it")

	status, owner := jobRow(t, job.ID)
	require.Equal(t, removalJobPending, status)
	require.Equal(t, "worker-a", owner, "worker-a's lease must survive a stranger's reschedule")

	held, err = p.db.rescheduleRemovalJob(job.ID, "worker-a", time.Now().UTC().Add(time.Minute), "boom")
	require.NoError(t, err)
	require.True(t, held)
	_, owner = jobRow(t, job.ID)
	require.Empty(t, owner, "rescheduling releases the lease")
}

// TestATerminalJobCannotBeRetiredTwice.
//
// The status half of the fence, and the one with forensic consequences: a job
// retired as CANCELLED by re-admission could be overwritten with done by a
// straggler, and the migration is explicit that retiring a cascade must not look
// like success. `status = pending` in the WHERE is what makes every terminal
// state final.
func TestATerminalJobCannotBeRetiredTwice(t *testing.T) {
	p, job := claimedJob(t, "worker-a")

	held, err := p.db.completeRemovalJob(job.ID, "worker-a", removalJobCancelled, "re-admitted", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, held)

	// Same worker, same lease, later write. The lease is already cleared, but the
	// status predicate is the one that has to hold here: a terminal row is done
	// being written to.
	held, err = p.db.completeRemovalJob(job.ID, "worker-a", removalJobDone, "", time.Now().UTC())
	require.NoError(t, err)
	require.False(t, held, "a cancelled job must not be re-labelled done")

	status, _ := jobRow(t, job.ID)
	require.Equal(t, removalJobCancelled, status,
		"the evidence that this cascade was cancelled must survive")
}

// TestWorkerIdentityIsStableWithinAProcess.
//
// A fence is only as meaningful as the identity it compares. The first version
// of workerIdentity returned project-removal-<UnixNano>, a fresh value per tick:
// an operator reading lease_owner could not tell which pod held a job, and the
// value could not be used to fence anything across ticks.
func TestWorkerIdentityIsStableWithinAProcess(t *testing.T) {
	_, p := setup(t)
	first := p.workerIdentity()
	require.NotEmpty(t, first)
	require.LessOrEqual(t, len(first), 64, "lease_owner is VARCHAR(64)")
	require.Equal(t, first, p.workerIdentity(), "the identity must not change between ticks")

	_, other := setup(t)
	require.Equal(t, first, other.workerIdentity(),
		"the identity belongs to the process, not to a Project value")
}
