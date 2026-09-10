package project

// What this file pins:
//
//	A `_verify` answer must never be a DENIAL stamped with a LIVE epoch.
//
// # The defect
//
// ProjectMemberships ran its four narrowing reads on a plain autocommit session, so the
// steps shared no snapshot. Measured on MySQL 8.0.33 (default REPEATABLE-READ): a read
// inside a transaction keeps the view established by its first read, and a read outside
// one does not.
//
// So a Space ban committing mid-request tore the answer:
//
//  1. Step 1 stamps epoch E through ProjectEpochsInSpace, whose IsActiveSpace fold sees
//     the Space as active.
//  2. The ban commits. Its ONLY write is one UPDATE of the `space` row
//     (modules/space.updateSpaceStatus) — no seat write, no epoch bump.
//  3. Step 3's space half INNER JOINs `space ... AND s.status = 1`, observes the ban,
//     and drops every seated uid.
//
// The response is `member:false` for everyone beside `member_epoch: E`, and the peer
// keys its cached DENIAL on E. Unbanning moves only the `space` row, so the epoch
// channel answers E again and the peer's staleness check agrees with its own denial.
// Everyone queried inside that window stays locked out until some unrelated membership
// change happens to bump that project — unbounded in a quiet project.
//
// # Why the fold did not already cover it
//
// Round 5 closed the STEADY-state directions: a banned Space folds to the absent
// sentinel, so `0 != E` breaks agreement, and unbanning gives `E != 0`. That holds for
// answers computed while the fold can see the ban. An answer torn ACROSS the ban commit
// carries the denial beside the live epoch, and the unban restores exactly that key.
//
// Step 3's own comment said reading the Space last "can at worst deny someone who was
// re-admitted microseconds ago, and they re-verify". True on the removal axis, because
// re-admission bumps the epoch and that is what fires the re-verify. False on the
// ban/unban axis: unban bumps nothing, so no trigger ever fires.
//
// # Why the test drives the tear through a hook
//
// The interleaving is a race. Reproducing it by timing would make this test
// probabilistic, and a probabilistic test for a defect this branch has already shipped
// three vacuous guards against is worth very little. The hook commits the ban at exactly
// the point the old code tore, from a SEPARATE connection so the commit is real.

import (
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyNeverStampsADenialWithALiveEpoch stages a Space ban between the epoch read
// and the narrowing reads, and asserts the answer that comes out is internally
// consistent.
//
// The assertion is deliberately NOT "member must be true". What must hold is that the
// answer and the epoch describe the same instant: either the answer still reflects the
// Space the epoch was read from (member:true at E — the peer's next `epochs` poll sees
// the ban through the fold, breaks agreement, and re-verifies), or the epoch is the
// absent sentinel. A denial at a live epoch is the one combination with no bound on it.
func TestVerifyNeverStampsADenialWithALiveEpoch(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "tornOwner")
	seedSpaceMember(t, spaceA, "tornOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "torn-verify")

	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	live := epochs[projectpkg.FoldID(created.ProjectID)]
	require.NotZero(t, live, "baseline: an active project in an active Space has a real epoch")

	// The ban lands exactly where the answer used to tear, committed on its own
	// connection so it is visible to anything not holding an earlier snapshot.
	banned := false
	restore, err := projectpkg.SetMembershipTearHookForTest(func() {
		if banned {
			return
		}
		banned = true
		_, execErr := testCtx.DB().Exec(
			"UPDATE `space` SET status = 0, updated_at = NOW() WHERE space_id = ?", spaceA)
		require.NoError(t, execErr)
	})
	require.NoError(t, err)
	defer restore()

	epoch, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"tornOwner"})
	require.NoError(t, err)
	require.True(t, banned, "the hook must have run, or this case proves nothing")

	_, isMember := roles[projectpkg.FoldID("tornOwner")]
	denialAtLiveEpoch := !isMember && epoch == live
	assert.False(t, denialAtLiveEpoch,
		"a DENIAL must never be stamped with the epoch that was read while the Space was "+
			"still active (got member=%v, epoch=%d, live=%d).\n\n"+
			"The peer keys its cached decision on (uid, project, member_epoch). Unbanning "+
			"writes only the `space` row — no seat, no bump — so the epoch channel answers "+
			"this same value again and the peer's staleness check agrees with its own cached "+
			"denial. Every uid queried inside the torn window stays locked out until some "+
			"unrelated membership change happens to bump that project, which in a quiet "+
			"project is never.",
		isMember, epoch, live)
}

// TestVerifyStepsShareOneSnapshot is the mechanism the assertion above rests on.
//
// Stated separately because the case above would also pass if ProjectMemberships had
// simply stopped reading the Space — which would be a much worse fix. This one shows
// the answer is consistent BECAUSE the steps see one instant, not because a step was
// removed: the seat is still reported, and it is reported through the same space-half
// read that denies in the steady state.
//
// # Why this drives the SEAT axis and not the Space-ban axis
//
// It used to close the Space, like the case above, and mutation testing by two
// reviewers showed the steady-state half was then vacuous: disabling the
// space.ActiveMembers conjunction entirely left both cases in this file GREEN. The
// reason is structural. On a banned Space, step 1's round-5 fold already turns the
// project absent and ProjectMemberships returns before step 3 runs at all, so
// "the steady state still denies" was satisfied by the fold rather than by the
// conjunction it claimed to pin.
//
// The discriminating shape leaves the project and the Space both ACTIVE and closes
// only the Space SEAT. Step 1 passes, step 2 still finds the project seat — the
// Space→project cascade is asynchronous and has not run — so step 3 is the only
// thing left that can deny, and it must.
//
// Recorded at length because "green for the wrong reason" is the failure mode this
// branch has shipped three times, and this file's own header warns about it.
func TestVerifyStepsShareOneSnapshot(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "snapOwner")
	seedSpaceMember(t, spaceA, "snapOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "torn-snapshot")

	closed := false
	restore, err := projectpkg.SetMembershipTearHookForTest(func() {
		if closed {
			return
		}
		closed = true
		res, execErr := testCtx.DB().Exec(
			"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?",
			spaceA, "snapOwner")
		require.NoError(t, execErr)
		n, execErr := res.RowsAffected()
		require.NoError(t, execErr)
		require.EqualValues(t, 1, n, "the hook must actually have closed the seat")
	})
	require.NoError(t, err)
	defer restore()

	_, roles, err := projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"snapOwner"})
	require.NoError(t, err)
	require.True(t, closed, "the hook must have run, or this case proves nothing")
	assert.Contains(t, roles, projectpkg.FoldID("snapOwner"),
		"the narrowing reads must see the Space membership the epoch was read from. A "+
			"seat closing after the first read belongs to the NEXT answer, not to this "+
			"one — and that removal bumped the epoch in its own transaction, so the peer "+
			"is told about it through the channel that has a bound.")

	// And a call that STARTS after the seat is closed must deny. The project and the
	// Space are both still active here, so step 1's fold cannot produce this answer:
	// only the space-half conjunction can.
	_, roles, err = projectpkg.ProjectMemberships(
		testCtx.DB(), spaceA, created.ProjectID, []string{"snapOwner"})
	require.NoError(t, err)
	assert.NotContains(t, roles, projectpkg.FoldID("snapOwner"),
		"a uid whose SPACE seat is closed must be denied even though the project seat "+
			"survives — otherwise the snapshot fix would have been a removal of the "+
			"space-half conjunction rather than a linearisation of it, and the "+
			"asynchronous cascade's whole window would be a live grant")

	// The fixture has to be the discriminating one, so pin it rather than trusting the
	// prose above: the project seat MUST still be open, or the denial above proves
	// nothing about step 3.
	var open []int
	_, err = testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = 1 AND removing = 0",
		created.ProjectID, "snapOwner").Load(&open)
	require.NoError(t, err)
	require.Equal(t, 1, open[0],
		"the project seat must still be open for this case to isolate the space half; "+
			"if the cascade became synchronous, step 2 now does the denying and this "+
			"case has silently stopped testing the conjunction")
}

// TestProjectMembershipsPinsItsOwnIsolationLevel is the difference between a test
// that MEASURES the deployment and a test that asserts the contract.
//
// The two cases above open their transaction on the shared session, so they inherit
// whatever `transaction_isolation` the server carries. On the CI and dev engines that
// is REPEATABLE READ, which means they would go on passing if the fix's transaction
// silently degraded to READ COMMITTED — and nothing in this repository sets that
// variable: not pkg/db, not the DSN template, not a boot check. A reviewer measured
// exactly that on 8.0.46: with the server globally at READ-COMMITTED and a bare
// session.Begin(), both cases above fail in the original torn shape.
//
// So this one takes the level away. It runs ProjectMemberships against a session
// whose own default is READ COMMITTED and asserts the answer is STILL consistent,
// which is only true if the function names its isolation level rather than inheriting
// it. Nothing here is skipped when the ambient level happens to be right — that is
// the point.
func TestProjectMembershipsPinsItsOwnIsolationLevel(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "isoOwner")
	seedSpaceMember(t, spaceA, "isoOwner", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "iso-verify")

	epochs, err := projectpkg.ProjectEpochsInSpace(testCtx.DB(), spaceA, []string{created.ProjectID})
	require.NoError(t, err)
	live := epochs[projectpkg.FoldID(created.ProjectID)]
	require.NotZero(t, live)

	// A pool of exactly ONE connection, so "the session default is READ COMMITTED" is a
	// property of the connection the transaction will actually run on rather than of a
	// connection it might get. go-sql-driver's ResetSession only performs a liveness
	// check — it does not send COM_RESET_CONNECTION — so the SET SESSION below survives
	// the checkout/checkin that happens between statements.
	conn, err := dbr.Open("mysql", testCtx.GetConfig().DB.MySQLAddr, nil)
	require.NoError(t, err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	session := conn.NewSession(nil)

	_, err = session.DB.Exec("SET SESSION transaction_isolation = 'READ-COMMITTED'")
	require.NoError(t, err, "this case needs SESSION scope only — it must not require "+
		"SUPER, and it must not disturb any other connection")
	require.Equal(t, "READ-COMMITTED", sessionIsolation(t, session),
		"the fixture must actually be hostile, or this case proves nothing")

	banned := false
	restore, err := projectpkg.SetMembershipTearHookForTest(func() {
		if banned {
			return
		}
		banned = true
		_, execErr := testCtx.DB().Exec(
			"UPDATE `space` SET status = 0, updated_at = NOW() WHERE space_id = ?", spaceA)
		require.NoError(t, execErr)
	})
	require.NoError(t, err)
	defer restore()

	epoch, roles, err := projectpkg.ProjectMemberships(
		session, spaceA, created.ProjectID, []string{"isoOwner"})
	require.NoError(t, err)
	require.True(t, banned, "the hook must have run, or this case proves nothing")

	_, isMember := roles[projectpkg.FoldID("isoOwner")]
	assert.False(t, !isMember && epoch == live,
		"the answer tore even though the function opens its own transaction (got "+
			"member=%v, epoch=%d, live=%d).\n\n"+
			"That means the transaction inherited this session's READ COMMITTED, under "+
			"which every statement takes a fresh view and the transaction linearises "+
			"nothing. ProjectMemberships must ask for REPEATABLE READ explicitly — "+
			"BeginTx with sql.TxOptions — so its correctness is a property of the code "+
			"rather than of whatever transaction_isolation the operator left set.",
		isMember, epoch, live)

	// And the pin must be scoped to that one transaction. `SET TRANSACTION ISOLATION
	// LEVEL` with neither GLOBAL nor SESSION applies to the next transaction only; if
	// it ever became a session-scoped write, this pooled connection would carry
	// REPEATABLE READ back to whoever borrowed it next.
	assert.Equal(t, "READ-COMMITTED", sessionIsolation(t, session),
		"ProjectMemberships must not leave its isolation level behind on the pooled "+
			"connection — the next borrower did not ask for it")
}

// sessionIsolation reads the connection's own default, not the transaction's.
func sessionIsolation(t *testing.T, session *dbr.Session) string {
	t.Helper()
	var got []string
	_, err := session.SelectBySql("SELECT @@SESSION.transaction_isolation").Load(&got)
	require.NoError(t, err)
	require.Len(t, got, 1)
	return got[0]
}
