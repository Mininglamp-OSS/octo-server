package project

import (
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Two-phase create (O6, docs/project-lifecycle-contract.md section 7).
//
// A project row exists from the moment create commits, but the peer control
// plane must not be able to authorize anyone into it before the subsystem side
// confirms the project has a container. Until then the two inbound endpoints
// answer exactly as they answer about a project that does not exist: epoch 0,
// member false.
//
// The gate is octo_project.activated_at, a one-way latch. It is NOT the
// provisioning table: that table records jobs, a read path is forbidden to gate
// on it (D12), and "status = ready" there means "we succeeded once", which is
// what a latch on the project row says more directly and without the read
// crossing into job state.

// twoPhaseCreateApplies reports whether a new project must wait for a
// confirmation before the peer may see it.
//
// It is a question about whether anything will ever ANSWER, not about whether
// the feature is desirable. The fleet target is the confirming step, so with it
// off nothing would ever set the latch and every new project would be invisible
// to the peer forever — a far worse failure than the early visibility the latch
// exists to prevent.
//
// Deliberately not its own env switch. A switch could be turned on while the
// target stayed off, which is exactly the state that strands projects, and
// there is no operational reason to want the wait without the thing waited on.
func (p *Project) twoPhaseCreateApplies() bool {
	_, ok := p.cfg.Provisioning.TargetByName(TargetFleet)
	return ok
}

// activateProject sets the latch, once.
//
// On the session rather than in a transaction: one idempotent UPDATE whose WHERE
// clause IS the compare-and-set, so there is nothing for a transaction to make
// atomic that the statement does not already.
//
// Guarded on activated_at IS NULL rather than blindly written, so a redelivered
// or retried confirmation cannot move the timestamp forward. The value is
// "when we were first told", and a later write would report the retry instead
// of the fact.
//
// Not guarded on status: a project disbanded between the create and the
// confirmation still gets its latch, because the latch records what the
// subsystem side said, and every read path already excludes a disbanded project
// on status. Guarding on status here would instead leave a permanently
// unlatched row that the stuck-in-provisioning census would report forever.
func (d *DB) activateProject(projectID string, now time.Time) (bool, error) {
	result, err := d.session.UpdateBySql(
		"UPDATE `octo_project` SET activated_at = ? WHERE project_id = ? AND activated_at IS NULL",
		now, projectID,
	).Exec()
	if err != nil {
		return false, fmt.Errorf("project: activate %s: %w", projectID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("project: activate %s rows: %w", projectID, err)
	}
	return affected > 0, nil
}

// countAwaitingActivation is the census behind the stuck-in-provisioning gauge.
//
// Bounded by status: a disbanded project that never activated is not stuck, it
// is finished, and counting it would make the gauge climb forever on a number
// nobody can act on.
func (d *DB) countAwaitingActivation() (int64, error) {
	var count int64
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` WHERE activated_at IS NULL AND status = ?",
		StatusNormal,
	).LoadOne(&count)
	if err != nil {
		return 0, fmt.Errorf("project: count awaiting activation: %w", err)
	}
	return count, nil
}

// oldestAwaitingActivationAge answers the question a count cannot: is this a
// burst of fresh creates or a queue that stopped moving. Age is what an alert
// should watch.
func (d *DB) oldestAwaitingActivationAge(now time.Time) (time.Duration, error) {
	var oldest []time.Time
	_, err := d.session.SelectBySql(
		"SELECT created_at FROM `octo_project` "+
			"WHERE activated_at IS NULL AND status = ? ORDER BY created_at LIMIT 1",
		StatusNormal,
	).Load(&oldest)
	if err != nil {
		return 0, fmt.Errorf("project: oldest awaiting activation: %w", err)
	}
	if len(oldest) == 0 {
		return 0, nil
	}
	if age := now.Sub(oldest[0]); age > 0 {
		return age, nil
	}
	return 0, nil
}

// confirmProjectActive is the worker-side half: a fleet ensure came back clean,
// so the project may now be seen by the peer.
//
// WHAT THE CONFIRMATION ACTUALLY CHECKED. The contract states the check as
// "workspace_id == project_id". What internal/projectprovision enforces today is
// that the id the target echoed equals the id we asked it to create
// (Ensure refuses the response otherwise, permanently and loudly). Those are the
// same check: the id we ask fleet to create IS its workspace id. They are not
// yet the same VALUE, because the fleet container id is still the opaque random
// form PR #850 shipped, which the task brief records as temporary — it becomes
// project_id once fleet lands the authorization narrowing that makes an
// unguessable id unnecessary. When that happens this call site does not change;
// the ids simply become equal and the echo check reads literally as the
// contract states it.
//
// Not fatal to the job. The ensure succeeded and the row is already being marked
// ready by the caller; failing here would retry a call the target has already
// performed. What a failure costs is a project that stays invisible to the peer
// until the next attempt or a human — which the awaiting-activation gauge is
// there to show, so it is logged at Error and left visible rather than swallowed.
func (p *Project) confirmProjectActive(projectID, target string) {
	if target != TargetFleet {
		// Only fleet confirms. drive provisioning is storage and says nothing
		// about whether the peer control plane may act on the project.
		return
	}
	latched, err := p.db.activateProject(projectID, time.Now().UTC())
	if err != nil {
		p.Error("项目两阶段创建：确认后置激活失败，该项目对对端仍不可见",
			zap.String("projectId", projectID), zap.Error(err))
		return
	}
	if latched {
		p.Info("项目两阶段创建已完成：容器已确认，对端可见",
			zap.String("projectId", projectID))
	}
}

// scanUnlatchedActivations is the reconcile half of the two-phase create latch.
//
// Scheduled rather than reactive, because the state it repairs is only reachable
// when the reactive path did NOT run: the pod died between marking the job ready
// and setting the latch, or the latch UPDATE itself failed. Both leave a project
// whose container exists but which the peer answers about as nonexistent — and
// the provisioning job is terminal, so nothing else will ever come back to it.
//
// Runs unconditionally, outside the reconcile enablement gate, for the reason
// scanEpochSanity does: it touches only this module's own tables, so the
// collation drift the gate exists for cannot reach it — and a repair that
// defaults to "no monitoring" is the failure that gate was itself narrowed for.
func (p *Project) scanUnlatchedActivations() {
	repaired, err := p.db.repairConfirmedButUnlatched(time.Now().UTC(), p.cfg.ReconcileLimit)
	if err != nil {
		p.Warn("修复未置位的项目激活闩锁失败", zap.Error(err))
		return
	}
	if len(repaired) > 0 {
		// Error, not Info: reaching here means the reactive latch did not run,
		// and every one of these projects was invisible to the peer for at least
		// one reconcile interval — its members denied on the peer side the whole
		// time. The repair is the right outcome; needing it is not.
		p.Error("修复了容器已确认但未置位激活闩锁的项目（此前对对端读作不存在）",
			zap.Int("repaired", len(repaired)),
			zap.Strings("projectIds", repaired))
	}
}
