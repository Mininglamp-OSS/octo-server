package space

import (
	"errors"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wiring tests for MemberRemovalTxStep.
//
// The step exists because the ASYNC cleanup registry cannot carry a fact that has
// to be true at commit. A downstream module (modules/project) uses it to move the
// invalidation signal a peer control plane reads to decide whether a cached
// authorization is stale: closing the member's project seats stays asynchronous,
// but the signal cannot, because the cleanup job can sit in backoff for minutes
// and has a terminal abandoned state after which nothing re-claims it.
//
// Two properties, and the second is the one that makes the first worth anything:
// the step RUNS inside the removal transaction, and when it FAILS the removal
// ROLLS BACK. A step that ran but whose failure was swallowed would commit a
// removal with no signal — exactly the state it was added to prevent.

func seedRemovalFixture(t *testing.T, spaceID, owner, target string) {
	t.Helper()
	seedSpace(t, spaceID, "tx step space", owner, 1)
	require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: target, Role: 0, Status: 1,
	}))
}

func memberStatus(t *testing.T, spaceID, uid string) (int, bool) {
	t.Helper()
	var status []int
	_, err := testCtx.DB().SelectBySql(
		"SELECT status FROM space_member WHERE space_id = ? AND uid = ?", spaceID, uid).Load(&status)
	require.NoError(t, err)
	if len(status) == 0 {
		return 0, false
	}
	return status[0], true
}

// restoreTxSteps puts the registry back, so a test's stand-in does not leak into
// the rest of the package. Registration is latest-wins by name, and there is no
// unregister, so the fixture re-registers a no-op under the same name.
func restoreTxSteps(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		RegisterMemberRemovalTxStep(name, func(*dbr.Tx, SeatRef) error { return nil })
	})
}

func TestMemberRemovalTxStepRunsInsideTheTransaction(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const name = "test_tx_step_runs"
	restoreTxSteps(t, name)

	var (
		calls     int
		gotSpace  string
		gotUID    string
		sawMember bool
	)
	RegisterMemberRemovalTxStep(name, func(tx *dbr.Tx, seat SeatRef) error {
		spaceID, uid := seat.SpaceID(), seat.UID()
		calls++
		gotSpace, gotUID = spaceID, uid
		// Read through the SAME transaction: this is what proves the step is
		// inside it rather than merely called near it. The member row must
		// already read as removed here — the step runs after the status write and
		// before the commit, which is the only window where a fact can be made
		// atomic with the removal.
		var status []int
		if _, err := tx.SelectBySql(
			"SELECT status FROM space_member WHERE space_id = ? AND uid = ?", spaceID, uid).Load(&status); err != nil {
			return err
		}
		sawMember = len(status) == 1 && status[0] == 0
		return nil
	})

	seedRemovalFixture(t, "tx-step-space-1", "u-owner-tx1", "u-target-tx1")
	removed, err := removeMemberLocked(testCtx.DB(), "tx-step-space-1", "u-target-tx1", 2, "u-owner-tx1", MemberRemoveReasonForceRemoved)
	require.NoError(t, err)
	require.True(t, removed)

	assert.Equal(t, 1, calls, "the step must run exactly once per removal")
	assert.Equal(t, "tx-step-space-1", gotSpace)
	assert.Equal(t, "u-target-tx1", gotUID)
	assert.True(t, sawMember,
		"the step must see the removal through its own transaction handle; if it cannot, it is "+
			"not running inside the transaction and nothing it writes is atomic with the removal")
}

// TestMemberRemovalTxStepFailureRollsBackTheRemoval is the property that makes
// the step meaningful.
//
// If a step's failure were swallowed, the removal would commit without the fact
// the step exists to establish — and for the membership-epoch case that means a
// peer keeps an authorization it cannot detect as stale, with no bound, because
// the async compensation has a terminal give-up state. Failing the removal is the
// deliberate direction: the caller retries.
func TestMemberRemovalTxStepFailureRollsBackTheRemoval(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const name = "test_tx_step_fails"
	restoreTxSteps(t, name)

	boom := errors.New("tx step refused")
	RegisterMemberRemovalTxStep(name, func(*dbr.Tx, SeatRef) error { return boom })

	seedRemovalFixture(t, "tx-step-space-2", "u-owner-tx2", "u-target-tx2")
	removed, err := removeMemberLocked(testCtx.DB(), "tx-step-space-2", "u-target-tx2", 2, "u-owner-tx2", MemberRemoveReasonForceRemoved)
	require.Error(t, err, "a failing tx step must fail the removal")
	assert.ErrorIs(t, err, boom, "and the cause must survive, so an operator can see which step refused")
	assert.False(t, removed)

	status, ok := memberStatus(t, "tx-step-space-2", "u-target-tx2")
	require.True(t, ok, "the member row must still exist")
	assert.Equal(t, 1, status,
		"the member must still be ACTIVE: committing the removal while the step that publishes "+
			"its invalidation signal failed is the exact state this hook exists to prevent")

	// And the outbox row must be gone with it, or the cleanup job would run for a
	// removal that never happened.
	var jobs []int
	_, qErr := testCtx.DB().SelectBySql(
		"SELECT 1 FROM space_member_removal_cleanup WHERE space_id = ? AND uid = ?",
		"tx-step-space-2", "u-target-tx2").Load(&jobs)
	require.NoError(t, qErr)
	assert.Empty(t, jobs, "the cleanup outbox row must roll back with the removal")
}
