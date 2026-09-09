package project

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/server"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Third review round. Each case fails against the code as it was before the corresponding fix.

// --- P1: reconcile cursors reset every tick, so rows past the page cap were never scanned ---

// TestReconcileCursorsSurviveAcrossTicks pins that a scan resumes where the previous tick
// stopped, instead of re-reading the first window forever.
//
// Every cursor used to be a local variable initialised per scan, so with the page cap in place
// the scans could only ever see the first reconcileMaxPages * ReconcileLimit rows. Past that,
// later rows were never examined at all.
func TestReconcileCursorsSurviveAcrossTicks(t *testing.T) {
	srv, p := setup(t)
	resetCursorsForTest()

	// Force the smallest possible window: 1 row per page, 1 page per tick.
	origLimit, origPages := p.cfg.ReconcileLimit, reconcileMaxPagesForTest()
	p.cfg.ReconcileLimit = 1
	setReconcileMaxPagesForTest(1)
	t.Cleanup(func() {
		p.cfg.ReconcileLimit = origLimit
		setReconcileMaxPagesForTest(origPages)
		resetCursorsForTest()
	})

	// Three orphan projects: their Space row does not exist, so all three are violations.
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	ids := []string{}
	for _, n := range []string{"orph-a", "orph-b", "orph-c"} {
		ids = append(ids, createProjectVia(t, srv, spaceA, token, n).ProjectID)
	}
	_, err := testCtx.DB().DeleteFrom("space").Where("space_id = ?", spaceA).Exec()
	require.NoError(t, err)

	// Tick 1 sees row 1 and must SAVE its position rather than discard it.
	p.scanOrphanProjects()
	c1 := orphanCursorForTest()
	assert.NotZero(t, c1, "a truncated tick must persist its cursor, not reset to the start")

	// Tick 2 must move past it.
	p.scanOrphanProjects()
	c2 := orphanCursorForTest()
	assert.Greater(t, c2, c1, "the next tick must resume from the saved cursor, not restart")

	// Tick 3 takes the third (last) row. The page was still full, so the scan cannot yet know
	// it is at the end — that takes one more tick returning a short page.
	p.scanOrphanProjects()
	c3 := orphanCursorForTest()
	assert.Greater(t, c3, c2)

	// Tick 4 reads a short page, which completes the rotation and resets the cursor so the next
	// rotation covers the table again instead of stopping at the high-water mark forever.
	p.scanOrphanProjects()
	assert.Zero(t, orphanCursorForTest(),
		"reaching the end must reset the rotation so it can start over")
	assert.Len(t, ids, 3)
}

// --- P1: a direct role promotion did not re-check the target's Space seat ---

// TestPromotionRequiresTargetStillInSpace pins the gap the transfer path was already hardened
// against but the direct role change was not: promoting a member whose Space seat is gone (the
// cascade has not caught up) creates a second owner, which lets the original owner leave with no
// transfer — and the cascade then closes the new owner, leaving nobody able to administer.
func TestPromotionRequiresTargetStillInSpace(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "leaver1")

	// Remove from the Space but do NOT run the cascade: the Project seat is still active.
	removeSpaceMember(t, spaceA, "leaver1")
	member, err := p.db.queryMember(created.ProjectID, "leaver1")
	require.NoError(t, err)
	require.Equal(t, MemberStatusActive, member.Status, "precondition: the seat is still active")

	// Promotion must be refused.
	for _, role := range []int{RoleAdmin, RoleOwner} {
		w := doJSON(t, srv, http.MethodPut,
			"/v1/projects/"+created.ProjectID+"/members/leaver1/role", ownerTok,
			map[string]any{"role": role})
		assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")
	}

	// A DEMOTION is still allowed: it narrows privilege, so blocking it would stop the operator
	// doing the safe thing.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleAdmin, created.ProjectID, "leaver1").Exec()
	require.NoError(t, err)
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/leaver1/role", ownerTok,
		map[string]any{"role": RoleCommon})
	assert.Equal(t, http.StatusOK, w.Code,
		"demotion must stay allowed even when the target is on their way out: %s", w.Body.String())
}

// --- P2: an abandoned job was reported as a leak even when a newer pending job existed ---

// TestAbandonedLeakIgnoresPairsWithANewerPendingJob pins the in-flight exemption on the abandoned
// scan. remove -> rejoin -> remove enqueues a second job (the outbox is deliberately not unique
// on (space_id, uid)), so a pair can hold an old abandoned job AND a live pending one at once.
// Alerting "needs manual intervention" about work that is already scheduled is a false alarm.
func TestAbandonedLeakIgnoresPairsWithANewerPendingJob(t *testing.T) {
	srv, p := setup(t)
	resetCursorsForTest()
	_, _, created := projectWithMembers(t, srv, "leaker1")
	removeSpaceMember(t, spaceA, "leaker1")

	// An exhausted job for the pair, and nothing else: a real leak.
	insertCleanupJob(t, spaceA, "leaker1", cleanupStatusAbandoned)
	rows, err := p.queryAbandonedLeakPage("", "", 50)
	require.NoError(t, err)
	assert.Len(t, violatingI1Rows(rows), 1, "an abandoned job with no live successor is a genuine leak")

	// Now a newer pending job for the same pair — that one will clean the seat.
	insertCleanupJob(t, spaceA, "leaker1", cleanupStatusPending)
	rows, err = p.queryAbandonedLeakPage("", "", 50)
	require.NoError(t, err)
	assert.Empty(t, violatingI1Rows(rows),
		"a pair with a live pending job must not be reported as needing manual intervention")
	_ = created
}

// --- P2: leave swallowed every bind error, so a malformed body left the project ---

// TestLeaveRejectsMalformedBodyButAllowsEmpty pins the distinction. An absent body is normal on
// this route; a truncated or malformed one used to parse as an empty struct, so transfer_to
// silently became "" and the member left anyway. A destructive action must not be the failure
// mode of a broken payload.
func TestLeaveRejectsMalformedBodyButAllowsEmpty(t *testing.T) {
	srv, p := setup(t)
	_, tokens, created := projectWithMembers(t, srv, "member1", "member2")

	// Malformed JSON must be rejected and must NOT remove the member.
	for _, body := range []string{`{"transfer_to":`, `not json`, `{"transfer_to": }`} {
		w := doRaw(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
			tokens["member1"], body)
		assertProjectErrorCode(t, w, "err.server.project.request_invalid")
		m, err := p.db.queryMember(created.ProjectID, "member1")
		require.NoError(t, err)
		assert.Equal(t, MemberStatusActive, m.Status,
			"a malformed body must not remove the member (body %q)", body)
	}

	// An absent body is still the normal, working case.
	w := doRaw(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
		tokens["member1"], "")
	require.Equal(t, http.StatusOK, w.Code, "an empty body must still work: %s", w.Body.String())
	// Two-phase removal (D4): leaving sets removing=1; the worker closes the seat.
	drainRemovalCascade(t, p)
	m, err := p.db.queryMember(created.ProjectID, "member1")
	require.NoError(t, err)
	assert.Equal(t, MemberStatusRemoved, m.Status)

	// And an explicit empty object works too.
	w = doRaw(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
		tokens["member2"], "{}")
	require.Equal(t, http.StatusOK, w.Code, "body {} must work: %s", w.Body.String())
}

// --- P2: ownership handover was not auditable ---

// TestOwnershipHandoverAuditsTheSuccessor pins that the log can answer "who holds this project
// now". Auditing only the departure or only the demotion loses the more important half.
func TestOwnershipHandoverAuditsTheSuccessor(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "heir1")

	// The handler emits both entries, so drive it through a router built on the same *Project
	// whose sink is captured — same shape as TestEveryWritePathEmitsAnAuditEntry.
	r := mountProject(t, p)
	entries := captureAuditOn(t, p, func() {
		w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
			ownerTok, map[string]any{"transfer_to": "heir1"})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	})
	assert.True(t, auditHasTarget(entries, auditLeave, "owner1"),
		"the departure must still be audited; got %+v", entries)
	assert.True(t, auditHasTarget(entries, auditRoleChange, "heir1"),
		"the successor must get their own audit entry so the log says who holds the project "+
			"now; got %+v", entries)
}

// --- P2: a partially-applied batch was reported as a whole-batch 403 ---

// TestPartiallyAppliedBatchReportsWhatCommitted pins the honest answer. Each target runs in its
// own transaction, so when the actor loses their rights mid-batch the earlier targets are
// durably applied; returning a bare 403 told the caller "nothing happened" while the database
// disagreed, and a client retrying the whole batch would re-apply the successful part.
func TestPartiallyAppliedBatchReportsWhatCommitted(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"t1", "t2", "t3"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	// The seam is a field on the instance, so the requests must hit a router mounted on THAT
	// instance — the shared testSrv routes a different one.
	r := mountProject(t, p)
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"t1", "t2", "t3"}})
	require.Equal(t, http.StatusOK, w.Code)

	// Revoke the actor's rights after the first removal commits, by hooking the per-target call.
	calls := 0
	orig := p.removeOneFn
	p.removeOneFn = func(projectID, spaceID, actorUID, targetUID string) (bool, error) {
		calls++
		if calls > 1 {
			return false, errPermissionDenied
		}
		return orig(projectID, spaceID, actorUID, targetUID)
	}
	t.Cleanup(func() { p.removeOneFn = orig })

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"t1", "t2", "t3"}})
	require.Equal(t, http.StatusOK, w.Code,
		"a partially-applied batch must report per-target results, not one status code: %s",
		w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "every target must be accounted for: %s", w.Body.String())
	assert.True(t, outcomes[0].OK, "t1 committed and must be reported as such")
	assert.Equal(t, reasonPermissionDenied, outcomes[1].Reason)
	assert.Equal(t, outcomeNotAttempted, outcomes[2].Reason,
		"targets the handler never reached must be distinguishable from refused ones")

	// And the committed removal really is committed — which is why discarding it was wrong.
	// Two-phase removal (D4): drive the cascade before reading the end state.
	drainRemovalCascade(t, p)
	m, err := p.db.queryMember(created.ProjectID, "t1")
	require.NoError(t, err)
	assert.Equal(t, MemberStatusRemoved, m.Status)

	// With NOTHING committed, a single status code is still the honest answer.
	calls = 1000
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"t2"}})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")
}

// TestCascadeStepNameUnchanged keeps the registry key stable; renaming it would silently detach
// the step from jobs already queued under the old name.
func TestCascadeStepNameUnchanged(t *testing.T) {
	assert.Equal(t, "project_member", spaceMemberRemovalStepName)
	_ = spacemod.MemberRemoveReasonKicked
}

// ---------- helpers used only by this file ----------

// doRaw sends a RAW body, so a test can send malformed JSON that doJSON (which marshals a Go
// value) cannot express.
func doRaw(t *testing.T, srv *server.Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// captureAuditOn runs fn with p's audit sink redirected, and returns what was emitted.
// Mirrors how observability_test.go injects (p.auditSink = rec.sink).
func captureAuditOn(t *testing.T, p *Project, fn func()) []AuditEntry {
	t.Helper()
	rec := &auditRecorder{}
	prev := p.auditSink
	p.auditSink = rec.sink
	defer func() { p.auditSink = prev }()
	fn()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]AuditEntry, len(rec.entries))
	copy(out, rec.entries)
	return out
}

func auditHasTarget(entries []AuditEntry, action, target string) bool {
	for _, e := range entries {
		if e.Action == action && e.TargetUID == target {
			return true
		}
	}
	return false
}

// insertCleanupJob is enqueueCleanupJob under the name this file uses.
func insertCleanupJob(t *testing.T, spaceID, uid string, status int) {
	t.Helper()
	enqueueCleanupJob(t, spaceID, uid, status)
}

// --- P2 (round 4): the add batch ran to completion, but the handler labelled everything
// after the first actor-level failure as "not_attempted" ---

// TestAddBatchStopsAtActorLevelFailureAndLabelsOnlyTheTail pins the fix.
//
// addMembers used to run EVERY target and only the handler stopped: it labelled results[i+1:]
// as not_attempted while those targets had already run — and some had committed. A committed
// add was reported as never tried, its audit entry was never written, and the same label meant
// the truth on the remove path and a lie on the add path.
//
// Now the batch itself stops at the first actor/project-level failure, so the label divides the
// batch exactly where execution stopped.
func TestAddBatchStopsAtActorLevelFailureAndLabelsOnlyTheTail(t *testing.T) {
	srv, p := setup(t)
	resetCursorsForTest()
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"a1", "a2", "a3"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	r := mountProject(t, p)

	calls := 0
	orig := p.addOneFn
	p.addOneFn = func(projectID, spaceID, actorUID, uid string) (bool, error) {
		calls++
		if uid == "a2" {
			// Simulate the actor's rights expiring while the batch is in flight.
			return false, errPermissionDenied
		}
		return orig(projectID, spaceID, actorUID, uid)
	}
	t.Cleanup(func() { p.addOneFn = orig })

	var w *httptest.ResponseRecorder
	entries := captureAuditOn(t, p, func() {
		w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": []string{"a1", "a2", "a3"}})
		require.Equal(t, http.StatusOK, w.Code,
			"a1 committed, so the response must report per-target results: %s", w.Body.String())
	})

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 3, "every uid must be accounted for exactly once: %s", w.Body.String())
	assert.True(t, outcomes[0].OK, "a1 committed")
	assert.Equal(t, reasonPermissionDenied, outcomes[1].Reason)
	assert.Equal(t, outcomeNotAttempted, outcomes[2].Reason, "a3 was never attempted")

	// The batch really did stop: a3 never even opened a transaction, so its seat does not exist.
	assert.Equal(t, 2, calls, "the batch must stop at the first actor-level failure")
	seat, err := p.db.queryMember(created.ProjectID, "a3")
	require.NoError(t, err)
	assert.Nil(t, seat, "a3 must not have been attempted, let alone committed")

	// The committed add is audited; the never-attempted one is not.
	assert.True(t, auditHasTarget(entries, auditMemberAdd, "a1"))
	assert.False(t, auditHasTarget(entries, auditMemberAdd, "a3"))
}
