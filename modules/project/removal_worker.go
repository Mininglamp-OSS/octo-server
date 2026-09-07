package project

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The project-side member-removal cascade worker (D5).
//
// Its own outbox and its own worker rather than another step on the Space job,
// for three structural reasons stated on the migration: different key
// ((project_id, uid) vs (space_id, uid)), project-sized fan-out inside a lease
// sized for Space cleanup, and the Space step contract's rule that a returned
// error re-drives the WHOLE job — so a project-side failure would re-drive the
// Space-side steps.

const (
	// removalPollInterval matches the Space cleanup poll. This queue is latency
	// sensitive in a way the reconcile scan is not: until the cascade finishes,
	// a removed member still holds group_member rows, and the reconcile scan
	// exempts them for exactly that reason. A slow poll widens the window the
	// exemption has to cover.
	removalPollInterval = 10 * time.Second
	// removalLease is how long a claimed job is reserved. The worker HEARTBEATS
	// it (see below), so this is the "worker died" timeout rather than a bet on
	// how long the work takes.
	removalLease = 2 * time.Minute
	// removalHeartbeatEvery must be comfortably shorter than removalLease.
	removalHeartbeatEvery = 30 * time.Second
	// removalBatch is how many jobs one tick claims.
	removalBatch = 20
	// removalMaxAttempts before a job is abandoned. Abandoned is terminal and
	// alerts; it does not retry, because a job failing this many times is a bug
	// or a broken dependency, and retrying forever hides both.
	removalMaxAttempts = 8
	// removalRetention is how long terminal rows are kept for forensics.
	removalRetention = 7 * 24 * time.Hour
	// removalPurgeBatch bounds one DELETE. The purge DRAINS by looping until a
	// batch comes back short, rather than deleting a fixed number per hour:
	// #797 records that the Space outbox's fixed 1000/hour is 24k/day, below
	// realistic churn, so its table grows forever.
	removalPurgeBatch = 500
	// removalPurgeEvery is how often the drain runs.
	removalPurgeEvery = time.Hour
)

var (
	removalWorkerOnce sync.Once
	removalRunning    atomic.Bool
)

// startRemovalWorker schedules the cascade poll and the retention purge.
func (p *Project) startRemovalWorker() {
	removalWorkerOnce.Do(func() {
		p.ctx.Schedule(removalPollInterval, p.runRemovalCascade)
		p.ctx.Schedule(jitter(removalPurgeEvery), p.purgeRemovalJobs)
	})
}

// runRemovalCascade claims a batch of pending jobs and works them.
func (p *Project) runRemovalCascade() {
	if !removalRunning.CompareAndSwap(false, true) {
		return // a batch is still in flight; skip rather than pile on
	}
	defer removalRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目成员移除级联 panic", zap.Any("recover", r))
		}
	}()

	now := time.Now().UTC()
	owner := p.workerIdentity()
	jobs, err := p.db.claimRemovalJobs(owner, removalBatch, now, removalLease)
	if err != nil {
		p.Error("认领项目移除工单失败", zap.Error(err))
		return
	}

	// Publish the backlog. It is read here rather than on its own timer because
	// this is the only place that already knows the queue is being looked at, and
	// a COUNT on an indexed status column every 10s is cheaper than a second
	// scheduled job. Without a writer the gauge sat at zero forever, which reads
	// as "the queue is empty" — the same failure shape P0's round 5 found on the
	// reconcile gauges.
	if pending, cErr := p.db.countPendingRemovalJobs(); cErr != nil {
		p.Warn("采集项目移除工单积压失败", zap.Error(cErr))
	} else {
		removalBacklog.Set(float64(pending))
	}
	if len(jobs) == 0 {
		return
	}

	// Heartbeat the WHOLE claimed batch for as long as this tick runs.
	//
	// One claim leases up to removalBatch jobs and they are then worked in
	// sequence, so the last one can wait for all the others. Renewing only the job
	// currently in flight — which is what the first version did — leaves the queued
	// remainder on the lease they were claimed with, and the tail of a slow batch
	// expires while this worker still intends to work it. Another pod then claims
	// those rows and runs the same cascade concurrently, and the two take group
	// locks in whatever order their group lists happen to produce.
	//
	// Renewing a job that has already finished is harmless: the statement is
	// guarded on status = pending, so a terminal row is not touched.
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	stop := p.startLeaseHeartbeat(ids, owner)
	defer stop()

	for _, job := range jobs {
		p.workRemovalJob(job, owner)
	}
}

// startLeaseHeartbeat renews `ids` every removalHeartbeatEvery until the returned
// function is called, which blocks until the goroutine has stopped.
func (p *Project) startLeaseHeartbeat(ids []int64, owner string) func() {
	stop := make(chan struct{})
	var beat sync.WaitGroup
	beat.Add(1)
	go func() {
		defer beat.Done()
		ticker := time.NewTicker(removalHeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				until := time.Now().UTC().Add(removalLease)
				if err := p.db.heartbeatRemovalLeases(ids, owner, until); err != nil {
					p.Warn("续租项目移除工单失败", zap.Int("jobs", len(ids)), zap.Error(err))
				}
			}
		}
	}()
	return func() {
		close(stop)
		beat.Wait()
	}
}

// workRemovalJob runs one job to a terminal state, or reschedules it.
//
// `owner` is the lease this job was claimed under. Every write that retires or
// releases the job is fenced on it (see db_removal.go): a worker that lost its
// lease mid-job abandons the write rather than landing it on top of the worker
// that took over.
func (p *Project) workRemovalJob(job RemovalJob, owner string) {
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目移除工单 panic",
				zap.Int64("job_id", job.ID),
				zap.String("project_id", job.ProjectID),
				zap.Any("recover", r))
		}
	}()

	// The lease is heartbeaten by runRemovalCascade for the whole claimed batch,
	// not per job — see startLeaseHeartbeat for why the batch is the right unit.

	// Re-read the member row UNDER LOCK before doing anything (D4).
	//
	// The job was enqueued when removal began and may have waited in the queue.
	// A member re-admitted in that window has removing = 0, and tearing their
	// groups down now would destroy a membership that is legitimate again. Same
	// shape as P0's checkSpaceSeatForCleanupTx re-check.
	cancelled, err := p.removalCancelled(job)
	if err != nil {
		p.rescheduleAfterFailure(job, owner, err)
		return
	}
	if cancelled {
		p.retireRemovalJob(job, owner, removalJobCancelled, "re-admitted")
		return
	}

	// Run every registered step. A step failing does NOT stop the others: partial
	// progress is durable (a group already left does not come back), and the job
	// retries what remains. The first error decides the job's fate.
	removal := MemberRemoval{
		ProjectID:   job.ProjectID,
		UID:         job.UID,
		SpaceID:     job.SpaceID,
		OperatorUID: job.OperatorUID,
		Reason:      job.Reason,
	}
	steps := snapshotMemberRemovalSteps()
	if len(steps) == 0 {
		// An empty registry must NOT read as "every step succeeded".
		//
		// Falling through would call finishRemoval and close the seat with the
		// member's group_member rows never detached — the exact I2 violation the
		// two-phase close exists to avoid, produced silently, with the job marked
		// done. The mirror-image registry in modules/space fails closed for the
		// same reason (preset_group_admitter.go: joinPresetGroups SKIPS and does
		// not fall back).
		//
		// Failing here instead leaves the seat at removing = 1 — a state in which
		// the member is already a non-member for every authorization read — and
		// surfaces as backlog plus the stall alert. Stuck and visible beats closed
		// and wrong.
		p.rescheduleAfterFailure(job, owner,
			fmt.Errorf("project: no member-removal cascade step is registered"))
		return
	}
	var firstErr error
	for _, step := range steps {
		if err := step.fn(p.ctx, removal); err != nil {
			p.Error("项目移除级联步骤失败",
				zap.String("step", step.name),
				zap.Int64("job_id", job.ID),
				zap.String("project_id", job.ProjectID),
				zap.String("uid", job.UID),
				zap.Error(err))
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", step.name, err)
			}
		}
	}
	if firstErr != nil {
		p.rescheduleAfterFailure(job, owner, firstErr)
		return
	}

	// Every step succeeded: close the seat for good.
	//
	// Re-checked under lock a second time, because the steps take time and a
	// re-admission can land while they run. finishMemberRemovalTx is guarded on
	// removing = 1, so a cancelled removal affects zero rows and the job is
	// retired as cancelled rather than closing a seat somebody just restored.
	if err := p.finishRemoval(job); err != nil {
		p.rescheduleAfterFailure(job, owner, err)
		return
	}
	p.retireRemovalJob(job, owner, removalJobDone, "")
}

// retireRemovalJob writes one terminal state and reports whether it landed.
//
// A lost lease is a WARN, not an error: the job is not lost, another worker owns
// it and will retire it. What must not happen silently is this worker believing
// it retired a job it did not — hence the return value, which the abandon path
// uses so its breadcrumb log describes what actually happened.
func (p *Project) retireRemovalJob(job RemovalJob, owner string, status int, lastErr string) bool {
	held, err := p.db.completeRemovalJob(job.ID, owner, status, lastErr, time.Now().UTC())
	if err != nil {
		p.Error("标记项目移除工单终态失败",
			zap.Int64("job_id", job.ID), zap.Int("status", status), zap.Error(err))
		return false
	}
	if !held {
		p.Warn("项目移除工单租约已易主，放弃写入终态",
			zap.Int64("job_id", job.ID),
			zap.Int("status", status),
			zap.String("lease_owner", owner))
	}
	return held
}

// removalCancelled reports whether the member was re-admitted since the job was
// enqueued.
func (p *Project) removalCancelled(job RemovalJob) (bool, error) {
	tx, err := p.ctx.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin cascade recheck: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	member, err := p.db.lockMemberForCascadeTx(tx, job.ProjectID, job.UID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit cascade recheck: %w", err)
	}
	if member == nil {
		// No row at all: nothing to cascade and nothing to close. Treat as
		// cancelled so the job retires instead of retrying forever.
		return true, nil
	}
	// removing == 0 means either re-admitted (status 1) or already finished
	// (status 0). Both mean this job has no work left.
	return member.Removing == 0, nil
}

// finishRemoval flips status to 0 and clears removing, under the row lock.
func (p *Project) finishRemoval(job RemovalJob) error {
	now := time.Now().UTC()
	tx, err := p.ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("project: begin finish removal: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	member, err := p.db.lockMemberForCascadeTx(tx, job.ProjectID, job.UID)
	if err != nil {
		return err
	}
	if member == nil || member.Removing == 0 {
		// Cancelled while the steps ran. Commit the (empty) transaction and let
		// the caller mark the job done: the steps that did run left the member
		// out of some groups but still in the project, which is NOT an invariant
		// violation — the subset relation still holds — it is visible in the
		// member lists, and an admin can re-add. Do not "fix" this by re-adding
		// them to those groups: that would race the very admission the
		// cancellation represents.
		return tx.Commit()
	}

	changed, err := p.db.finishMemberRemovalTx(tx, job.ProjectID, job.UID, now)
	if err != nil {
		return err
	}
	if changed {
		// The epoch was already bumped when `removing` was set — that is when
		// the membership changed from every consumer's point of view. Bumping
		// again here would make one removal move the epoch twice, and the
		// acceptance requires it to move by exactly +1 per membership change.
		p.invalidateProjectMemberCache(job.ProjectID, job.UID)
	}
	return tx.Commit()
}

// rescheduleAfterFailure applies backoff, or abandons a job that has run out of
// attempts.
func (p *Project) rescheduleAfterFailure(job RemovalJob, owner string, cause error) {
	now := time.Now().UTC()
	if job.Attempts >= removalMaxAttempts {
		if !p.retireRemovalJob(job, owner, removalJobAbandoned, cause.Error()) {
			// The write did not land, so this job is not abandoned — either it
			// errored or another worker owns it now. retireRemovalJob has already
			// said which; claiming an abandon on top of that would be a false
			// breadcrumb in the one log an operator reads after the stall alert.
			return
		}
		// Abandoned is terminal and means a member's project seat is stuck at
		// removing = 1 with group rows still in place. The reconcile scan's
		// stall alert is what surfaces it; this log is the breadcrumb.
		p.Error("项目移除工单已放弃，成员席位停在 removing=1",
			zap.Int64("job_id", job.ID),
			zap.String("project_id", job.ProjectID),
			zap.String("uid", job.UID),
			zap.Int("attempts", job.Attempts),
			zap.Error(cause))
		return
	}
	backoff := time.Duration(1<<uint(job.Attempts)) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	held, err := p.db.rescheduleRemovalJob(job.ID, owner, now.Add(backoff), cause.Error())
	if err != nil {
		p.Error("重排项目移除工单失败", zap.Int64("job_id", job.ID), zap.Error(err))
		return
	}
	if !held {
		p.Warn("项目移除工单租约已易主，放弃重排",
			zap.Int64("job_id", job.ID), zap.String("lease_owner", owner))
	}
}

// purgeRemovalJobs drains terminal rows older than the retention window.
//
// Loops until a batch comes back short, rather than deleting a fixed number per
// tick. A fixed cap that is below the arrival rate is not a slower purge, it is
// no purge: the table grows without bound and every scan over it gets slower.
func (p *Project) purgeRemovalJobs() {
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目移除工单清理 panic", zap.Any("recover", r))
		}
	}()
	before := time.Now().UTC().Add(-removalRetention)
	var total int64
	for {
		n, err := p.db.purgeFinishedRemovalJobs(before, removalPurgeBatch)
		if err != nil {
			p.Error("清理项目移除工单失败", zap.Error(err))
			return
		}
		total += n
		if n < int64(removalPurgeBatch) {
			break
		}
	}
	if total > 0 {
		p.Info("已清理终态项目移除工单", zap.Int64("deleted", total))
	}
}

// workerIdentity labels a lease.
//
// Process-stable, not per-tick. Two things depend on that. An operator reading
// lease_owner has to be able to answer "which pod is holding this job", which a
// fresh timestamp per tick cannot; and the fence on the terminal writes is only
// as meaningful as the identity it compares. A hostname+pid pair distinguishes
// pods, survives every tick of one process, and changes on restart — which is
// exactly when a lease SHOULD stop being recognised as ours.
//
// Two ticks of the same process cannot collide on a job: runRemovalCascade
// skips while a batch is still in flight (removalRunning).
//
// The column is 64 chars (see the migration); a long hostname is truncated from
// the left, keeping the pid and the distinguishing tail of the name.
var workerIdentityOnce sync.Once
var workerIdentityValue string

func (p *Project) workerIdentity() string {
	workerIdentityOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" {
			// No hostname is not a reason to fall back to something unstable:
			// the pid still distinguishes this process from another on the same
			// node, and an unstable owner would silently disable the fence.
			host = "unknown"
		}
		id := fmt.Sprintf("project-removal-%s-%d", host, os.Getpid())
		if len(id) > 64 {
			id = id[len(id)-64:]
		}
		workerIdentityValue = id
	})
	return workerIdentityValue
}
