package project

import (
	"strings"
	"sync"
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
// test for the lock-order inversion.
//
// The bump used to be ONE statement — `UPDATE octo_project p INNER JOIN
// octo_project_member pm ...` — whose lock order is chosen by the OPTIMIZER and
// flips with cardinality. Measured on MySQL 8.0.33 against this schema: with a
// handful of projects it drives from `p` (project -> member, the documented order);
// with two hundred it drives from `pm` (member -> project, inverted). The
// production shape is the second one, and both deadlock victim directions were
// reproduced, including the one where InnoDB rolls back the SPACE REMOVAL — which
// this step's contract turns into a FAILED REVOCATION.
//
// So this test seeds the inverting cardinality and runs concurrent project
// membership writes against a stream of removals. It asserts on the OUTCOME rather
// than on the plan: every removal must succeed and every removed member's projects
// must have moved. A 1213 that the retry absorbs is fine; a 1213 that reaches the
// caller is the bug.
//
// Not a proof of absence — a scheduling-dependent test never is. It is a
// regression net over the exact shape that was broken, and it fails reliably
// against the single-statement version.
func TestSpaceRemovalDoesNotDeadlockAgainstProjectMembershipWrites(t *testing.T) {
	srv, p := setup(t)
	p.registerSpaceMemberRemovalCleanup()

	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "dlOwner")
	seedSpaceMember(t, spaceA, "dlOwner", 2, 1)

	// The cardinality that inverts the join order: many projects in the Space, each
	// target sitting in only a few of them.
	const projectCount = 60
	projects := make([]string, 0, projectCount)
	for i := 0; i < projectCount; i++ {
		created := createProjectVia(t, srv, spaceA, ownerToken,
			"dl-project-"+strings.Repeat("x", i%3)+itoa(i))
		projects = append(projects, created.ProjectID)
	}

	const targets = 8
	targetIDs := make([]string, 0, targets)
	for i := 0; i < targets; i++ {
		uid := "dlTarget" + itoa(i)
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
		targetIDs = append(targetIDs, uid)
		// Three seats each, spread across the pool so the concurrent writer and the
		// remover contend on overlapping project rows.
		for k := 0; k < 3; k++ {
			admitted, err := p.addOneMember(projects[(i*3+k)%projectCount], spaceA, "dlOwner", uid)
			require.NoError(t, err)
			require.True(t, admitted)
		}
	}

	// A separate uid whose seats churn for the whole run: this is the transaction
	// that takes project -> octo_project_member in the sanctioned order, i.e. the
	// other half of the cycle.
	seedUser(t, "dlChurn")
	seedSpaceMember(t, spaceA, "dlChurn", 0, 1)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			pid := projects[i%projectCount]
			i++
			// Add then remove, both through the module's own retry, so a deadlock the
			// retry absorbs here does not fail the test — only one that reaches a
			// removal caller does.
			if _, err := p.addOneMember(pid, spaceA, "dlOwner", "dlChurn"); err != nil {
				continue
			}
			_, _ = p.removeMember(pid, spaceA, "dlOwner", "dlChurn")
		}
	}()

	var failures []string
	for _, uid := range targetIDs {
		removed, err := spacemod.RemoveMemberForTest(
			testCtx, spaceA, uid, 2, "dlOwner", spacemod.MemberRemoveReasonKicked)
		if err != nil {
			failures = append(failures, uid+": "+err.Error())
			continue
		}
		if !removed {
			failures = append(failures, uid+": the seat was not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	assert.Empty(t, failures,
		"every Space removal must succeed while project membership writes run concurrently. "+
			"A transient 1213 is absorbed by the bounded retry; one that reaches the caller "+
			"means the epoch bump is back in a lock cycle with the project-side writes, and by "+
			"this step's own contract that ROLLS THE REMOVAL BACK — the revocation fails, which "+
			"is the state the step was added to prevent")
}

// TestSpaceMemberEpochBumpSeesConcurrentAdmission pins the read-view argument the
// two-statement bump depends on.
//
// The enumeration is a NON-LOCKING read (taking S locks on octo_project_member is
// what created the inversion), so under REPEATABLE READ it reads the removal
// transaction's snapshot. That is only safe because of a property of the OTHER
// module's write paths: every seat admission locks the target's space_member row
// FIRST (`FOR SHARE OF sm`), before it touches any octo_project* table. So an
// admission that has not committed is blocked by the removal's X lock on that row,
// and one that HAS committed did so before that lock was taken — therefore before
// this transaction's read view was assigned.
//
// If a future refactor moves the seat lock later in the admission path, that
// argument silently stops holding and the epoch stops moving for the racing seat.
// Nothing about this file's own code would change, which is why the property is
// pinned here rather than described in a comment.
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

	// The admission starts first and holds the space_member S lock across a delay,
	// so the removal below blocks on it and its read view cannot be assigned until
	// the admission has committed.
	admitted := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := p.addOneMember(racing.ProjectID, spaceA, "rvOwner", "rvTarget")
		admitted <- err
	}()
	<-started

	removed, err := spacemod.RemoveMemberForTest(
		testCtx, spaceA, "rvTarget", 2, "rvOwner", spacemod.MemberRemoveReasonKicked)
	require.NoError(t, err)
	require.True(t, removed)

	require.NoError(t, <-admitted)

	// Whichever order the two committed in, the invariant is the same: if the seat
	// exists at the end, its project's epoch must have moved. The seat outliving an
	// unmoved epoch is the stale-grant state.
	seat, err := testDB.queryMember(racing.ProjectID, "rvTarget")
	require.NoError(t, err)
	if seat == nil || seat.Status != MemberStatusActive || seat.Removing != 0 {
		t.Skip("the admission lost the race and was refused; nothing to invalidate")
	}
	assert.Greater(t, epochOf(t, racing.ProjectID), before,
		"a seat admitted concurrently with the Space removal is live but unreachable "+
			"through the Space conjunction, and its epoch never moved — a peer that cached "+
			"a grant at the old epoch keeps it. The enumeration's read view must be assigned "+
			"AFTER the removal takes its space_member X lock; see "+
			"bumpMemberEpochForSpaceMemberTx")
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
	_, roles, err := projectpkg.ProjectMemberships(
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

	_, roles, err = projectpkg.ProjectMemberships(
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

	epoch, roles, err := projectpkg.ProjectMemberships(testCtx.DB(), spaceA, upper,
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
