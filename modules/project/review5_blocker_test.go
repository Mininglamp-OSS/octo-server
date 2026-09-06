package project

// PR #841 review round 2 (yujiawei P1-1..P1-3, independently confirmed by Jerry-Xin as
// B-1..B-3). Three guarantees this PR itself introduced were each applied at all-but-one
// of their call sites, and the omission was unrecorded in every case:
//
//	B-1  requireActorSpaceSeatTx reached five privileged writes; addOneMember was not one.
//	B-2  MemberRole's `ok` is honoured by projectMiddleware and discarded by the list route.
//	B-3  createProject locks `space` before `space_member`, the reverse of the order
//	     modules/space/db.go records as a known Error 1213 incident.
//
// Each test below fails on the pre-fix tree for the stated reason, not incidentally.

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- B-2 ----------

// TestProjectListRefusesAnActorWhoseSpaceSeatIsGoneBehindAStalePositiveCache pins the one
// call site that discards MemberRole's membership result.
//
// The list route's only Space gate is spaceIDParamMiddleware, which answers from the shared
// space:member:{spaceID}:{uid} cache. This test reproduces the stale-positive state the
// Space module itself logs when its DEL and its negative-cache fallback both fail (and which
// cache-aside `Set` can also reinstate for a full TTL): the seat is gone in the database and
// the cache still says member. The handler holds the authoritative answer — MemberRole's ok
// — and must not serve the Space's project list to a non-member after throwing it away.
func TestProjectListRefusesAnActorWhoseSpaceSeatIsGoneBehindAStalePositiveCache(t *testing.T) {
	srv, _ := setup(t)
	_, tokens, created := projectWithMembers(t, srv, "gone1")

	// Warm the Space-gate cache with a POSITIVE entry while the seat is still real.
	w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", tokens["gone1"], nil)
	require.Equal(t, http.StatusOK, w.Code, "precondition: a real member can list; body: %s", w.Body.String())
	require.NotEmpty(t, redisKeys(t, "space:member:"+spaceA+":gone1"),
		"precondition: the Space gate must have cached a positive entry")

	// Close the Space seat WITHOUT invalidating that entry — the two-failure branch.
	removeSpaceMember(t, spaceA, "gone1")
	require.NotEmpty(t, redisKeys(t, "space:member:"+spaceA+":gone1"),
		"precondition: the stale positive must survive, or this test proves nothing")

	w = doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", tokens["gone1"], nil)
	assertProjectErrorCode(t, w, "err.shared.auth.forbidden")
	assert.NotContains(t, w.Body.String(), created.ProjectID,
		"a user with no Space seat must not learn any project id in that Space")
}

// ---------- B-3 ----------

// isDeadlockErr reports whether err carries MySQL 1213 (ER_LOCK_DEADLOCK) anywhere in its
// chain. The service wraps store errors, so errors.As is the only reliable test.
func isDeadlockErr(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213
	}
	return false
}

// TestCreateProjectDoesNotDeadlockAgainstTheSpaceDisbandLockOrder drives the real
// createProject against a transaction that replicates modules/space's disband lock order
// (space_member FOR UPDATE, then UPDATE space — lockActiveMemberUIDsTx followed by the
// status flip, in both disbandSpace and forceDisbandSpace).
//
// Both locks are record locks on rows that exist, so the cycle is real rather than the
// gap-lock case modules/space/db.go:71-88 analyses: create holds X(space) and waits for
// S(space_member); disband holds X(space_member) and waits for X(space). InnoDB breaks it
// with 1213, and the victim may be the operator's disband — a step of the member-removal
// security cascade — which then answers 500.
//
// The fix is a lock-order swap, so the assertion is symmetric: NEITHER side may see 1213,
// and create must refuse cleanly because the Space is no longer active.
func TestCreateProjectDoesNotDeadlockAgainstTheSpaceDisbandLockOrder(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedUser(t, "creator1")
	seedSpaceMember(t, spaceA, "creator1", 0, 1)
	// A second seat so the disband side's range lock is not a single row.
	seedUser(t, "other1")
	seedSpaceMember(t, spaceA, "other1", 0, 1)

	// The disband side, holding X on every active space_member row of the Space.
	txB, err := testCtx.DB().Begin()
	require.NoError(t, err)
	defer txB.RollbackUnlessCommitted()
	var lockedUIDs []string
	_, err = txB.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id=? AND status=1 FOR UPDATE", spaceA,
	).Load(&lockedUIDs)
	require.NoError(t, err)
	require.Len(t, lockedUIDs, 2, "precondition: the disband side must hold both seats")

	type createOutcome struct{ err error }
	done := make(chan createOutcome, 1)
	go func() {
		_, cErr := p.createProject(createInput{
			SpaceID:         spaceA,
			Creator:         "creator1",
			Name:            "deadlock-probe",
			Discoverability: DiscoverabilitySpaceListed,
		})
		done <- createOutcome{err: cErr}
	}()

	// Let the creating transaction reach whichever lock it blocks on. With the pre-fix
	// order it has already taken X(space) by now; with the fixed order it is parked on
	// S(space_member) and holds nothing.
	time.Sleep(700 * time.Millisecond)

	_, updErr := txB.UpdateBySql("UPDATE `space` SET status=0 WHERE space_id=?", spaceA).Exec()

	var got createOutcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("createProject 未在 15s 内返回：加锁顺序把它挂死了")
	}
	if updErr == nil {
		require.NoError(t, txB.Commit())
	}

	assert.False(t, isDeadlockErr(updErr),
		"the Space-disband transaction must not be deadlocked by this module's lock order: %v", updErr)
	assert.False(t, isDeadlockErr(got.err),
		"createProject must not deadlock against the Space-disband lock order: %v", got.err)
	assert.ErrorIs(t, got.err, errNotSpaceMember,
		"with the Space disbanded first, create must refuse cleanly rather than fail on a lock: %v", got.err)
}
