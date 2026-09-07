package project

// P1 round-1 review, the findings both reviewers ranked highest. Each case here
// was written against the shipped code and observed RED before the fix.
//
// The unifying theme is the one P1's own journal names: a two-phase state
// machine turns every existing `status == active` check into a question that has
// to be re-answered, and the ones that answer wrongly fail silently in the
// safe-looking direction. Four of the six below are exactly that.

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seatState reads the two columns the two-phase close is made of.
func seatState(t *testing.T, projectID, uid string) (status int, removing int) {
	t.Helper()
	type row struct {
		Status   int `db:"status"`
		Removing int `db:"removing"`
	}
	var rows []row
	_, err := testCtx.DB().SelectBySql(
		"SELECT status, removing FROM `octo_project_member` WHERE project_id = ? AND uid = ?",
		projectID, uid).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1, "expected exactly one seat row for %s/%s", projectID, uid)
	return rows[0].Status, rows[0].Removing
}

// beginRemovalWithoutDraining puts a seat into the closing state and leaves the
// job in the queue, which is the window every case below is about.
func beginRemovalWithoutDraining(t *testing.T, p *Project, projectID, uid string) {
	t.Helper()
	changed, err := p.removeMember(projectID, spaceA, "owner1", uid)
	require.NoError(t, err)
	require.True(t, changed)
	status, removing := seatState(t, projectID, uid)
	require.Equal(t, MemberStatusActive, status, "phase one must leave status at 1")
	require.Equal(t, 1, removing, "phase one must set removing")
}

// pendingJobsFor counts outstanding cleanup jobs for one seat.
func pendingJobsFor(t *testing.T, projectID, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member_removal_cleanup` "+
			"WHERE project_id = ? AND uid = ? AND status = ?",
		projectID, uid, removalJobPending).LoadOne(&n))
	return n
}

// ---------- HIGH-1 ----------

// TestSpaceCascadeDoesNotStrandASeatAtRemovingOne.
//
// db_removal.go's own state table says `status=0 removing=1` MUST NOT EXIST, and
// the stall scan is written to report it. deactivateMemberTx set status without
// touching removing, so the Space cascade — which closes project seats for a uid
// leaving the Space — produced that state on any seat whose project-side removal
// was still in flight.
//
// The consequence is not cosmetic. finishMemberRemovalTx is guarded on
// `status = 1 AND removing = 1`, so it can never match such a row: the job runs
// its steps, affects zero rows, and the seat sits at removing = 1 forever while
// scanRemovingStalls alerts on it every tick with nothing an operator can do.
func TestSpaceCascadeDoesNotStrandASeatAtRemovingOne(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")
	beginRemovalWithoutDraining(t, p, created.ProjectID, "member1")

	// The uid now leaves the Space. deactivateSeatForCascade re-checks the Space
	// seat under lock and skips a member who still holds one, so the seat has to
	// actually be closed for the cascade to reach the write under test.
	removeSpaceMember(t, spaceA, "member1")
	_, err := p.deactivateSeatForCascade(created.ProjectID, spaceA, "member1", "op", "space_removed")
	require.NoError(t, err)

	status, removing := seatState(t, created.ProjectID, "member1")
	assert.Equal(t, MemberStatusRemoved, status, "the Space cascade must close the seat")
	assert.Equal(t, 0, removing,
		"status=0 with removing=1 is the state db_removal.go documents as impossible: "+
			"finishMemberRemovalTx can never match it, so the seat would stall forever and "+
			"the stall scan would alert on it with no remedy")
	assert.Zero(t, pendingJobsFor(t, created.ProjectID, "member1"),
		"the job must be retired in the same transaction that closed the seat — leaving it "+
			"pending burns a lease and an attempt to discover work that no longer exists, "+
			"which is exactly what cancelPendingRemovalJobsTx exists to avoid")
}

// TestDisbandDoesNotStrandASeatAtRemovingOne is the same defect on the other
// writer: disbandProjectTx closes every seat in one UPDATE and had the same
// omission.
func TestDisbandDoesNotStrandASeatAtRemovingOne(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "member1")
	beginRemovalWithoutDraining(t, p, created.ProjectID, "member1")

	w := doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	status, removing := seatState(t, created.ProjectID, "member1")
	assert.Equal(t, MemberStatusRemoved, status)
	assert.Equal(t, 0, removing,
		"disband must clear removing on the seats it closes, for the same reason the Space "+
			"cascade must")
	assert.Zero(t, pendingJobsFor(t, created.ProjectID, "member1"))
	_ = p
}

// ---------- HIGH-2 ----------

// TestCascadeSkipsADisbandedProjectGroup.
//
// queryProjectGroupNosWithActiveMember had no group-status filter, so a
// DISBANDED project group the departing member still has a row in was handed to
// RemoveGroupMembers — which refuses a disbanded group outright ("group not
// found or disbanded"). Every attempt failed identically, so the job burned all
// eight attempts, was marked abandoned (terminal), and left the seat stuck at
// removing = 1 permanently. One disbanded group anywhere in a project was enough
// to break removal for every member still in it.
//
// Skipping is the right answer rather than "remove them anyway": a disbanded
// group grants nothing, its rows are left in place by group disband itself, and
// there is no API that would clean them up.
func TestCascadeSkipsADisbandedProjectGroup(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")

	live := "g-live-" + util.GenerUUID()[:8]
	dead := "g-dead-" + util.GenerUUID()[:8]
	seedProjectGroupWithMembers(t, live, spaceA, created.ProjectID, "owner1", "owner1", "member1")
	seedProjectGroupWithMembers(t, dead, spaceA, created.ProjectID, "owner1", "owner1", "member1")
	// 2 is modules/group's GroupStatusDisband. Spelled out because modules/project
	// must not import modules/group.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = 2 WHERE group_no = ?", dead).Exec()
	require.NoError(t, err)

	beginRemovalWithoutDraining(t, p, created.ProjectID, "member1")
	drainRemovalCascade(t, p)

	status, removing := seatState(t, created.ProjectID, "member1")
	assert.Equal(t, MemberStatusRemoved, status,
		"the removal must complete: a disbanded group in the project must not be able to "+
			"abandon the job and strand the seat")
	assert.Equal(t, 0, removing)
	assert.False(t, groupMemberActive(t, live, "member1"),
		"the live project group must still be cleaned up")
}

// ---------- HIGH-3 ----------

// TestI2ScanExaminesEveryMemberOfAGroupThatSpansAPage.
//
// The I2 cursor advanced to the LAST GROUP ID the page touched, so the next page
// started strictly after that group — and any member rows of that group beyond
// the page boundary were never examined. The comment claimed this was
// "self-correcting: the next rotation sees them", which is false: the pages fall
// on the same boundary every rotation, so the same members are skipped forever.
//
// A group with more members than one page, whose LAST member (by uid order) is
// not in the project, is the sharpest form: the one row that matters is the one
// that is always cut off.
func TestI2ScanExaminesEveryMemberOfAGroupThatSpansAPage(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")

	// uid ordering matters: the intruder must sort last, so it lands past the
	// page boundary rather than inside the first page.
	groupNo := "g-span-" + util.GenerUUID()[:8]
	seedProjectGroupWithMembers(t, groupNo, spaceA, created.ProjectID, "owner1",
		"owner1", "member1", "zz-intruder")

	p.cfg.ReconcileLimit = 2 // strictly fewer than the group's three members
	resetCursorsForTest()
	p.scanI2Violations()

	assert.Equal(t, float64(1), promtestutil.ToFloat64(i2Violations),
		"the member past the page boundary must be examined; with the old cursor the scan "+
			"jumped over the rest of the group and reported zero violations forever")
}

// ---------- HIGH-4 ----------

// TestAClosingSeatCannotAdministerTheProject.
//
// actorRoleTx returned the stored role for any row with status = 1, so an owner
// whose seat was closing kept every privilege — including disband, the most
// destructive operation in the module — for the whole cascade window. Every
// other authorization read in P1 (pkg/project's three predicates,
// countActiveOwnersTx, listMembers) treats removing = 1 as a non-member; this
// one did not, and it is the one that decides who may destroy the project.
func TestAClosingSeatCannotAdministerTheProject(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "member1")

	// Close the owner's own seat. Written directly because no endpoint lets an
	// owner start their own removal without first transferring — the state is
	// reachable through the Space cascade, and it is the state under test.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 1 WHERE project_id = ? AND uid = ?",
		created.ProjectID, "owner1").Exec()
	require.NoError(t, err)
	flushProjectCache(t, testCtx)

	// Driven at the SERVICE layer on purpose. projectMiddleware already refuses a
	// closing seat, because it resolves the role through pkg/project.MemberRole
	// which carries `removing = 0` — so an HTTP-level test would pass with or
	// without this fix and prove nothing. actorRoleTx exists precisely because
	// that middleware answer came from a cache read taken before the transaction
	// opened; it is the in-transaction re-read, and it is the last thing standing
	// between a stale positive and a disbanded project.
	_, err = p.disbandProject(created.ProjectID, "owner1", spaceA)
	assert.ErrorIs(t, err, errPermissionDenied,
		"actorRoleTx must treat a closing seat as a non-member: it is the re-read that "+
			"catches a role the middleware's cache got wrong, on the most destructive "+
			"operation in the module")

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, StatusNormal, row.Status, "the project must survive the refused disband")
}

// ---------- HIGH-6 ----------

// TestLastOwnerCannotTransferToAClosingMember.
//
// promoteSuccessorTx accepted any successor with status = 1, while
// countActiveOwnersTx already excludes removing = 1. The two together produce
// the exact outcome the last-owner guard exists to prevent: the departing owner
// hands ownership to a seat that is on its way out, the cascade closes it
// moments later, and the project is left ACTIVE WITH ZERO OWNERS — unmanageable,
// with nothing in P0 or P1 able to promote a member without an owner.
func TestLastOwnerCannotTransferToAClosingMember(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "member1")
	beginRemovalWithoutDraining(t, p, created.ProjectID, "member1")

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
		ownerTok, map[string]any{"transfer_to": "member1"})
	assert.NotEqual(t, http.StatusOK, w.Code,
		"the sole owner must not be allowed to transfer to a seat that is closing: body=%s",
		w.Body.String())

	status, removing := seatState(t, created.ProjectID, "owner1")
	assert.Equal(t, MemberStatusActive, status, "the owner must still hold their seat")
	assert.Equal(t, 0, removing)
}
