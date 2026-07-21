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

// TestRouteMissingDeadLettersAfterWindow: an event that has waited on a missing route
// past routeMissingMaxWindow is a genuine misconfiguration — dead-letter it (immediate
// DLQ, reason preserved) so it stays visible, instead of deferring forever.
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

	// Acted well past routeMissingMaxWindow ago.
	event := Event{
		EventID: 1003002, SenderUID: "notification", Owner: "docs",
		ActionType: "access_request.decision",
		ActedAt:    time.Now().Add(-routeMissingMaxWindow - time.Minute).Unix(),
	}
	queue := &concurrentLeaseQueue{
		leases:   []Lease{{Event: event, Token: "lease", Attempt: 1}},
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

// TestRouteMissingExpired covers the window boundary and the unset-timestamp guard.
func TestRouteMissingExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	windowSecs := int64(routeMissingMaxWindow / time.Second)
	cases := []struct {
		name    string
		actedAt int64
		want    bool
	}{
		{"unset acted_at is never expired", 0, false},
		{"negative acted_at is never expired", -5, false},
		{"just acted", now.Unix(), false},
		{"one second before the window", now.Unix() - windowSecs + 1, false},
		{"exactly at the window", now.Unix() - windowSecs, true},
		{"past the window", now.Unix() - windowSecs - 60, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeMissingExpired(tc.actedAt, now); got != tc.want {
				t.Fatalf("routeMissingExpired(%d, now) = %v, want %v", tc.actedAt, got, tc.want)
			}
		})
	}
}
