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
	cancelled, err := p.removalCancelled(job, owner)
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
	steps := snapshotMemberRemovalSteps()
	removal := MemberRemoval{
		ProjectID:   job.ProjectID,
		UID:         job.UID,
		SpaceID:     job.SpaceID,
		OperatorUID: job.OperatorUID,
		Reason:      job.Reason,
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
	closed, err := p.finishRemoval(job, owner)
	if err != nil {
		p.rescheduleAfterFailure(job, owner, err)
		return
	}
	if !closed {
		// The seat was not ours to close. Retiring the job is still right — its
		// own work is done — but say so, because "cascade finished and the seat
		// is still open" is the one shape that used to be invisible.
		p.Info("项目移除工单：席位不归本工单关闭，保持 removing 由后续工单接手",
			zap.Int64("job_id", job.ID),
			zap.String("project_id", job.ProjectID),
			zap.String("uid", job.UID))
	}
	p.retireRemovalJob(job, owner, removalJobDone, "")
}

// retireRemovalJob writes one terminal state and reports whether it landed.
//
// A refused write is not an error: the job is not lost. What must not happen
// silently is this worker believing it retired a job it did not — hence the
// return value, which the abandon path uses so its breadcrumb log describes what
// actually happened.
//
// The fence refuses for two very different reasons, and they must not read the
// same in a log. A lease that changed hands is a worker that fell behind and a
// thing to look at. A job already retired as CANCELLED is D4 working exactly as
// designed: re-admission retires the row in its own transaction, and it catches
// CLAIMED rows too, so EVERY cancelled cascade ends with the worker's own
// terminal write being refused. Reporting that as a lease-steal would make the
// designed path look like an incident on every occurrence.
func (p *Project) retireRemovalJob(job RemovalJob, owner string, status int, lastErr string) bool {
	held, err := p.db.completeRemovalJob(job.ID, owner, status, lastErr, time.Now().UTC())
	if err != nil {
		p.Error("标记项目移除工单终态失败",
			zap.Int64("job_id", job.ID), zap.Int("status", status), zap.Error(err))
		return false
	}
	if held {
		return true
	}
	// Read the row back to say which of the two it was. One extra query, only on
	// the path that already decided not to write.
	current, readErr := p.db.removalJobStatus(job.ID)
	switch {
	case readErr != nil:
		p.Warn("项目移除工单终态写入被拒，且无法复读工单状态",
			zap.Int64("job_id", job.ID), zap.Int("status", status), zap.Error(readErr))
	case current == removalJobCancelled:
		p.Info("项目移除工单已被重新加入取消，放弃写入终态",
			zap.Int64("job_id", job.ID), zap.Int("intended_status", status))
	default:
		p.Warn("项目移除工单租约已易主，放弃写入终态",
			zap.Int64("job_id", job.ID),
			zap.Int("status", status),
			zap.Int("current_status", current),
			zap.String("lease_owner", owner))
	}
	return false
}

// removalCancelled reports whether this job still has work: the member is
// mid-removal AND the removal in progress is the one this job was enqueued for.
//
// The second half is not redundant. `removing = 1` says a removal is in
// progress, not WHICH one, and the outbox deliberately allows several jobs per
// (project, uid) because remove → re-add → remove must enqueue a second one. A
// job that answers "there is a removal in flight, so it must be mine" is the
// same mistake the seat close made — see jobStillOwnsRemovalTx.
func (p *Project) removalCancelled(job RemovalJob, owner string) (bool, error) {
	tx, err := p.ctx.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin cascade recheck: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	member, err := p.db.lockMemberForCascadeTx(tx, job.ProjectID, job.UID)
	if err != nil {
		return false, err
	}
	if member == nil {
		// No row at all: nothing to cascade and nothing to close. Treat as
		// cancelled so the job retires instead of retrying forever.
		return true, tx.Commit()
	}
	if member.Removing == 0 {
		// Either re-admitted (status 1) or already finished (status 0). Both
		// mean this job has no work left.
		return true, tx.Commit()
	}

	mine, err := p.db.jobStillOwnsRemovalTx(tx, job.ID, owner)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit cascade recheck: %w", err)
	}
	// Not reachable today — the claim only picks up pending rows and
	// cancelPendingRemovalJobsTx retires claimed ones too, so a job that got
	// this far is ours. It is here because the alternative reading ("removing =
	// 1, therefore my work") is exactly what has to stop being assumed.
	return !mine, nil
}

// finishRemoval flips status to 0 and clears removing, under the row lock.
//
// Reports whether it actually closed the seat. `false` with no error is not a
// failure: it means the seat is not this job's to close — either the removal
// was cancelled while the steps ran, or the `removing = 1` now on the row
// belongs to a LATER removal cycle whose own job has not run yet.
func (p *Project) finishRemoval(job RemovalJob, owner string) (bool, error) {
	now := time.Now().UTC()
	tx, err := p.ctx.DB().Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin finish removal: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	member, err := p.db.lockMemberForCascadeTx(tx, job.ProjectID, job.UID)
	if err != nil {
		return false, err
	}
	if member == nil || member.Removing == 0 {
		// Cancelled while the steps ran. Commit the (empty) transaction and let
		// the caller mark the job done: the steps that did run left the member
		// out of some groups but still in the project, which is NOT an invariant
		// violation — the subset relation still holds — it is visible in the
		// member lists, and an admin can re-add. Do not "fix" this by re-adding
		// them to those groups: that would race the very admission the
		// cancellation represents.
		return false, tx.Commit()
	}

	// The seat says A removal is in flight. Whose?
	//
	// Without this the answer was assumed to be "mine", and the assumption is
	// wrong exactly once per remove → re-add → join → re-remove sequence: this
	// job would close cycle 2's seat, cycle 2's job would then find removing = 0
	// and retire without running a step, and the group joined between the cycles
	// would keep an active member row for a uid the project says is gone. I2 has
	// no read-path filter, so that uid keeps full access to it — the leak the
	// whole change exists to prevent, produced by the close itself.
	mine, err := p.db.jobStillOwnsRemovalTx(tx, job.ID, owner)
	if err != nil {
		return false, err
	}
	if !mine {
		// Leave `removing = 1` alone. The job that owns it is claimed on a later
		// tick and re-snapshots the group list, which is what picks up the group
		// joined in between.
		return false, tx.Commit()
	}

	changed, err := p.db.finishMemberRemovalTx(tx, job.ProjectID, job.UID, now)
	if err != nil {
		return false, err
	}
	if changed {
		// The epoch was already bumped when `removing` was set — that is when
		// the membership changed from every consumer's point of view. Bumping
		// again here would make one removal move the epoch twice, and the
		// acceptance requires it to move by exactly +1 per membership change.
		p.invalidateProjectMemberCache(job.ProjectID, job.UID)
	}
	return changed, tx.Commit()
}

// rescheduleAfterFailure applies backoff, or abandons a job that has run out of
// attempts.
func (p *Project) rescheduleAfterFailure(job RemovalJob, owner string, cause error) {
	now := time.Now().UTC()
	if job.Attempts >= removalMaxAttempts {
		if !p.retireRemovalJob(job, owner, removalJobAbandoned, cause.Error()) {
			// Not abandoned after all — the write was refused. retireRemovalJob
			// has already said why.
			// The write did not land, so this job is not abandoned — either it
			// errored or another worker owns it now. retireRemovalJob has already
			// said which; claiming an abandon on top of that would be a false
			// breadcrumb in the one log an operator reads after the stall alert.
			return
		}
		// Abandoned is terminal and means a member's project seat is stuck at
		// removing = 1 with group rows still in place. The counter is what an
		// alert hangs off — the brief asks for backlog AND abandoned counts, and
		// the stall scan only notices the symptom half an hour later. This log is
		// the breadcrumb beside it.
		removalAbandoned.Inc()
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
