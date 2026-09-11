package project

import (
	"context"
	"strings"
	"testing"
	"time"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Execution coverage for the epoch bump, driven through the REAL Space-removal
// transaction rather than through a hand-rolled one.
//
// This lane did not exist, and its absence is precisely how a lock-order inversion
// shipped: TestSpaceRemovalMovesTheEpochAtCommit calls the step with a transaction
// it opened itself, and modules/space's own tx-step tests register STUBS. So the
// real statement had never once run inside the real removal transaction, and no
// test could see anything about how the two interact — not the lock order, not the
// read view, not whether the step is even reached from the handler's path.

// removalEntry names the two production entry points that close ONE member's seat
// while the Space stays active. Both must move the epoch; they take different
// transactions to get there, and only one of them was ever exercised.
type removalEntry struct {
	name   string
	remove func(t *testing.T, spaceID, uid, operator string)
}

func removalEntries() []removalEntry {
	return []removalEntry{
		{
			// The kick / self-leave path: modules/space.removeMemberLocked, one
			// transaction per uid.
			name: "removeMemberLocked",
			remove: func(t *testing.T, spaceID, uid, operator string) {
				t.Helper()
				removed, err := spacemod.RemoveMemberForTest(
					testCtx, spaceID, uid, 2, operator, spacemod.MemberRemoveReasonKicked)
				require.NoError(t, err)
				require.True(t, removed, "the fixture seat must actually have been closed")
			},
		},
		{
			// The admin batch path: modules/space.removeMembersForce, ONE
			// transaction for the whole batch. Different lock accumulation, and its
			// read view is assigned at a different point — which is the property
			// the bump's non-locking enumeration depends on.
			name: "removeMembersForce",
			remove: func(t *testing.T, spaceID, uid, operator string) {
				t.Helper()
				removed, err := spacemod.RemoveMembersForceForTest(
					testCtx, spaceID, []string{uid}, operator)
				require.NoError(t, err)
				require.Equal(t, []string{uid}, removed)
			},
		},
	}
}

// TestRealSpaceRemovalMovesTheEpoch runs the REGISTERED step inside the REAL
// removal transaction, for both entry points.
//
// What this catches that the existing tests cannot: the step is reached from the
// production transaction (not just callable), it commits together with the seat
// closing, and it does not fail there — a statement that deadlocks or errors
// inside that transaction rolls the removal back, so "the removal succeeded AND
// the epoch moved" is one assertion, not two.
func TestRealSpaceRemovalMovesTheEpoch(t *testing.T) {
	for _, entry := range removalEntries() {
		t.Run(entry.name, func(t *testing.T) {
			srv, p := setup(t)
			p.registerSpaceMemberRemovalCleanup()

			seedSpace(t, spaceA, 1)
			ownerToken := seedUser(t, "realRemOwner")
			seedSpaceMember(t, spaceA, "realRemOwner", 2, 1)
			seedUser(t, "realRemTarget")
			seedSpaceMember(t, spaceA, "realRemTarget", 0, 1)

			// Two projects the target sits in, and one it does not: a bump must be
			// scoped to the member's own seats, because every needless bump costs
			// every consumer of that project a re-verify.
			inA := createProjectVia(t, srv, spaceA, ownerToken, "real-rem-a")
			inB := createProjectVia(t, srv, spaceA, ownerToken, "real-rem-b")
			untouched := createProjectVia(t, srv, spaceA, ownerToken, "real-rem-none")
			for _, pid := range []string{inA.ProjectID, inB.ProjectID} {
				admitted, err := p.addOneMember(pid, spaceA, "realRemOwner", "realRemTarget")
				require.NoError(t, err)
				require.True(t, admitted)
			}

			before := map[string]int64{
				inA.ProjectID:       epochOf(t, inA.ProjectID),
				inB.ProjectID:       epochOf(t, inB.ProjectID),
				untouched.ProjectID: epochOf(t, untouched.ProjectID),
			}

			entry.remove(t, spaceA, "realRemTarget", "realRemOwner")

			// The project seats are deliberately still open: closing them is the
			// asynchronous cascade's job. That is the whole window this bump exists
			// for, so a test that let the cascade run would prove nothing.
			seat, err := testDB.queryMember(inA.ProjectID, "realRemTarget")
			require.NoError(t, err)
			require.NotNil(t, seat, "the project seat must still be open — the cascade "+
				"has not run, which is exactly the window the epoch bump covers")

			for _, pid := range []string{inA.ProjectID, inB.ProjectID} {
				assert.Greater(t, epochOf(t, pid), before[pid],
					"project %s: member_epoch must move in the SAME transaction that closed "+
						"the Space seat. Left to the cascade it moves minutes later, or never "+
						"once the job is abandoned — and until then a peer re-reading the epoch "+
						"gets the same value and keeps the grant the removal revoked", pid)
			}
			assert.Equal(t, before[untouched.ProjectID], epochOf(t, untouched.ProjectID),
				"a project the removed member was never in must not be churned")
		})
	}
}

// TestSpaceRemovalDoesNotDeadlockAgainstProjectMembershipWrites is the regression
// test for the lock-order inversion, orchestrated DETERMINISTICALLY.
//
// The bump used to be one statement — `UPDATE octo_project p INNER JOIN
// octo_project_member pm ...` — whose lock order is not a property of the SQL at
// all: the OPTIMIZER picks the driving table and it flips with cardinality.
// Measured on MySQL 8.0.33 against this schema, and re-checked against the
// fixture this test builds:
//
//	3 active projects / 3 seats    -> p driving (ref)         => project -> member
//	60 active projects / 3 seats   -> pm driving (index_merge) => member -> project
//
// The second is the production shape and it reverses the order documented at the
// top of service.go. A 1213 there rolls the member removal back (this step's
// contract), so under contention the REVOCATION FAILS.
//
// # Why this is orchestrated rather than raced
//
// The first version of this test ran a churn goroutine against a stream of
// removals and passed against the REINTRODUCED defect three times out of three:
// both transactions are sub-millisecond, so the interleaving that closes the cycle
// essentially never happens by chance. It was a green test over a live deadlock —
// worse than no test, because it reads as coverage.
//
// So the cycle is built explicitly, mirroring the shape reproduced by hand:
//
//	T2 (project side, sanctioned order):  X on octo_project(pX)  ...then...  X on octo_project_member(pX, target)
//	T1 (removal side, INVERTED order):    S on octo_project_member(*, target)  ...then...  X on octo_project(pX)
//
// T1 must reach its second lock while T2 holds the first, and vice versa. With the
// single-statement bump both halves are inside ONE statement, so InnoDB closes the
// cycle and reports 1213 — verified by reintroducing it. With the two-statement
// bump T1 takes no octo_project_member lock at all, so there is no cycle to close.
//
// The assertion is on the OUTCOME: the removal must succeed and the epoch must
// move. A 1213 absorbed by the bounded retry is acceptable; one that reaches the
// caller is the bug.
func TestSpaceRemovalDoesNotDeadlockAgainstProjectMembershipWrites(t *testing.T) {
	// Enough active projects in the Space to make the optimizer drive from
	// octo_project_member. The plan is asserted below rather than assumed, because
	// this whole test is about a plan-dependent lock order.
	const projectCount = 60

	_, p := setup(t)
	p.registerSpaceMemberRemovalCleanup()

	seedSpace(t, spaceA, 1)
	seedUser(t, "dlOwner")
	seedSpaceMember(t, spaceA, "dlOwner", 2, 1)
	seedUser(t, "dlTarget")
	seedSpaceMember(t, spaceA, "dlTarget", 0, 1)

	// The quotas are raised rather than worked around: the cardinality IS the test.
	// With the default daily cap of 20 the fixture cannot reach the shape that
	// inverts the join order — and an earlier version of this test failed on the
	// quota instead, i.e. it never exercised the lock order at all.
	p.cfg.MaxDailyCreate = projectCount + 10
	p.cfg.MaxPerCreator = projectCount + 10
	p.cfg.MaxPerSpace = projectCount + 10

	projects := make([]string, 0, projectCount)
	for i := 0; i < projectCount; i++ {
		created, err := p.createProject(createInput{
			SpaceID: spaceA, Creator: "dlOwner", Name: "dl-project-" + itoa(i),
		})
		require.NoError(t, err, "fixture project %d", i)
		projects = append(projects, created.ProjectID)
	}

	// Three seats for the target, so the bump's enumeration returns several rows and
	// the S locks of the inverted plan cover more than the contended project.
	contended := projects[7]
	for _, pid := range []string{projects[3], contended, projects[41]} {
		admitted, err := p.addOneMember(pid, spaceA, "dlOwner", "dlTarget")
		require.NoError(t, err)
		require.True(t, admitted)
	}

	requireMemberDrivenPlan(t, spaceA, "dlTarget")

	before := epochOf(t, contended)

	// T2 — the project side, in the SANCTIONED order: the octo_project row first,
	// the seat row second, with a gap in between for T1 to walk into.
	t2Err := make(chan error, 1)
	t2Holding := make(chan struct{})
	t2Proceed := make(chan struct{})
	go func() {
		t2Err <- func() error {
			tx, err := testCtx.DB().Begin()
			if err != nil {
				return err
			}
			defer tx.RollbackUnlessCommitted()

			// X on octo_project(contended).
			if _, err := p.db.lockActiveProjectTx(tx, contended); err != nil {
				return err
			}
			close(t2Holding)
			<-t2Proceed

			// X on octo_project_member(contended, dlTarget) — the second half of the
			// cycle. Under the single-statement bump T1 is holding an S lock on this
			// exact row while waiting for the octo_project row this transaction holds.
			if _, err := tx.UpdateBySql(
				"UPDATE octo_project_member SET updated_at = ? "+
					"WHERE project_id = ? AND uid = ?",
				time.Now().UTC(), contended, "dlTarget",
			).Exec(); err != nil {
				return err
			}
			return tx.Commit()
		}()
	}()

	<-t2Holding

	// T1 — the real removal transaction. Its bump is the step under test.
	t1Err := make(chan error, 1)
	removed := make(chan bool, 1)
	go func() {
		ok, err := spacemod.RemoveMemberForTest(
			testCtx, spaceA, "dlTarget", 2, "dlOwner", spacemod.MemberRemoveReasonKicked)
		removed <- ok
		t1Err <- err
	}()

	// Let T1 reach its bump and block on the octo_project row T2 holds, THEN release
	// T2 into the seat row T1 has locked. That ordering is what closes the cycle;
	// releasing T2 earlier just serializes the two.
	time.Sleep(400 * time.Millisecond)
	close(t2Proceed)

	select {
	case err := <-t1Err:
		require.NoError(t, err,
			"the Space removal must not fail. A 1213 reaching this caller means the epoch "+
				"bump is back in a lock cycle with the project-side writes — and by this "+
				"step's own contract that ROLLS THE REMOVAL BACK, so the revocation fails: "+
				"exactly the state the step was added to prevent")
		require.True(t, <-removed, "the seat must actually have been closed")
	case <-time.After(30 * time.Second):
		t.Fatal("the removal never completed; the two transactions are deadlocked or stuck")
	}

	require.NoError(t, <-t2Err, "the project-side transaction must not fail either: it takes "+
		"the documented order, so if it is InnoDB's victim the other side is the one going "+
		"against the order")

	assert.Greater(t, epochOf(t, contended), before,
		"and the epoch must actually have moved — a removal that committed without its "+
			"invalidation signal is the defect this whole step exists for")
}

// requireMemberDrivenPlan asserts the fixture really did produce the INVERTED join
// order, i.e. that this test is exercising the shape it claims to.
//
// Without this the test is cardinality-dependent in a silent way: on a smaller
// fixture the optimizer drives from octo_project and there is no cycle to close, so
// the test would pass for the wrong reason on a machine where the statistics land
// differently. It runs EXPLAIN on the single-statement form regardless of which
// implementation is compiled in, because what it is checking is a property of the
// schema and the data, not of the Go code.
func requireMemberDrivenPlan(t *testing.T, spaceID, uid string) {
	t.Helper()
	if _, err := testCtx.DB().Exec(
		"ANALYZE TABLE `octo_project`, `octo_project_member`"); err != nil {
		t.Logf("ANALYZE TABLE failed (%v); the plan check below may read stale statistics", err)
	}
	var rows []struct {
		Table string `db:"table"`
	}
	_, err := testCtx.DB().SelectBySql(
		"EXPLAIN UPDATE octo_project p "+
			"INNER JOIN octo_project_member pm ON pm.project_id = p.project_id "+
			"SET p.member_epoch = p.member_epoch + 1 "+
			"WHERE p.space_id = ? AND p.status = ? "+
			"  AND pm.space_id = ? AND pm.uid = ? AND pm.status = ? AND pm.removing = 0",
		spaceID, StatusNormal, spaceID, uid, MemberStatusActive,
	).Load(&rows)
	if err != nil {
		t.Skipf("EXPLAIN unavailable (%v); cannot confirm this fixture inverts the join order, "+
			"and asserting on a deadlock that may not be reachable would be a flake", err)
	}
	require.NotEmpty(t, rows, "EXPLAIN returned no rows")
	require.Equal(t, "pm", rows[0].Table,
		"this fixture must make the optimizer drive from octo_project_member — that is the "+
			"plan whose lock order is INVERTED and the only one that can deadlock against a "+
			"project-side write. Driving from octo_project means the test would pass without "+
			"exercising anything; raise projectCount until the plan flips")
}

// TestSpaceMemberEpochBumpSeesConcurrentAdmission pins the read-view argument the
// two-statement bump depends on.
//
// The enumeration is a NON-LOCKING read (taking S locks on octo_project_member is
// what created the lock-order inversion), so under REPEATABLE READ it reads the
// removal transaction's snapshot. That is only safe because of a property of the
// OTHER module's write paths: every seat admission locks the target's space_member
// row FIRST (`FOR SHARE OF sm`, lockSpaceSeatsTx) before it touches any
// octo_project* table. So an admission that has not committed BLOCKS the removal's
// X lock on that row, and one that HAS committed did so before that lock was
// taken — therefore before this transaction's first consistency read, which is the
// enumeration itself.
//
// Written as a DETERMINISTIC interleaving rather than a race, because the
// interesting order is the rare one. An earlier version of this test just launched
// the two concurrently and skipped when the admission lost — which is the shape
// that reports green while asserting nothing.
//
// The admission half is driven through this module's real statements
// (lockSpaceSeatsTx -> admitMemberTx -> bumpMemberEpochTx, the same three
// addOneMemberOnce runs) in a transaction the test holds open, so if a refactor
// moves the seat lock later in that sequence this test stops proving the property
// it claims — which is the point of pinning it here rather than describing it in a
// comment.
func TestSpaceMemberEpochBumpSeesConcurrentAdmission(t *testing.T) {
	srv, p := setup(t)
	p.registerSpaceMemberRemovalCleanup()

	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "rvOwner")
	seedSpaceMember(t, spaceA, "rvOwner", 2, 1)
	seedUser(t, "rvTarget")
	seedSpaceMember(t, spaceA, "rvTarget", 0, 1)

	racing := createProjectVia(t, srv, spaceA, ownerToken, "rv-racing")
	before := epochOf(t, racing.ProjectID)

	// The admission, up to but not including COMMIT. It holds the target's
	// space_member row under FOR SHARE, which is what makes the removal below block.
	admTx, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer admTx.RollbackUnlessCommitted()

	held, err := p.db.lockSpaceSeatsTx(admTx, spaceA, []string{"rvOwner", "rvTarget"})
	require.NoError(t, err)
	require.True(t, held["rvTarget"], "the fixture target must hold a Space seat")

	now := time.Now().UTC()
	changed, err := p.db.admitMemberTx(admTx, &MemberModel{
		ProjectID: racing.ProjectID,
		UID:       "rvTarget",
		SpaceID:   spaceA,
		Role:      RoleCommon,
		InviteUID: "rvOwner",
		CreatedAt: now,
		UpdatedAt: now,
	})
	require.NoError(t, err)
	require.True(t, changed)
	_, err = p.db.bumpMemberEpochTx(admTx, racing.ProjectID, now)
	require.NoError(t, err)

	// The removal starts now and MUST block: its first statement is
	// `SELECT role ... FOR UPDATE` on the row the admission holds under FOR SHARE.
	removalDone := make(chan error, 1)
	go func() {
		_, rmErr := spacemod.RemoveMemberForTest(
			testCtx, spaceA, "rvTarget", 2, "rvOwner", spacemod.MemberRemoveReasonKicked)
		removalDone <- rmErr
	}()

	// Give it long enough to have reached — and blocked on — that lock. If it did
	// NOT block, the admission's seat lock is no longer where the argument needs it
	// and the check below is what fails.
	select {
	case rmErr := <-removalDone:
		t.Fatalf("the removal completed while an admission held the target's space_member "+
			"row under FOR SHARE (err=%v). The epoch bump's non-locking enumeration is only "+
			"safe because an in-flight admission BLOCKS the removal; if the seat lock moved "+
			"or was dropped, a seat committing after the removal's read view is assigned goes "+
			"live with an unmoved epoch", rmErr)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, admTx.Commit())

	select {
	case rmErr := <-removalDone:
		require.NoError(t, rmErr)
	case <-time.After(10 * time.Second):
		t.Fatal("the removal never completed after the admission committed")
	}

	// The seat is live (the cascade is asynchronous and has not run), so the epoch
	// must have moved TWICE: once for the admission, once for the removal. Exactly
	// one bump is the regression — it means the removal's enumeration read a
	// snapshot older than the admission's commit and never saw the seat.
	seat, err := testDB.queryMember(racing.ProjectID, "rvTarget")
	require.NoError(t, err)
	require.NotNil(t, seat)
	require.Equal(t, MemberStatusActive, seat.Status)
	require.Equal(t, 0, seat.Removing)

	assert.Equal(t, before+2, epochOf(t, racing.ProjectID),
		"the epoch must move for the admission AND for the removal. Landing on before+1 "+
			"means the removal's enumeration missed the seat the admission had just "+
			"committed: that seat is live, unreachable through the Space conjunction, and "+
			"riding an epoch a peer's staleness check still agrees with")
}

// TestVerifyAnswersFoldedUIDs covers the fail-OPEN fold defect at the seam it was
// reachable through.
//
// projectpkg.FoldID replaced strings.ToLower, which is Unicode case mapping and
// therefore LOOSER than utf8mb4_general_ci: U+212A (KELVIN SIGN) folds to "k" in
// Go while general_ci keeps them distinct. So a caller sending both "k" and U+212A
// got TWO member answers — with roles — out of ONE database row.
func TestVerifyAnswersFoldedUIDs(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "foldOwner")
	seedSpaceMember(t, spaceA, "foldOwner", 2, 1)
	seedUser(t, "foldmember")
	seedSpaceMember(t, spaceA, "foldmember", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "fold-project")
	admitted, err := p.addOneMember(created.ProjectID, spaceA, "foldOwner", "foldmember")
	require.NoError(t, err)
	require.True(t, admitted)

	// Case variance is folded on both sides: the caller's spelling matches, and the
	// answer is keyed by the fold so the handler can find it.
	_, roles, err := projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"FOLDMEMBER"})
	require.NoError(t, err)
	assert.Contains(t, roles, projectpkg.FoldID("FOLDMEMBER"),
		"a caller holding a re-cased uid must still be recognised: the SQL matches it "+
			"under general_ci, so an exact-match Go lookup would deny a real member")

	// The Unicode compatibility character must NOT be folded onto the ASCII member.
	const kelvin = "K" // KELVIN SIGN; strings.ToLower folds this to "k"
	assert.NotEqual(t, projectpkg.FoldID(kelvin), projectpkg.FoldID("K"),
		"FoldID must be ASCII-only. strings.ToLower folds U+212A onto \"k\" while "+
			"utf8mb4_general_ci does not, which made the fold LOOSER than the collation "+
			"and served a uid the database never matched as a member with a role")

	_, roles, err = projectpkg.ProjectMemberships(context.Background(),
		testCtx.DB(), spaceA, created.ProjectID, []string{"foldmember", "fold" + kelvin + "ember"})
	require.NoError(t, err)
	assert.Len(t, roles, 1,
		"one database row must produce exactly one member answer; a fold coarser than the "+
			"collation turns one row into several positives")
}

// TestProjectMembershipsFoldsTheProjectID covers the third seam — the one inside
// pkg/project that the handler-level fold could not reach.
//
// ProjectEpochsInSpace keys its map by the fold; this function used to look up the
// caller's RAW project_id in it. So a caller sending `P-ABC` for a stored `p-abc`
// matched in SQL, missed the Go lookup, and was served member_epoch 0 with every
// uid member:false — fail-closed, but a silently wrong answer, and the handler's
// fold ran too late to help because the function had already returned.
func TestProjectMembershipsFoldsTheProjectID(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "pidOwner")
	seedSpaceMember(t, spaceA, "pidOwner", 2, 1)
	seedUser(t, "pidMember")
	seedSpaceMember(t, spaceA, "pidMember", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "pid-project")
	admitted, err := p.addOneMember(created.ProjectID, spaceA, "pidOwner", "pidMember")
	require.NoError(t, err)
	require.True(t, admitted)

	upper := strings.ToUpper(created.ProjectID)
	require.NotEqual(t, created.ProjectID, upper, "the fixture id must contain letters to "+
		"vary the case of; a hex id without a-f would make this vacuous")

	epoch, roles, err := projectpkg.ProjectMemberships(context.Background(), testCtx.DB(), spaceA, upper,
		[]string{"pidMember"})
	require.NoError(t, err)
	assert.NotZero(t, epoch, "a re-cased project_id matches in SQL under general_ci, so the "+
		"Go-side epoch lookup must fold too — otherwise a live project reads as absent")
	assert.Contains(t, roles, projectpkg.FoldID("pidMember"),
		"and the member answer must survive with it")
}

// itoa is strconv.Itoa without the import, kept local so the fixture loops read as
// fixtures.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
