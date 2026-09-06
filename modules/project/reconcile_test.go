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
// cascade deliberately SKIPS a banned Space because the seat is still real, so reporting
// those seats as violations would flag correct behaviour as a defect — forever, since
// nothing will ever "fix" them.
func TestReconcileDoesNotFlagBannedSpaceMembers(t *testing.T) {
	srv, p := setup(t)
	_, _, _ = projectWithMembers(t, srv, "m1")
	require.Equal(t, 0, violationCount(t, p))

	// A removed Space seat inside a BANNED Space: the cascade skips it, so the scan must
	// too.
	removeSpaceMember(t, spaceA, "m1")
	require.Equal(t, 1, violationCount(t, p), "precondition: an active Space would flag it")

	setSpaceStatus(t, spaceA, 2)
	assert.Equal(t, 0, violationCount(t, p),
		"members of a banned Space must not be reported as I1 violations")
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

	leakCount := func() int64 {
		var n int64
		require.NoError(t, p.db.session.SelectBySql(
			"SELECT COUNT(*) FROM `space_member_removal_cleanup` c "+
				"WHERE c.status = ? "+
				"  AND EXISTS (SELECT 1 FROM `octo_project_member` pm "+
				"               WHERE pm.space_id = c.space_id AND pm.uid = c.uid AND pm.status = ?)",
			cleanupStatusAbandoned, MemberStatusActive,
		).LoadOne(&n))
		return n
	}

	enqueueCleanupJob(t, spaceA, "leaked", cleanupStatusPending)
	assert.Equal(t, int64(0), leakCount(), "a pending job is not a leak")
	assert.Equal(t, 0, violationCount(t, p), "and it is exempt from the I1 scan")

	enqueueCleanupJob(t, spaceA, "leaked", cleanupStatusAbandoned)
	assert.Equal(t, int64(1), leakCount(), "an abandoned job with a surviving seat is a leak")
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
	// Every paged walk must also be page-capped, so one tick cannot run unbounded.
	if strings.Count(src, "reconcileMaxPages") < 4 {
		t.Errorf("expected every paged scan to bound its page count with reconcileMaxPages, "+
			"found %d references", strings.Count(src, "reconcileMaxPages"))
	}
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
		"SELECT COUNT(*) FROM `octo_project` WHERE status = ?",
		"SELECT COUNT(*) FROM `octo_project_member` WHERE status = ?",
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
	if strings.Contains(src, "COUNT(*)") {
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
