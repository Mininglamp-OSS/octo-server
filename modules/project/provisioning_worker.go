package project

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/internal/projectprovision"
	"go.uber.org/zap"
)

// The eager subsystem-provisioning worker (brief D2, Shape S).
//
// This file, and only this file, is allowed to import
// internal/projectprovision. That is the "outbound confinement" rule: a request
// handler that reaches fleet or drive makes a user request depend on another
// service being up, so the outbound client stays behind a package boundary a
// source guard can check (TestProvisioningClientIsConfinedToTheWorker).
//
// The driver is the outbox row, not this code. Retry, backoff and the give-up
// decision are all persisted, because a pod restart must not lose them. What lives
// here is claim / call / record.

const (
	// provisioningSweepInterval / provisioningSweepLimit pace the exhausted-row
	// sweep. A minute is plenty: what it moves are rows whose executor was killed,
	// and noticing that a minute late costs nothing since they are no longer
	// claimable. The limit bounds a mass-failure aftermath — a target down for
	// longer than the whole retry budget exhausts every pending row at once.
	provisioningSweepInterval = time.Minute
	provisioningSweepLimit    = 500
	// provisioningPurgeInterval / provisioningPurgeLimit pace retention cleanup.
	provisioningPurgeInterval = time.Hour
	provisioningPurgeLimit    = 1000
)

// provisionEnsurer is the one-method surface the worker needs from
// internal/projectprovision.
//
// Declared here, where it is USED, so api.go — the file that carries this module's
// request handlers — does not import the outbound package merely to name a struct
// field's type. That makes the confinement guard a real check on the dependency
// rather than one that has to carve out an exception for its own type declaration,
// and it gives the worker an injection point if a future test needs one.
type provisionEnsurer interface {
	Ensure(ctx context.Context, target projectprovision.Target, req projectprovision.EnsureRequest) (projectprovision.EnsureResponse, error)
}

// permanentProvisioningOutcomes are the failure categories that another attempt cannot
// repair, so a row carrying one goes straight to `abandoned` instead of consuming its
// whole retry budget.
//
// The set is deliberately SMALL, and the boundary is "is our own request wrong, or did
// the receiver disagree about which resource this is". Everything else keeps the bounded
// retry, including 4xx: a 404 is what a target that has not deployed its ensure endpoint
// looks like (the expected state until precondition P-2 lands) and a 401 is what a
// rotated secret looks like — both become 200 after a deployment on the other side, with
// nothing changing here, so abandoning them would turn a self-healing situation into one
// needing a manual requeue.
//
//   - container_id_mismatch: the target answered with a different id, so our mapping row
//     points at a container nobody owns. Retrying cannot repair that, and it re-calls the
//     target every time; it needs a human to work out which side generated the id.
//   - invalid_request: the row is missing an identifying field, which means the enqueue
//     side is broken.
//   - encode_failed: a code defect in our own marshalling.
var permanentProvisioningOutcomes = map[string]bool{
	"container_id_mismatch": true,
	"invalid_request":       true,
	"encode_failed":         true,
}

func isPermanentProvisioningOutcome(outcome string) bool {
	return permanentProvisioningOutcomes[outcome]
}

// newProvisionClient builds the single outbound client.
//
// It lives here rather than inline in New() so that api.go — the file that carries
// this module's request handlers — never names the outbound package's constructor.
// That is what makes TestProvisioningClientIsConfinedToTheWorker a check on where the
// egress can be reached FROM, rather than a check that happens to tolerate one
// construction site inside the handler file.
func newProvisionClient() provisionEnsurer {
	return projectprovision.NewClient(nil, nil)
}

// provisioningWorkerPrefix identifies this process in lease_owner. Kept short
// because the per-claim counter is appended and the column is VARCHAR(64) —
// modules/space measured that two full UUIDs plus a prefix is 78 characters, which
// MySQL rejects outright with "Data too long", killing every claim.
var provisioningWorkerPrefix = "pp-" + util.GenerUUID()

// provisioningClaimSeq gives every claim a unique suffix.
var provisioningClaimSeq atomic.Uint64

// newProvisioningClaimOwner mints a unique owner per CLAIM, not per process.
//
// Two triggers exist — the post-create nudge and the interval tick — and a
// process-level constant would make the `AND lease_owner = ?` guard on
// finish/release true for both of them at once. Whichever finished first would
// write the terminal state, and the other's later release would then reopen a row
// that had already succeeded.
func newProvisioningClaimOwner() string {
	return provisioningWorkerPrefix + "-" + strconv.FormatUint(provisioningClaimSeq.Add(1), 10)
}

// provisioningWorkerOnce keeps one set of timers per PROCESS.
//
// Route() runs once in production but once per testutil.NewTestServer in tests,
// and modules/user alone builds ~200 of them in a single binary. Without this,
// each package's test run accumulates hundreds of never-cancelled timingwheel
// timers all pointed at the same MySQL, waking together and spending the package's
// time budget on background work unrelated to the case under test. modules/space
// has the same guard for the same measured reason.
var provisioningWorkerOnce sync.Once

// provisioningRunning is the in-process reentrancy guard.
//
// The interval tick schedules the next firing before running the current one, so a
// slow batch would otherwise stack: with an unhealthy target every job takes the
// full 10s timeout, a batch of 20 takes minutes, and ticks would pile up
// concurrent batches each holding DB connections and hammering the same
// destination. Cross-replica safety is the DB lease; this is the same-process half.
var provisioningRunning atomic.Bool

// startProvisioningWorker mounts the timers. Called from Route().
//
// A no-op when no target is enabled, which is the default: nothing is enqueued in
// that state either, so the whole slice is inert rather than idling.
func (p *Project) startProvisioningWorker() {
	if !p.cfg.Provisioning.Enabled() {
		return
	}
	provisioningWorkerOnce.Do(func() {
		p.ctx.Schedule(p.cfg.Provisioning.Interval, p.processProvisioningJobs)
		p.ctx.Schedule(provisioningSweepInterval, p.sweepExhaustedProvisioningJobs)
		p.ctx.Schedule(provisioningPurgeInterval, p.purgeProvisioningJobs)
		// Metrics on the module's sparse tick: countProvisioningByTargetStatus is a
		// whole-table GROUP BY, and these gauges serve trend questions.
		p.ctx.Schedule(p.cfg.MetricsInterval, p.refreshProvisioningMetrics)
	})
}

// publishProvisioningConfigMetrics seeds the misconfiguration gauge and logs the
// reasons. Called from New() so the signal exists before Route() runs, and before
// any request could depend on it.
func (p *Project) publishProvisioningConfigMetrics() {
	for _, t := range p.cfg.Provisioning.Targets {
		provisioningTargetMisconfigured.WithLabelValues(t.Name).Set(0)
	}
	for _, name := range p.cfg.Provisioning.Misconfigured {
		provisioningTargetMisconfigured.WithLabelValues(name).Set(1)
	}
	for _, err := range p.cfg.Provisioning.Problems {
		// Error, not Warn: an operator asked for this target and it will not
		// provision anything. The message never carries the secret or the observed
		// secret length; see checkSecretExclusivity.
		p.Error("project provisioning target rejected at config load; that target will not provision",
			zap.Error(err))
	}
}

// nudgeProvisioningWorker runs one batch off the request path after a successful
// create, so "eager" means seconds rather than up to a full interval.
//
// One goroutine per create, not per target, and it is reentrancy-guarded by
// processProvisioningJobs. The bound on how many of these can exist is the project
// creation rate, which is itself capped by the per-day and per-Space quotas —
// unlike modules/space's original per-uid fan-out, which turned disbanding one
// large Space into thousands of goroutines.
func (p *Project) nudgeProvisioningWorker() {
	if !p.cfg.Provisioning.Enabled() {
		return
	}
	go p.processProvisioningJobs()
}

// enabledProvisioningTargetNames is the claim filter.
func (p *Project) enabledProvisioningTargetNames() []string {
	names := make([]string, 0, len(p.cfg.Provisioning.Targets))
	for _, t := range p.cfg.Provisioning.Targets {
		names = append(names, t.Name)
	}
	return names
}

// processProvisioningJobs claims and runs up to one batch.
func (p *Project) processProvisioningJobs() {
	if !p.cfg.Provisioning.Enabled() {
		return
	}
	if !provisioningRunning.CompareAndSwap(false, true) {
		return // a batch is already in flight; yield
	}
	defer provisioningRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			p.Error("project provisioning batch panicked", zap.Any("recover", r))
		}
	}()

	targets := p.enabledProvisioningTargetNames()
	if len(targets) == 0 {
		return
	}
	// One goroutine per target, each with its own share of the batch.
	//
	// A single shared loop over `target IN (...)` let one unhealthy target starve the
	// other, and the client's short timeout does NOT prevent that — the arithmetic was
	// simply wrong. With fleet down and a due backlog, every one of a 20-row batch costs
	// the full 10s timeout, so a batch runs for up to 200s; provisioningRunning makes
	// every 15s tick in that window a no-op, so healthy drive rows wait behind fleet's
	// timeouts for minutes.
	//
	// A per-target quota alone would only BOUND that wait. Separate goroutines remove it:
	// each target advances at its own pace and an unhealthy one delays nobody. Two
	// targets means at most two goroutines, so this is a fixed, tiny fan-out — not the
	// per-uid fan-out modules/space had to walk back.
	perTarget := p.cfg.Provisioning.BatchSize / len(targets)
	if perTarget < 1 {
		perTarget = 1
	}
	maxAttempts := p.cfg.Provisioning.MaxAttempts
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			// Per-goroutine recovery: a panic here would otherwise take down the process,
			// because the batch-level recover is on a different goroutine's stack.
			defer func() {
				if r := recover(); r != nil {
					p.Error("project provisioning target batch panicked",
						zap.Any("recover", r), zap.String("target", target))
				}
			}()
			p.runProvisioningBatchForTarget(target, perTarget, maxAttempts)
		}(target)
	}
	wg.Wait()
}

// runProvisioningBatchForTarget claims and runs up to `limit` rows for one target.
func (p *Project) runProvisioningBatchForTarget(target string, limit int, maxAttempts uint32) {
	only := []string{target}
	for processed := 0; processed < limit; processed++ {
		owner := newProvisioningClaimOwner()
		job, err := p.db.claimProvisioningJob(owner, only, maxAttempts, time.Now().UTC())
		if err != nil {
			p.Error("claim project provisioning job failed",
				zap.String("target", target), zap.Error(err))
			return
		}
		if job == nil {
			return
		}
		p.runProvisioningJob(job, owner)
	}
}

// runProvisioningJob performs one ensure call and records the outcome.
//
// The panic recovery is at THIS level, not only around the batch. A panic caught
// one level up would skip the release, leaving the row claimed with no last_error
// and no backoff: it would sit until the lease expired, be re-claimed, and panic
// again. attempts advances at claim time so it would still converge to abandoned,
// but every cycle burns a full lease period AND a batch slot that a healthy row
// needed.
func (p *Project) runProvisioningJob(job *provisioningJob, owner string) {
	defer func() {
		if r := recover(); r != nil {
			p.Error("project provisioning job panicked",
				zap.Any("recover", r), zap.Uint64("jobId", job.ID),
				zap.String("target", job.Target), zap.String("projectId", job.ProjectID))
			p.releaseOrAbandon(job, owner, "panic", errors.New("provisioning job panicked"))
		}
	}()

	target, ok := p.cfg.Provisioning.TargetByName(job.Target)
	if !ok {
		// Configuration changed between the claim and here. Retry rather than
		// abandon: the operator may be mid-rollout, and the claim filter means the
		// row simply stops being picked up if the target stays off.
		p.releaseOrAbandon(job, owner, "target_disabled", errProvisioningDisabled)
		return
	}

	req := projectprovision.EnsureRequest{
		ContainerID: job.ContainerID,
		ProjectID:   job.ProjectID,
		OctoSpaceID: job.SpaceID,
		Name:        provisioningContainerName,
	}
	// issue_prefix is fleet's per-workspace field and octo-server stores none of it
	// (D4). Sending it empty at creation lets fleet apply its own default; 任务前缀
	// is then edited in fleet, and fleet's own help text agrees that changing it only
	// affects new issues.

	started := time.Now()
	// context.Background(), not a request context: nothing here is on a request
	// path, and the call is bounded by the client's own per-target timeout.
	_, err := p.provisionClient.Ensure(context.Background(), target.Target, req)
	provisioningDuration.WithLabelValues(job.Target).Observe(time.Since(started).Seconds())

	if err != nil {
		outcome := projectprovision.Category(err)
		if outcome == "" {
			outcome = "unclassified"
		}
		p.releaseOrAbandon(job, owner, outcome, err)
		return
	}
	observeProvisioningAttempt(job.Target, "ready")
	// Note what is NOT recorded: the container id returned by the target. It equals
	// what we sent (the client refuses the response otherwise), and the row already
	// holds it, so there is nothing to write back — which is the point of generating
	// the id at enqueue time (D1). fleet's `slug` is likewise dropped: fleet
	// addresses workspaces BY slug and our container_id IS that slug, so storing a
	// second copy would only create something to drift.
	p.finishProvisioning(job, owner, provisionStatusReady, "")
}

// provisioningContainerName is the display name sent to the target.
//
// Deliberately NOT the project name, and the divergence from brief D2's sketched
// body is worth stating rather than leaving as a silent simplification.
//
// Three reasons. The name is authoritative in octo-server and never synced outbound
// (D3), so a copy on the other side can only drift. Reading octo_project here would
// add a query to every attempt for a field the target treats as a label. And a
// project name is user-supplied free text: sending it turns provisioning into a
// content-egress path with its own escaping and disclosure questions, which this
// slice does not want to open — a target that needs the real name reads it from
// GET /v1/projects/:project_id, which is what D3 already tells fleet to do for
// `context`.
//
// So the value carries no user content: a stable, low-information label. If the
// product later wants the real name on the subsystem side, that is a deliberate
// change with an owner, not a default.
const provisioningContainerName = "octo-project"

// releaseOrAbandon schedules the retry, or writes the terminal abandoned state when
// the budget is gone.
//
// It is also the ONE place a failed attempt is counted, and that placement is the fix
// for a metric that was wrong in two directions at once. Counting at the call site
// meant the panic and target-disabled paths — which reach here but not the ensure
// call — recorded nothing at all, so a job that panicked on every attempt was
// invisible in provisioning_attempts_total until it abandoned. And the abandon branch
// then added a SECOND increment labelled "abandoned", so the last failing attempt of
// any job was counted twice under two different outcomes and
// sum by(outcome)(provisioning_attempts_total) did not equal the number of attempts.
//
// Now: exactly one increment per attempt, labelled with the real reason — including
// the attempt that exhausts the budget, whose reason is the interesting part. "How
// many rows have given up" is a different question and is already answered by the
// provisioning_rows{status="abandoned"} gauge, so it does not need a counter label
// competing with the outcomes.
func (p *Project) releaseOrAbandon(job *provisioningJob, owner, outcome string, cause error) {
	now := time.Now().UTC()
	observeProvisioningAttempt(job.Target, outcome)
	permanent := isPermanentProvisioningOutcome(outcome)
	if permanent || job.Attempts >= p.cfg.Provisioning.MaxAttempts {
		reason := "retries exhausted"
		if permanent {
			// Short-circuited on the FIRST attempt. Spending ~23 minutes and a dozen more
			// calls to reach the same terminal state would only delay the alert and keep
			// hitting a target that already told us the answer.
			reason = "permanent failure"
		}
		// Error, and on purpose it fires once per row rather than once per tick:
		// abandoned has NO automatic re-drive. Once the target's precondition (brief
		// P-2, a service identity) lands, these rows need a deliberate requeue.
		p.Error("project provisioning abandoned; no automatic re-drive, needs a requeue",
			zap.Uint64("jobId", job.ID), zap.String("target", job.Target),
			zap.String("projectId", job.ProjectID), zap.String("outcome", outcome),
			zap.Bool("permanent", permanent),
			zap.Uint32("attempts", job.Attempts), zap.Error(cause))
		p.finishProvisioning(job, owner, provisionStatusAbandoned, outcome+": "+reason)
		return
	}
	p.Warn("project provisioning attempt failed; will retry",
		zap.Uint64("jobId", job.ID), zap.String("target", job.Target),
		zap.String("projectId", job.ProjectID), zap.String("outcome", outcome),
		zap.Uint32("attempts", job.Attempts), zap.Error(cause))
	// last_error carries the outcome label and the error's own string. Both are
	// container-id-free by construction: projectprovision.EnsureError.Error is built
	// from a category and a status only.
	if err := p.db.releaseProvisioningJob(job.ID, owner, job.Attempts, outcome+": "+cause.Error(), now); err != nil {
		if errors.Is(err, errProvisioningLeaseLost) {
			p.Warn("project provisioning lease changed hands before release", zap.Uint64("jobId", job.ID))
			return
		}
		p.Warn("release project provisioning job failed", zap.Uint64("jobId", job.ID), zap.Error(err))
	}
}

// finishProvisioning writes a terminal status. A lost lease is logged, not
// retried: another replica took over, and the ensure contract makes duplicate
// execution safe.
func (p *Project) finishProvisioning(job *provisioningJob, owner string, status uint8, note string) {
	ok, err := p.db.finishProvisioningJob(job.ID, owner, status, note, time.Now().UTC())
	if err != nil {
		p.Warn("write project provisioning terminal status failed",
			zap.Uint64("jobId", job.ID), zap.Error(err))
		return
	}
	if !ok {
		// Two ways to get here and both are benign: another replica re-claimed after
		// a lease expiry, or the project was disbanded while this job ran and
		// disbandProjectTx moved the row to disband_pending. Neither wants a retry.
		p.Warn("project provisioning row no longer claimable; terminal status not written",
			zap.Uint64("jobId", job.ID), zap.String("target", job.Target))
	}
}

// sweepExhaustedProvisioningJobs pushes hard-killed rows to abandoned.
func (p *Project) sweepExhaustedProvisioningJobs() {
	abandoned, err := p.db.abandonExhaustedProvisioningJobs(
		p.cfg.Provisioning.MaxAttempts, time.Now().UTC(), provisioningSweepLimit)
	if err != nil {
		p.Warn("sweep exhausted project provisioning jobs failed", zap.Error(err))
		return
	}
	if abandoned > 0 {
		p.Error("project provisioning jobs abandoned by sweep; no automatic re-drive, needs a requeue",
			zap.Int64("abandoned", abandoned))
	}
}

// purgeProvisioningJobs deletes disband_pending rows past the retention window.
// ready and abandoned rows are never purged — see provisioningRetention.
func (p *Project) purgeProvisioningJobs() {
	deleted, err := p.db.purgeFinishedProvisioningJobs(
		time.Now().UTC().Add(-provisioningRetention), provisioningPurgeLimit)
	if err != nil {
		p.Warn("purge project provisioning jobs failed", zap.Error(err))
		return
	}
	if deleted > 0 {
		p.Info("purged disband-pending project provisioning rows", zap.Int64("deleted", deleted))
	}
}

// refreshProvisioningMetrics republishes the row census and the unnarrowed-surface
// gauge.
//
// Buckets absent from the query are reset to 0 rather than left at their last
// value: a gauge that keeps a stale reading after the condition clears is worse
// than no gauge, because it reads as an unresolved alert.
func (p *Project) refreshProvisioningMetrics() {
	rows, err := p.db.countProvisioningByTargetStatus()
	if err != nil {
		p.Warn("refresh project provisioning metrics failed", zap.Error(err))
		return
	}
	observed := map[string]map[string]int64{}
	for _, row := range rows {
		if observed[row.Target] == nil {
			observed[row.Target] = map[string]int64{}
		}
		observed[row.Target][provisioningStatusLabel(row.Status)] = row.Rows
	}
	statuses := []string{"pending", "ready", "abandoned", "disband_pending"}
	for _, target := range []string{TargetFleet, TargetDrive} {
		for _, status := range statuses {
			provisioningRows.WithLabelValues(target, status).Set(float64(observed[target][status]))
		}
		// Two corrections here over the first version, both in the direction of
		// OVER-counting, because this gauge is billed as the reason the slice can ship
		// before R2/R3 — a security surface measurement that under-reports is worse than
		// none.
		//
		// 1. `ready + pending + abandoned`, not `ready` alone. The first version argued
		//    "a pending row has no container on the other side yet", which is not true
		//    under at-least-once delivery: if the receiver creates the container and the
		//    response is lost, the container exists while the row stays pending and may
		//    later go abandoned. The module already said so elsewhere —
		//    markProvisioningDisbandPendingTx explains that abandoned rows "record 'we
		//    never confirmed a container', but we may well have created one and failed to
		//    record it". Both statements could not be right.
		//
		//    disband_pending is still excluded, and that exclusion is a choice rather
		//    than the same mistake: those containers are explicitly enumerated for
		//    reclaim, and they remain visible as provisioning_rows{status=
		//    "disband_pending"} for anyone who wants total-ever-created.
		//
		// 2. An UNCONFIGURED target counts as not narrowed. The first version zeroed the
		//    gauge whenever the target was absent from cfg.Targets — so the documented
		//    rollback (clear OCTO_PROJECT_PROVISION_TARGETS) made the exposure read 0
		//    while every container still existed and was still unnarrowed. Narrowing is a
		//    property of the SUBSYSTEM, not of whether we are currently talking to it.
		narrowed := false
		if t, ok := p.cfg.Provisioning.TargetByName(target); ok {
			narrowed = t.Narrowed
		}
		unnarrowed := 0.0
		if !narrowed {
			unnarrowed = float64(observed[target]["ready"] +
				observed[target]["pending"] +
				observed[target]["abandoned"])
		}
		provisioningUnnarrowedContainers.WithLabelValues(target).Set(unnarrowed)
	}
}
