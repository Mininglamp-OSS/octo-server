package cardactiondispatch

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

// newRedisTestQueue builds a RedisQueue against a local Redis, skipping when none is reachable
// (matching TestRedisQueueDLQRetentionIsPerEvent). It registers cleanup of the keyspace and
// returns the client so a test can inspect low-level keys (e.g. the route_missing_since hash).
func newRedisTestQueue(t *testing.T, name string) (*RedisQueue, *rd.Client) {
	t.Helper()
	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6379"})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prefix := fmt.Sprintf("test:%s:%d", name, time.Now().UnixNano())
	queue, err := NewRedisQueue(client, QueueConfig{Prefix: prefix, LiveTTL: time.Hour, DLQRetention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewRedisQueue() error = %v", err)
	}
	t.Cleanup(func() {
		if keys, _ := client.Keys(prefix + "*").Result(); len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
	})
	return queue, client
}

// TestReplayDLQPastRetentionIsNonDestructive pins the fix for the CLI replay data-loss path:
// replaying an entry older than the resolved retention REFUSES it (returns false) but must NOT
// delete it — the running server's Depths() prune is the single pruning authority. Otherwise a
// CLI whose retention is shorter than the server's would silently destroy a recoverable entry.
func TestReplayDLQPastRetentionIsNonDestructive(t *testing.T) {
	queue, _ := newRedisTestQueue(t, "card_action_replay_nondestructive")
	now := time.Now().Truncate(time.Millisecond)
	event := testDispatchEvent()
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if outcome, err := queue.Nack(*lease, now, time.Hour, 1, "forced"); err != nil || outcome != NackDeadLettered {
		t.Fatalf("Nack() = (%q, %v), want dead_lettered", outcome, err)
	}

	// A replay whose due is past the retention window judges the entry expired and refuses it,
	// but must leave it in the DLQ.
	if replayed, err := queue.ReplayDLQ(event.EventID, now.Add(30*24*time.Hour+time.Millisecond)); err != nil || replayed {
		t.Fatalf("ReplayDLQ(past retention) = (%v, %v), want (false, nil)", replayed, err)
	}
	depths, err := queue.DepthsNoPrune()
	if err != nil {
		t.Fatalf("DepthsNoPrune() error = %v", err)
	}
	if depths.DLQ != 1 {
		t.Fatalf("DLQ depth after a refused replay = %d, want 1 (the entry must survive, not be deleted)", depths.DLQ)
	}

	// Proof the entry really survived: a within-window replay now succeeds.
	if replayed, err := queue.ReplayDLQ(event.EventID, now.Add(time.Hour)); err != nil || !replayed {
		t.Fatalf("ReplayDLQ(within window) = (%v, %v), want (true, nil)", replayed, err)
	}
}

// TestRouteMissingSeenAtAnchorsOnFirstMiss pins the first-observed-miss marker: it stamps once
// and returns that same time on later calls (so the bounded window is measured from the first
// miss, not from a moving now), and ReplayDLQ clears it so a replayed event starts fresh.
func TestRouteMissingSeenAtAnchorsOnFirstMiss(t *testing.T) {
	queue, _ := newRedisTestQueue(t, "card_action_first_miss")
	event := testDispatchEvent()
	first := time.Now().Truncate(time.Millisecond)

	seen1, err := queue.RouteMissingSeenAt(event.EventID, first)
	if err != nil {
		t.Fatalf("RouteMissingSeenAt(first) error = %v", err)
	}
	if !seen1.Equal(first) {
		t.Fatalf("first RouteMissingSeenAt = %v, want %v", seen1, first)
	}
	// A later re-check must return the FIRST stamp — the window does not slide forward.
	seen2, err := queue.RouteMissingSeenAt(event.EventID, first.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("RouteMissingSeenAt(second) error = %v", err)
	}
	if !seen2.Equal(first) {
		t.Fatalf("second RouteMissingSeenAt = %v, want the first stamp %v (window must anchor on first miss)", seen2, first)
	}

	// DLQ the event, then replay it: the marker must be cleared so a replayed event gets a
	// fresh window rather than inheriting its pre-DLQ first-miss time.
	if err := queue.Enqueue(event, first); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(first, time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if outcome, err := queue.Nack(*lease, first, time.Hour, 1, "forced"); err != nil || outcome != NackDeadLettered {
		t.Fatalf("Nack() = (%q, %v), want dead_lettered", outcome, err)
	}
	if replayed, err := queue.ReplayDLQ(event.EventID, first.Add(time.Minute)); err != nil || !replayed {
		t.Fatalf("ReplayDLQ() = (%v, %v), want (true, nil)", replayed, err)
	}

	fresh := first.Add(time.Hour)
	seen3, err := queue.RouteMissingSeenAt(event.EventID, fresh)
	if err != nil {
		t.Fatalf("RouteMissingSeenAt(after replay) error = %v", err)
	}
	if !seen3.Equal(fresh) {
		t.Fatalf("RouteMissingSeenAt after replay = %v, want fresh %v (marker must be cleared on replay)", seen3, fresh)
	}
}

// TestRouteMissingMarkerClearedOnTerminalTransitions pins the marker lifecycle: the
// route_missing_since field for an event must be removed on successful delivery (Ack) and on
// terminal dead-letter (Nack). Otherwise the shared hash accumulates a field per completed event
// under sustained route-missing traffic — a whole-hash TTL cannot expire individual fields, and
// every new miss refreshes it, so the key never sheds orphaned fields (the leak Jerry-Xin caught).
func TestRouteMissingMarkerClearedOnTerminalTransitions(t *testing.T) {
	queue, client := newRedisTestQueue(t, "card_action_marker_lifecycle")
	event := testDispatchEvent()
	field := strconv.FormatInt(event.EventID, 10)
	now := time.Now().Truncate(time.Millisecond)

	markerExists := func() bool {
		exists, err := client.HExists(queue.keys.routeMissingSince, field).Result()
		if err != nil {
			t.Fatalf("HExists() error = %v", err)
		}
		return exists
	}

	// Delivered path: marker present after the first miss, gone after Ack.
	if _, err := queue.RouteMissingSeenAt(event.EventID, now); err != nil {
		t.Fatalf("RouteMissingSeenAt() error = %v", err)
	}
	if !markerExists() {
		t.Fatal("marker missing right after RouteMissingSeenAt")
	}
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if acked, err := queue.Ack(event.EventID, lease.Token); err != nil || !acked {
		t.Fatalf("Ack() = (%v, %v), want (true, nil)", acked, err)
	}
	if markerExists() {
		t.Fatal("route-missing marker still present after Ack; it must be cleared on delivery")
	}

	// Dead-letter path: marker re-created on a fresh miss, gone after terminal Nack.
	if _, err := queue.RouteMissingSeenAt(event.EventID, now); err != nil {
		t.Fatalf("RouteMissingSeenAt() error = %v", err)
	}
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err = queue.Claim(now, time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if outcome, err := queue.Nack(*lease, now, time.Hour, 1, "route_missing"); err != nil || outcome != NackDeadLettered {
		t.Fatalf("Nack() = (%q, %v), want dead_lettered", outcome, err)
	}
	if markerExists() {
		t.Fatal("route-missing marker still present after terminal dead-letter; it must be cleared on DLQ")
	}
}

// TestReclaimRefundsRouteMissingDeferAttemptLeak pins the fix for the claim->Defer attempt leak.
// claimScript speculatively increments the attempt on every claim and only a completed Defer
// restores it, so a crash / Redis error / lease expiry BETWEEN claim and Defer during a
// route-missing self-heal cycle would strand that +1: ReclaimExpired requeues the event but the
// increment was never undone, and across cycles the counter creeps up until the event trips
// attempts_exhausted on a route it never got to try. ReclaimExpired now refunds the increment for a
// reclaimed lease that still carries a route_missing_since marker (the durable signal that the event
// is in the defer loop), making a reclaimed defer cycle net-zero — while leaving DELIVERY leases
// (no marker) counting their attempt, since that bounds real delivery retries.
func TestReclaimRefundsRouteMissingDeferAttemptLeak(t *testing.T) {
	t.Run("route-missing deferred lease is refunded on reclaim", func(t *testing.T) {
		queue, client := newRedisTestQueue(t, "card_action_reclaim_refund")
		event := testDispatchEvent()
		event.EventID = 6230001
		field := strconv.FormatInt(event.EventID, 10)
		now := time.Now().Truncate(time.Millisecond)

		if err := queue.Enqueue(event, now); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		lease, err := queue.Claim(now, time.Second)
		if err != nil || lease == nil || lease.Attempt != 1 {
			t.Fatalf("Claim() = (%+v, %v), want attempt 1", lease, err)
		}
		// The dispatcher stamps the first-miss marker before it would Defer. Simulate a crash in the
		// window AFTER that stamp but BEFORE Defer: the marker is set, the lease is never released.
		if _, err := queue.RouteMissingSeenAt(event.EventID, now); err != nil {
			t.Fatalf("RouteMissingSeenAt() error = %v", err)
		}

		// The lease (1s) has expired by now+2s; reclaim requeues it and, seeing the marker, refunds
		// the orphaned +1.
		if reclaimed, err := queue.ReclaimExpired(now.Add(2*time.Second), 10); err != nil || reclaimed != 1 {
			t.Fatalf("ReclaimExpired() = (%d, %v), want (1, nil)", reclaimed, err)
		}
		attempts, err := client.HGet(queue.keys.attempts, field).Int()
		if err != nil {
			t.Fatalf("HGet(attempts) error = %v", err)
		}
		if attempts != 0 {
			t.Fatalf("attempts after reclaim = %d, want 0 (the crashed claim's +1 must be refunded)", attempts)
		}

		// The next claim re-consumes exactly one attempt, so the event is back to attempt 1 — NOT 2.
		// Without the refund it would read attempt 2, and enough such cycles trip attempts_exhausted.
		lease, err = queue.Claim(now.Add(2*time.Second), time.Second)
		if err != nil || lease == nil {
			t.Fatalf("re-Claim() = (%+v, %v)", lease, err)
		}
		if lease.Attempt != 1 {
			t.Fatalf("attempt after reclaiming a route-missing deferred lease = %d, want 1 (the defer cycle must be net-zero even across a crash)", lease.Attempt)
		}
	})

	t.Run("delivery lease without a marker keeps its attempt on reclaim", func(t *testing.T) {
		queue, _ := newRedisTestQueue(t, "card_action_reclaim_keeps")
		event := testDispatchEvent()
		event.EventID = 6230002
		now := time.Now().Truncate(time.Millisecond)

		if err := queue.Enqueue(event, now); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		lease, err := queue.Claim(now, time.Second)
		if err != nil || lease == nil || lease.Attempt != 1 {
			t.Fatalf("Claim() = (%+v, %v), want attempt 1", lease, err)
		}
		// No RouteMissingSeenAt: this is an ordinary delivery lease whose worker crashed. Its attempt
		// legitimately counts a real delivery try and must survive reclaim so MaxAttempts still bounds
		// retries (mirrors the pinned TestRedisQueueLeaseTokenRetryDLQAndReplay contract).
		if reclaimed, err := queue.ReclaimExpired(now.Add(2*time.Second), 10); err != nil || reclaimed != 1 {
			t.Fatalf("ReclaimExpired() = (%d, %v), want (1, nil)", reclaimed, err)
		}
		lease, err = queue.Claim(now.Add(2*time.Second), time.Second)
		if err != nil || lease == nil {
			t.Fatalf("re-Claim() = (%+v, %v)", lease, err)
		}
		if lease.Attempt != 2 {
			t.Fatalf("attempt after reclaiming a markerless delivery lease = %d, want 2 (delivery retries must still be bounded)", lease.Attempt)
		}
	})
}

// TestClearRouteMissingRestoresDeliveryReclaimBound pins the fix for the marker-discriminator gap:
// the route_missing_since marker is cleared only on Ack/Nack/replay, so a once-route-missing event
// whose route RETURNS still carries the marker while it delivers. Without clearing it on route
// resolution, a hard crash during that delivery (no Ack/Nack) would have ReclaimExpired refund the
// attempt — asymmetrically exempting the event from the MaxAttempts bound. ClearRouteMissing drops
// the marker (token-protected) the moment the route resolves, so a delivery-phase crash reclaims as
// a real attempt (the attempt advances) like any delivery lease.
func TestClearRouteMissingRestoresDeliveryReclaimBound(t *testing.T) {
	queue, client := newRedisTestQueue(t, "card_action_clear_marker")
	event := testDispatchEvent()
	event.EventID = 6230003
	field := strconv.FormatInt(event.EventID, 10)
	now := time.Now().Truncate(time.Millisecond)

	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, time.Second)
	if err != nil || lease == nil || lease.Attempt != 1 {
		t.Fatalf("Claim() = (%+v, %v), want attempt 1", lease, err)
	}
	// The event was route-missing (marker set); then its route returned.
	if _, err := queue.RouteMissingSeenAt(event.EventID, now); err != nil {
		t.Fatalf("RouteMissingSeenAt() error = %v", err)
	}

	// A stale worker (wrong token) must NOT be able to clear the marker.
	if cleared, err := queue.ClearRouteMissing(event.EventID, "wrong-token"); err != nil || cleared {
		t.Fatalf("ClearRouteMissing(wrong token) = (%v, %v), want (false, nil)", cleared, err)
	}
	if exists, _ := client.HExists(queue.keys.routeMissingSince, field).Result(); !exists {
		t.Fatal("marker cleared by a non-owner; the clear must be token-protected")
	}

	// The lease owner clears it on route resolution.
	if cleared, err := queue.ClearRouteMissing(event.EventID, lease.Token); err != nil || !cleared {
		t.Fatalf("ClearRouteMissing(owner) = (%v, %v), want (true, nil)", cleared, err)
	}
	if exists, _ := client.HExists(queue.keys.routeMissingSince, field).Result(); exists {
		t.Fatal("marker still present after ClearRouteMissing by the lease owner")
	}

	// Simulate a hard crash during delivery: the lease (1s) expires and is reclaimed. With the marker
	// gone the event is treated as a real delivery attempt, so the attempt ADVANCES to 2 (bound
	// preserved) instead of being refunded to 1.
	if reclaimed, err := queue.ReclaimExpired(now.Add(2*time.Second), 10); err != nil || reclaimed != 1 {
		t.Fatalf("ReclaimExpired() = (%d, %v), want (1, nil)", reclaimed, err)
	}
	lease, err = queue.Claim(now.Add(2*time.Second), time.Second)
	if err != nil || lease == nil {
		t.Fatalf("re-Claim() = (%+v, %v)", lease, err)
	}
	if lease.Attempt != 2 {
		t.Fatalf("attempt after reclaiming a delivery lease whose route-missing marker was cleared = %d, want 2 (the MaxAttempts bound must be preserved once the route returns)", lease.Attempt)
	}
}
