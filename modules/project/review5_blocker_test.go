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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	// Release the space_member locks BEFORE waiting on create. This ordering is not
	// incidental: with the lock order fixed, create is parked on S(space_member) and can only
	// proceed once this transaction ends, so waiting first would hang the test on its own
	// orchestration rather than on the defect. With the order broken, the UPDATE above has already come
	// back 1213 and the deferred rollback covers it.
	if updErr == nil {
		require.NoError(t, txB.Commit())
	}

	var got createOutcome
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("createProject 未在 15s 内返回：加锁顺序把它挂死了")
	}

	assert.False(t, isDeadlockErr(updErr),
		"the Space-disband transaction must not be deadlocked by this module's lock order: %v", updErr)
	assert.False(t, isDeadlockErr(got.err),
		"createProject must not deadlock against the Space-disband lock order: %v", got.err)
	assert.ErrorIs(t, got.err, errNotSpaceMember,
		"with the Space disbanded first, create must refuse cleanly rather than fail on a lock: %v", got.err)
}

// ---------- N-1: the wire classification B-1 made reachable ----------

// withAddSeam swaps the add seam so every target before failOn is admitted for real.
func withAddSeamOn(t *testing.T, p *Project, failOn string, inject error, calls *int) {
	t.Helper()
	orig := p.addOneFn
	p.addOneFn = func(projectID, spaceID, actorUID, uid string) (bool, error) {
		*calls++
		if uid == failOn {
			return false, inject
		}
		return orig(projectID, spaceID, actorUID, uid)
	}
	t.Cleanup(func() { p.addOneFn = orig })
}

// TestActorLevelSpaceSeatLossStopsTheAddBatchAndIsNamedCorrectly covers the classification
// Jerry-Xin asked to land with B-1: before it, an actor whose Space seat closed mid-batch fell
// to the default arm, so their own expired standing was reported per uid as "store_failed"
// while the loop kept opening one doomed transaction for every remaining target.
func TestActorLevelSpaceSeatLossStopsTheAddBatchAndIsNamedCorrectly(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"a1", "a2", "a3"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	r := mountProject(t, p)

	calls := 0
	withAddSeamOn(t, p, "a2", errActorNotSpaceMember, &calls)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"a1", "a2", "a3"}})
	require.Equal(t, http.StatusOK, w.Code,
		"a1 committed, so the honest answer is a per-target report: %s", w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "every uid must be accounted for: %s", w.Body.String())
	assert.True(t, outcomes[0].OK, "a1 was admitted before the actor's seat closed")
	assert.Equal(t, reasonNotSpaceMember, outcomes[1].Reason,
		"the actor's own missing Space seat must not be reported as store_failed")
	assert.Equal(t, outcomeNotAttempted, outcomes[2].Reason,
		"the tail really was never attempted")
	assert.Equal(t, 2, calls,
		"the batch must STOP at the actor-level failure, not open a transaction per remaining uid")
}

// TestActorLevelSpaceSeatLossWithNothingCommittedIsOneStatusCode pins the other half of the
// contract: with nothing committed, a single status code is the honest answer — and it must
// name the Space seat, not the project role. Sending the caller to check their project role
// would point them at the one thing that is still intact.
func TestActorLevelSpaceSeatLossWithNothingCommittedIsOneStatusCode(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "b1")
	seedSpaceMember(t, spaceA, "b1", 0, 1)
	r := mountProject(t, p)

	calls := 0
	withAddSeamOn(t, p, "b1", errActorNotSpaceMember, &calls)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"b1"}})
	assertProjectErrorCode(t, w, "err.server.project.actor_not_space_member")
	assert.Equal(t, 1, calls, "the batch must not continue past an actor-level refusal")
}

// TestActorLevelSpaceSeatLossOnRemoveIsClassifiedToo covers the same classification on the
// removal endpoint, which drives its loop in the HANDLER rather than the service — so it had
// the defect in its own shape: the default arm both mislabelled the refusal and kept the loop
// running.
func TestActorLevelSpaceSeatLossOnRemoveIsClassifiedToo(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "d1", "d2", "d3")
	r := mountProject(t, p)

	// Nothing committed: one status code, naming the Space seat.
	calls := 0
	withRemoveSeam(t, p, "d1", errActorNotSpaceMember, &calls)
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"d1", "d2", "d3"}})
	assertProjectErrorCode(t, w, "err.server.project.actor_not_space_member")
	assert.Equal(t, 1, calls,
		"the handler must stop at the actor-level refusal, not run the remaining targets")
}

// TestActorLevelSpaceSeatLossOnRemoveReportsWhatCommitted is the partial-commit half on the
// removal endpoint.
func TestActorLevelSpaceSeatLossOnRemoveReportsWhatCommitted(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "e1", "e2", "e3")
	r := mountProject(t, p)

	calls := 0
	withRemoveSeam(t, p, "e2", errActorNotSpaceMember, &calls)
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"e1", "e2", "e3"}})
	require.Equal(t, http.StatusOK, w.Code,
		"e1 committed, so the report must say so: %s", w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "body: %s", w.Body.String())
	assert.True(t, outcomes[0].OK, "e1's removal committed")
	assert.Equal(t, reasonNotSpaceMember, outcomes[1].Reason)
	assert.Equal(t, outcomeNotAttempted, outcomes[2].Reason)
	assert.Equal(t, 2, calls, "the handler must stop rather than run e3")
}

// TestTargetLevelSpaceSeatLossDoesNotStopTheAddBatch is the switch-order guard.
//
// errActorNotSpaceMember WRAPS errNotSpaceMember, so a `case errors.Is(err, errNotSpaceMember)`
// arm placed above the actor arm would swallow the actor-level refusal — and, read the other
// way, an actor arm written too broadly would stop the batch on an ordinary rejected uid. This
// pins the target-level direction: one uid without a Space seat is one rejected uid, and the
// rest of the batch still runs.
func TestTargetLevelSpaceSeatLossDoesNotStopTheAddBatch(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"c1", "c2"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	// c1 has no Space seat at all — a genuine TARGET-level refusal, no seam needed.
	seedUser(t, "c0")
	r := mountProject(t, p)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"c0", "c1", "c2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "the batch must have continued past the rejected uid: %s", w.Body.String())
	assert.Equal(t, reasonNotSpaceMember, outcomes[0].Reason, "c0 holds no Space seat")
	assert.True(t, outcomes[1].OK, "c1 must still have been admitted")
	assert.True(t, outcomes[2].OK, "c2 must still have been admitted")
	assert.NotEqual(t, outcomeNotAttempted, outcomes[2].Reason,
		"a target-level refusal must not label the rest of the batch not_attempted")
}

// ---------- the read-view trap ----------

// TestCreateDoesNotTakeItsSpaceSeatLockThroughAJoin is a source guard for a defect that is
// invisible at the call site and silent at runtime.
//
// checkSpaceMembershipForWriteTx JOINs `space`. A table outside a `FOR SHARE OF` list is read
// as a CONSISTENT read, and a consistent read OPENS the transaction's read view — so using
// that helper as createProject's first statement freezes the snapshot before the `space` row
// lock, and all three creation quotas are then counted against it. Six concurrent creates
// passed MaxPerSpace=1 that way, and nothing about the call site hints at it.
//
// The concurrency acceptance tests catch the consequence. This guard names the cause, because
// the obvious future "cleanup" — collapsing the two helpers into one — reintroduces it.
// readLinesWithoutComments returns name's source with comment text removed but the LINE
// structure intact.
//
// stripComments (api_i18n_test.go) collapses the whole file onto one line, which is right for
// "does this token appear anywhere" guards and useless for slicing one function out. And the
// comments must go: this file's own prose names the helper the guard forbids, so a raw read
// would match the warning rather than the code.
func readLinesWithoutComments(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	require.NoError(t, err, "read %s", name)
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// funcBody returns the source of the function whose signature starts with sig, up to the next
// top-level func.
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	require.GreaterOrEqual(t, start, 0, "%s must exist", sig)
	rest := src[start+len(sig):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return src[start : start+len(sig)+end]
	}
	return src[start:]
}

// TestCreateDoesNotTakeItsSpaceSeatLockThroughAJoin is a source guard for a defect that is
// invisible at the call site and silent at runtime.
//
// checkSpaceMembershipForWriteTx JOINs `space`. A table outside a `FOR SHARE OF` list is read
// as a CONSISTENT read, and a consistent read OPENS the transaction's read view — so using
// that helper as createProject's first statement freezes the snapshot before the `space` row
// lock, and all three creation quotas are then counted against it. Six concurrent creates
// passed MaxPerSpace=1 that way, and nothing about the call site hints at it.
//
// The concurrency acceptance tests catch the consequence. This guard names the cause, because
// the obvious future "cleanup" — collapsing the two helpers into one — reintroduces it.
func TestCreateDoesNotTakeItsSpaceSeatLockThroughAJoin(t *testing.T) {
	fn := funcBody(t, readLinesWithoutComments(t, "service.go"),
		"func (p *Project) createProject(")
	assert.Contains(t, fn, "lockSpaceSeatRowTx",
		"createProject must take the JOIN-free seat lock")
	assert.NotContains(t, fn, "checkSpaceMembershipForWriteTx",
		"createProject must NOT use the JOINing helper: the JOIN opens the read view before "+
			"the `space` lock and every creation quota is then counted from a stale snapshot")

	// The JOIN-free helper must stay JOIN-free.
	helper := funcBody(t, readLinesWithoutComments(t, "db.go"),
		"func (d *DB) lockSpaceSeatRowTx(")
	assert.NotContains(t, strings.ToUpper(helper), "JOIN",
		"lockSpaceSeatRowTx must not grow a JOIN: that is exactly what opens the read view")
	assert.Contains(t, helper, "FOR SHARE",
		"the seat check must still be a LOCKING read, or a concurrent Space removal can "+
			"commit between it and the insert")
}

// ---------- N-2: a broken payload must not be a destructive default ----------

// TestRoleEndpointRejectsAMissingRoleInsteadOfDemoting pins the sibling of the round-1
// leave-handler hardening ("a destructive action must not be the failure mode of a broken
// payload"). roleReq.Role was a plain int, so `{}` and `{"role": null}` both decoded to
// RoleCommon (0), passed IsValidRole, and silently demoted the target with a 200.
func TestRoleEndpointRejectsAMissingRoleInsteadOfDemoting(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "n2a")

	// Promote n2a so a silent demotion is observable.
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/n2a/role", ownerTok,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, RoleAdmin, roleOfMember(t, created.ProjectID, "n2a"))

	for name, body := range map[string]any{
		"empty object": map[string]any{},
		"null role":    map[string]any{"role": nil},
	} {
		t.Run(name, func(t *testing.T) {
			w := doJSON(t, srv, http.MethodPut,
				"/v1/projects/"+created.ProjectID+"/members/n2a/role", ownerTok, body)
			assertProjectErrorCode(t, w, "err.server.project.request_invalid")
			assert.Equal(t, RoleAdmin, roleOfMember(t, created.ProjectID, "n2a"),
				"a payload that names no role must not change the role")
		})
	}

	// A role that IS named still works, including the zero value.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/n2a/role", ownerTok,
		map[string]any{"role": RoleCommon})
	require.Equal(t, http.StatusOK, w.Code, "an explicit demotion is legitimate: %s", w.Body.String())
	assert.Equal(t, RoleCommon, roleOfMember(t, created.ProjectID, "n2a"))
}

// roleOfMember reads a member's current project role straight from the database.
func roleOfMember(t *testing.T, projectID, uid string) int {
	t.Helper()
	m, err := testDB.queryMember(projectID, uid)
	require.NoError(t, err)
	require.NotNil(t, m, "member %s must exist", uid)
	return m.Role
}
