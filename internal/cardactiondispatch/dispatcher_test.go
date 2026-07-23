package cardactiondispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

type concurrentLeaseQueue struct {
	mu       sync.Mutex
	leases   []Lease
	acked    chan int64
	nacked   chan nackCall
	renewed  chan renewCall
	deferred chan deferCall
	// routeMissingSince fakes the durable first-observed-miss marker. A test may pre-seed an
	// entry to simulate an event that has already waited (→ past the window); otherwise the
	// first RouteMissingSeenAt call stamps `now`, mirroring the real HSETNX-then-read.
	routeMissingSince map[int64]time.Time
}

type nackCall struct {
	now          time.Time
	delay        time.Duration
	leaseAttempt int
	maxAttempts  int
	reason       string
}

type renewCall struct {
	eventID       int64
	token         string
	leaseDuration time.Duration
}

type deferCall struct {
	eventID int64
	token   string
	due     time.Time
}

func (q *concurrentLeaseQueue) Claim(time.Time, time.Duration) (*Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.leases) == 0 {
		return nil, nil
	}
	lease := q.leases[0]
	q.leases = q.leases[1:]
	return &lease, nil
}
func (q *concurrentLeaseQueue) Ack(eventID int64, _ string) (bool, error) {
	if q.acked != nil {
		q.acked <- eventID
	}
	return true, nil
}
func (q *concurrentLeaseQueue) Nack(lease Lease, now time.Time, delay time.Duration, maxAttempts int, reason string) (NackOutcome, error) {
	if q.nacked != nil {
		q.nacked <- nackCall{
			now: now, delay: delay, leaseAttempt: lease.Attempt,
			maxAttempts: maxAttempts, reason: reason,
		}
	}
	if lease.Attempt >= maxAttempts {
		return NackDeadLettered, nil
	}
	return NackRequeued, nil
}
func (q *concurrentLeaseQueue) Renew(eventID int64, token string, _ time.Time, leaseDuration time.Duration) (bool, error) {
	if q.renewed != nil {
		q.renewed <- renewCall{eventID: eventID, token: token, leaseDuration: leaseDuration}
	}
	return true, nil
}
func (q *concurrentLeaseQueue) Defer(eventID int64, token string, due time.Time) (bool, error) {
	if q.deferred != nil {
		q.deferred <- deferCall{eventID: eventID, token: token, due: due}
	}
	return true, nil
}
func (*concurrentLeaseQueue) ReclaimExpired(time.Time, int) (int, error) { return 0, nil }
func (q *concurrentLeaseQueue) RouteMissingSeenAt(eventID int64, now time.Time) (time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.routeMissingSince == nil {
		q.routeMissingSince = make(map[int64]time.Time)
	}
	if seen, ok := q.routeMissingSince[eventID]; ok {
		return seen, nil
	}
	q.routeMissingSince[eventID] = now
	return now, nil
}

type capturingDeliverer struct {
	owners chan string
}

type callbackDelivererFunc func(context.Context, *Route, DecisionRequest) (DecisionResult, error)

func (f callbackDelivererFunc) Deliver(ctx context.Context, route *Route, request DecisionRequest) (DecisionResult, error) {
	return f(ctx, route, request)
}

func (d *capturingDeliverer) Deliver(_ context.Context, route *Route, _ DecisionRequest) (DecisionResult, error) {
	d.owners <- route.Owner
	return DecisionResult{Disposition: DispositionApplied, State: StateApproved}, nil
}

type blockingDeliverer struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingDeliverer) Deliver(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
	d.entered <- struct{}{}
	<-d.release
	return DecisionResult{Disposition: DispositionApplied, State: StateApproved}, nil
}

type contextAwareBlockingDeliverer struct {
	entered chan struct{}
	release chan struct{}
}

type retryableDeliverer struct{}

func (*retryableDeliverer) Deliver(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
	return DecisionResult{}, &DeliveryError{Category: "consumer_5xx", retryable: true}
}

func (d *contextAwareBlockingDeliverer) Deliver(ctx context.Context, _ *Route, _ DecisionRequest) (DecisionResult, error) {
	d.entered <- struct{}{}
	select {
	case <-d.release:
		return DecisionResult{Disposition: DispositionApplied, State: StateApproved}, nil
	case <-ctx.Done():
		return DecisionResult{}, ctx.Err()
	}
}

func TestDispatcherHonorsPerRouteConcurrency(t *testing.T) {
	spec := validRouteSpec()
	spec.MaxInFlight = 2
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	queue := &concurrentLeaseQueue{}
	for i := int64(1); i <= 5; i++ {
		event := testDispatchEvent()
		event.EventID = i
		queue.leases = append(queue.leases, Lease{Event: event, Token: strconv.FormatInt(i, 10), Attempt: 1})
	}
	deliverer := &blockingDeliverer{entered: make(chan struct{}, 5), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(deliverer.release) }) }
	defer release()
	dispatcher, err := NewDispatcher(queue, registry, deliverer, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{LeaseDuration: time.Second})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	var wait sync.WaitGroup
	for i := 0; i < 5; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = dispatcher.ProcessOne(context.Background(), time.Now())
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-deliverer.entered:
		case <-time.After(time.Second):
			t.Fatal("expected two callbacks to enter")
		}
	}
	select {
	case <-deliverer.entered:
		t.Fatal("third callback exceeded route MaxInFlight=2")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	wait.Wait()
}

func TestDispatcherDefersSaturatedRouteAndProcessesOtherRoute(t *testing.T) {
	docs := validRouteSpec()
	docs.MaxInFlight = 1
	tasks := docs
	tasks.Owner = "tasks"
	tasks.ActionType = "task.decision"
	tasks.URL = "https://tasks.internal/v1/card-actions/decide"
	registry, err := NewRegistry([]RouteSpec{docs, tasks}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	docsEvent := testDispatchEvent()
	tasksEvent := testDispatchEvent()
	tasksEvent.EventID = 43
	tasksEvent.Owner = tasks.Owner
	tasksEvent.ActionType = tasks.ActionType
	queue := &concurrentLeaseQueue{
		leases: []Lease{
			{Event: docsEvent, Token: "docs-lease", Attempt: 1},
			{Event: tasksEvent, Token: "tasks-lease", Attempt: 1},
		},
		acked:    make(chan int64, 1),
		deferred: make(chan deferCall, 1),
	}
	deliverer := &capturingDeliverer{owners: make(chan string, 1)}
	dispatcher, err := NewDispatcher(queue, registry, deliverer, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	docsSlot := dispatcher.routeSlots[routeKey{senderUID: docs.SenderUID, owner: docs.Owner, actionType: docs.ActionType}]
	docsSlot <- struct{}{}
	defer func() { <-docsSlot }()
	now := time.Now().Truncate(time.Millisecond)
	dispatcher.clock = func() time.Time { return now }

	firstDone := make(chan error, 1)
	go func() {
		_, processErr := dispatcher.ProcessOne(context.Background(), now)
		firstDone <- processErr
	}()
	select {
	case processErr := <-firstDone:
		if processErr != nil {
			t.Fatalf("ProcessOne(saturated route) error = %v", processErr)
		}
	case <-time.After(time.Second):
		t.Fatal("saturated route blocked while holding a lease")
	}
	select {
	case call := <-queue.deferred:
		if call.eventID != docsEvent.EventID || call.token != "docs-lease" || !call.due.Equal(now.Add(25*time.Millisecond)) {
			t.Fatalf("Defer() call = %+v", call)
		}
	default:
		t.Fatal("saturated route lease was not deferred")
	}

	if processed, processErr := dispatcher.ProcessOne(context.Background(), now); processErr != nil || !processed {
		t.Fatalf("ProcessOne(other route) = (%v, %v), want (true, nil)", processed, processErr)
	}
	select {
	case owner := <-deliverer.owners:
		if owner != tasks.Owner {
			t.Fatalf("delivered owner = %q, want %q", owner, tasks.Owner)
		}
	default:
		t.Fatal("available route was not delivered")
	}
}

func TestDispatcherStopDoesNotWaitOnSaturatedRouteSlot(t *testing.T) {
	spec := validRouteSpec()
	spec.MaxInFlight = 1
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	event := testDispatchEvent()
	queue := &concurrentLeaseQueue{
		leases:   []Lease{{Event: event, Token: "lease-42", Attempt: 1}},
		deferred: make(chan deferCall, 1),
	}
	dispatcher, err := NewDispatcher(queue, registry, &stubDeliverer{}, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	slot := dispatcher.routeSlots[routeKey{senderUID: spec.SenderUID, owner: spec.Owner, actionType: spec.ActionType}]
	slot <- struct{}{}
	defer func() { <-slot }()
	if err := dispatcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-queue.deferred:
	case <-time.After(time.Second):
		dispatcher.Stop()
		t.Fatal("saturated route event was not deferred")
	}
	stopped := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop() waited on a saturated route slot")
	}
}

func TestDispatcherStartProcessesUpToPerRouteConcurrency(t *testing.T) {
	spec := validRouteSpec()
	spec.MaxInFlight = 2
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	queue := &concurrentLeaseQueue{}
	for i := int64(1); i <= 3; i++ {
		event := testDispatchEvent()
		event.EventID = i
		queue.leases = append(queue.leases, Lease{Event: event, Token: strconv.FormatInt(i, 10), Attempt: 1})
	}
	deliverer := &blockingDeliverer{entered: make(chan struct{}, 3), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(deliverer.release) }) }
	dispatcher, err := NewDispatcher(queue, registry, deliverer, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if err := dispatcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer dispatcher.Stop()
	defer release()

	for i := 0; i < 2; i++ {
		select {
		case <-deliverer.entered:
		case <-time.After(time.Second):
			t.Fatal("dispatcher did not fill the configured route concurrency")
		}
	}
	select {
	case <-deliverer.entered:
		t.Fatal("dispatcher exceeded route MaxInFlight=2")
	case <-time.After(20 * time.Millisecond):
	}
	release()
}

func TestDispatcherStopCompletesClaimedEvent(t *testing.T) {
	spec := validRouteSpec()
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	event := testDispatchEvent()
	queue := &concurrentLeaseQueue{
		leases: []Lease{{Event: event, Token: "lease-42", Attempt: 1}},
		acked:  make(chan int64, 1),
	}
	deliverer := &contextAwareBlockingDeliverer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(deliverer.release) }) }
	defer release()
	dispatcher, err := NewDispatcher(queue, registry, deliverer, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if err := dispatcher.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-deliverer.entered:
	case <-time.After(time.Second):
		dispatcher.Stop()
		t.Fatal("dispatcher did not claim the event")
	}

	stopped := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop() returned before the claimed event reached a durable transition")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case eventID := <-queue.acked:
		if eventID != event.EventID {
			t.Fatalf("Ack() event_id = %d, want %d", eventID, event.EventID)
		}
	case <-time.After(time.Second):
		t.Fatal("claimed event was not acknowledged during shutdown")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop() did not return after the claimed event completed")
	}
}

func TestDispatcherRenewsLeaseDuringSlowFinalization(t *testing.T) {
	spec := validRouteSpec()
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	event := testDispatchEvent()
	queue := &concurrentLeaseQueue{
		leases:  []Lease{{Event: event, Token: "lease-42", Attempt: 1}},
		acked:   make(chan int64, 1),
		renewed: make(chan renewCall, 1),
	}
	finalizeEntered := make(chan struct{}, 1)
	releaseFinalize := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFinalize) }) }
	defer release()
	finalizer := FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		finalizeEntered <- struct{}{}
		<-releaseFinalize
		return nil
	})
	leaseDuration := 60 * time.Millisecond
	dispatcher, err := NewDispatcher(queue, registry, &stubDeliverer{}, finalizer, DispatcherConfig{
		LeaseDuration: leaseDuration,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, processErr := dispatcher.ProcessOne(context.Background(), time.Now())
		done <- processErr
	}()
	select {
	case <-finalizeEntered:
	case <-time.After(time.Second):
		t.Fatal("finalizer did not start")
	}
	select {
	case call := <-queue.renewed:
		if call.eventID != event.EventID || call.token != "lease-42" || call.leaseDuration != leaseDuration {
			t.Fatalf("Renew() call = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("slow finalization did not renew its lease")
	}
	release()
	select {
	case processErr := <-done:
		if processErr != nil {
			t.Fatalf("ProcessOne() error = %v", processErr)
		}
	case <-time.After(time.Second):
		t.Fatal("ProcessOne() did not finish after finalizer release")
	}
	select {
	case eventID := <-queue.acked:
		if eventID != event.EventID {
			t.Fatalf("Ack() event_id = %d, want %d", eventID, event.EventID)
		}
	default:
		t.Fatal("finalized event was not acknowledged")
	}
}

func TestRedisQueueRenewExtendsOnlyMatchingLease(t *testing.T) {
	queue := newDispatchTestQueue(t)
	now := time.Now().Truncate(time.Millisecond)
	event := testDispatchEvent()
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, 50*time.Millisecond)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if renewed, renewErr := queue.Renew(event.EventID, "wrong-token", now.Add(40*time.Millisecond), 100*time.Millisecond); renewErr != nil || renewed {
		t.Fatalf("Renew(wrong token) = (%v, %v), want (false, nil)", renewed, renewErr)
	}
	if renewed, renewErr := queue.Renew(event.EventID, lease.Token, now.Add(40*time.Millisecond), 100*time.Millisecond); renewErr != nil || !renewed {
		t.Fatalf("Renew(valid token) = (%v, %v), want (true, nil)", renewed, renewErr)
	}
	if reclaimed, reclaimErr := queue.ReclaimExpired(now.Add(60*time.Millisecond), 10); reclaimErr != nil || reclaimed != 0 {
		t.Fatalf("ReclaimExpired(before renewed lease) = (%d, %v), want (0, nil)", reclaimed, reclaimErr)
	}
	if reclaimed, reclaimErr := queue.ReclaimExpired(now.Add(150*time.Millisecond), 10); reclaimErr != nil || reclaimed != 1 {
		t.Fatalf("ReclaimExpired(after renewed lease) = (%d, %v), want (1, nil)", reclaimed, reclaimErr)
	}
}

func TestRedisQueueDeferPreservesAttemptAndOwnership(t *testing.T) {
	queue := newDispatchTestQueue(t)
	now := time.Now().Truncate(time.Millisecond)
	event := testDispatchEvent()
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, time.Second)
	if err != nil || lease == nil || lease.Attempt != 1 {
		t.Fatalf("Claim() = (%+v, %v), want attempt 1", lease, err)
	}
	due := now.Add(100 * time.Millisecond)
	if deferred, deferErr := queue.Defer(event.EventID, "wrong-token", due); deferErr != nil || deferred {
		t.Fatalf("Defer(wrong token) = (%v, %v), want (false, nil)", deferred, deferErr)
	}
	if deferred, deferErr := queue.Defer(event.EventID, lease.Token, due); deferErr != nil || !deferred {
		t.Fatalf("Defer(valid token) = (%v, %v), want (true, nil)", deferred, deferErr)
	}
	if premature, claimErr := queue.Claim(now.Add(50*time.Millisecond), time.Second); claimErr != nil || premature != nil {
		t.Fatalf("Claim(before deferred due) = (%+v, %v), want (nil, nil)", premature, claimErr)
	}
	reclaimed, claimErr := queue.Claim(due, time.Second)
	if claimErr != nil || reclaimed == nil || reclaimed.Attempt != 1 {
		t.Fatalf("Claim(after defer) = (%+v, %v), want preserved attempt 1", reclaimed, claimErr)
	}
	if acked, ackErr := queue.Ack(event.EventID, reclaimed.Token); ackErr != nil || !acked {
		t.Fatalf("Ack() = (%v, %v), want (true, nil)", acked, ackErr)
	}
}

func TestDispatcherSchedulesRetryFromFailureTime(t *testing.T) {
	spec := validRouteSpec()
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	claimedAt := time.Unix(1_784_073_600, 0)
	failedAt := claimedAt.Add(spec.Timeout)
	queue := &concurrentLeaseQueue{
		leases: []Lease{{Event: testDispatchEvent(), Token: "lease-42", Attempt: 1}},
		nacked: make(chan nackCall, 1),
	}
	dispatcher, err := NewDispatcher(queue, registry, &retryableDeliverer{}, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	dispatcher.clock = func() time.Time { return failedAt }
	if processed, err := dispatcher.ProcessOne(context.Background(), claimedAt); err != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, err)
	}
	select {
	case call := <-queue.nacked:
		if !call.now.Equal(failedAt) {
			t.Fatalf("Nack() time = %s, want failure time %s", call.now, failedAt)
		}
		if call.delay != spec.BaseBackoff {
			t.Fatalf("Nack() delay = %s, want %s", call.delay, spec.BaseBackoff)
		}
	default:
		t.Fatal("Nack() was not called")
	}
}

func TestDispatcherDeadLettersLeaseBeyondRouteAttemptLimitWithoutDelivery(t *testing.T) {
	spec := validRouteSpec()
	spec.MaxAttempts = 2
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	queue := &concurrentLeaseQueue{
		leases: []Lease{{Event: testDispatchEvent(), Token: "lease-42", Attempt: spec.MaxAttempts + 1}},
		acked:  make(chan int64, 1),
		nacked: make(chan nackCall, 1),
	}
	var deliveryCount atomic.Int32
	deliverer := callbackDelivererFunc(func(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
		deliveryCount.Add(1)
		return DecisionResult{}, nil
	})
	var finalizeCount atomic.Int32
	dispatcher, err := NewDispatcher(queue, registry, deliverer, FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		finalizeCount.Add(1)
		return nil
	}), DispatcherConfig{LeaseDuration: time.Second})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if processed, processErr := dispatcher.ProcessOne(context.Background(), time.Now()); processErr != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, processErr)
	}
	if deliveryCount.Load() != 0 || finalizeCount.Load() != 0 {
		t.Fatalf("delivery/finalize counts = %d/%d, want 0/0", deliveryCount.Load(), finalizeCount.Load())
	}
	select {
	case call := <-queue.nacked:
		if call.leaseAttempt != spec.MaxAttempts+1 || call.maxAttempts != spec.MaxAttempts || call.reason != "attempts_exhausted" {
			t.Fatalf("Nack() call = %+v", call)
		}
	default:
		t.Fatal("exhausted lease was not dead-lettered")
	}
	select {
	case eventID := <-queue.acked:
		t.Fatalf("exhausted event %d was acknowledged", eventID)
	default:
	}
}

func TestHTTPDelivererSignsExactBodyAndDecodesTypedResult(t *testing.T) {
	var received DecisionRequest
	var signatureOK atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode raw request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(raw, &received); err != nil {
			t.Errorf("decode typed request: %v", err)
		}
		timestamp := r.Header.Get(HeaderTimestamp)
		eventID := r.Header.Get(HeaderEventID)
		if Verify(testCallbackSecret, r.Header.Get(HeaderSignature), r.Method, r.URL.EscapedPath(), timestamp, eventID, raw) {
			signatureOK.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a","display":{"title":"Roadmap"}}`))
	}))
	defer server.Close()

	registry := registryForTestServer(t, server.URL, 3)
	resolution := registry.Resolve("notification", "docs", "access_request.decision")
	deliverer := NewHTTPDeliverer(server.Client().Transport, func() time.Time {
		return time.Unix(1_784_073_600, 0)
	})
	request := DecisionRequestFromEvent(testDispatchEvent())
	result, err := deliverer.Deliver(context.Background(), resolution.Route, request)
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if !signatureOK.Load() {
		t.Fatal("callback signature did not verify against the exact body")
	}
	if received.EventID != 42 || received.Decision != "approve" || received.DocID != "doc-1" ||
		received.RequestID != "request-1" || received.OperatorUID != "user-b" {
		t.Fatalf("callback request = %+v", received)
	}
	if result.State != StateApproved || result.RequesterUID != "user-a" {
		t.Fatalf("Deliver() result = %+v", result)
	}
}

func TestHTTPDelivererNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	callbackURL := server.URL + "/start"
	registry := registryForTestServer(t, callbackURL, 1)
	deliverer := NewHTTPDeliverer(server.Client().Transport, time.Now)
	_, err := deliverer.Deliver(context.Background(), registry.Resolve("notification", "docs", "access_request.decision").Route, DecisionRequestFromEvent(testDispatchEvent()))
	if err == nil {
		t.Fatal("Deliver() error = nil for redirect response")
	}
	if redirected.Load() {
		t.Fatal("HTTP deliverer followed a callback redirect")
	}
}

func TestHTTPDelivererDefaultTransportIgnoresProxyEnvironment(t *testing.T) {
	deliverer := NewHTTPDeliverer(nil, time.Now)
	transport, ok := deliverer.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport type = %T, want *http.Transport", deliverer.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("default callback transport must ignore HTTP_PROXY/HTTPS_PROXY")
	}
}

func TestHTTPDelivererClassifiesRetryableFailures(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	registry := registryForTestServer(t, server.URL, 2)
	deliverer := NewHTTPDeliverer(server.Client().Transport, time.Now)
	_, err := deliverer.Deliver(context.Background(), registry.Resolve("notification", "docs", "access_request.decision").Route, DecisionRequestFromEvent(testDispatchEvent()))
	if err == nil || !Retryable(err) {
		t.Fatalf("Deliver(503) error = %v, want retryable", err)
	}
}

func TestDispatcherHappyPathFinalizesThenAcknowledges(t *testing.T) {
	queue := newDispatchTestQueue(t)
	event := testDispatchEvent()
	now := time.Now().Truncate(time.Millisecond)
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	var callbackCount atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callbackCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a","display":{"title":"Roadmap"}}`))
	}))
	defer server.Close()
	registry := registryForTestServer(t, server.URL, 3)

	var mu sync.Mutex
	var finalized []DecisionResult
	finalizer := FinalizerFunc(func(_ context.Context, gotEvent Event, result DecisionResult) error {
		mu.Lock()
		defer mu.Unlock()
		if gotEvent.EventID != event.EventID {
			t.Errorf("Finalize event_id = %d, want %d", gotEvent.EventID, event.EventID)
		}
		finalized = append(finalized, result)
		return nil
	})
	dispatcher, err := NewDispatcher(queue, registry, NewHTTPDeliverer(server.Client().Transport, func() time.Time { return now }), finalizer, DispatcherConfig{
		LeaseDuration: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	processed, err := dispatcher.ProcessOne(context.Background(), now)
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, err)
	}
	if callbackCount.Load() != 1 {
		t.Fatalf("callback count = %d, want 1", callbackCount.Load())
	}
	mu.Lock()
	if len(finalized) != 1 || finalized[0].State != StateApproved {
		t.Fatalf("finalized = %+v, want one approved result", finalized)
	}
	mu.Unlock()
	assertQueueDepths(t, queue, QueueDepths{})
}

func TestDispatcherRetries5xxThenMovesToDLQ(t *testing.T) {
	queue := newDispatchTestQueue(t)
	event := testDispatchEvent()
	now := time.Now().Truncate(time.Millisecond)
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	registry := registryForTestServer(t, server.URL, 2)
	dispatcher, err := NewDispatcher(queue, registry, NewHTTPDeliverer(server.Client().Transport, func() time.Time { return now }), FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		t.Fatal("finalizer called for failed callback")
		return nil
	}), DispatcherConfig{LeaseDuration: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	transitionNow := now
	dispatcher.clock = func() time.Time { return transitionNow }

	processed, err := dispatcher.ProcessOne(context.Background(), now)
	if err != nil || !processed {
		t.Fatalf("first ProcessOne() = (%v, %v), want handled retry", processed, err)
	}
	assertQueueDepths(t, queue, QueueDepths{Ready: 1})

	transitionNow = now.Add(defaultBaseBackoff)
	processed, err = dispatcher.ProcessOne(context.Background(), transitionNow)
	if err != nil || !processed {
		t.Fatalf("second ProcessOne() = (%v, %v), want handled DLQ", processed, err)
	}
	assertQueueDepths(t, queue, QueueDepths{DLQ: 1})
}

func TestDispatcherRetriesFinalizationWithoutDuplicatingQueueState(t *testing.T) {
	queue := newDispatchTestQueue(t)
	event := testDispatchEvent()
	now := time.Now().Truncate(time.Millisecond)
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	var callbackCount atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callbackCount.Add(1)
		_, _ = w.Write([]byte(`{"disposition":"replayed","state":"approved","requester_uid":"user-a","display":{"title":"Roadmap"}}`))
	}))
	defer server.Close()

	var finalizeCount atomic.Int32
	finalizer := FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		if finalizeCount.Add(1) == 1 {
			return errors.New("transient card mutation failure")
		}
		return nil
	})
	dispatcher, err := NewDispatcher(queue, registryForTestServer(t, server.URL, 3), NewHTTPDeliverer(server.Client().Transport, func() time.Time { return now }), finalizer, DispatcherConfig{LeaseDuration: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	transitionNow := now
	dispatcher.clock = func() time.Time { return transitionNow }
	if processed, err := dispatcher.ProcessOne(context.Background(), now); err != nil || !processed {
		t.Fatalf("first ProcessOne() = (%v, %v)", processed, err)
	}
	transitionNow = now.Add(defaultBaseBackoff)
	if processed, err := dispatcher.ProcessOne(context.Background(), transitionNow); err != nil || !processed {
		t.Fatalf("second ProcessOne() = (%v, %v)", processed, err)
	}
	if callbackCount.Load() != 2 || finalizeCount.Load() != 2 {
		t.Fatalf("callback/finalize counts = %d/%d, want 2/2", callbackCount.Load(), finalizeCount.Load())
	}
	assertQueueDepths(t, queue, QueueDepths{})
}

func registryForTestServer(t *testing.T, callbackURL string, maxAttempts int) *Registry {
	t.Helper()
	spec := validRouteSpec()
	spec.URL = callbackURL
	spec.MaxAttempts = maxAttempts
	registry, err := NewRegistry([]RouteSpec{spec}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry(test server) error = %v", err)
	}
	return registry
}

func testDispatchEvent() Event {
	return Event{
		EventID:     42,
		SenderUID:   "notification",
		Owner:       "docs",
		ActionType:  "access_request.decision",
		MessageID:   "1001",
		ChannelID:   "user-b",
		ChannelType: 1,
		SpaceID:     "space-1",
		ActionID:    "approve",
		OperatorUID: "user-b",
		ClientToken: "tap-1",
		ActedAt:     1_784_073_600,
		Inputs:      map[string]interface{}{},
		Data: map[string]interface{}{
			"owner":       "docs",
			"action_type": "access_request.decision",
			"decision":    "approve",
			"doc_id":      "doc-1",
			"request_id":  "request-1",
		},
	}
}

func newDispatchTestQueue(t *testing.T) *RedisQueue {
	t.Helper()
	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6379"})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	prefix := "test:card_action_dispatcher:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	queue, err := NewRedisQueue(client, QueueConfig{Prefix: prefix, LiveTTL: time.Hour, DLQRetention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewRedisQueue() error = %v", err)
	}
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})
	return queue
}

func assertQueueDepths(t *testing.T, queue *RedisQueue, want QueueDepths) {
	t.Helper()
	got, err := queue.Depths()
	if err != nil {
		t.Fatalf("Depths() error = %v", err)
	}
	if got != want {
		t.Fatalf("Depths() = %+v, want %+v", got, want)
	}
}
