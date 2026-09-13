package project

import (
	"context"
	"database/sql"
	"strings"
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
	// BOUNDED, and the bound is the assertion that matters. "Positive" was the
	// original check and it passed while the query scanned into []time.Time and
	// got back the ZERO time — now.Sub(year zero) is about 2.5 million hours,
	// which is very positive. A project created seconds ago cannot be an hour old.
	assert.Less(t, age, time.Hour,
		"the age must be the row's actual age; an unbounded assertion passes on the "+
			"garbage a failed scan produces, which is how this shipped once already")

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

// TestARejectedFleetConfigDoesNotDemolishTheGate is B-6.
//
// A target absent from cfg.Targets has two causes that want opposite behaviour:
// never configured (nothing will confirm — latch, or the project is invisible
// forever) and configured-but-REJECTED at load (one env fix away from
// confirming). The latch is irreversible, so conflating them made every project
// genuinely awaiting confirmation permanently visible to the peer on the next
// reconcile tick, and fixing the env afterwards could not undo it.
func TestARejectedFleetConfigDoesNotDemolishTheGate(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500)
	p, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "rejected-config")

	p.processProvisioningJobs()
	require.Nil(t, activatedAtOf(t, created.ProjectID),
		"precondition: the ensure failed, so the project is awaiting confirmation")

	// Exactly the cfg shape boot produces when ValidateTarget or
	// checkSecretExclusivity rejects the fleet target: dropped from Targets,
	// recorded in Misconfigured, process boots anyway.
	p.cfg.Provisioning.Targets = nil
	p.cfg.Provisioning.Misconfigured = []string{TargetFleet}

	require.False(t, p.twoPhaseCreateApplies(),
		"a rejected target is not something that will confirm a project created NOW; what "+
			"protects the project below is its existing job row, not this predicate")

	p.scanUnlatchedActivations()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"a rejected fleet configuration must NOT latch a project that is already awaiting "+
			"confirmation. Its job row exists, so a confirmation is still one env fix away, "+
			"and the latch is irreversible — one boot-time typo would otherwise make it "+
			"visible to the peer forever. The sharpest trigger is this module's own credential "+
			"guard, which drops the fleet target on a secret collision it exists to refuse.")
}

// TestARejectedFleetConfigActivatesNewProjectsAtInsertAndStaysThatWay is the
// end-to-end case the insert path never had, and the one that exposed the
// contradiction this replaces.
//
// It used to assert the opposite — that a project created while fleet is
// requested-but-rejected WAITS — and that assertion was true only until the next
// reconcile tick. Such a project gets no fleet job row (a rejected target never
// reaches enqueueProvisioningTx), and latchUnconfirmableProjects' predicate is
// exactly "no fleet job exists", so the tick latched it: irreversibly, behind a
// count-only Warn indistinguishable from the benign straggler case, with the
// awaiting-activation gauge dropping back to zero. The suite could not see it
// because this case stopped at the insert and never ran a tick.
//
// So the policy is now the one the repair already enforced, and the assertion runs
// the tick to prove the two agree rather than stopping where they still look like
// they might.
func TestARejectedFleetConfigActivatesNewProjectsAtInsertAndStaysThatWay(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)
	p.cfg.Provisioning.Targets = nil
	p.cfg.Provisioning.Misconfigured = []string{TargetFleet}

	created := createVia(t, r, token, "rejected-config-create")

	at := activatedAtOf(t, created.ProjectID)
	require.NotNil(t, at,
		"a rejected target will not confirm a project created now, so the insert must say so "+
			"rather than promising a wait the reconcile then cancels")

	// No job row, which is WHY the old insert-time wait could not survive: this is
	// exactly the state latchUnconfirmableProjects treats as unconfirmable.
	assert.Empty(t, readProvisioningRows(t, created.ProjectID),
		"a rejected target writes no fleet job, which is exactly the state "+
			"latchUnconfirmableProjects reads as unconfirmable")

	// And the tick changes nothing, because there is nothing left to disagree about.
	p.scanUnlatchedActivations()
	after := activatedAtOf(t, created.ProjectID)
	require.NotNil(t, after)
	assert.Equal(t, at.UTC(), after.UTC(),
		"the repair must not move a latch the insert already set; a second timestamp here "+
			"would mean the two statements still disagree, just more quietly")
}

// TestNeverConfiguredFleetStillLatches is the other side, and it is what stops
// the B-6 fix from re-opening B-4: with fleet absent from BOTH lists, nothing
// will ever confirm and the straggler repair must still run.
func TestNeverConfiguredFleetStillLatches(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "neverFleet")
	seedSpaceMember(t, spaceA, "neverFleet", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "never-configured")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET activated_at = NULL WHERE project_id = ?",
		created.ProjectID).Exec()
	require.NoError(t, err)

	require.Empty(t, p.cfg.Provisioning.Misconfigured, "precondition: not merely rejected")
	require.False(t, p.twoPhaseCreateApplies())

	p.scanUnlatchedActivations()
	assert.NotNil(t, activatedAtOf(t, created.ProjectID),
		"with fleet in neither Targets nor Misconfigured, nothing will ever confirm — the "+
			"repair must still latch, or B-4 is back")
}

// TestAnOldPodDoesNotLatchANewPodsInFlightProject is P1-C case (a), the rolling
// ENABLEMENT of the fleet target.
//
// Pods restart one at a time, so a pod with the new config creates a project
// with a pending fleet job while a pod with the old config is still running.
// Deciding "will anything confirm this?" from the old pod's configuration
// answered yes-nothing-will for every unlatched row, so it latched the new pod's
// project — visible to the peer before its container exists, permanently, since
// the latch is one-way. The reconcile interval is five minutes; any rolling
// restart outlasts it.
func TestAnOldPodDoesNotLatchANewPodsInFlightProject(t *testing.T) {
	fleet := newFakeTarget(t)
	fleet.setStatus(500) // the job stays pending, i.e. still able to confirm later
	newPod, r, token := provisioningSetup(t, fleet)
	created := createVia(t, r, token, "rolling-enable")
	newPod.processProvisioningJobs()
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition: awaiting confirmation")

	// The OLD pod: same database, fleet in neither list, because its process
	// started before the operator added the target. Constructed directly rather
	// than through setup(), which truncates the tables the new pod just wrote.
	oldPod := New(testCtx)
	require.False(t, oldPod.twoPhaseCreateApplies(),
		"precondition: this pod's config knows nothing about fleet")

	oldPod.scanUnlatchedActivations()

	assert.Nil(t, activatedAtOf(t, created.ProjectID),
		"a pod whose config predates the fleet enablement must not latch a project whose "+
			"fleet job already exists. The question is per project — does THIS row have a "+
			"job — and the answer is in the database, not in this pod's environment")
}

// TestAProjectWithNoFleetJobIsLatchedEvenWhileFleetIsEnabled is P1-C case (b).
//
// A project can hold activated_at NULL with no fleet job at all: a pod that died
// between the project INSERT and enqueueProvisioningTx, or a row written by an
// older binary. Skipping such a row while fleet is "requested" left it unreachable
// by BOTH repairs — the other one needs a job in ready state — so it was invisible
// to the peer forever, and fixing the env did not recover it.
//
// Latching it is the milder of the two failures: the peer sees a project whose
// container is not there yet, which is the window that exists today anyway.
//
// The fixture forces that state directly. It used to reach it through a
// rejected-config window, and that is no longer a way to produce it: a project
// created while fleet is requested-but-rejected is now activated AT INSERT
// (twoPhaseCreateApplies stopped treating rejected as requested), precisely
// because this test and the insert-time comment used to say opposite things about
// the same row. Forcing the state keeps the property under test while removing
// the contradiction that produced it.
func TestAProjectWithNoFleetJobIsLatchedEvenWhileFleetIsEnabled(t *testing.T) {
	fleet := newFakeTarget(t)
	p, r, token := provisioningSetup(t, fleet)

	created := createVia(t, r, token, "no-job")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project` SET activated_at = NULL WHERE project_id = ?",
		created.ProjectID).Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().DeleteBySql(
		"DELETE FROM `octo_project_provisioning` WHERE project_id = ?",
		created.ProjectID).Exec()
	require.NoError(t, err)
	require.Nil(t, activatedAtOf(t, created.ProjectID), "precondition: awaiting confirmation")
	require.Empty(t, readProvisioningRows(t, created.ProjectID),
		"precondition: no fleet job exists for this project")

	require.True(t, p.twoPhaseCreateApplies(), "precondition: fleet is live on this pod")

	p.scanUnlatchedActivations()

	assert.NotNil(t, activatedAtOf(t, created.ProjectID),
		"a project with NO fleet job must be latched even while fleet is enabled: nothing "+
			"will ever confirm it, the confirmation repair needs a ready job it does not have, "+
			"and the alternative is invisible to the peer forever")
}

// TestBothActivationRepairsRunEvenWhenTheFirstFails pins that one repair's error
// does not skip the other.
//
// A source guard rather than an engine test, because the condition is a DB error
// from the first statement and there is no seam to inject one: p.db is a concrete
// *DB, and every way of breaking latchUnconfirmableProjects from the outside
// (dropping the provisioning table, say) breaks its sibling too, which is the one
// thing the test would need to keep working.
//
// It is worth pinning anyway because of WHICH one used to be dropped. The early
// return sat after the first call, so a transient failure there skipped
// repairConfirmedButUnlatched — the repair that recovers projects the peer
// currently reads as ABSENT, with their members denied on the peer side for as
// long as it keeps failing. The one that kept running was the benign one: it
// latches rows nothing will ever confirm, where a tick late changes nothing. The
// two read different predicates, share no transaction, and have no ordering
// between them, so there was never a reason to couple their failures.
func TestBothActivationRepairsRunEvenWhenTheFirstFails(t *testing.T) {
	body := funcBody(t, readStripped(t, "activation.go"), "func (p *Project) scanUnlatchedActivations(")

	latchAt := strings.Index(body, "p.db.latchUnconfirmableProjects(")
	repairAt := strings.Index(body, "p.db.repairConfirmedButUnlatched(")
	require.GreaterOrEqual(t, latchAt, 0, "scanUnlatchedActivations must still latch unconfirmable projects")
	require.GreaterOrEqual(t, repairAt, 0, "scanUnlatchedActivations must still repair confirmed-but-unlatched projects")
	require.Less(t, latchAt, repairAt, "fixture drift: this guard assumes the latch runs first")

	// Tokenised, not split on newlines: readStripped collapses the source to a
	// single whitespace-separated line, so a line-based check silently passes on
	// everything. The first version of this guard did exactly that and survived its
	// own mutation — the failure mode two guards on this branch already had.
	//
	// scanUnlatchedActivations returns nothing, so any `return` token in it is a
	// bare early return and there is no `return expr` to distinguish.
	between := body[latchAt:repairAt]
	for _, tok := range strings.Fields(between) {
		require.NotEqual(t, "return", tok,
			"no early return may sit between the two repairs: it would skip "+
				"repairConfirmedButUnlatched, which is the one that makes peer-invisible "+
				"projects visible again. Log the first failure and carry on.\nbetween: "+between)
	}
}
