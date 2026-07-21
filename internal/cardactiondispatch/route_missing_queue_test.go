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
