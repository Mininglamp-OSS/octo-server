package cardactiondispatch

import (
	"context"
	"testing"
	"time"
)

// TestRouteMissingDefersWithoutConsumingAttempt pins the hardened route-missing
// behavior. An event only reaches this queue when its route existed at enqueue time,
// so a miss at dispatch means the route was absent from THIS process's registry then
// (a rolling deploy / restart that came up before OCTO_CARD_ACTION_ROUTES loaded it,
// with the durable queue outliving that window). Within routeMissingMaxWindow the event
// must be DEFERRED — not nacked — so it waits out the window and dispatches on its
// ORIGINAL attempt budget once the route returns, instead of burning attempts (which
// would trip attempts_exhausted the moment the route came back). Deliver/Finalize never
// run while the route is missing.
func TestRouteMissingDefersWithoutConsumingAttempt(t *testing.T) {
	registry, err := NewRegistry(nil, testGetenv) // deliberately WITHOUT the docs route
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	neverDeliver := callbackDelivererFunc(func(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
		t.Fatal("Deliver must not run when the route is missing (docs-backend is never contacted)")
		return DecisionResult{}, nil
	})
	neverFinalize := FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		t.Fatal("Finalize must not run when the route is missing (the card is never rewritten)")
		return nil
	})

	// Fresh event (acted just now) → well inside routeMissingMaxWindow.
	event := Event{
		EventID: 1003002, SenderUID: "notification", Owner: "docs",
		ActionType: "access_request.decision", ActedAt: time.Now().Unix(),
	}
	queue := &concurrentLeaseQueue{
		leases:   []Lease{{Event: event, Token: "lease-1003002", Attempt: 1}},
		nacked:   make(chan nackCall, 1),
		deferred: make(chan deferCall, 1),
	}
	dispatcher, err := NewDispatcher(queue, registry, neverDeliver, neverFinalize, DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	processed, procErr := dispatcher.ProcessOne(context.Background(), time.Now())
	if !processed || procErr != nil {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, procErr)
	}

	// Must DEFER (no attempt consumed), not nack/dead-letter.
	select {
	case dc := <-queue.deferred:
		if dc.eventID != event.EventID || dc.token != "lease-1003002" {
			t.Fatalf("defer targeted %+v, want event %d token lease-1003002", dc, event.EventID)
		}
	default:
		t.Fatal("route-missing event was not deferred (attempt would be consumed by a nack)")
	}
	select {
	case nc := <-queue.nacked:
		t.Fatalf("route-missing event was nacked (%+v); a nack consumes an attempt and can trip attempts_exhausted when the route returns", nc)
	default:
	}
}

// TestRouteMissingDeadLettersAfterWindow: an event whose route was FIRST observed missing
// more than routeMissingMaxWindow ago is a genuine misconfiguration — dead-letter it (immediate
// DLQ, reason preserved) so it stays visible, instead of deferring forever. The window is
// anchored on the first-observed miss (pre-seeded here), not on Event.ActedAt.
func TestRouteMissingDeadLettersAfterWindow(t *testing.T) {
	registry, err := NewRegistry(nil, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	neverDeliver := callbackDelivererFunc(func(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
		t.Fatal("Deliver must not run when the route is missing")
		return DecisionResult{}, nil
	})
	neverFinalize := FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		t.Fatal("Finalize must not run when the route is missing")
		return nil
	})

	event := Event{
		EventID: 1003002, SenderUID: "notification", Owner: "docs",
		ActionType: "access_request.decision", ActedAt: time.Now().Unix(),
	}
	queue := &concurrentLeaseQueue{
		leases:   []Lease{{Event: event, Token: "lease", Attempt: 1}},
		nacked:   make(chan nackCall, 1),
		deferred: make(chan deferCall, 1),
		// Route first observed missing well past the window → dead-letter on this claim.
		routeMissingSince: map[int64]time.Time{
			event.EventID: time.Now().Add(-routeMissingMaxWindow - time.Minute),
		},
	}
	dispatcher, err := NewDispatcher(queue, registry, neverDeliver, neverFinalize, DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	processed, procErr := dispatcher.ProcessOne(context.Background(), time.Now())
	if !processed || procErr != nil {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, procErr)
	}

	select {
	case dc := <-queue.deferred:
		t.Fatalf("expired route-missing event was deferred (%+v); want a dead-letter after the window", dc)
	default:
	}
	select {
	case nc := <-queue.nacked:
		if nc.reason != "route_missing" {
			t.Fatalf("nack reason = %q, want route_missing", nc.reason)
		}
		if nc.leaseAttempt < nc.maxAttempts {
			t.Fatalf("leaseAttempt=%d < maxAttempts=%d; want an immediate dead-letter", nc.leaseAttempt, nc.maxAttempts)
		}
	default:
		t.Fatal("expired route-missing event was neither deferred nor dead-lettered")
	}
}

// TestRouteMissingOldActedAtDefersOnFirstMiss proves the window is anchored on the FIRST
// observed miss, not on Event.ActedAt. An event that sat in the durable queue far longer than
// routeMissingMaxWindow before its first dispatch attempt — a long restart / outage / backlog,
// exactly the window this guards — must still DEFER on its first transient miss and get the full
// self-heal window, not be dead-lettered immediately because its acted-at is already old. (An
// unset ActedAt=0 is just an extreme of the same case: with no marker yet, the first miss stamps
// now and the event defers — so it can never wedge in a permanent defer loop either.)
func TestRouteMissingOldActedAtDefersOnFirstMiss(t *testing.T) {
	registry, err := NewRegistry(nil, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	neverDeliver := callbackDelivererFunc(func(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
		t.Fatal("Deliver must not run when the route is missing")
		return DecisionResult{}, nil
	})
	neverFinalize := FinalizerFunc(func(context.Context, Event, DecisionResult) error {
		t.Fatal("Finalize must not run when the route is missing")
		return nil
	})

	// Acted (and enqueued) an hour ago, far past routeMissingMaxWindow, but this is its FIRST
	// route miss — no first-seen marker yet, so the window starts now.
	event := Event{
		EventID: 1003002, SenderUID: "notification", Owner: "docs",
		ActionType: "access_request.decision",
		ActedAt:    time.Now().Add(-time.Hour).Unix(),
	}
	queue := &concurrentLeaseQueue{
		leases:   []Lease{{Event: event, Token: "lease", Attempt: 1}},
		nacked:   make(chan nackCall, 1),
		deferred: make(chan deferCall, 1),
		// No routeMissingSince entry → first miss stamps now → full window ahead.
	}
	dispatcher, err := NewDispatcher(queue, registry, neverDeliver, neverFinalize, DispatcherConfig{
		LeaseDuration: time.Second,
		PollInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	processed, procErr := dispatcher.ProcessOne(context.Background(), time.Now())
	if !processed || procErr != nil {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, procErr)
	}

	// Must DEFER on its first miss despite the hour-old ActedAt.
	select {
	case dc := <-queue.deferred:
		if dc.eventID != event.EventID {
			t.Fatalf("defer targeted %+v, want event %d", dc, event.EventID)
		}
	default:
		t.Fatal("old-ActedAt event on its FIRST route miss was not deferred; the window must anchor on the first observed miss, not ActedAt")
	}
	select {
	case nc := <-queue.nacked:
		t.Fatalf("old-ActedAt event was dead-lettered on its first miss (%+v); the window must anchor on the first observed miss, not ActedAt", nc)
	default:
	}
}

// TestRouteMissingExpired covers the window boundary. The window is anchored on the first
// observed miss (firstSeen), so it is a pure elapsed-time check with no unset-timestamp edge.
func TestRouteMissingExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name      string
		firstSeen time.Time
		want      bool
	}{
		{"just observed missing", now, false},
		{"one second before the window", now.Add(-routeMissingMaxWindow + time.Second), false},
		{"exactly at the window", now.Add(-routeMissingMaxWindow), true},
		{"past the window", now.Add(-routeMissingMaxWindow - time.Minute), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeMissingExpired(tc.firstSeen, now); got != tc.want {
				t.Fatalf("routeMissingExpired(%v, now) = %v, want %v", tc.firstSeen, got, tc.want)
			}
		})
	}
}
