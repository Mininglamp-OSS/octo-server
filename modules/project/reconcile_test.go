package project

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enqueueCleanupJob writes a row into the Space module's cleanup outbox with the given
// status. The reconcile scan reads that table to decide which I1 "violations" are actually
// in-flight work, so the exemption cannot be tested without it.
//
// Written directly rather than through modules/space's (unexported) enqueue helper: what
// is under test is this module's reading of the table, and the columns it reads are
// space_id / uid / status.
func enqueueCleanupJob(t *testing.T, spaceID, uid string, status int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO space_member_removal_cleanup "+
			"(space_id, uid, operator_uid, reason, status, next_attempt_at) VALUES (?, ?, ?, ?, ?, ?)",
		spaceID, uid, "operator", "kicked", status, time.Now().UTC(),
	).Exec()
	require.NoError(t, err)
}

// injectOrphanSeat creates an active project seat by raw SQL for a uid with no active
// Space seat — the exact state a lost cascade leaves behind, and the one the scan must
// find.
func injectOrphanSeat(t *testing.T, projectID, spaceID, uid string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		projectID, uid, spaceID, RoleCommon, MemberStatusActive, "owner1", now, now,
	).Exec()
	require.NoError(t, err)
}

func violationCount(t *testing.T, p *Project) int {
	t.Helper()
	rows, err := p.queryI1ViolationPage("", "", p.cfg.ReconcileLimit)
	require.NoError(t, err)
	return len(violatingI1Rows(rows))
}

// TestReconcileFlagsInjectedI1Violation covers the base case: a seat with no active Space
// seat and no cleanup job scheduled is a real violation.
func TestReconcileFlagsInjectedI1Violation(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)

	seedUser(t, "orphan")
	injectOrphanSeat(t, created.ProjectID, spaceA, "orphan")

	rows, err := p.queryI1ViolationPage("", "", p.cfg.ReconcileLimit)
	require.NoError(t, err)
	rows = violatingI1Rows(rows)
	require.Len(t, rows, 1)
	assert.Equal(t, "orphan", rows[0].UID)
	assert.Equal(t, created.ProjectID, rows[0].ProjectID)
	assert.Equal(t, spaceA, rows[0].SpaceID)
}

// TestReconcileExemptsPairsWithPendingCleanupJob is the exemption that keeps the alert
// meaningful.
//
// Cleanup work is enqueued in the Space-removal transaction but executed by a poller, so
// EVERY normal removal produces exactly this state for at least one poll interval. Without
// the exemption the alert fires on every kick and becomes noise before the feature has a
// single user.
func TestReconcileExemptsPairsWithPendingCleanupJob(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	seedUser(t, "inflight")
	injectOrphanSeat(t, created.ProjectID, spaceA, "inflight")
	require.Equal(t, 1, violationCount(t, p), "precondition: it is a violation without a job")

	enqueueCleanupJob(t, spaceA, "inflight", cleanupStatusPending)
	assert.Equal(t, 0, violationCount(t, p),
		"a pair with a pending cleanup job is in-flight work, not a violation")
}

// TestReconcileStillFlagsPairsWhoseJobFinished pins the other side of the exemption: only
// PENDING jobs exempt. A done job means the cascade already ran, so a surviving active seat
// is a real leak — and an abandoned job is reported by its own scan with a different
// meaning.
func TestReconcileStillFlagsPairsWhoseJobFinished(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	seedUser(t, "stale")
	injectOrphanSeat(t, created.ProjectID, spaceA, "stale")

	enqueueCleanupJob(t, spaceA, "stale", 1) // done
	assert.Equal(t, 1, violationCount(t, p),
		"a completed cleanup job must not exempt a surviving active seat")

	enqueueCleanupJob(t, spaceA, "stale", cleanupStatusAbandoned)
	assert.Equal(t, 1, violationCount(t, p),
		"an abandoned job is a real leak; it must not silence the I1 scan either")
}

// TestReconcileDoesNotFlagBannedSpaceMembers pins that the scan and the cascade agree. The
// cascade deliberately SKIPS a banned Space MEMBER because the seat is still real, so reporting
// those seats as violations would flag correct behaviour as a defect — forever, since nothing
// will ever "fix" them.
//
// FIXTURE CORRECTED (PR #841 round 5). This test used to call removeSpaceMember first — setting
// space_member.status = 0 — and then ban, which is NOT a "banned Space member" at all: under
// cleanup semantics that uid is a non-member, so the cascade does NOT skip them
// (deactivateSeatForCascade proceeds when stillMember is false) and the seat IS a leak. The old
// fixture therefore asserted "the scan and the cascade agree" on precisely the one case where
// they disagreed, and certified the over-broad predicate that produced it. The seat must stay
// ACTIVE for this test to be about what its name says.
//
// The complementary direction — banned Space, seat already closed, must BE flagged — is in
// TestI1ScanUsesCleanupSemanticsForBannedSpaces, together with a cross-scan agreement case.
func TestReconcileDoesNotFlagBannedSpaceMembers(t *testing.T) {
	srv, p := setup(t)
	_, _, _ = projectWithMembers(t, srv, "m1")
	require.Equal(t, 0, violationCount(t, p))

	// m1 KEEPS their Space seat (space_member.status stays 1). That is what makes them a member
	// of the banned Space rather than a leak inside it.
	setSpaceStatus(t, spaceA, 2)
	assert.Equal(t, 0, violationCount(t, p),
		"members of a banned Space must not be reported as I1 violations")

	// And the ban is not doing the exempting on its own: close the seat and the same scan, same
	// banned Space, must flag it. Without this line the assertion above is satisfied by any
	// predicate that blanket-exempts banned Spaces.
	removeSpaceMember(t, spaceA, "m1")
	assert.Equal(t, 1, violationCount(t, p),
		"a banned Space must not hide a seat whose Space seat is closed — the exemption is about "+
			"membership, not about the ban")
}

// TestReconcileFlagsDisbandedSpaceMembers pins that the relaxed predicate's OTHER side is
// still a violation: a DISBANDED Space (status=0) holds no seats, so every surviving active
// project seat in it is real.
//
// The expected count is 2, not 1 — the owner is flagged as well as the removed member, even
// though the owner's space_member row is still status=1, because CheckMembership requires
// space.status=1 and the Space is gone. That is the intended reading: once a Space is
// disbanded nobody in it holds a seat.
//
// In production those seats do not sit there flagged: the Space disband path enqueues a
// cleanup job for every member in the same transaction, so all of them are exempt while
// their jobs are pending and closed once the jobs run. This case sets status=0 directly,
// with no jobs, which is the post-cascade leak shape rather than the normal one.
func TestReconcileFlagsDisbandedSpaceMembers(t *testing.T) {
	srv, p := setup(t)
	_, _, _ = projectWithMembers(t, srv, "m1")
	removeSpaceMember(t, spaceA, "m1")
	setSpaceStatus(t, spaceA, 0)
	assert.Equal(t, 2, violationCount(t, p),
		"a disbanded Space holds no seats, so both the owner and the removed member are violations")

	// And with cleanup jobs pending — what the disband path actually writes — none of them
	// is reported.
	enqueueCleanupJob(t, spaceA, "owner1", cleanupStatusPending)
	enqueueCleanupJob(t, spaceA, "m1", cleanupStatusPending)
	assert.Equal(t, 0, violationCount(t, p))
}

// TestReconcileCursorPagesWithoutRepeating pins the boundedness requirement: the scan is a
// LIMIT plus a primary-key cursor, never an unbounded join against space_member.
func TestReconcileCursorPagesWithoutRepeating(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"o1", "o2", "o3"} {
		seedUser(t, uid)
		injectOrphanSeat(t, created.ProjectID, spaceA, uid)
	}

	seen := map[string]bool{}
	cursorProject, cursorUID := "", ""
	for pages := 0; pages < 10; pages++ {
		rows, err := p.queryI1ViolationPage(cursorProject, cursorUID, 2)
		require.NoError(t, err)
		for _, row := range violatingI1Rows(rows) {
			key := row.ProjectID + "/" + row.UID
			assert.False(t, seen[key], "cursor must not flag %s twice", key)
			seen[key] = true
		}
		if len(rows) < 2 {
			break
		}
		last := rows[len(rows)-1]
		cursorProject, cursorUID = last.ProjectID, last.UID
	}
	assert.Len(t, seen, 3)
}

// TestReconcileFlagsOrphanProjects covers a project whose Space row is gone.
func TestReconcileFlagsOrphanProjects(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)

	_, err := testCtx.DB().DeleteFrom("space").Where("space_id = ?", spaceA).Exec()
	require.NoError(t, err)

	rows, err := p.queryInspectedProjectPage(0, p.cfg.ReconcileLimit)
	require.NoError(t, err)
	orphans := make([]*orphanRow, 0, 1)
	for _, r := range rows {
		if r.Violating {
			orphans = append(orphans, r)
		}
	}
	require.Len(t, orphans, 1)
	assert.Equal(t, created.ProjectID, orphans[0].ProjectID)
}

// TestRunReconcileIsReadOnly pins D7: the job may run on every pod because it mutates
// nothing. A mutating action added later must first take a DB CAS claim.
func TestRunReconcileIsReadOnly(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "m1")
	seedUser(t, "orphan")
	injectOrphanSeat(t, created.ProjectID, spaceA, "orphan")

	before := epochOf(t, created.ProjectID)
	beforeMember, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)

	p.runReconcile()
	p.refreshDistributionMetrics()

	assert.Equal(t, before, epochOf(t, created.ProjectID), "reconcile must not write")
	afterMember, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	assert.Equal(t, beforeMember.Status, afterMember.Status)
	assert.Equal(t, beforeMember.Role, afterMember.Role)

	orphan, err := p.db.queryMember(created.ProjectID, "orphan")
	require.NoError(t, err)
	require.NotNil(t, orphan)
	assert.Equal(t, MemberStatusActive, orphan.Status,
		"the scan reports the violation; repairing it is deliberately not its job")
}

// TestAbandonedCleanupLeakIsItsOwnSignal pins the two-alert split. A pending job is a
// normal bounded window and is exempted from the I1 scan; an abandoned one has exhausted
// its budget, nothing re-drives it, and it needs a human. One number for both would train
// the on-call to ignore the one that matters.
func TestAbandonedCleanupLeakIsItsOwnSignal(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	seedUser(t, "leaked")
	injectOrphanSeat(t, created.ProjectID, spaceA, "leaked")

	// countAbandonedLeak drives the PRODUCTION scan (scanAbandonedCleanupLeak) and reads its
	// gauge. This test used to run a hand-written COUNT(*) over space_member_removal_cleanup
	// instead — which never invoked the scan at all, so both assertions passed with the entire
	// abandoned-leak scan deleted, AND the query was the job-counting shape reconcile.go
	// documents as wrong three ways (under-reports a member in several projects, double-counts
	// re-removals, false-positives on rejoins). The test had made the deleted bug its oracle
	// (PR #841 round 4, P2-3d).
	// 1) A pending job is work in flight, not a leak — and the I1 scan exempts it too.
	enqueueCleanupJob(t, spaceA, "leaked", cleanupStatusPending)
	assert.Equal(t, 0, countAbandonedLeak(t, p), "a pending job is not a leak")
	assert.Equal(t, 0, violationCount(t, p), "and it is exempt from the I1 scan")

	// 2) An abandoned job while a pending one still exists is STILL not a leak: the pending job
	// will close the seat. This is the exemption reconcile.go documents, and it is the reason
	// the production scan and this test's previous hand-written COUNT(*) disagreed — that query
	// had no such condition, so it reported a leak here.
	enqueueCleanupJob(t, spaceA, "leaked", cleanupStatusAbandoned)
	assert.Equal(t, 0, countAbandonedLeak(t, p),
		"an abandoned job is not a leak while a newer pending job will still clean the seat")

	// 3) With no pending job left, the abandoned one IS a leak — counted per SEAT.
	_, err := testCtx.DB().UpdateBySql(
		"DELETE FROM space_member_removal_cleanup WHERE space_id = ? AND uid = ? AND status = ?",
		spaceA, "leaked", cleanupStatusPending).Exec()
	require.NoError(t, err)
	assert.Equal(t, 1, countAbandonedLeak(t, p),
		"one abandoned job with one surviving seat and nothing scheduled to clean it is one "+
			"leaked SEAT")
}

// TestReconcileQueriesAreBounded pins C3 at the source level: every scan carries a LIMIT.
// An unbounded scan competes with the message paths for the same connections, and it does
// so worst exactly when the tables are largest.
func TestReconcileQueriesAreBounded(t *testing.T) {
	src := readStripped(t, "reconcile.go")
	selects := 0
	for _, stmt := range splitOnSelectBySql(src) {
		selects++
		// EVERY scan must carry LIMIT ?. There is deliberately no aggregate exemption:
		// the earlier version of this guard waved through anything containing COUNT(*),
		// which is precisely how an unbounded `SELECT COUNT(*) ... WHERE EXISTS (...)` over
		// the whole cleanup table passed review. An exemption that covers the one shape you
		// most need to catch is worse than no guard, because it reads as coverage.
		if !containsAny(stmt, "LIMIT ?") {
			t.Errorf("reconcile.go has a SELECT without LIMIT ? (brief C3 requires every "+
				"reconcile query be bounded by LIMIT + cursor): %s", stmt)
		}
	}
	if selects == 0 {
		t.Fatal("no SelectBySql found in reconcile.go; the guard would pass vacuously")
	}
	// Every paged walk must also be page-capped, so one tick cannot run unbounded — checked
	// PER SCAN.
	//
	// The previous form was `strings.Count(src, "reconcileMaxPages") < 4`, which could not fail:
	// the identifier appears in its own declaration and twice inside each of
	// reconcileMaxPagesForTest / setReconcileMaxPagesForTest (strings.Count matches the
	// substring inside those function NAMES), for 7 references before any loop bound is counted.
	// Deleting the cap from all five scans still left the count at 7 (PR #841 round 4, P2-3c).
	scans := scanFuncNames(readLinesWithoutComments(t, "reconcile.go"))
	if len(scans) < 5 {
		t.Fatalf("expected at least five reconcile scans, found %d: %v — the enumeration "+
			"stopped matching, which would make this check vacuous", len(scans), scans)
	}
	lineSrc := readLinesWithoutComments(t, "reconcile.go")
	for _, scan := range scans {
		body := scanFuncBody(lineSrc, scan)
		if !strings.Contains(body, "page < reconcileMaxPages") {
			t.Errorf("%s does not bound its page loop with reconcileMaxPages, so one tick can "+
				"walk the whole table", scan)
		}
	}
}

// scanFuncNames enumerates the reconcile scan entry points from the source, so a newly added
// scan is covered without editing this test.
func scanFuncNames(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "func (p *Project) scan") && strings.HasSuffix(line, "() {") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "func (p *Project) "), "() {")
			out = append(out, name)
		}
	}
	return out
}

// scanFuncBody returns one scan's source, up to the next top-level func.
func scanFuncBody(src, name string) string {
	i := strings.Index(src, "func (p *Project) "+name+"() {")
	if i < 0 {
		return ""
	}
	rest := src[i+1:]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}

func splitOnSelectBySql(src string) []string {
	var out []string
	parts := splitAfter(src, "SelectBySql(")
	for i, part := range parts {
		if i == 0 {
			continue
		}
		end := indexOf(part, ").Load")
		if end < 0 {
			end = indexOf(part, ").LoadOne")
		}
		if end < 0 {
			end = len(part)
		}
		out = append(out, part[:end])
	}
	return out
}

// Small string helpers kept local so the guard reads as one idea rather than a chain of
// strings.* calls.
func containsAny(s, sub string) bool { return indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitAfter(s, sep string) []string {
	var out []string
	for {
		i := indexOf(s, sep)
		if i < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:i+len(sep)])
		s = s[i+len(sep):]
	}
}

// TestMetricsCollectionAggregatesAreRegistered is the counterpart to
// TestReconcileQueriesAreBounded for metrics_collect.go.
//
// The gauges there report totals, so they are whole-table aggregates and cannot be paged.
// Rather than exempting a PATTERN (which is how the unbounded abandoned-leak COUNT slipped
// through — the old guard waved past anything containing COUNT(*)), each unbounded statement is
// whitelisted BY ITS TEXT. A new query in this file either carries LIMIT ? or has to be added to
// this list, which is a reviewable edit rather than an invisible pass.
func TestMetricsCollectionAggregatesAreRegistered(t *testing.T) {
	allowedUnbounded := []string{
		// readStripped flattens backticks, so the allowlist matches the joined text.
		"SELECT COUNT(*) FROM octo_project WHERE status = ?",
		"SELECT COUNT(*) FROM octo_project_member WHERE status = ?",
	}
	src := readStripped(t, "metrics_collect.go")
	stmts := splitOnSelectBySql(src)
	if len(stmts) == 0 {
		t.Fatal("no SelectBySql found in metrics_collect.go; the guard would pass vacuously")
	}
	for _, stmt := range stmts {
		if containsAny(stmt, "LIMIT ?") {
			continue
		}
		registered := false
		for _, a := range allowedUnbounded {
			if strings.Contains(stmt, a) {
				registered = true
				break
			}
		}
		if !registered {
			t.Errorf("metrics_collect.go has an unbounded SELECT that is not in the "+
				"allowedUnbounded list — add LIMIT ? or register it explicitly: %s", stmt)
		}
	}
}

// TestReconcileFileHasNoUnboundedAggregate pins that the aggregates really did leave
// reconcile.go, so the zero-exemption guard above cannot be softened by moving one back.
func TestReconcileFileHasNoUnboundedAggregate(t *testing.T) {
	src := readStripped(t, "reconcile.go")
	if strings.Contains(src, "COUNT(*)") || strings.Contains(src, "COUNT(1)") {
		t.Error("reconcile.go must contain no COUNT(*): totals belong in metrics_collect.go, " +
			"and allowing one here is what forced the aggregate exemption that hid an " +
			"unbounded scan")
	}
}

// violatingI1Rows filters an inspected page down to the rows the scan would flag — the
// per-row predicate moved from WHERE into a SELECT flag (Q4), so tests assert on the flag.
func violatingI1Rows(rows []*i1Row) []*i1Row {
	out := make([]*i1Row, 0, len(rows))
	for _, r := range rows {
		if r.Violating {
			out = append(out, r)
		}
	}
	return out
}
