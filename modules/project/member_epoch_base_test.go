package project

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The value 0 is reserved: the integration contract that consumes member_epoch
// defines it as "the project does not exist or is not visible". These tests pin
// that no real project can hold it, which is the only thing that makes that
// meaning true.
//
// They run against a real row rather than a stub on purpose. The collision they
// guard lived in the column default and the insert path, so a fake store would
// have reported whatever it was handed and agreed with itself.

// TestFreshProjectEpochIsNeverTheAbsentSentinel is the direct assertion.
//
// The failure it prevents: a solo project nobody has added to reports the same
// epoch as a disbanded one. A consumer caches a positive authorization answer
// under that epoch, the project is later disbanded, the epoch query filters on
// status and answers 0 for the now-absent project — and 0 equals the cached 0,
// so the consumer's staleness check AGREES and the grant survives. Forever,
// because the epoch never moves again.
func TestFreshProjectEpochIsNeverTheAbsentSentinel(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochBase")
	seedSpaceMember(t, spaceA, "epochBase", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "epoch-base")
	require.NotEmpty(t, created.ProjectID)

	row, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)

	assert.Equal(t, StatusNormal, row.Status, "a freshly created project is active")
	assert.NotZero(t, row.MemberEpoch,
		"member_epoch 0 is the integration contract's sentinel for a project that does not "+
			"exist; an active project holding it makes a consumer's staleness check agree with "+
			"a disbanded project's answer")
	assert.EqualValues(t, 1, row.MemberEpoch, "creation starts the epoch at 1")

	// The response carries the same value; a client and the peer must not see
	// different epochs for the same project.
	assert.EqualValues(t, row.MemberEpoch, created.MemberEpoch,
		"the response epoch must match the stored one")

	members, err := testDB.countActiveMembers(created.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, 1, members, "the owner seat exists, so this is a real project with a real member")
}

// TestEpochStillIncrementsFromTheNewBase checks the change did not cost the
// property the epoch exists for: it must still move on every membership write.
//
// Starting at 1 rather than 0 is only safe if 1 is a floor and not a plateau —
// if creation had been made to set the epoch and a later write had stopped
// bumping it, this suite would still have seen a non-zero value and passed.
func TestEpochStillIncrementsFromTheNewBase(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochBump")
	seedSpaceMember(t, spaceA, "epochBump", 0, 1)
	seedUser(t, "epochBumpTarget")
	seedSpaceMember(t, spaceA, "epochBumpTarget", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "epoch-bump")
	before, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		token, addMembersPayload("epochBumpTarget"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	after, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.Greater(t, after.MemberEpoch, before.MemberEpoch,
		"a membership write must still advance the epoch from its new base")
}

// TestCreateBumpsTheEpochRatherThanSeedingIt is the source-side half.
//
// The value has to be moved off the column default by creation itself, because
// the default is the reserved sentinel — and it has to be moved by the shared
// INCREMENT rather than seeded in the insert column list, which is the shape the
// write-discipline guards forbid. (An earlier draft of this fix did seed the
// column; `TestIsOfficialHasNoWriter` rejected it, and this header described that
// rejected draft for one round.) A future edit that drops the bump would silently
// restore the collision on newly created projects only — invisible to any test
// that does not look at a fresh row, which is why this pins the mechanism as well
// as the outcome.
func TestCreateBumpsTheEpochRatherThanSeedingIt(t *testing.T) {
	src := readLinesWithoutComments(t, "service.go")

	// The transaction body, not createProjectOnce, since #887 split the two: the
	// outer function now prepares the seat refs, opens the transaction, commits and
	// runs the post-commit hooks, while every statement this guard is about lives in
	// createProjectTxWithSeatRefs. The delegation is asserted first so a later split
	// cannot leave this guard reading a function that no longer performs the create —
	// the way it silently would have read an empty createProjectOnce after the merge.
	outer := funcBody(t, src, "func (p *Project) createProjectOnce(")
	require.Contains(t, outer, "createProjectTxWithSeatRefs",
		"createProjectOnce must delegate the create transaction to createProjectTxWithSeatRefs; "+
			"if that changed, point the assertions below at whatever function now holds the "+
			"project insert, rather than letting them pass against a body that has neither")

	create := funcBody(t, src, "func (p *Project) createProjectTxWithSeatRefs(")
	assert.True(t, strings.Contains(create, "bumpMemberEpochTx"),
		"the create transaction must move the epoch off the column default, or every new project "+
			"reports the value the integration contract reserves for a project that does not exist")

	// Via the shared increment, NOT by seeding the column at insert. member_epoch
	// may only ever be written as member_epoch + 1 — TestIsOfficialHasNoWriter
	// forbids it in the insert column list and TestMemberEpochOnlyEverIncrements
	// forbids every other write shape. Fixing the sentinel collision must not
	// come at the cost of that discipline, so this pins which mechanism was used.
	for _, col := range projectInsertColumns {
		assert.NotEqual(t, "member_epoch", col,
			"member_epoch must not be seeded through the insert column list")
	}

	// The bump has to follow the project insert: bumpMemberEpochTx is guarded on
	// status = StatusNormal, so before the row exists it matches nothing and
	// silently does nothing — a failure with no error and no test, unless pinned.
	insertAt := strings.Index(create, "insertProjectTx")
	bumpAt := strings.Index(create, "bumpMemberEpochTx")
	require.Greater(t, insertAt, -1, "the create transaction must insert the project row")
	require.Greater(t, bumpAt, insertAt,
		"the epoch bump must come AFTER the project insert, or its status guard matches no row")

	// And the bump's affected-row count must be CHECKED here, not discarded. The
	// status guard means "no error" and "it happened" are different facts, and a
	// silent zero-row bump would ship a project on the reserved absent sentinel
	// while the response reports 1. Every other call site may ignore the count —
	// a no-op on a disbanded project is intended there.
	assert.Contains(t, create, "bumped == 0",
		"the create transaction must check that the epoch bump matched a row; discarding the count "+
			"turns the one case where a no-op is a security state into a silent wrong answer")
}

// ---------- the invariant after the migration instant ----------

// forceAbsentSentinelEpoch puts a project back on the reserved value, which is
// what a not-yet-upgraded pod (mid rolling deploy) or a rolled-back binary does
// by simply inserting at the column default.
//
// An absolute assignment, deliberately: it is the shape production code may never
// use, and reproducing the hazard requires producing exactly it. Test files are
// outside TestMemberEpochOnlyEverIncrements' scan for that reason.
func forceAbsentSentinelEpoch(t *testing.T, projectID string) {
	t.Helper()
	_, err := testDB.session.UpdateBySql(
		"UPDATE `octo_project` SET `member_epoch` = 0 WHERE `project_id` = ?", projectID,
	).Exec()
	require.NoError(t, err)
}

func forceStatus(t *testing.T, projectID string, status int) {
	t.Helper()
	_, err := testDB.session.UpdateBySql(
		"UPDATE `octo_project` SET `status` = ? WHERE `project_id` = ?", status, projectID,
	).Exec()
	require.NoError(t, err)
}

// TestReconcileRepairsAnActiveProjectBackOnTheSentinel is the durability half of
// the fix, and the reason the migration alone is not enough.
//
// The migration enforces "no active project holds 0" at ONE instant: the boot
// that applies it. Two windows re-open it — a rolling deploy where old pods still
// insert at the column default after the backfill has run, and the rollback the
// migration's own Down section describes, which restores that create path
// indefinitely while the ledger row prevents the backfill from ever re-running.
// Neither is caught by anything else: the negative-value and regression checks in
// this scan do not look at 0, and no other query in the repository asks for the
// `status = 1 AND member_epoch = 0` shape.
func TestReconcileRepairsAnActiveProjectBackOnTheSentinel(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochRepair")
	seedSpaceMember(t, spaceA, "epochRepair", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "epoch-repair")
	require.NotEmpty(t, created.ProjectID)
	forceAbsentSentinelEpoch(t, created.ProjectID)

	// Precondition, asserted rather than assumed: without it a repair that never
	// ran would be indistinguishable from one that had nothing to do.
	before, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.EqualValues(t, 0, before.MemberEpoch, "the hazard must actually be staged")

	resetCursorsForTest()
	p.scanEpochSanity()

	after, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, after.MemberEpoch,
		"an ACTIVE project sitting on the contract's absent sentinel must be lifted off it")

	// Idempotent: a second rotation finds nothing to do, so a deployment that is
	// merely slow to converge does not churn the epoch (and every churn costs the
	// consumer a re-verify).
	resetCursorsForTest()
	p.scanEpochSanity()
	again, err := testDB.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, again.MemberEpoch,
		"the repair must be a one-shot per row, not a bump on every rotation")
}

// TestReconcileLeavesHealthyAndDisbandedEpochsAlone is the bounding half.
//
// A repair that fires too widely is worse than the hazard: every bump invalidates
// a consumer's cached authorization, so an over-broad predicate turns a scheduled
// scan into a re-verify storm. And a DISBANDED project on 0 is CORRECT — the
// endpoint filters it out and answers 0 for it either way, so lifting it would be
// a write with no reader.
func TestReconcileLeavesHealthyAndDisbandedEpochsAlone(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochLeave")
	seedSpaceMember(t, spaceA, "epochLeave", 0, 1)
	seedUser(t, "epochLeaveTarget")
	seedSpaceMember(t, spaceA, "epochLeaveTarget", 0, 1)

	// A healthy project, moved past the base by a real roster write.
	healthy := createProjectVia(t, srv, spaceA, token, "epoch-healthy")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+healthy.ProjectID+"/members/add",
		token, addMembersPayload("epochLeaveTarget"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	healthyBefore, err := testDB.queryByProjectID(healthy.ProjectID)
	require.NoError(t, err)
	require.Greater(t, healthyBefore.MemberEpoch, int64(1), "the healthy row must be past the base")

	// A disbanded project parked on the sentinel.
	gone := createProjectVia(t, srv, spaceA, token, "epoch-disbanded")
	forceAbsentSentinelEpoch(t, gone.ProjectID)
	forceStatus(t, gone.ProjectID, StatusDisbanded)

	resetCursorsForTest()
	p.scanEpochSanity()

	healthyAfter, err := testDB.queryByProjectID(healthy.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, healthyBefore.MemberEpoch, healthyAfter.MemberEpoch,
		"a project already past the sentinel must not be touched")

	goneAfter, err := testDB.queryByProjectID(gone.ProjectID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, goneAfter.MemberEpoch,
		"a disbanded project reads as 0 through the endpoint anyway; repairing it would be a "+
			"write with no reader, and the predicate must stay pinned to status = 1")
}

// TestMigrationBackfillLiftsOnlyTheSentinelRows executes the migration statement
// itself against real rows.
//
// The Up/Down lap proves the file applies; it does not prove WHAT it does, and
// this statement is the one carrying the security fix. Three properties, one
// engine run each: bounded (a row past the base is untouched), effective (a
// sentinel row is lifted by exactly one), idempotent (a re-run matches nothing).
func TestMigrationBackfillLiftsOnlyTheSentinelRows(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "epochBackfill")
	seedSpaceMember(t, spaceA, "epochBackfill", 0, 1)
	seedUser(t, "epochBackfillTarget")
	seedSpaceMember(t, spaceA, "epochBackfillTarget", 0, 1)

	sentinel := createProjectVia(t, srv, spaceA, token, "epoch-backfill-zero")
	forceAbsentSentinelEpoch(t, sentinel.ProjectID)

	past := createProjectVia(t, srv, spaceA, token, "epoch-backfill-past")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+past.ProjectID+"/members/add",
		token, addMembersPayload("epochBackfillTarget"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	pastBefore, err := testDB.queryByProjectID(past.ProjectID)
	require.NoError(t, err)

	backfill := migrationBackfillStatement(t)
	runBackfill := func() {
		_, execErr := testDB.session.UpdateBySql(backfill).Exec()
		require.NoError(t, execErr)
	}

	runBackfill()
	lifted, err := testDB.queryByProjectID(sentinel.ProjectID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, lifted.MemberEpoch, "a sentinel row must be lifted to 1")
	untouched, err := testDB.queryByProjectID(past.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, pastBefore.MemberEpoch, untouched.MemberEpoch,
		"the backfill is bounded to rows still on the sentinel")

	runBackfill()
	again, err := testDB.queryByProjectID(sentinel.ProjectID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, again.MemberEpoch,
		"re-running the backfill must be a no-op — the predicate no longer matches")
}

// migrationBackfillStatement extracts the Up statement from the shipped file, so
// this test cannot drift from what the deployment actually runs.
func migrationBackfillStatement(t *testing.T) string {
	t.Helper()
	raw, err := sqlFS.ReadFile("sql/20260908000002_project_member_epoch_base_one.sql")
	require.NoError(t, err)
	body := string(raw)
	up := body[:mustIndex(t, body, "-- +migrate Down")]
	var stmt string
	for _, line := range strings.Split(up, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		stmt = strings.TrimSuffix(line, ";")
	}
	require.Contains(t, stmt, "member_epoch",
		"the Up section must still carry exactly one member_epoch statement")
	return stmt
}
