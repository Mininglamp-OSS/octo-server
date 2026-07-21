package cardactiondispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

type dispatchQueue interface {
	Claim(now time.Time, leaseDuration time.Duration) (*Lease, error)
	Renew(eventID int64, token string, now time.Time, leaseDuration time.Duration) (bool, error)
	Defer(eventID int64, token string, due time.Time) (bool, error)
	Ack(eventID int64, token string) (bool, error)
	Nack(lease Lease, now time.Time, delay time.Duration, maxAttempts int, reason string) (NackOutcome, error)
	ReclaimExpired(now time.Time, limit int) (int, error)
	// RouteMissingSeenAt records (once) and returns when this event's route was first
	// observed missing at dispatch, so the bounded route-missing window is measured from
	// that point rather than from Event.ActedAt.
	RouteMissingSeenAt(eventID int64, now time.Time) (time.Time, error)
}

type callbackDeliverer interface {
	Deliver(ctx context.Context, route *Route, request DecisionRequest) (DecisionResult, error)
}

type Finalizer interface {
	Finalize(ctx context.Context, event Event, result DecisionResult) error
}

type FinalizerFunc func(context.Context, Event, DecisionResult) error

func (f FinalizerFunc) Finalize(ctx context.Context, event Event, result DecisionResult) error {
	return f(ctx, event, result)
}

type DispatcherConfig struct {
	LeaseDuration   time.Duration
	PollInterval    time.Duration
	ReclaimInterval time.Duration
	Metrics         *Metrics
	Logger          interface {
		Warn(string, ...zap.Field)
		Error(string, ...zap.Field)
	}
}

type Dispatcher struct {
	queue     dispatchQueue
	registry  *Registry
	deliverer callbackDeliverer
	finalizer Finalizer
	config    DispatcherConfig
	metrics   *Metrics
	logger    interface {
		Warn(string, ...zap.Field)
		Error(string, ...zap.Field)
	}
	routeSlots  map[routeKey]chan struct{}
	workerCount int
	clock       func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	wait   sync.WaitGroup
}

func NewDispatcher(queue dispatchQueue, registry *Registry, deliverer callbackDeliverer, finalizer Finalizer, cfg DispatcherConfig) (*Dispatcher, error) {
	if queue == nil || registry == nil || deliverer == nil || finalizer == nil {
		return nil, errors.New("cardactiondispatch: dispatcher dependencies are required")
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.ReclaimInterval == 0 {
		cfg.ReclaimInterval = 5 * time.Second
	}
	if cfg.LeaseDuration <= 0 || cfg.PollInterval <= 0 || cfg.ReclaimInterval <= 0 {
		return nil, errors.New("cardactiondispatch: dispatcher durations must be positive")
	}
	routeSlots := make(map[routeKey]chan struct{}, len(registry.routes))
	workerCount := 0
	for key, route := range registry.routes {
		routeSlots[key] = make(chan struct{}, route.MaxInFlight)
		workerCount += route.MaxInFlight
	}
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > 100 {
		workerCount = 100
	}
	return &Dispatcher{
		queue: queue, registry: registry, deliverer: deliverer, finalizer: finalizer,
		config: cfg, metrics: cfg.Metrics, logger: cfg.Logger,
		routeSlots: routeSlots, workerCount: workerCount, clock: time.Now,
	}, nil
}

// ProcessOne claims and completely handles at most one due event. Callback and
// finalization failures are converted into a durable retry/DLQ transition; an
// error is returned only when the queue state itself could not be made safe.
func (d *Dispatcher) ProcessOne(ctx context.Context, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	lease, err := d.queue.Claim(now, d.config.LeaseDuration)
	if err != nil {
		d.metrics.observeError("", "queue_error")
		return false, err
	}
	if lease == nil {
		return false, nil
	}
	owner := lease.Event.Owner
	started := time.Now()
	resultLabel := "error"
	d.metrics.beginLease(owner)
	defer func() {
		d.metrics.endLease(owner)
		d.metrics.finish(owner, resultLabel, started)
	}()
	route, ok := d.registry.Route(lease.Event.SenderUID, lease.Event.Owner, lease.Event.ActionType)
	if !ok {
		d.metrics.observeError(owner, "route_missing")
		// A missing route is TRANSIENT, not permanent: an event only reaches this
		// queue when its route existed at enqueue time (Registry.Resolve returned a
		// callback), so a miss here means the route was absent from THIS process's
		// registry at dispatch time — a rolling deploy, or a restart/redeploy that
		// came up before OCTO_CARD_ACTION_ROUTES carried the route — while the durable
		// queue outlived that window. DEFER (no attempt consumed) so the event rides
		// out the window and dispatches on its ORIGINAL attempt budget once the route
		// returns. Consuming an attempt here (a nack) would let route-absence waiting
		// exhaust route.MaxAttempts, so the event would trip the attempts_exhausted
		// gate the moment its route came back — the opposite of self-healing.
		//
		// The window is measured from the FIRST observed miss (persisted per event via
		// RouteMissingSeenAt), NOT from Event.ActedAt. An event can sit in the durable
		// queue arbitrarily long before its first dispatch attempt — a long restart /
		// outage / backlog, exactly the window this guards — and must still get the full
		// self-heal window on its first transient miss, instead of being dead-lettered
		// immediately because its acted-at is already older than the window. Only after it
		// has waited past routeMissingMaxWindow SINCE that first miss do we dead-letter it
		// as a genuine misconfiguration, preserving the DLQ breadcrumb (reason=route_missing).
		firstSeen, seenErr := d.queue.RouteMissingSeenAt(lease.Event.EventID, d.clock())
		if seenErr != nil {
			d.metrics.observeError(owner, "queue_error")
			d.refreshDepthMetrics()
			return true, seenErr
		}
		if routeMissingExpired(firstSeen, d.clock()) {
			outcome, nackErr := d.nack(*lease, d.clock(), false, lease.Attempt, "route_missing")
			resultLabel = resultForNack(outcome, nackErr)
			d.logTransition(lease, "route_missing", outcome, nackErr)
			d.refreshDepthMetrics()
			return true, nackErr
		}
		deferred, deferErr := d.queue.Defer(lease.Event.EventID, lease.Token, d.clock().Add(routeMissingDeferInterval))
		if deferErr != nil {
			d.metrics.observeError(owner, "queue_error")
			d.refreshDepthMetrics()
			return true, deferErr
		}
		if !deferred {
			d.metrics.observeError(owner, "ack_lost")
			d.refreshDepthMetrics()
			return true, errors.New("cardactiondispatch: lease ownership lost before route-missing defer")
		}
		resultLabel = "deferred"
		d.refreshDepthMetrics()
		return true, nil
	}
	if lease.Attempt > route.MaxAttempts {
		const category = "attempts_exhausted"
		d.metrics.observeError(owner, category)
		outcome, nackErr := d.queue.Nack(*lease, d.clock(), 0, route.MaxAttempts, category)
		resultLabel = resultForNack(outcome, nackErr)
		d.logTransition(lease, category, outcome, nackErr)
		d.refreshDepthMetrics()
		return true, nackErr
	}
	slot := d.routeSlots[routeKey{senderUID: route.SenderUID, owner: route.Owner, actionType: route.ActionType}]
	if slot != nil {
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			deferred, deferErr := d.queue.Defer(lease.Event.EventID, lease.Token, d.clock().Add(d.config.PollInterval))
			if deferErr != nil {
				d.metrics.observeError(owner, "queue_error")
				d.refreshDepthMetrics()
				return true, deferErr
			}
			if !deferred {
				d.metrics.observeError(owner, "ack_lost")
				d.refreshDepthMetrics()
				return true, errors.New("cardactiondispatch: lease ownership lost before capacity defer")
			}
			resultLabel = "deferred"
			d.refreshDepthMetrics()
			return true, nil
		}
	}
	// Once an event is leased, process it to a durable queue transition even if
	// shutdown begins. HTTP still has the per-route timeout, so Stop is bounded.
	workCtx := context.WithoutCancel(ctx)
	result, err := d.deliverer.Deliver(workCtx, route, DecisionRequestFromEvent(lease.Event))
	if err != nil {
		category := errorCategory(err)
		d.metrics.observeError(owner, category)
		retry := Retryable(err)
		outcome, nackErr := d.nack(*lease, d.clock(), retry, route.MaxAttempts, category)
		if outcome == NackRequeued {
			d.metrics.observeRetry(owner)
		}
		resultLabel = resultForNack(outcome, nackErr)
		d.logTransition(lease, category, outcome, nackErr)
		d.refreshDepthMetrics()
		return true, nackErr
	}
	stopHeartbeat := d.startLeaseHeartbeat(*lease)
	finalizeErr := d.finalizer.Finalize(workCtx, lease.Event, result)
	if renewErr := stopHeartbeat(); renewErr != nil {
		d.metrics.observeError(owner, "queue_error")
		d.refreshDepthMetrics()
		return true, renewErr
	}
	if finalizeErr != nil {
		category := finalizeErrorCategory(finalizeErr)
		d.metrics.observeError(owner, category)
		if category == "applicant_notify_failed" {
			d.metrics.observeApplicantNotifyFailure(owner)
		}
		outcome, nackErr := d.nack(*lease, d.clock(), true, route.MaxAttempts, category)
		if outcome == NackRequeued {
			d.metrics.observeRetry(owner)
		}
		resultLabel = resultForNack(outcome, nackErr)
		d.logTransition(lease, category, outcome, nackErr)
		d.refreshDepthMetrics()
		return true, nackErr
	}
	acked, err := d.queue.Ack(lease.Event.EventID, lease.Token)
	if err != nil {
		d.metrics.observeError(owner, "queue_error")
		return true, err
	}
	if !acked {
		d.metrics.observeError(owner, "ack_lost")
		return true, errors.New("cardactiondispatch: lease ownership lost before ack")
	}
	resultLabel = "ok"
	d.refreshDepthMetrics()
	return true, nil
}

func (d *Dispatcher) startLeaseHeartbeat(lease Lease) func() error {
	stop := make(chan struct{})
	done := make(chan error, 1)
	interval := d.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case now := <-ticker.C:
				renewed, err := d.queue.Renew(lease.Event.EventID, lease.Token, now, d.config.LeaseDuration)
				if err != nil {
					done <- err
					return
				}
				if !renewed {
					done <- errors.New("cardactiondispatch: lease ownership lost during finalization")
					return
				}
			}
		}
	}()
	return func() error {
		close(stop)
		return <-done
	}
}

func (d *Dispatcher) logTransition(lease *Lease, category string, outcome NackOutcome, err error) {
	if d.logger == nil || lease == nil {
		return
	}
	fields := []zap.Field{
		zap.Int64("event_id", lease.Event.EventID),
		zap.String("owner", metricOwner(lease.Event.Owner)),
		zap.Int("attempt", lease.Attempt),
		zap.String("category", metricErrorCategory(category)),
		zap.String("queue_outcome", string(outcome)),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	if outcome == NackDeadLettered || err != nil {
		d.logger.Error("card action dispatch failed", fields...)
		return
	}
	d.logger.Warn("card action dispatch scheduled retry", fields...)
}

func (d *Dispatcher) nack(lease Lease, now time.Time, retry bool, maxAttempts int, reason string) (NackOutcome, error) {
	delay := time.Duration(0)
	if retry {
		delay = retryBackoff(lease.Attempt, d.routeFor(lease.Event))
	} else {
		// A permanent protocol/configuration error moves to DLQ immediately.
		maxAttempts = lease.Attempt
	}
	return d.queue.Nack(lease, now, delay, maxAttempts, reason)
}

func (d *Dispatcher) routeFor(event Event) *Route {
	route, _ := d.registry.Route(event.SenderUID, event.Owner, event.ActionType)
	return route
}

func retryBackoff(attempt int, route *Route) time.Duration {
	if route == nil {
		return 0
	}
	delay := route.BaseBackoff
	for i := 1; i < attempt && delay < route.MaxBackoff; i++ {
		if delay > route.MaxBackoff/2 {
			return route.MaxBackoff
		}
		delay *= 2
	}
	if delay > route.MaxBackoff {
		return route.MaxBackoff
	}
	return delay
}

// routeMissingMaxWindow bounds how long an event may wait on a missing route before
// it is dead-lettered as a genuine misconfiguration. It is generous enough to ride
// out a rolling deploy / restart window; within it the event is DEFERRED (no attempt
// consumed), so a route that returns still delivers on its original attempt budget
// rather than tripping the attempts_exhausted gate.
const routeMissingMaxWindow = 15 * time.Minute

// routeMissingDeferInterval is how often a route-missing event is re-checked while it
// waits inside routeMissingMaxWindow. It is deliberately coarser than PollInterval so a
// long-missing route does not busy re-claim the same event every poll tick.
const routeMissingDeferInterval = 5 * time.Second

// routeMissingExpired reports whether an event whose route was first observed missing at
// firstSeen has now waited past routeMissingMaxWindow and must be dead-lettered. The window
// is anchored on the FIRST observed miss (a durable per-event marker set by RouteMissingSeenAt),
// not on Event.ActedAt, so a long queue dwell before the first dispatch attempt never robs the
// event of its self-heal window. firstSeen is always a real stamp (the marker is written on the
// first miss), so there is no "unset timestamp" edge — an event with any ActedAt, including an
// unset/legacy one, gets a full window from its first miss and can never wedge in permanent defer.
func routeMissingExpired(firstSeen time.Time, now time.Time) bool {
	return now.Sub(firstSeen) >= routeMissingMaxWindow
}

func errorCategory(err error) string {
	var deliveryErr *DeliveryError
	if errors.As(err, &deliveryErr) {
		return deliveryErr.Category
	}
	return "unknown"
}

type categorizedFinalizerError interface {
	Category() string
}

func finalizeErrorCategory(err error) string {
	var categorized categorizedFinalizerError
	if errors.As(err, &categorized) {
		return metricErrorCategory(categorized.Category())
	}
	return "finalize_failed"
}

func resultForNack(outcome NackOutcome, err error) string {
	if err != nil {
		return "error"
	}
	if outcome == NackDeadLettered {
		return "dlq"
	}
	return "retry"
}

type queueDepthReader interface {
	Depths() (QueueDepths, error)
}

func (d *Dispatcher) refreshDepthMetrics() {
	reader, ok := d.queue.(queueDepthReader)
	if !ok {
		return
	}
	depths, err := reader.Depths()
	if err != nil {
		d.metrics.observeError("", "queue_error")
		return
	}
	d.metrics.setDepths(depths.Ready, depths.DLQ)
}

func (d *Dispatcher) Start(parent context.Context) error {
	if parent == nil {
		return errors.New("cardactiondispatch: parent context is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return errors.New("cardactiondispatch: dispatcher already started")
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.wait.Add(1)
	go d.run(ctx)
	return nil
}

func (d *Dispatcher) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.cancel = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		d.wait.Wait()
	}
}

func (d *Dispatcher) run(ctx context.Context) {
	defer d.wait.Done()
	poll := time.NewTicker(d.config.PollInterval)
	reclaim := time.NewTicker(d.config.ReclaimInterval)
	defer poll.Stop()
	defer reclaim.Stop()
	var workers sync.WaitGroup
	for i := 0; i < d.workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-poll.C:
					for {
						processed, err := d.ProcessOne(ctx, now)
						if err != nil || !processed {
							break
						}
						now = time.Now()
					}
				}
			}
		}()
	}
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-reclaim.C:
			_, _ = d.queue.ReclaimExpired(now, 100)
		}
	}
}

func (d *Dispatcher) String() string {
	return fmt.Sprintf("cardactiondispatch.Dispatcher{lease=%s,poll=%s}", d.config.LeaseDuration, d.config.PollInterval)
}
