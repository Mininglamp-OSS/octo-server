package project

import (
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression cases for the defects found in code review. Each one fails against the code as
// it was written, so the fix is pinned rather than merely applied.

// TestCascadeFinishesEveryPage pins the fix for the page-drop leak.
//
// The step used to process ONE page and return nil, which made the worker mark the job done —
// and nothing re-drives a done job, because the reconcile scan is read-only by design. Every
// seat past the first page survived forever. This drives more seats than one page holds and
// asserts all of them are closed by a single invocation.
func TestCascadeFinishesEveryPage(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "wide")
	seedSpaceMember(t, spaceA, "wide", 0, 1)

	// A page is 200; seeding 200 real projects over HTTP would be slow, so shrink the page
	// and create a handful more than it. The paging LOGIC is what is under test, not the
	// literal 200.
	const seats = 5
	origPage := cascadePageSize
	cascadePageSize = 2
	t.Cleanup(func() { cascadePageSize = origPage })

	for i := 0; i < seats; i++ {
		proj := createProjectVia(t, srv, spaceA, ownerTok, "wide-"+string(rune('a'+i)))
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+proj.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"wide"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	removeSpaceMember(t, spaceA, "wide")
	require.NoError(t, runCascade(t, p, spaceA, "wide", "owner1", spacemod.MemberRemoveReasonKicked),
		"a multi-page walk must complete in one invocation, not return nil after page 1")

	var remaining int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE space_id = ? AND uid = ? AND status = ?",
		spaceA, "wide", MemberStatusActive,
	).LoadOne(&remaining))
	assert.Equal(t, 0, remaining,
		"every seat must be closed; a surviving one is the permanent leak this fix removes")
}

// TestCascadeReturnsRetryableErrorWhenBudgetExhausted pins the other half: when the page
// budget really does run out, the step must return an error so the job is re-claimed. Returning
// nil would mark the job done and lose the remaining work.
func TestCascadeReturnsRetryableErrorWhenBudgetExhausted(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "wide")
	seedSpaceMember(t, spaceA, "wide", 0, 1)

	origPage, origMax := cascadePageSize, cascadeMaxPages
	cascadePageSize, cascadeMaxPages = 1, 2
	t.Cleanup(func() { cascadePageSize, cascadeMaxPages = origPage, origMax })

	for i := 0; i < 4; i++ {
		proj := createProjectVia(t, srv, spaceA, ownerTok, "budget-"+string(rune('a'+i)))
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+proj.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"wide"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	removeSpaceMember(t, spaceA, "wide")
	err := runCascade(t, p, spaceA, "wide", "owner1", spacemod.MemberRemoveReasonKicked)
	require.Error(t, err, "with the budget spent the step must ask for a retry, not report success")
	assert.True(t, errors.Is(err, errCascadeIncomplete))

	// And it made progress, so the retry converges rather than spinning.
	var remaining int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` WHERE space_id = ? AND uid = ? AND status = ?",
		spaceA, "wide", MemberStatusActive,
	).LoadOne(&remaining))
	assert.Less(t, remaining, 4, "each pass must close seats so retries terminate")

	// Draining it takes further passes and then reports success.
	for i := 0; i < 5 && remaining > 0; i++ {
		_ = runCascade(t, p, spaceA, "wide", "owner1", spacemod.MemberRemoveReasonKicked)
		require.NoError(t, testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM `octo_project_member` WHERE space_id = ? AND uid = ? AND status = ?",
			spaceA, "wide", MemberStatusActive,
		).LoadOne(&remaining))
	}
	assert.Equal(t, 0, remaining)
}

// TestReconcileIsReentrancyGuarded pins the guard that keeps two overlapping scans out of the
// epoch history. The timing wheel fires `go task()` per tick without waiting for the previous
// run, so overlap is the normal case for a scan slower than its interval — not an edge case.
func TestReconcileIsReentrancyGuarded(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")
	_ = created

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runReconcile()
			p.refreshDistributionMetrics()
		}()
	}
	wg.Wait()
	// The assertion is the absence of a race (this file is run under -race in CI) and the
	// absence of a panic; the guard also means most of those calls returned immediately.
}

// TestEpochHistoryIsSafeUnderConcurrentObserve pins the replacement for the reassigned
// sync.Map. Reassigning a sync.Map is a data race twice over — an unsynchronised write to a
// shared variable, and copying a struct containing a Mutex.
func TestEpochHistoryIsSafeUnderConcurrentObserve(t *testing.T) {
	var h epochHistory
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.observe("p", int64(j))
			}
		}(i)
	}
	wg.Wait()

	// And it detects an actual regression on the same replica.
	var h2 epochHistory
	regressed, _ := h2.observe("x", 5)
	assert.False(t, regressed, "the first observation has nothing to compare against")
	regressed, prev := h2.observe("x", 4)
	assert.True(t, regressed)
	assert.Equal(t, int64(5), prev)
	regressed, _ = h2.observe("x", 9)
	assert.False(t, regressed)
}

// TestI1CheckRunsInsideTheWriteTransaction pins that the membership read is on the
// TRANSACTION, not on the session.
//
// It used to call pkg/space.CheckMembership, which takes a *dbr.Session and therefore runs on
// a different pooled connection in its own implicit transaction — proving nothing about the
// state at COMMIT time. The observable consequence of the fix is a shared lock on the
// space_member row: a concurrent Space removal can no longer commit between the check and the
// membership write, which is the entire point of the brief's "inside the request transaction".
//
// Note what this test deliberately does NOT do: mutate the Space from another connection and
// re-read inside the same transaction. Under REPEATABLE READ the transaction keeps its
// snapshot, so that would assert MySQL isolation rather than anything about this code.
func TestI1CheckRunsInsideTheWriteTransaction(t *testing.T) {
	srv, p := setup(t)
	_, _, _ = projectWithMembers(t, srv)
	seedUser(t, "target")
	seedSpaceMember(t, spaceA, "target", 0, 1)
	seedSpace(t, spaceB, 1)
	seedUser(t, "elsewhere")
	seedSpaceMember(t, spaceB, "elsewhere", 0, 1)

	// The predicate itself, read inside a transaction — all three uids in ONE statement, which is
	// how every write path now takes these locks (see lockSpaceSeatsTx: one statement is what
	// keeps this module out of the row-level 1213 cycle with the Space disband scan). Asserting
	// them together also pins that a batch judges each uid independently.
	tx, err := p.db.session.Begin()
	require.NoError(t, err)
	held, err := p.db.lockSpaceSeatsTx(tx, spaceA,
		[]string{"target", "elsewhere", "nobody-at-all"})
	require.NoError(t, err, "FOR SHARE OF sm must parse (needs MySQL 8.0.1+)")
	assert.True(t, held["target"])
	assert.False(t, held["elsewhere"], "a member of another Space must not satisfy I1 here")
	assert.False(t, held["nobody-at-all"])
	require.NoError(t, tx.Rollback())

	// A banned Space fails the predicate — same as CheckMembership, because this is an
	// authorization decision. Read in a FRESH transaction so the snapshot includes the ban.
	setSpaceStatus(t, spaceA, 2)
	tx, err = p.db.session.Begin()
	require.NoError(t, err)
	held, err = p.db.lockSpaceSeatsTx(tx, spaceA, []string{"target"})
	require.NoError(t, err)
	assert.False(t, held["target"], "space.status=2 must fail an authorization predicate")
	require.NoError(t, tx.Rollback())
	setSpaceStatus(t, spaceA, 1)

	// And the lock really serializes: while the check's transaction is open, an UPDATE of that
	// space_member row (what a Space removal does) must block. That is the window the old
	// session-scoped read left open.
	tx, err = p.db.session.Begin()
	require.NoError(t, err)
	held, err = p.db.lockSpaceSeatsTx(tx, spaceA, []string{"target"})
	require.NoError(t, err)
	require.True(t, held["target"])

	blocked := probeSpaceMemberUpdateBlocks(t, spaceA, "target")
	assert.True(t, blocked,
		"a concurrent Space removal must block on the shared lock; if it does not, the check "+
			"is not serialized against it and a non-member can still be admitted")

	require.NoError(t, tx.Rollback())
	// Once the transaction is gone the same UPDATE goes through.
	assert.False(t, probeSpaceMemberUpdateBlocks(t, spaceA, "target"),
		"after rollback the row must be writable again")
}

// probeSpaceMemberUpdateBlocks reports whether UPDATE space_member blocks on a lock right now.
// It uses its own connection with a 1s innodb_lock_wait_timeout so the probe is fast and a
// 1205 is unambiguous.
func probeSpaceMemberUpdateBlocks(t *testing.T, spaceID, uid string) bool {
	t.Helper()
	conn, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	_, err = conn.Exec("SET SESSION innodb_lock_wait_timeout = 1")
	require.NoError(t, err)
	_, err = conn.Exec("UPDATE space_member SET version = version + 1 WHERE space_id = ? AND uid = ?",
		spaceID, uid)
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1205 {
		return true
	}
	t.Fatalf("unexpected probe error (wanted 1205 lock wait timeout or success): %v", err)
	return false
}

// TestActorRoleIsReReadUnderTheProjectLock pins the privilege TOCTOU fix.
//
// The actor's role used to come from the middleware, i.e. from the Redis membership cache,
// read before the transaction opened. Here the cache is deliberately left saying "admin"
// while the database says otherwise, and the write must still be refused.
func TestActorRoleIsReReadUnderTheProjectLock(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "admin1", "plain1")
	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/admin1/role",
		ownerTok, map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Warm admin1's cached role as admin.
	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", tokens["admin1"], nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Demote in the database ONLY, leaving the cache stale — exactly the state the old code
	// would have acted on.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleCommon, created.ProjectID, "admin1").Exec()
	require.NoError(t, err)

	// An ACTOR-level failure is one top-level 403, not a 200 carrying a per-uid note: the
	// caller is not authorized at all, and no target in the batch could have succeeded.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"plain1"}})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	// plain1 still holds their seat.
	member, err := p.db.queryMember(created.ProjectID, "plain1")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusActive, member.Status)
}

// TestPageParamsCannotOverflowIntoAServerError pins the clamp.
//
// ?page=9223372036854775807 used to overflow (page-1)*limit to a negative OFFSET, which MySQL
// rejects with 1064, so the handler answered err.server.project.query_failed with
// http_status 500 — a user-triggerable 5xx and a stream of internal-error logs.
func TestPageParamsCannotOverflowIntoAServerError(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")

	for _, q := range []string{
		"?page=9223372036854775807",
		"?page=9223372036854775807&limit=200",
		"?page=4611686018427387904",
	} {
		w := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects"+q, ownerTok, nil)
		assert.Equal(t, http.StatusOK, w.Code,
			"list %s must be an empty page, not a 5xx: %s", q, w.Body.String())
		w = doJSON(t, srv, http.MethodGet,
			"/v1/projects/"+created.ProjectID+"/members"+q, ownerTok, nil)
		assert.Equal(t, http.StatusOK, w.Code,
			"roster %s must be an empty page, not a 5xx: %s", q, w.Body.String())
	}
}

// TestCascadeClosesStaleSeatOnDisbandedProject pins the semantics change that keeps the
// paging loop's "no progress" signal honest.
//
// deactivateSeatForCascade used to return (false, nil) when the project row was not active,
// on the reasoning that disbandProject already closed its seats. Two problems: an active seat
// on a disbanded project (however it arose) would then never be cleaned and the reconcile scan
// would report it forever, and the caller's loop would read "no progress" and stop with the
// seat still active. Now the stale row is closed — without an epoch bump, since the project is
// disbanded and no consumer is watching its epoch.
func TestCascadeClosesStaleSeatOnDisbandedProject(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "stale")
	seedSpaceMember(t, spaceA, "stale", 0, 1)

	w := doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	epochAfterDisband := epochOf(t, created.ProjectID)

	// Inject the orphan shape: an active seat on an already-disbanded project.
	injectOrphanSeat(t, created.ProjectID, spaceA, "stale")
	removeSpaceMember(t, spaceA, "stale")

	require.NoError(t, runCascade(t, p, spaceA, "stale", "owner1", "kicked"))

	member, err := p.db.queryMember(created.ProjectID, "stale")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusRemoved, member.Status,
		"a stale seat on a disbanded project must be closed, not skipped")
	assert.Equal(t, epochAfterDisband, epochOf(t, created.ProjectID),
		"closing a stale seat on a disbanded project must not move its epoch")
}
