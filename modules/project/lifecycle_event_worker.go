package project

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The delivery worker for the project lifecycle outbox .
//
// Same shape as the member-removal cascade worker in removal_worker.go —
// leased claim, heartbeat, bounded attempts, terminal abandon, retention purge —
// because it has the same hazards. The differences are commented where they
// appear, so the two can be read side by side.

const (
	// lifecycleEventPollInterval is how often a pod looks for work.
	//
	// Faster than the reconcile scan and matched to the removal cascade, because
	// the queue is latency sensitive in the same way: until a member revocation
	// is delivered, the removed member can still execute on the peer. The peer
	// has its own periodic re-check as a backstop, but that backstop is measured
	// in tens of seconds and this queue should not be the slow half.
	lifecycleEventPollInterval = 5 * time.Second
	// lifecycleEventLease is the "worker died" timeout. The worker heartbeats, so
	// this is not a bet on how long delivery takes.
	lifecycleEventLease = 2 * time.Minute
	// lifecycleEventHeartbeatEvery must be comfortably shorter than the lease.
	lifecycleEventHeartbeatEvery = 30 * time.Second
	// lifecycleEventBatch is how many events one tick claims.
	//
	// Smaller than the removal batch: these are delivered SEQUENTIALLY and each
	// one costs a network round trip, so a large batch mostly lengthens the tail
	// that sits on a claimed lease waiting its turn.
	lifecycleEventBatch = 10
	// lifecycleEventMaxAttempts before a retryable failure is abandoned.
	//
	// Higher than the removal cascade's, because the dependency is a different
	// service rather than this one's own database: a peer deploy or a network
	// partition should not exhaust the budget. With the backoff below, this is
	// roughly half an hour of trying.
	lifecycleEventMaxAttempts = 12
	// lifecycleEventMaxBackoff caps the exponential delay.
	lifecycleEventMaxBackoff = 5 * time.Minute
	// lifecycleEventRetention is how long terminal rows are kept for forensics. The
	// audit question these answer -- did we tell the peer, and when -- is asked
	// after an incident, so the window outlives a typical investigation.
	lifecycleEventRetention  = 30 * 24 * time.Hour
	lifecycleEventPurgeBatch = 500
	lifecycleEventPurgeEvery = time.Hour
	// lifecycleEventSweepEvery is how often exhausted-but-pending rows are retired.
	lifecycleEventSweepEvery = 5 * time.Minute
)

var (
	lifecycleEventWorkerOnce sync.Once
	lifecycleEventRunning    atomic.Bool
)

// errFleetTerminal marks a refusal that must not be retried.
var errFleetTerminal = errors.New("project: fleet rejected the event permanently")

// startLifecycleEventWorker schedules delivery, the backlog gauges and the purge.
//
// Nothing starts when the integration is off or incompletely configured. That
// matters beyond saving a tick: the delivery loop, the gauges and the purge all
// query a table that a deployment which never enabled this feature may not care
// about, and an unconditional worker is how this module previously shipped three
// failing scans on every pod with no projects and no traffic.
func (p *Project) startLifecycleEventWorker() {
	if !p.lifecycleEventsEnabled() {
		if p.cfg.LifecycleEventsEnabled {
			// Asked for, but unusable. Say which half is missing — without this
			// the feature is simply inert and the next person debugs the peer.
			//
			// The problem field is included because a REFUSED secret and an
			// unset one look identical from cfg alone (both leave it empty), and
			// those two want opposite fixes: one is "set the env", the other is
			// "the env is set to a value another capability already uses".
			// Three distinguishable causes, because they want different fixes:
			// the env is unset, the env is set to a value another capability
			// already uses (LifecycleEventProblem), or the value is set but not
			// usable — a URL with a query string, a secret below the key floor.
			// The last one is what the endpoint validator now reports; before it
			// was consulted here the integration simply enqueued forever.
			p.Error("项目生命周期事件已开启但配置不可用，发件箱不会入队也不会投递",
				zap.Bool("url_set", p.cfg.LifecycleEventURL != ""),
				zap.Bool("secret_set", p.cfg.LifecycleEventSecret != ""),
				zap.NamedError("secret_collision", p.cfg.LifecycleEventProblem),
				zap.NamedError("endpoint", validateLifecycleEndpoint(
					p.cfg.LifecycleEventURL, p.cfg.LifecycleEventSecret)))
		}
		return
	}
	lifecycleEventWorkerOnce.Do(func() {
		p.ctx.Schedule(lifecycleEventPollInterval, p.runLifecycleEventDelivery)
		p.ctx.Schedule(jitter(lifecycleEventPurgeEvery), p.purgeLifecycleEvents)
		// Sparser than delivery: it only has work when a worker died mid-attempt,
		// and each pass is bounded. Jittered so replicas do not converge on it.
		p.ctx.Schedule(jitter(lifecycleEventSweepEvery), p.sweepExhaustedLifecycleEvents)
	})
}

// lifecycleSenderOrDefault returns the injected sender, or builds the real one.
//
// The seam exists so worker behaviour is testable without an HTTP server; see
// lifecycleSender. Production never sets the field.
func (p *Project) lifecycleSenderOrDefault() (lifecycleSender, error) {
	if p.lifecycleEventSender != nil {
		return p.lifecycleEventSender, nil
	}
	return newLifecycleHTTPClient(p.cfg.LifecycleEventURL, p.cfg.LifecycleEventSecret, p.cfg.LifecycleEventTimeout)
}

// runLifecycleEventDelivery claims a batch and sends it.
func (p *Project) runLifecycleEventDelivery() {
	if !lifecycleEventRunning.CompareAndSwap(false, true) {
		return // a batch is still in flight; skip rather than pile on
	}
	defer lifecycleEventRunning.Store(false)
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目生命周期事件投递 panic", zap.Any("recover", r))
		}
	}()

	now := time.Now().UTC()
	owner := p.workerIdentity()
	rows, err := p.db.claimLifecycleEvents(owner, lifecycleEventBatch, now, lifecycleEventLease)
	if err != nil {
		p.Error("认领项目生命周期事件失败", zap.Error(err))
		return
	}
	p.publishLifecycleEventBacklog()
	if len(rows) == 0 {
		return
	}

	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	stopHeartbeat := p.startLifecycleLeaseHeartbeat(ids, owner)
	defer stopHeartbeat()

	sender, err := p.lifecycleSenderOrDefault()
	if err != nil {
		// A misconfigured endpoint cannot be fixed by retrying this tick, so the
		// batch is handed back with its attempt refunded rather than burned
		// against a URL that will not become valid on its own. The enqueue gate
		// now asks the same validator (lifecycleEventsEnabled), so reaching here
		// means the configuration changed under a running process; the startup
		// log named the problem, this is the running reminder.
		p.Error("项目生命周期事件投递端点配置无效，本轮跳过", zap.Error(err))
		if relErr := p.db.releaseUnattemptedLifecycleEvents(ids, owner); relErr != nil {
			p.Warn("交还未投递的项目生命周期事件失败", zap.Error(relErr))
		}
		return
	}

	// No per-project bookkeeping here, deliberately: the CLAIM guarantees at most
	// one pending event per project (see claimLifecycleEvents), so a batch cannot
	// contain two rows of the same project and there is nothing for this loop to
	// order. An earlier version tracked a `blocked` map here instead, which held
	// inside one batch and inverted on the very next tick — the full account is on
	// the claim query, along with why a per-process map could not have worked
	// across replicas anyway.
	for _, row := range rows {
		p.deliverLifecycleEvent(sender, row, owner)
	}
}

// sweepExhaustedLifecycleEvents retires pending rows that have no attempt budget
// left, which the delivery path can no longer reach.
//
// See abandonExhaustedLifecycleEvents for why the claim-time budget predicate
// requires this: without it an exhausted row is unclaimable AND blocks its
// project's queue forever, because the claim refuses any project with an older
// pending sibling.
func (p *Project) sweepExhaustedLifecycleEvents() {
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目生命周期事件预算清扫 panic", zap.Any("recover", r))
		}
	}()
	now := time.Now().UTC()
	// Only rows this pod actually transitioned: two pods sweeping the same window
	// would otherwise both emit the abandoned counter and the Error alert for the
	// same event, and a row claimed between the SELECT and the guarded UPDATE
	// would be logged as abandoned while its new owner is mid-delivery.
	rows, err := p.db.abandonExhaustedLifecycleEvents(now,
		"exhausted: attempts spent without a terminal outcome", lifecycleEventBatch)
	if err != nil {
		p.Error("清扫预算耗尽的项目生命周期事件失败", zap.Error(err))
		return
	}
	for _, row := range rows {
		lifecycleEventOutcome.WithLabelValues(row.EventType, "abandoned", "exhausted").Inc()
		// Same severity and the same fields as the delivery-path abandon: this is
		// not a tidier outcome for having been reached by a sweep. For a member
		// revocation it still means the peer will never be told.
		p.Error("项目生命周期事件预算耗尽且无终态，已清扫为放弃；对端不会收到该变更",
			zap.Int64("id", row.ID),
			zap.String("event_id", row.EventID),
			zap.String("event_type", row.EventType),
			zap.String("project_id", row.ProjectID),
			zap.Int("attempts", row.Attempts),
			zap.Error(errFleetTerminal))
	}
}

// startLifecycleLeaseHeartbeat keeps the WHOLE claimed batch leased while the worker
// walks it.
//
// The whole batch, for the reason spelled out on heartbeatLifecycleEventLeases: the
// tail of a slow batch would otherwise expire while this worker still intends to
// send it, and another pod would claim and deliver those rows concurrently.
func (p *Project) startLifecycleLeaseHeartbeat(ids []int64, owner string) func() {
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(lifecycleEventHeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				until := time.Now().UTC().Add(lifecycleEventLease)
				if err := p.db.heartbeatLifecycleEventLeases(ids, owner, until); err != nil {
					p.Warn("续租项目生命周期事件失败", zap.Error(err))
				}
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

// deliverLifecycleEvent sends one event and records the outcome.
//
// Returns whether this event is DONE — delivered, or terminally abandoned.
//
// Production ORDERING DOES NOT REST ON THIS VALUE: runLifecycleEventDelivery
// discards it, because the claim's NOT EXISTS predicate already guarantees at
// most one pending row per project per batch, and that guarantee holds across
// replicas where a per-process decision could not. The return exists for the
// tests that drive outcomes directly. An earlier version of this comment said a
// false answer "makes the caller hold back everything queued behind it", which
// described a contract no caller implements — on a path that carries
// revocations, that is an invitation for a second call site to rely on it.
//
// Abandoned still counts as done, and that distinction is real rather than
// bookkeeping: `abandoned` is not pending, so the claim predicate stops
// declining the project and its tail becomes claimable. Nothing retries an
// abandoned row, so treating it as not-done would convert one lost event into a
// project that never receives another.
func (p *Project) deliverLifecycleEvent(sender lifecycleSender, row lifecycleEventRow, owner string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.LifecycleEventTimeout)
	defer cancel()

	res := sender.Send(ctx, row.envelope())
	lifecycleEventAttempts.WithLabelValues(row.EventType, res.Class).Inc()
	now := time.Now().UTC()

	switch {
	case res.OK:
		held, err := p.db.completeLifecycleEvent(row.ID, owner, lifecycleEventDelivered, "", now)
		if err != nil {
			// The peer HAS it; only our bookkeeping failed. Reporting "not done"
			// would hold the project's queue behind a row that will be redelivered
			// (idempotently) next tick anyway — but that is the honest answer,
			// because until the row is marked, this event is still in the queue
			// ahead of the ones behind it.
			p.Error("标记项目生命周期事件已投递失败",
				zap.Int64("id", row.ID), zap.String("event_id", row.EventID), zap.Error(err))
			return false
		}
		if !held {
			// Delivered, but this worker no longer owned the row. The peer has
			// the event either way — it is idempotent on event_id — so this is a
			// lease-hygiene warning, not a delivery failure. Done from this
			// project's ordering point of view: whoever holds the lease marks it.
			p.Warn("项目生命周期事件已投递但租约已易主",
				zap.Int64("id", row.ID), zap.String("event_id", row.EventID))
			return true
		}
		lifecycleEventOutcome.WithLabelValues(row.EventType, "delivered", lifecycleErrNone).Inc()
		return true

	case !res.Retryable:
		// Terminal refusal. Abandon immediately rather than burning the attempt
		// budget on an answer that cannot change — the budget exists to outlast a
		// transient peer, and spending it here only delays the alert.
		p.abandonLifecycleEvent(row, owner, res, now)
		return true

	case row.Attempts >= lifecycleEventMaxAttempts:
		p.abandonLifecycleEvent(row, owner, res, now)
		return true

	default:
		backoff := time.Duration(1<<uint(row.Attempts)) * time.Second
		if backoff > lifecycleEventMaxBackoff {
			backoff = lifecycleEventMaxBackoff
		}
		held, err := p.db.rescheduleLifecycleEvent(row.ID, owner, now.Add(backoff), res.Class+": "+res.Detail)
		if err != nil {
			p.Error("重排项目生命周期事件失败", zap.Int64("id", row.ID), zap.Error(err))
			return false
		}
		if !held {
			p.Warn("项目生命周期事件租约已易主，放弃重排",
				zap.Int64("id", row.ID), zap.String("lease_owner", owner))
		}
		return false
	}
}

// abandonLifecycleEvent retires a row and says loudly why.
//
// Abandoned is terminal and it is NOT a tidy outcome: for a member revocation it
// means this deployment has given up telling the peer that someone lost access,
// and nothing else in the system will retry. The log line and the counter are
// what an alert hangs off, and the event id is included because it is the key
// the peer can be queried by during reconciliation.
func (p *Project) abandonLifecycleEvent(row lifecycleEventRow, owner string, res lifecycleDeliveryResult, now time.Time) {
	held, err := p.db.completeLifecycleEvent(row.ID, owner, lifecycleEventAbandoned, res.Class+": "+res.Detail, now)
	if err != nil {
		p.Error("放弃项目生命周期事件失败", zap.Int64("id", row.ID), zap.Error(err))
		return
	}
	if !held {
		p.Warn("项目生命周期事件租约已易主，未标记放弃", zap.Int64("id", row.ID))
		return
	}
	lifecycleEventOutcome.WithLabelValues(row.EventType, "abandoned", res.Class).Inc()
	p.Error("项目生命周期事件已放弃投递，对端不会收到该变更",
		zap.Int64("id", row.ID),
		zap.String("event_id", row.EventID),
		zap.String("event_type", row.EventType),
		zap.String("project_id", row.ProjectID),
		zap.Int("attempts", row.Attempts),
		zap.String("error_class", res.Class),
		// truncateError, not res.Detail raw: Detail embeds the `code` parsed out of
		// up to 8 KiB of peer response body, and truncateError is what bounds it
		// for the database column. A log line has no column to stop it.
		zap.String("detail", truncateError(res.Detail)),
		zap.Error(errFleetTerminal))
}

// publishLifecycleEventBacklog refreshes both queue gauges.
//
// Two gauges, because a count alone cannot separate a healthy burst from a stuck
// queue. Age can, and age is what an alert should watch.
func (p *Project) publishLifecycleEventBacklog() {
	pending, err := p.db.countPendingLifecycleEvents()
	if err != nil {
		p.Warn("统计项目生命周期事件积压失败", zap.Error(err))
		return
	}
	lifecycleEventBacklog.Set(float64(pending))

	age, err := p.db.oldestPendingLifecycleEventAge(time.Now().UTC())
	if err != nil {
		p.Warn("统计项目生命周期事件最旧年龄失败", zap.Error(err))
		return
	}
	lifecycleEventOldestAgeSeconds.Set(age.Seconds())
}

// purgeLifecycleEvents drains terminal rows past the retention window.
//
// Loops until a batch comes back short rather than deleting a fixed number per
// tick: a fixed cap below the arrival rate is not a slower purge, it is no
// purge, and every scan over the table gets slower forever.
func (p *Project) purgeLifecycleEvents() {
	defer func() {
		if r := recover(); r != nil {
			p.Error("项目生命周期事件清理 panic", zap.Any("recover", r))
		}
	}()
	before := time.Now().UTC().Add(-lifecycleEventRetention)
	for {
		n, err := p.db.purgeFinishedLifecycleEvents(before, lifecycleEventPurgeBatch)
		if err != nil {
			p.Error("清理项目生命周期事件失败", zap.Error(err))
			return
		}
		if n < int64(lifecycleEventPurgeBatch) {
			return
		}
	}
}
