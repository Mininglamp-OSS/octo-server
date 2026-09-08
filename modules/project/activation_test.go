package project

import (
	"context"
	"database/sql"
	"testing"
	"time"

	pkgproject "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two-phase create (O6, docs/project-lifecycle-contract.md section 7).
//
// The property under test is a NEGATIVE one: between create and the subsystem
// confirmation, the peer control plane must answer about the project exactly as
// it answers about one that does not exist. A test that only checked the happy
// path would pass with the gate deleted.

// activatedAtOf reads the latch.
//
// sql.NullTime rather than *time.Time: dbr materializes a NULL into a non-nil
// pointer at the zero time, so a `*time.Time` result cannot distinguish "not
// confirmed" from "confirmed at year zero" — and the first version of this
// helper reported every unlatched project as latched.
func activatedAtOf(t *testing.T, projectID string) *time.Time {
	t.Helper()
	var rows []sql.NullTime
	_, err := testCtx.DB().SelectBySql(
		"SELECT activated_at FROM `octo_project` WHERE project_id = ?", projectID,
	).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1, "no project row for %s", projectID)
	if !rows[0].Valid {
		return nil
	}
	at := rows[0].Time
	return &at
}

// TestCreateActivatesImmediatelyWithNoFleetTarget is the default posture, and it
// is the one that must not regress: with nothing configured to confirm, a
// project that waited would wait forever and be invisible to the peer for its
// whole life.
func TestCreateActivatesImmediatelyWithNoFleetTarget(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "actOwner")
	seedSpaceMember(t, spaceA, "actOwner", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "activate-default")

	require.NotNil(t, activatedAtOf(t, created.ProjectID),
		"with no fleet target nothing would ever confirm this project, so the create must "+
			"latch it immediately — a NULL here is a project the peer can never see")

	epochs, err := pkgproject.ProjectEpochsInSpace(testCtx.DB().NewSession(nil), spaceA,
		[]string{created.ProjectID})
	require.NoError(t, err)
	assert.Contains(t, epochs, created.ProjectID, "the peer must see it right away")
}

// TestCreateWaitsForConfirmationWithFleetEnabled is the gate itself.
func TestCreateWaitsForConfirmationWithFleetEnabled(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-wait")

	require.Nil(t, activatedAtOf(t, created.ProjectID),
		"with the fleet target on, a fresh project must wait for confirmation")

	// The whole point: the peer cannot see it, and cannot authorize anyone into it.
	session := testCtx.DB().NewSession(nil)
	epochs, err := pkgproject.ProjectEpochsInSpace(session, spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.NotContains(t, epochs, created.ProjectID,
		"an unconfirmed project must be indistinguishable from one that does not exist, or "+
			"the peer can grant access into a workspace that is not there yet")

	epoch, roles, err := pkgproject.ProjectMemberships(context.Background(), session, spaceA, created.ProjectID,
		[]string{"owner1"})
	require.NoError(t, err)
	assert.Zero(t, epoch, "epoch 0, the same answer as a nonexistent project")
	assert.Empty(t, roles,
		"the CREATOR is an owner in this repository and must still read as a non-member to "+
			"the peer while the project is unconfirmed")

	// And the confirmation flips it.
	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.Equal(t, provisionStatusReady, rows[0].Status, "last_error=%q", rows[0].LastError)

	require.NotNil(t, activatedAtOf(t, created.ProjectID), "a ready fleet job must latch the project")
	epochs, err = pkgproject.ProjectEpochsInSpace(session, spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	assert.Contains(t, epochs, created.ProjectID, "confirmed projects are visible")
	_, roles, err = pkgproject.ProjectMemberships(context.Background(), session, spaceA, created.ProjectID,
		[]string{"owner1"})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"owner1": RoleOwner}, roles)
}

// TestFailedProvisioningLeavesTheProjectUnseen: the contract says a confirmation
// that never arrives is terminal plus an alert, NOT an eventual activation.
// Latching on anything other than success would defeat the gate entirely.
func TestFailedProvisioningLeavesTheProjectUnseen(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-fail")

	p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.NotEqual(t, provisionStatusReady, rows[0].Status)

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"a failed confirmation must NOT latch the project: the gate exists precisely for "+
			"the case where the container was not created")
}

// TestDriveReadyDoesNotActivate: drive is storage. It says nothing about whether
// the peer control plane may act on the project, and treating it as confirmation
// would open the gate on a deployment that runs drive without fleet.
func TestDriveReadyDoesNotActivate(t *testing.T) {
	fleet := newFakeTarget(t)
	drive := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet, drive)
	created := createVia(t, r, token, "activate-drive")

	// Take fleet out of the picture without touching the enqueued rows, so only
	// the drive job can run this tick.
	p.cfg.Provisioning.Targets = []provisionTarget{driveTargetOn(drive)}
	p.processProvisioningJobs()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"a ready DRIVE job must not activate the project; only the fleet confirmation does")
}

// TestActivationIsALatch pins that a second confirmation does not move the
// timestamp. The value answers "when were we first told", and a redelivery
// rewriting it would report the retry instead of the fact.
func TestActivationIsALatch(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-latch")

	p.processProvisioningJobs()
	first := activatedAtOf(t, created.ProjectID)
	require.NotNil(t, first)

	requeueProvisioningRow(t, readProvisioningRows(t, created.ProjectID)[0].ID)
	time.Sleep(5 * time.Millisecond)
	p.processProvisioningJobs()

	second := activatedAtOf(t, created.ProjectID)
	require.NotNil(t, second)
	assert.True(t, first.Equal(*second),
		"activated_at must not move on a redelivery: %v -> %v", first, second)
}

// TestAwaitingActivationCensusCountsOnlyLiveProjects. A disbanded project that
// never activated is finished, not stuck, and counting it would make the gauge
// an operator watches climb forever on a number nobody can act on.
func TestAwaitingActivationCensusCountsOnlyLiveProjects(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-census")

	count, err := p.db.countAwaitingActivation()
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	age, err := p.db.oldestAwaitingActivationAge(time.Now().UTC())
	require.NoError(t, err)
	assert.Positive(t, age.Seconds(), "the age gauge is what an alert watches")

	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET status = ? WHERE project_id = ?",
		StatusDisbanded, created.ProjectID).Exec()
	require.NoError(t, err)

	count, err = p.db.countAwaitingActivation()
	require.NoError(t, err)
	assert.Zero(t, count, "a disbanded project is finished, not stuck")
}

// TestReconcileLatchesAConfirmedButUnlatchedProject covers the gap the reactive
// path cannot: confirmProjectActive runs AFTER the provisioning job is marked
// terminal, and terminal jobs are never re-claimed. So a pod killed between the
// two statements — or a transient error on the latch UPDATE — left a project
// whose container demonstrably exists permanently invisible to the peer, with a
// gauge showing it and nothing able to act on it.
func TestReconcileLatchesAConfirmedButUnlatchedProject(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-repair")

	p.processProvisioningJobs()
	require.NotNil(t, activatedAtOf(t, created.ProjectID))

	// Reproduce the crash window: the job is ready, the latch is not set.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET activated_at = NULL WHERE project_id = ?",
		created.ProjectID).Exec()
	require.NoError(t, err)
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition")

	p.scanUnlatchedActivations()

	require.NotNil(t, activatedAtOf(t, created.ProjectID),
		"a project whose fleet job reached ready must be latched by the repair scan; "+
			"nothing else will ever come back to a terminal job")
}

// TestReconcileDoesNotLatchAnUnconfirmedProject is the half that makes the
// repair safe. Latching on anything short of a clean ensure would defeat the
// gate entirely — it would just delay activation by one reconcile interval.
func TestReconcileDoesNotLatchAnUnconfirmedProject(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "activate-norepair")

	p.processProvisioningJobs()
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition: the ensure failed")

	p.scanUnlatchedActivations()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"the repair scan must latch only on a READY fleet job: latching on a failed or "+
			"pending one would make the gate a delay rather than a gate")
}

// TestDriveReadyDoesNotSatisfyTheRepair: drive is storage and says nothing about
// whether the peer may act on the project. The repair must be as narrow as the
// reactive path it backs up.
func TestDriveReadyDoesNotSatisfyTheRepair(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500)
	drive := newFakeTarget(t)
	// BOTH targets stay configured. Dropping fleet from the config to isolate the
	// drive job would change which repair runs at all — with no fleet target
	// nothing can ever confirm the project, and the straggler branch latches it on
	// purpose. Keeping fleet enabled but failing is what puts the confirmation
	// repair, and only it, under test.
	p, r, token := provisioningSetup(t, fleet, drive)
	created := createVia(t, r, token, "activate-repair-drive")

	p.processProvisioningJobs()
	byTarget := map[string]uint8{}
	for _, row := range readProvisioningRows(t, created.ProjectID) {
		byTarget[row.Target] = row.Status
	}
	require.Equal(t, provisionStatusReady, byTarget[TargetDrive], "precondition: drive succeeded")
	require.NotEqual(t, provisionStatusReady, byTarget[TargetFleet], "precondition: fleet did not")
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition")
	require.True(t, p.twoPhaseCreateApplies(), "precondition: the fleet target is enabled")

	p.scanUnlatchedActivations()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"a ready DRIVE job must not satisfy the repair: drive is storage and says nothing "+
			"about whether the peer control plane may act on the project")
}

// TestRollingUpgradeStragglersGetLatched is P1-4: the case with no confirming
// step at all.
//
// activated_at is a column with a ONE-SHOT backfill. Between the first pod
// applying the migration and the last old pod draining, old binaries insert
// through the previous column list — which does not name it — so those rows land
// NULL after the backfill has already run, and both inbound endpoints then hide
// them from the peer.
//
// The confirmation repair cannot reach them: it needs a ready fleet provisioning
// row, and with the fleet target off none is ever written. So without this they
// are invisible to the peer forever and awaiting_activation climbs monotonically,
// with hand-written SQL as the only remedy.
func TestRollingUpgradeStragglersGetLatched(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "stragglerOwner")
	seedSpaceMember(t, spaceA, "stragglerOwner", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "straggler")
	// Exactly what an old binary leaves behind: the column exists, the row does
	// not name it, so it is NULL after the backfill has run.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET activated_at = NULL WHERE project_id = ?",
		created.ProjectID).Exec()
	require.NoError(t, err)
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition")
	require.False(t, p.twoPhaseCreateApplies(), "precondition: nothing will confirm it")

	p.scanUnlatchedActivations()

	require.NotNil(t, activatedAtOf(t, created.ProjectID),
		"with the fleet target off nothing will ever confirm this project, so the scan must "+
			"latch it — the alternative is not 'confirmed later', it is invisible forever")
}

// TestStragglerRepairDoesNotFireWhileSomethingCanStillConfirm keeps the two
// repairs apart. Latching unconditionally would turn the gate into a delay of
// one reconcile interval rather than a gate.
func TestStragglerRepairDoesNotFireWhileSomethingCanStillConfirm(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "straggler-gated")

	p.processProvisioningJobs()
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition: the ensure failed")
	require.True(t, p.twoPhaseCreateApplies(), "precondition: the fleet target is on")

	p.scanUnlatchedActivations()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"while the fleet target is enabled the straggler repair must NOT run: a confirmation "+
			"is still possible, and latching here would make the gate a delay")
}
