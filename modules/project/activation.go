package project

import (
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Two-phase create (O6, docs/project-lifecycle-contract.md section 7).
//
// A project row exists from the moment create commits, but the peer control
// plane must not be able to authorize anyone into it before the subsystem side
// confirms the project has a container. Until then the two INTERNAL inbound
// endpoints answer exactly as they answer about a project that does not exist:
// epoch 0, member false.
//
// # A third surface answers about project membership and is NOT gated
//
// POST /v1/auth/verify (modules/user, answerProjectMembership) reaches
// pkg/project.MembershipsInSpace, which carries no activation term. The same
// database state therefore produces two different answers: the internal epochs
// endpoint reports the project absent while /v1/auth/verify reports member: true
// with a role.
//
// Stated here rather than left to be discovered, because this preamble used to
// read as complete peer-facing coverage and is cited as such. The gap is bounded
// by WHOSE question that endpoint answers: its uid comes from the presented token,
// never from the request body, so an unactivated project can only ever be revealed
// to somebody who already holds a seat in it — in practice its own creator, during
// the provisioning window.
//
// Gating it is a product decision rather than a correctness one, which is why it
// is not done here: the cost is that a creator's own just-created project vanishes
// from their own client until fleet confirms. Whoever takes that decision should
// also correct this section, docs/project-lifecycle-contract.md section 7, and the
// header of migration 20260908000007.
//
// The gate is octo_project.activated_at, a one-way latch. It is NOT the
// provisioning table: that table records jobs, a read path is forbidden to gate
// on it (D12), and "status = ready" there means "we succeeded once", which is
// what a latch on the project row says more directly and without the read
// crossing into job state.

// twoPhaseCreateApplies decides, AT INSERT, whether a new project must wait for
// a confirmation before the peer may see it. It is the only remaining caller of
// this question, and that is deliberate: at insert there is no row to read yet,
// so this pod's configuration is the only thing there is to ask. Everywhere the
// row already exists — both reconcile repairs — the question is answered from
// the row instead, because the answer is per project and this is not.
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
//
// # A REQUESTED-BUT-REJECTED target does NOT count as "something will answer"
//
// Absent from cfg.Targets has two causes — never configured, and configured but
// REJECTED at load (ValidateTarget failed, or checkSecretExclusivity found a
// collision; the target is dropped from Targets, recorded in Misconfigured, and
// the process boots anyway behind a loud gauge).
//
// This predicate used to treat rejected as requested, so a project created during
// that window waited. That produced a CONTRADICTION with latchUnconfirmableProjects
// ~150 lines below, and the contradiction, not the policy, is what shipped:
//
//   - a rejected target never reaches enqueueProvisioningTx, so no fleet job row is
//     written at create;
//   - latchUnconfirmableProjects' predicate is per row and reads exactly "no fleet
//     job exists";
//   - so the very next reconcile tick latched the project anyway — irreversibly,
//     with only a count-only Warn, and with the awaiting-activation gauge dropping
//     back to zero because the census counts activated_at IS NULL.
//
// The consolation this comment used to promise — "projects wait, the gauges climb,
// the operator has a gauge and a boot-time Error" — was therefore false after one
// tick. Two statements a hundred lines apart disagreed and the suite could not see
// it, because the pins created their project while fleet was WORKING (so a job row
// existed and the per-row predicate declined) and never covered a project created
// while the config was already rejected.
//
// Resolved by making the insert agree with the repair rather than the other way
// round: a rejected target means no confirmation is coming for a project created
// now, so such a project is activated at insert. The end state is identical to what
// shipped — visible to the peer, no container — minus the irreversible latch, the
// misleading Warn and the false paragraph.
//
// What that gives up, stated plainly: a secret-reuse mistake, which
// checkSecretExclusivity exists to REFUSE, now makes new projects peer-visible
// immediately rather than one tick later. The guard still refuses the credential;
// what it cannot do is hold the visibility gate shut for projects whose confirmation
// nothing will deliver.
//
// The stronger fix, NOT taken here because it changes octo_project_provisioning's
// semantics: persist "fleet was requested for this project" PER ROW even when the
// target is rejected — a job row in a non-claimable blocked status — so the repair
// can tell never-requested from requested-but-rejected, and so the blocked rows
// become claimable once the env is fixed. That recovers the window's projects
// instead of accepting them; it is the right shape if this window is ever judged to
// matter.
//
// Projects already IN FLIGHT when the config is rejected are unaffected by any of
// this: they have a job row, so the per-row predicate declines to latch them and
// they keep waiting for the env fix. TestARejectedFleetConfigDoesNotDemolishTheGate
// pins that, and it is the protection that was actually load-bearing.
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
	// sql.NullTime, never []time.Time, and reinterpreted rather than converted —
	// see utcread.go. Written the obvious way this read returned the ZERO time
	// with no error, so the gauge published about 2.5 million hours on every
	// deployment; and even once it scanned, a loc=Local DSN would have shifted it
	// by the process offset in whichever direction breaks the alert.
	var oldest []sql.NullTime
	_, err := d.session.SelectBySql(
		"SELECT created_at FROM `octo_project` "+
			"WHERE activated_at IS NULL AND status = ? ORDER BY created_at LIMIT 1",
		StatusNormal,
	).Load(&oldest)
	if err != nil {
		return 0, fmt.Errorf("project: oldest awaiting activation: %w", err)
	}
	oldestUTC, ok := firstUTCFromColumn(oldest)
	if !ok {
		return 0, nil
	}
	if age := now.UTC().Sub(oldestUTC); age > 0 {
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
	now := time.Now().UTC()

	// BOTH repairs run, every tick, and neither is gated on this pod's
	// configuration.
	//
	// The gating was the bug. Asking twoPhaseCreateApplies() here answered a
	// per-PROJECT question ("will anything ever confirm THIS row?") from a
	// per-PROCESS fact ("is fleet in this pod's config?"), while the latch it
	// drives is per row and irreversible. During a rolling enablement an old pod
	// latched a new pod's freshly created projects; during a rejected-config
	// window, projects created with no fleet job were skipped by this repair and
	// unreachable by the other one, so they were invisible to the peer forever.
	//
	// Both predicates now read the row instead: latch what has no fleet job at
	// all, repair what has one that reached ready. A project whose job exists and
	// is still working is matched by neither, which is exactly the state that
	// should keep waiting.
	// The two repairs are INDEPENDENT, so one failing must not skip the other.
	//
	// This used to return on the first error, which cost the wrong one: the second
	// repair is the one that recovers projects the peer currently reads as absent,
	// and the first is the one whose failure is benign (it latches rows nothing
	// will ever confirm — a tick late changes nothing). Different tables, no
	// shared transaction, no ordering between them; the next tick retries either
	// way, which is why this is small, but when they do not both run it is
	// reliably the expensive one that is dropped.
	latched, err := p.db.latchUnconfirmableProjects(now, p.cfg.ReconcileLimit)
	if err != nil {
		p.Warn("补置无人确认的项目激活闩锁失败", zap.Error(err))
	}
	if latched > 0 {
		// Warn, not Error: a project with no fleet job is the expected shape after
		// a rolling upgrade, or on any deployment that never enabled the target.
		// It is still logged, because a count that keeps growing after the rollout
		// window means old pods are still inserting.
		p.Warn("补置了无 fleet 工单的项目激活闩锁（没有任何东西会去确认它们）",
			zap.Int64("latched", latched))
	}

	repaired, err := p.db.repairConfirmedButUnlatched(now, p.cfg.ReconcileLimit)
	if err != nil {
		p.Warn("修复未置位的项目激活闩锁失败", zap.Error(err))
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
