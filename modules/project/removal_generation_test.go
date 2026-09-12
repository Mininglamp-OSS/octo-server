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
// End state under the current contract: the Project seat closes, while native group membership
// and the group's Project relation remain unchanged. Project removal does not own native chat
// membership; group-side operations are the only code allowed to mutate those rows.
//
// The interleaving is made deterministic with a row lock rather than a sleep,
// the same way modules/group pins its handover race.

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

// seedProjectGroupWithMember builds a native Project-attributed group plus one
// native member row for the Project-removal boundary regression.
//
// Raw SQL because modules/project cannot import modules/group — that import edge
// is the boundary under test. The rows are the same shape the group module's own
// fixtures write.
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

// TestProjectRemovalLeavesNativeGroupMembershipAndRelationUntouched is the Project/group
// boundary regression. Closing a Project seat must not remove a native group member or clear
// the group's project_id relation.
func TestProjectRemovalLeavesNativeGroupMembershipAndRelationUntouched(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "member1")
	groupNo := seedProjectGroupWithMember(t, spaceA, created.ProjectID, "member1")

	w := doJSON(t, srv, "POST", "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"member1"}})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	drainRemovalCascade(t, p)

	status, removing := seatState(t, created.ProjectID, "member1")
	require.Equal(t, MemberStatusRemoved, status)
	require.Zero(t, removing)
	require.True(t, groupMemberIsActive(t, groupNo, "member1"),
		"Project removal must not mutate native group membership")

	var relation string
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT project_id FROM `group` WHERE group_no = ?", groupNo).LoadOne(&relation))
	require.Equal(t, created.ProjectID, relation,
		"Project removal must not clear the native group's Project relation")
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
	require.True(t, groupMemberIsActive(t, g1, "member1"),
		"Project seat closure must not mutate native group membership")

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
