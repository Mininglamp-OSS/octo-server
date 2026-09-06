package project

import (
	"fmt"
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
	for _, job := range jobs {
		p.workRemovalJob(job, owner)
	}
}

// workRemovalJob runs one job to a terminal state, or reschedules it.
func (p *Project) workRemovalJob(job RemovalJob, owner string) {
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目移除工单 panic",
				zap.Int64("job_id", job.ID),
				zap.String("project_id", job.ProjectID),
				zap.Any("recover", r))
		}
	}()

	// Heartbeat the lease for as long as this job runs.
	//
	// #797's open P1 on the Space outbox is the absence of exactly this: without
	// a heartbeat the abandon sweep marks a still-running FINAL attempt as
	// abandoned, and because abandoned is terminal the work is then never
	// retried. A project removal fans out over every group in the project, so it
	// is more likely to outrun a lease than the Space job that already does.
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
				if err := p.db.heartbeatRemovalLease(job.ID, owner, time.Now().UTC().Add(removalLease)); err != nil {
					p.Warn("续租项目移除工单失败", zap.Int64("job_id", job.ID), zap.Error(err))
				}
			}
		}
	}()
	defer func() {
		close(stop)
		beat.Wait()
	}()

	// Re-read the member row UNDER LOCK before doing anything (D4).
	//
	// The job was enqueued when removal began and may have waited in the queue.
	// A member re-admitted in that window has removing = 0, and tearing their
	// groups down now would destroy a membership that is legitimate again. Same
	// shape as P0's checkSpaceSeatForCleanupTx re-check.
	cancelled, err := p.removalCancelled(job)
	if err != nil {
		p.rescheduleAfterFailure(job, err)
		return
	}
	if cancelled {
		if err := p.db.completeRemovalJob(job.ID, removalJobCancelled, "re-admitted", time.Now().UTC()); err != nil {
			p.Error("标记项目移除工单为已取消失败", zap.Int64("job_id", job.ID), zap.Error(err))
		}
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
	var firstErr error
	for _, step := range snapshotMemberRemovalSteps() {
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
		p.rescheduleAfterFailure(job, firstErr)
		return
	}

	// Every step succeeded: close the seat for good.
	//
	// Re-checked under lock a second time, because the steps take time and a
	// re-admission can land while they run. finishMemberRemovalTx is guarded on
	// removing = 1, so a cancelled removal affects zero rows and the job is
	// retired as cancelled rather than closing a seat somebody just restored.
	if err := p.finishRemoval(job); err != nil {
		p.rescheduleAfterFailure(job, err)
		return
	}
	if err := p.db.completeRemovalJob(job.ID, removalJobDone, "", time.Now().UTC()); err != nil {
		p.Error("标记项目移除工单完成失败", zap.Int64("job_id", job.ID), zap.Error(err))
	}
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
func (p *Project) rescheduleAfterFailure(job RemovalJob, cause error) {
	now := time.Now().UTC()
	if job.Attempts >= removalMaxAttempts {
		if err := p.db.completeRemovalJob(job.ID, removalJobAbandoned, cause.Error(), now); err != nil {
			p.Error("标记项目移除工单为 abandoned 失败", zap.Int64("job_id", job.ID), zap.Error(err))
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
	if err := p.db.rescheduleRemovalJob(job.ID, now.Add(backoff), cause.Error()); err != nil {
		p.Error("重排项目移除工单失败", zap.Int64("job_id", job.ID), zap.Error(err))
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

// workerIdentity labels a lease. Not a security boundary — it exists so an
// operator can tell which pod is holding a job.
func (p *Project) workerIdentity() string {
	return fmt.Sprintf("project-removal-%d", time.Now().UnixNano())
}
