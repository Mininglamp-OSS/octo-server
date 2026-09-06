package project

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// reconcileRunning / metricsRunning are process-level reentrancy guards.
//
// Required, not defensive. config.Context.Schedule is backed by a timing wheel that fires
// `go task()` on every tick WITHOUT waiting for the previous run (timingwheel.go:117), so a
// scan that takes longer than its interval overlaps itself. modules/space guards its cleanup
// worker the same way and for the same reason (removalCleanupRunning). Here the overlap
// would also race the epoch-history map below.
// reconcileMaxPages bounds how many pages any one scan walks in a single tick.
//
// Without it a scan whose violation set keeps growing could run for an unbounded time on every
// tick, which is the opposite of what a bounded scan is for. Hitting the cap means the numbers
// reported this tick are a floor rather than a total — acceptable for an alerting signal,
// PROVIDED the next tick picks up where this one stopped. See reconcileCursors for why that
// proviso needed real work rather than a comment.
// A var rather than a const so a test can shrink the window and exercise the cross-tick
// resume path without seeding tens of thousands of rows. Not thread-safe; tests that change
// it must not run in parallel and must restore it.
var reconcileMaxPages = 50

var (
	reconcileRunning atomic.Bool
	metricsRunning   atomic.Bool
)

// reconcileCursors carries each scan's position ACROSS ticks.
//
// This is load-bearing, not tidiness. Every cursor used to be a local variable initialised at
// the top of its scan, so each tick restarted from the beginning and the page cap meant the
// scans could only ever see the first reconcileMaxPages * ReconcileLimit rows — 25,000 at the
// defaults. Past that, later rows were NEVER examined: a project with a higher id could sit in
// permanent violation and no tick would look at it. The comment above claimed "the next tick
// continues", which is exactly the sort of claim the code has to actually implement.
//
// Semantics: a scan resumes from its saved cursor, and resets to the start once it reaches the
// end of the table (a short page). So the scans rotate through the whole keyspace across ticks
// instead of re-reading one window forever, which is the same "cursor rotation under a shared
// budget" shape the notify welcome ledger uses.
//
// A cursor may skip a row that was deleted or changed state mid-rotation. That is acceptable
// for an alerting scan and self-correcting: the next full rotation sees it again.
// A rotation also has to carry its RUNNING TOTAL, not just its position. The gauges report a
// whole-population number; if a tick only covers part of the keyspace, Set-ing that partial
// count would publish a value smaller than reality and an alert threshold would never fire.
// So each scan accumulates across ticks and publishes only when a rotation completes.
type reconcileCursors struct {
	mu sync.Mutex
	// i1Project / i1UID form the composite cursor over octo_project_member's primary key.
	i1Project        string
	i1UID            string
	i1Running        int
	orphan           int64
	orphanRun        int
	epoch            int64
	abandonedProject string
	abandonedUID     string
	abandonRun       int
}

var cursors reconcileCursors

// i1Resume returns the saved position and running total for the I1 rotation.
func (c *reconcileCursors) i1Resume() (string, string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.i1Project, c.i1UID, c.i1Running
}

// i1Save stores progress. done=true means the rotation reached the end of the table: the caller
// gets the complete total to publish, and the rotation restarts next tick.
func (c *reconcileCursors) i1Save(project, uid string, running int, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done {
		c.i1Project, c.i1UID, c.i1Running = "", "", 0
		return
	}
	c.i1Project, c.i1UID, c.i1Running = project, uid, running
}

// abandonedResume / abandonedSave mirror i1Resume/i1Save for the abandoned-leak rotation,
// which walks the same (project_id, uid) key space.
func (c *reconcileCursors) abandonedResume() (string, string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.abandonedProject, c.abandonedUID, c.abandonRun
}

func (c *reconcileCursors) abandonedSave(project, uid string, running int, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done {
		c.abandonedProject, c.abandonedUID, c.abandonRun = "", "", 0
		return
	}
	c.abandonedProject, c.abandonedUID, c.abandonRun = project, uid, running
}

// idResume / idSave are the same contract for the single-int64-cursor rotations.
func (c *reconcileCursors) idResume(cursor *int64, running *int) (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *cursor, *running
}

func (c *reconcileCursors) idSave(cursor *int64, running *int, pos int64, total int, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done {
		*cursor, *running = 0, 0
		return
	}
	*cursor, *running = pos, total
}

// reconcileMaxPagesForTest / setReconcileMaxPagesForTest expose the page cap to tests.
func reconcileMaxPagesForTest() int     { return reconcileMaxPages }
func setReconcileMaxPagesForTest(n int) { reconcileMaxPages = n }

// orphanCursorForTest reports the saved position of the orphan rotation.
func orphanCursorForTest() int64 {
	cursors.mu.Lock()
	defer cursors.mu.Unlock()
	return cursors.orphan
}

// resetCursorsForTest puts every rotation back to its start so a test can assert on a full
// scan without inheriting a position from an earlier case.
func resetCursorsForTest() {
	cursors.mu.Lock()
	defer cursors.mu.Unlock()
	cursors.i1Project, cursors.i1UID = "", ""
	cursors.orphan, cursors.epoch = 0, 0
	cursors.orphanRun, cursors.abandonRun = 0, 0
	cursors.abandonedProject, cursors.abandonedUID = "", ""
}

// reconcileWorkerOnce guarantees the process schedules the reconcile timers exactly
// once.
//
// Not optional. Route() runs once in production but once PER testutil.NewTestServer
// in tests, and config.Context.Schedule has no cancellation entry point — modules/space
// learned this the hard way (see removalCleanupWorkerOnce: modules/user builds 196
// test servers in one package, which stacked up nearly 400 timers that never stop,
// all pointed at the same MySQL/Redis, waking together and spending the package's
// time budget on work unrelated to the test under way).
var reconcileWorkerOnce sync.Once

// startReconcileWorker schedules the reconcile and metrics ticks.
//
// The interval is jittered because the job may run on every pod (D7): without
// jitter every replica scans at the same instant, turning a cheap periodic read into
// a synchronized burst against the same tables the message paths use.
func (p *Project) startReconcileWorker() {
	reconcileWorkerOnce.Do(func() {
		p.ctx.Schedule(jitter(p.cfg.ReconcileInterval), p.runReconcile)
		p.ctx.Schedule(jitter(p.cfg.MetricsInterval), p.refreshDistributionMetrics)
	})
}

// jitter spreads a fixed interval by up to +25% so replicas desynchronize.
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Minute
	}
	//nolint:gosec // scheduling jitter, not a security decision
	return base + time.Duration(rand.Int63n(int64(base/4)+1))
}

// runReconcile executes every scan.
//
// Read-only, so it is safe on every pod: duplicate detection only duplicates
// alerts. Any MUTATING reconcile action added later must first take a database CAS
// claim, in the shape of the welcome ledger's claim_owner/claim_expire_at lease —
// otherwise two pods repair the same row concurrently.
func (p *Project) runReconcile() {
	if !reconcileRunning.CompareAndSwap(false, true) {
		return // a scan is already in flight; skip this tick rather than pile on
	}
	defer reconcileRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目对账 panic", zap.Any("recover", r))
		}
	}()
	p.scanI1Violations()
	p.scanAbandonedCleanupLeak()
	p.scanOrphanProjects()
	p.scanEpochSanity()
}

// ---------- scan 1: I1 violations ----------

// i1Row identifies one violating seat.
type i1Row struct {
	ProjectID string `db:"project_id"`
	UID       string `db:"uid"`
	SpaceID   string `db:"space_id"`
	// violating is the per-row predicate flag. The base-row page is LIMIT-bounded and every
	// predicate is evaluated as a SELECT-list flag over it, so the query's cost is bounded by
	// rows EXAMINED; the scan filters on this flag in Go. A WHERE-clause predicate would bound
	// rows returned instead, and a healthy table would be walked end to end every tick.
	Violating bool `db:"violating"`
}

// scanI1Violations counts project seats with no active Space seat behind them,
// EXCLUDING two categories that are not violations:
//
//   - pairs with an outstanding (pending) cleanup job. The Space-removal cascade is
//     enqueued transactionally and executed by a poller, so every kick produces rows
//     in exactly this state for at least one poll interval. Without the exemption the
//     alert fires on every normal removal and becomes noise before the feature has a
//     single user.
//   - members of a BANNED Space (status=2). Their Space seat is real — cleanup skips
//     them by design (CheckMembershipForCleanup) — so flagging them would report the
//     correct behaviour as a defect.
//
// A cleanup job in the ABANDONED state is a SEPARATE alert with the opposite
// meaning; see scanAbandonedCleanupLeak.
func (p *Project) scanI1Violations() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("i1_violations").Observe(time.Since(start).Seconds()) }()

	cursorProject, cursorUID, total := cursors.i1Resume()
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.i1PageFn(cursorProject, cursorUID, p.cfg.ReconcileLimit)
		if err != nil {
			// BREAK, not return: the *Save call at the end of this function is the only place
			// progress is persisted, so returning here threw away every page already walked in
			// this tick. With a persistent failure at page N+1 that meant re-scanning 1..N
			// forever — re-emitting the same Error lines for the prefix on every tick and never
			// reaching a row past the failure. Breaking keeps the cursor and the running total,
			// with completed=false so nothing is published from a partial rotation.
			p.Warn("对账 I1 违约扫描失败", zap.Error(err))
			break
		}
		if len(rows) == 0 {
			completed = true
			break
		}
		for _, row := range rows {
			if !row.Violating {
				continue
			}
			p.Error("I1 违约：项目成员已无对应 Space 席位，且无在途清理工单",
				zap.String("projectId", row.ProjectID), zap.String("uid", row.UID),
				zap.String("spaceId", row.SpaceID))
			total++
		}
		// The cursor advances over INSPECTED rows (every base row), not over flagged ones —
		// that is what makes a short page mean "table end" even when nothing violated.
		last := rows[len(rows)-1]
		cursorProject, cursorUID = last.ProjectID, last.UID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}

	// Publish only a complete rotation. A truncated tick has counted part of the keyspace, and
	// Set-ing that would report fewer violations than exist.
	if completed {
		i1Violations.Set(float64(total))
	}
	cursors.i1Save(cursorProject, cursorUID, total, completed)
}

// queryI1ViolationPage returns one bounded page of INSPECTED member rows, each flagged with
// whether it violates I1. The predicates are SELECT-list flags over the LIMIT-bounded base
// row set, so the query's cost is bounded by rows examined (yujiawei Q4, PR #841 round 1):
// in the healthy case — no violations at all — the old WHERE-clause shape walked the primary
// key to the end of the table on every tick, because LIMIT only bounded what came back.
func (p *Project) queryI1ViolationPage(cursorProject, cursorUID string, limit int) ([]*i1Row, error) {
	var rows []*i1Row
	_, err := p.db.session.SelectBySql(
		"SELECT pm.project_id, pm.uid, pm.space_id, "+
			// The I1 flag: the seat is ACTIVE, has no in-flight cleanup job, is not in a banned
			// Space, and has no active Space seat behind it — the same predicates the WHERE
			// version had, evaluated per inspected row instead of filtering the result set.
			//
			// pm.status belongs in here with the others, and it was the one left behind. Member
			// rows are never deleted (removal flips status), so as closed seats accumulate a
			// WHERE on status again bounds rows RETURNED rather than rows EXAMINED — the exact
			// property Q4 was about — and no index leads with status (the PK is
			// (project_id, uid), and the three secondary indexes lead with space_id/uid/
			// project_id).
			"(pm.status = ? "+
			" AND NOT EXISTS (SELECT 1 FROM `space_member_removal_cleanup` c "+
			"             WHERE c.space_id = pm.space_id AND c.uid = pm.uid AND c.status = ?) "+
			" AND NOT EXISTS (SELECT 1 FROM `space` sb "+
			"                  WHERE sb.space_id = pm.space_id AND sb.status = ?) "+
			" AND NOT EXISTS (SELECT 1 FROM `space_member` sm "+
			"                  INNER JOIN `space` s ON s.space_id = sm.space_id AND s.status = ? "+
			"                  WHERE sm.space_id = pm.space_id AND sm.uid = pm.uid AND sm.status = ?) "+
			") AS violating "+
			"FROM `octo_project_member` pm "+
			"WHERE (pm.project_id, pm.uid) > (?, ?) "+
			"ORDER BY pm.project_id, pm.uid LIMIT ?",
		MemberStatusActive, cleanupStatusPending, spaceStatusBanned, spaceStatusNormal,
		spaceMemberStatusActive, cursorProject, cursorUID, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: scan I1 violations: %w", err)
	}
	return rows, nil
}

// Status literals for tables owned by other modules.
//
// Spelled out rather than imported: modules/space owns them, and while this package
// already imports modules/space for the cleanup registry, the cleanup job's status
// constants are unexported there. pkg/space's CheckMembershipForCleanup spells its
// own literal out for the same reason. If modules/space ever changes these, this
// scan is wrong in a way MySQL will not complain about — which is why each one is
// named here rather than left as a bare digit in the SQL.
const (
	// cleanupStatusPending mirrors modules/space's removalCleanupPending (0).
	cleanupStatusPending = 0
	// cleanupStatusAbandoned mirrors modules/space's removalCleanupAbandoned (2).
	cleanupStatusAbandoned = 2
	// spaceStatusNormal / spaceStatusBanned mirror modules/space's SpaceStatus* .
	spaceStatusNormal = 1
	spaceStatusBanned = 2
	// spaceMemberStatusActive mirrors space_member.status = 1.
	spaceMemberStatusActive = 1
)

// ---------- scan 2: abandoned cleanup leak ----------

// scanAbandonedCleanupLeak counts seats still active behind an ABANDONED cleanup
// job.
//
// This is a different alert from scanI1Violations and the difference is the whole
// point of having two. A pending job is a normal, bounded window and is exempted
// there. An abandoned job has exhausted its retry budget (20 attempts, ~70 minutes),
// nothing re-drives it, and the member keeps their project seat until a human
// intervenes. Folding the two into one number trains the on-call to ignore both, and
// the one that matters is this one.
func (p *Project) scanAbandonedCleanupLeak() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("abandoned").Observe(time.Since(start).Seconds()) }()

	// What this counts, and why the previous version counted the wrong thing.
	//
	// The gauge is named for LEAKED SEATS, and that is what it must report. Counting abandoned
	// JOBS instead was wrong three separate ways: one job whose member sat in five projects
	// counted 1 (under-report); a (space, uid) pair removed and re-removed produced two
	// abandoned jobs and counted 2 for the same leak (double-count); and a user who had since
	// rejoined the Space still counted, alerting on nothing (false positive).
	//
	// So the scan walks octo_project_member rows — the actual leaked seats — deduplicated by
	// construction because (project_id, uid) is the primary key. A row counts only when an
	// abandoned job exists for its (space_id, uid) AND the user does NOT currently hold a Space
	// seat. The seat check uses CLEANUP semantics (space.status <> 0), matching the cascade, so a
	// banned Space is not reported as a leak.
	//
	// Paged with LIMIT + a primary-key cursor, per brief C3. The previous version was a single
	// COUNT(*) over every abandoned job in the retention window with a correlated EXISTS per
	// row, which grows without bound and competes with the message paths. The guard test that
	// was supposed to catch that exempted anything containing COUNT(*) — the exemption is now
	// gone, so an unbounded aggregate here fails the guard.
	// Same cross-tick rotation as the other scans: the page cap must not mean "only ever look
	// at the first window".
	cursorProject, cursorUID, running := cursors.abandonedResume()
	leaked := int64(running)
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryAbandonedLeakPage(cursorProject, cursorUID, p.cfg.ReconcileLimit)
		if err != nil {
			// break, not return — see scanI1Violations for why the cursor save must be reached.
			p.Warn("对账 abandoned 工单泄漏扫描失败", zap.Error(err))
			break
		}
		if len(rows) == 0 {
			completed = true
			break
		}
		for _, r := range rows {
			if !r.Violating {
				continue
			}
			leaked++
			p.Error("Space 成员移除工单已 abandoned，项目席位仍活跃：无自动重驱动，需人工介入",
				zap.String("projectId", r.ProjectID), zap.String("spaceId", r.SpaceID),
				zap.String("uid", r.UID))
		}
		last := rows[len(rows)-1]
		cursorProject, cursorUID = last.ProjectID, last.UID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		i1AbandonedLeak.Set(float64(leaked))
	}
	cursors.abandonedSave(cursorProject, cursorUID, int(leaked), completed)
}

// queryAbandonedLeakPage returns one bounded page of INSPECTED member rows, each flagged
// with whether it is a real abandoned-cleanup leak: an exhausted job exists for the pair, no
// NEWER pending job will clean the seat, and the member holds no Space seat. Same
// flag-over-base-page shape as the I1 query — cost bounded by rows examined (Q4).
func (p *Project) queryAbandonedLeakPage(cursorProject, cursorUID string, limit int) ([]*i1Row, error) {
	var rows []*i1Row
	_, err := p.db.session.SelectBySql(
		"SELECT pm.project_id, pm.uid, pm.space_id, "+
			// The seat is still ACTIVE. In the flag rather than the WHERE clause for the same
			// reason as the I1 scan: member rows are never deleted, so filtering on status
			// there would bound rows returned instead of rows examined.
			"(pm.status = ? "+
			// An abandoned job exists for this pair: nothing will re-drive THAT job.
			" AND EXISTS (SELECT 1 FROM `space_member_removal_cleanup` c "+
			"          WHERE c.space_id = pm.space_id AND c.uid = pm.uid AND c.status = ?) "+
			// ...but a NEWER job may already be queued for the same pair, and that one will
			// clean the seat. remove -> rejoin -> remove enqueues a second job (the outbox is
			// deliberately not unique on (space_id, uid)), so a pair can hold an old abandoned
			// job AND a live pending one at once. Reporting that as "no automatic re-drive,
			// needs manual intervention" is a false alarm about work that is already scheduled —
			// and the in-flight exemption is exactly what the I1 scan already does.
			" AND NOT EXISTS (SELECT 1 FROM `space_member_removal_cleanup` q "+
			"                  WHERE q.space_id = pm.space_id AND q.uid = pm.uid AND q.status = ?) "+
			// And the member really has no Space seat right now — cleanup semantics, so a
			// banned Space is not a leak, and a rejoined user is not one either.
			" AND NOT EXISTS (SELECT 1 FROM `space_member` sm "+
			"                  INNER JOIN `space` s ON s.space_id = sm.space_id AND s.status <> 0 "+
			"                  WHERE sm.space_id = pm.space_id AND sm.uid = pm.uid AND sm.status = 1) "+
			") AS violating "+
			"FROM `octo_project_member` pm "+
			"WHERE (pm.project_id, pm.uid) > (?, ?) "+
			"ORDER BY pm.project_id, pm.uid LIMIT ?",
		MemberStatusActive, cleanupStatusAbandoned, cleanupStatusPending,
		cursorProject, cursorUID, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query abandoned leak page: %w", err)
	}
	return rows, nil
}

// ---------- scan 3: orphan projects ----------

type orphanRow struct {
	ID        int64  `db:"id"`
	ProjectID string `db:"project_id"`
	SpaceID   string `db:"space_id"`
	Violating bool   `db:"violating"`
}

// scanOrphanProjects counts active projects whose Space row no longer exists.
func (p *Project) scanOrphanProjects() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("orphan").Observe(time.Since(start).Seconds()) }()

	cursor, total := cursors.idResume(&cursors.orphan, &cursors.orphanRun)
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryInspectedProjectPage(cursor, p.cfg.ReconcileLimit)
		if err != nil {
			// break, not return — see scanI1Violations for why the cursor save must be reached.
			p.Warn("对账孤儿项目扫描失败", zap.Error(err))
			break
		}
		if len(rows) == 0 {
			completed = true
			break
		}
		for _, row := range rows {
			if !row.Violating {
				continue
			}
			p.Error("项目所属 Space 已不存在",
				zap.String("projectId", row.ProjectID), zap.String("spaceId", row.SpaceID))
			total++
		}
		cursor = rows[len(rows)-1].ID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		orphanProjects.Set(float64(total))
	}
	cursors.idSave(&cursors.orphan, &cursors.orphanRun, cursor, total, completed)
}

// queryInspectedProjectPage returns one bounded page of ACTIVE project rows, each flagged
// with whether its Space is gone. Flag-over-base-page, like the member scans (Q4).
func (p *Project) queryInspectedProjectPage(cursor int64, limit int) ([]*orphanRow, error) {
	var rows []*orphanRow
	_, err := p.db.session.SelectBySql(
		"SELECT p.id, p.project_id, p.space_id, "+
			// Orphan = the Space is GONE (row absent) or DISBANDED (status=0). Both are
			// unrecoverable states that leave the project permanently invisible. A BANNED Space
			// (status=2) is deliberately not an orphan: a ban is recoverable, and flagging one
			// would cry wolf every tick of every ban.
			// NOTE — this gauge does not return to zero on its own. A legitimately disbanded
			// Space leaves its projects behind (nothing disbands them, by design: P0 owns no
			// cross-module teardown), so every project in a disbanded Space is counted on every
			// completed rotation with no repair path in P0. Read it the same way as
			// i1_abandoned_cleanup_leak: a standing figure needing a human, not a transient
			// that clears. The brief asks for "a project whose space_id no longer exists";
			// status=0 is included because it is equally unrecoverable and equally invisible,
			// and excluding it would have hidden the disband case the round-1 review asked for.
			//
			// p.status is part of the flag, not the WHERE clause: disbanded projects are kept
			// as rows, and no index leads with status (the PK is id, and the secondary indexes
			// lead with space_id or creator), so filtering here would bound rows returned
			// rather than rows examined as the count of disbanded projects grows.
			"(p.status = ? AND NOT EXISTS (SELECT 1 FROM `space` s "+
			"             WHERE s.space_id = p.space_id AND s.status <> 0)) AS violating "+
			"FROM `octo_project` p "+
			"WHERE p.id > ? "+
			"ORDER BY p.id LIMIT ?",
		StatusNormal, cursor, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query inspected project page: %w", err)
	}
	return rows, nil
}

// ---------- scan 4: epoch sanity ----------

// epochHistory is a bounded, process-local record of the highest member_epoch this replica
// has observed per project.
//
// A mutex plus a plain map, NOT a sync.Map. The previous version reset the history with
// `lastSeenEpoch = sync.Map{}`, which is a data race twice over: assigning to a shared
// package variable is an unsynchronised write, and a sync.Map must not be copied at all
// (it contains a Mutex). With the timing wheel firing `go task()` per tick, two overlapping
// scans could hit that concurrently. The reentrancy guard above now prevents the overlap,
// but a diagnostic must not depend on a second mechanism for its own memory safety.
//
// Best-effort by construction, and labelled as such wherever it is reported. The
// authoritative guarantee that member_epoch never goes backwards is the WRITE discipline:
// every statement in this package is `member_epoch = member_epoch + 1`, never an absolute
// assignment, enforced by a source guard test. A read-only scan cannot establish
// monotonicity — it would have to remember the previous value, and it runs on every pod, so
// a "regression" it saw could just be two replicas reading either side of one increment.
// What this catches is the crude case: a value that went down on the SAME replica between
// two scans.
type epochHistory struct {
	mu   sync.Mutex
	seen map[string]int64
}

// epochHistoryCap bounds the map so a deployment with very many projects cannot turn a
// diagnostic into a memory leak. On overflow the history is dropped wholesale rather than
// evicted cleverly: losing it costs one missed observation of a best-effort check.
const epochHistoryCap = 50000

// observe records epoch for projectID and reports whether it went backwards.
func (h *epochHistory) observe(projectID string, epoch int64) (regressed bool, previous int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = make(map[string]int64)
	}
	if len(h.seen) >= epochHistoryCap {
		h.seen = make(map[string]int64)
	}
	prev, ok := h.seen[projectID]
	h.seen[projectID] = epoch
	if ok && epoch < prev {
		return true, prev
	}
	return false, prev
}

var lastSeenEpoch epochHistory

type epochRow struct {
	ID          int64  `db:"id"`
	ProjectID   string `db:"project_id"`
	MemberEpoch int64  `db:"member_epoch"`
}

// scanEpochSanity flags negative epochs and same-replica regressions.
func (p *Project) scanEpochSanity() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("epoch").Observe(time.Since(start).Seconds()) }()

	cursor, _ := cursors.idResume(&cursors.epoch, new(int))
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		var rows []*epochRow
		_, err := p.db.session.SelectBySql(
			"SELECT id, project_id, member_epoch FROM `octo_project` "+
				"WHERE id > ? ORDER BY id LIMIT ?",
			cursor, p.cfg.ReconcileLimit,
		).Load(&rows)
		if err != nil {
			// break, not return — see scanI1Violations for why the cursor save must be reached.
			p.Warn("对账 member_epoch 扫描失败", zap.Error(err))
			break
		}
		for _, row := range rows {
			if row.MemberEpoch < 0 {
				epochAnomalies.Inc()
				p.Error("member_epoch 为负值", zap.String("projectId", row.ProjectID),
					zap.Int64("epoch", row.MemberEpoch))
				continue
			}
			if regressed, previous := lastSeenEpoch.observe(row.ProjectID, row.MemberEpoch); regressed {
				epochAnomalies.Inc()
				p.Error("member_epoch 相比本副本上次观测出现回退（best-effort 检查）",
					zap.String("projectId", row.ProjectID),
					zap.Int64("previous", previous), zap.Int64("current", row.MemberEpoch))
			}
		}
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
		cursor = rows[len(rows)-1].ID
	}
	cursors.idSave(&cursors.epoch, new(int), cursor, 0, completed)
}

// ---------- distribution metrics ----------

type distributionRow struct {
	MemberCount int `db:"member_count"`
}
