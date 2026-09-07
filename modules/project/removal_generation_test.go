package project

// The seat close belongs to ONE removal, not to whichever removal is in flight.
//
// PR #846's round-3 review found that neither of the worker's decision points
// carried the job's identity: `finishMemberRemovalTx`'s WHERE is
// `(project_id, uid, status = 1, removing = 1)` and `finishRemoval` treated any
// `removing = 1` as "mine, safe to close". The seat carries no removal
// generation, so a worker that was still fanning out while the member was
// re-admitted, joined another of the project's groups, and then removed AGAIN
// came back to cycle 2's flag and closed it. Cycle 2's own job then found
// `removing = 0`, concluded it had no work, and retired without running a step.
//
// End state: the project says not-a-member while the group joined between the
// cycles keeps an active `group_member` row — and because I2 has no read-path
// filter, that uid keeps full access to it. The I2 scan reports it and nothing
// repairs it: both jobs are terminal and no endpoint re-drives a cascade. Every
// log line on the way there reads like a designed path.
//
// The interleaving is made deterministic with a row lock rather than a sleep,
// the same way modules/group pins its handover race.

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

// seedProjectGroupWithMember builds a project group plus one member row, which
// is all queryProjectGroupNosWithActiveMember needs to find it.
//
// Raw SQL because modules/project cannot import modules/group — that import
// edge is the one P1 keeps at zero. The rows are the same shape the group
// module's own fixtures write.
func seedProjectGroupWithMember(t *testing.T, spaceID, projectID, uid string) string {
	t.Helper()
	groupNo := util.GenerUUID()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, space_id, project_id) "+
			"VALUES (?, 'g', '', 1, 1, ?, ?)", groupNo, spaceID, projectID).Exec()
	require.NoError(t, err)
	for _, m := range []struct {
		uid  string
		role int
	}{{"owner1", 1}, {uid, 0}} {
		_, err = testCtx.DB().InsertBySql(
			"INSERT INTO group_member (group_no, uid, remark, role, `version`, status, vercode, "+
				"is_deleted, invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
				"VALUES (?, ?, '', ?, 1, 1, ?, 0, '', 0, 0, 0, '', NOW())",
			groupNo, m.uid, m.role, util.GenerUUID()).Exec()
		require.NoError(t, err)
	}
	return groupNo
}

func groupMemberIsActive(t *testing.T, groupNo, uid string) bool {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no = ? AND uid = ? AND is_deleted = 0",
		groupNo, uid).LoadOne(&n))
	return n > 0
}

// waitForCascadeToPark blocks until the cascade is parked on the member row this
// test holds. The signal is the server's own process list: a `group_member …
// FOR UPDATE` executing for a second or more is the cascade waiting on us, and
// nothing else in this test runs such a statement.
func waitForCascadeToPark(t *testing.T, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("the cascade finished without ever blocking on the held member row: " +
				"this test's premise is gone, so it would prove nothing")
		default:
		}
		var n int
		require.NoError(t, testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM information_schema.processlist "+
				"WHERE command = 'Query' AND time >= 1 "+
				"  AND info LIKE '%group_member%' AND info LIKE '%FOR UPDATE%'").LoadOne(&n))
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the cascade never parked on the held member row within 30s")
}

// TestAStaleCascadeDoesNotCloseTheNextRemovalsSeat is the interleaving.
func TestAStaleCascadeDoesNotCloseTheNextRemovalsSeat(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")
	projectID := created.ProjectID

	g1 := seedProjectGroupWithMember(t, spaceA, projectID, "member1")

	// Cycle 1.
	beginRemovalWithoutDraining(t, p, projectID, "member1")
	jobs, err := p.db.claimRemovalJobs("worker-a", removalBatch, time.Now().UTC(), removalLease)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	j1 := jobs[0]

	// Park the cascade inside G1's removal.
	blocker, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer blocker.RollbackUnlessCommitted()
	var held []string
	_, err = blocker.SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND uid = ? FOR UPDATE",
		g1, "member1").Load(&held)
	require.NoError(t, err)
	require.Len(t, held, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.workRemovalJob(j1, "worker-a")
	}()
	waitForCascadeToPark(t, done)

	// While worker A is parked: re-admit, join another project group, remove again.
	readded, err := p.addOneMemberOnce(projectID, spaceA, "owner1", "member1")
	require.NoError(t, err)
	require.True(t, readded, "the re-admission must land while the cascade is in flight")
	g2 := seedProjectGroupWithMember(t, spaceA, projectID, "member1")
	removedAgain, err := p.removeMember(projectID, spaceA, "owner1", "member1")
	require.NoError(t, err)
	require.True(t, removedAgain, "cycle 2 must open its own removal")

	require.NoError(t, blocker.Commit())
	<-done

	// The fix: worker A's job is cycle 1's, so it must NOT close cycle 2's seat.
	status, removing := seatState(t, projectID, "member1")
	require.Equal(t, MemberStatusActive, status,
		"worker A ran cycle 1's job; closing the seat here closes CYCLE 2's removal, "+
			"and cycle 2's own job then retires without detaching anything")
	require.Equal(t, 1, removing, "cycle 2's removal must still be in progress")

	// And cycle 2's job, when it runs, must do the work — including for the group
	// joined between the cycles, which worker A's snapshot never contained.
	drainRemovalCascade(t, p)

	status, removing = seatState(t, projectID, "member1")
	require.Equal(t, MemberStatusRemoved, status, "cycle 2 must close the seat")
	require.Zero(t, removing)
	require.False(t, groupMemberIsActive(t, g2, "member1"),
		"the group joined between the two removals must not keep an active member row: "+
			"I2 has no read-path filter, so that row is full access for a uid the "+
			"project says is gone")
	require.False(t, groupMemberIsActive(t, g1, "member1"))
}

// TestAJobThatOwnsTheRemovalStillClosesTheSeat is the control: the fence must
// refuse a stale job without refusing the ordinary one.
func TestAJobThatOwnsTheRemovalStillClosesTheSeat(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")
	projectID := created.ProjectID
	g1 := seedProjectGroupWithMember(t, spaceA, projectID, "member1")

	beginRemovalWithoutDraining(t, p, projectID, "member1")
	jobs, err := p.db.claimRemovalJobs("worker-a", removalBatch, time.Now().UTC(), removalLease)
	require.NoError(t, err)
	require.Len(t, jobs, 1)

	p.workRemovalJob(jobs[0], "worker-a")

	status, removing := seatState(t, projectID, "member1")
	require.Equal(t, MemberStatusRemoved, status, "an uninterrupted cascade must close its seat")
	require.Zero(t, removing)
	require.False(t, groupMemberIsActive(t, g1, "member1"))

	jobStatus, _ := jobRow(t, jobs[0].ID)
	require.Equal(t, removalJobDone, jobStatus)
}

// TestTheSeatCloseIsFencedOnTheJob pins the predicate directly, without the
// timing: a job that is no longer pending-and-ours must not close a seat that
// reads removing = 1.
func TestTheSeatCloseIsFencedOnTheJob(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")
	projectID := created.ProjectID

	beginRemovalWithoutDraining(t, p, projectID, "member1")
	jobs, err := p.db.claimRemovalJobs("worker-a", removalBatch, time.Now().UTC(), removalLease)
	require.NoError(t, err)
	require.Len(t, jobs, 1)

	// Retire the job out from under the worker, exactly as re-admission does.
	held, err := p.db.completeRemovalJob(jobs[0].ID, "worker-a", removalJobCancelled,
		"re-admitted", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, held)

	closed, err := p.finishRemoval(jobs[0], "worker-a")
	require.NoError(t, err, "a seat that is not ours to close is not an error")
	require.False(t, closed)

	status, removing := seatState(t, projectID, "member1")
	require.Equal(t, MemberStatusActive, status)
	require.Equal(t, 1, removing, "the flag belongs to whoever opened it, and stays theirs")
}
