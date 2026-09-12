package project

// PR #841 review round 2 (yujiawei P1-1..P1-3, independently confirmed by Jerry-Xin as
// B-1..B-3). Three guarantees this PR itself introduced were each applied at all-but-one
// of their call sites, and the omission was unrecorded in every case:
//
//	B-1  the actor Space-seat check reached five privileged writes; addOneMember was not one.
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
		// createProjectOnce, NOT createProject: the wrapper retries 1213, so if InnoDB victimises
		// the CREATE, attempt 2 runs after the disband transaction has released its locks and
		// succeeds — and this reproducer would then pass on a coin toss (same defect as the
		// row-order reproducer, PR #841 round 4, P2-3a).
		_, cErr := p.createProjectOnce(createInput{
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

// TestTargetLevelSpaceSeatLossRejectsAtomicAddBatch pins the target-level I1 refusal under the
// current all-or-nothing members/add contract. A missing target seat rejects the transaction
// and must not leave later targets partially admitted.
func TestTargetLevelSpaceSeatLossRejectsAtomicAddBatch(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"c1", "c2"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	seedUser(t, "c0")
	r := mountProject(t, p)

	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, addMembersPayload("c0", "c1", "c2"))
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")
	for _, uid := range []string{"c0", "c1", "c2"} {
		member, err := p.db.queryMember(created.ProjectID, uid)
		require.NoError(t, err)
		assert.Nil(t, member, "an atomic target-level refusal must admit nobody, including %s", uid)
	}
}

// mustRead reads a source file in this package.
func mustRead(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	require.NoError(t, err, "read %s", name)
	return string(data)
}

// readLinesWithoutComments returns name's source with comment text removed but the LINE
// structure intact.
//
// stripComments (api_i18n_test.go) collapses the whole file onto one line, which is right for
// "does this token appear anywhere" guards and useless for slicing one function out. And the
// comments must go: this file's own prose names the helpers the guards forbid, so a raw read
// would match the warning rather than the code.
func readLinesWithoutComments(t *testing.T, name string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(mustRead(t, name), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// implBody returns the body of a write path's IMPLEMENTATION, preferring the `...Once` form
// over its retry wrapper.
//
// This indirection is load-bearing, and it exists because of a near-miss: wrapping the seven
// write entry points in retryOnLockConflict renamed every implementation to `...Once`, and three
// source guards silently began inspecting the empty wrapper instead. One failed loudly; two
// went GREEN while checking nothing. A guard that can be defeated by a rename is not a guard, so
// resolution happens here, once, for all of them.
func implBody(t *testing.T, src, receiverAndName string) string {
	t.Helper()
	if i := strings.Index(src, receiverAndName+"Once("); i >= 0 {
		return funcBody(t, src, receiverAndName+"Once(")
	}
	return funcBody(t, src, receiverAndName+"(")
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
