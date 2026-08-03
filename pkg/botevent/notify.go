package botevent

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// Producers ring through Notify, never through Ring.
//
// # Why the ring may not be synchronous
//
// The highest-volume producer is saveRobotMessage, and it does not run on a
// goroutine of its own budget: modules/robot's message listener does a
// **blocking** `msgSem <- struct{}{}` on the listener goroutine itself before
// spawning the worker, with a capacity of 100. So anything that makes
// saveRobotMessage slower holds a msgSem slot longer, and once all 100 are held
// the listener blocks and bot message fan-out stops **process-wide, for every
// bot** — including bots that never long-poll.
//
// A synchronous ring puts a network call in that slot whose tail is set by a
// separate, small, possibly-cold Redis pool. Even with short timeouts that is
// seconds per message against a degraded Redis, which is exactly the input that
// turns a bounded semaphore into a full stall. The cost/benefit is also entirely
// one-sided today: `wait` is opt-in and no shipped client sends it, so every
// deployment would pay that tail and none would collect the latency benefit.
//
// This repo already rejected the same shape for the same reason.
// modules/bot_api/auth.go's registry warm-up runs `go reg.Warm(...)`
// fire-and-forget because running it inline "would block every DB-fallback auth
// on a Redis write — and when Redis is degraded (the exact case driving the
// fallback), that adds the client's write-timeout to each request."
//
// # Why dropping a ring is allowed, and a lost event is not
//
// The bell is a hint; the sorted set is the authority. A ring that is coalesced,
// dropped, or lost at shutdown costs a waiter at most one chunk (5s), because
// the hold falls back to a chunk-paced authoritative re-read. That asymmetry is
// what makes an at-most-once, best-effort delivery path acceptable here — and it
// is the only reason it is acceptable. Nothing that must not be lost may be
// routed through this file.
//
// # Why the queue is bounded by bots rather than by messages
//
// Ring trims the bell to a single element, so N rings for one bot are
// indistinguishable from one. Coalescing on robotID is therefore not an
// optimisation but the natural shape: at most one ring can be pending per bot,
// so the queue is bounded by the number of *distinct bots* receiving traffic,
// not by message rate. A burst of 10k messages to 50 bots produces at most 50
// queued rings.

const (
	// ringWorkers bounds how many rings are in flight at once. It is the number
	// ringPoolSize is derived from — workers must never queue for a connection
	// inside the pool, or the pool timeout becomes part of the ring's tail.
	//
	// Sized for the degraded case rather than the healthy one. A ring is ~70µs
	// against a loopback Redis, so even one worker clears far more than any
	// plausible bot message rate; what the count actually buys is throughput
	// while some workers are parked on a timing-out Redis.
	ringWorkers = 8

	// ringQueueDepth bounds pending *distinct bots*, not pending messages (see
	// the coalescing note above). Well above any realistic count of bots
	// simultaneously receiving traffic on one replica, and a queued entry is one
	// string, so this is a backstop rather than a tuned value.
	ringQueueDepth = 1024

	// ringLogInterval rate-limits the two degradation warnings. Eight workers
	// against a timing-out Redis would otherwise emit tens of lines a second —
	// answering a Redis incident with a log flood is the same failure shape this
	// whole change exists to remove.
	ringLogInterval = 30 * time.Second
)

// doorbell owns every piece of mutable state the ring path touches. Bundling it
// rather than keeping loose package vars is what makes the workers race-free
// when a test swaps the whole thing out: each worker closes over its own
// *doorbell, so replacing the active one cannot be observed mid-flight.
type doorbell struct {
	queue chan string

	mu      sync.Mutex
	pending map[string]struct{}

	failLog *throttledWarn
	dropLog *throttledWarn
}

var (
	ringStartOnce sync.Once
	// activeBell is read on the hottest producer path and written only by
	// start-up (and by tests), so it is an atomic pointer rather than a
	// mutex-guarded var.
	activeBell atomic.Pointer[doorbell]
)

// throttledWarn emits at most one line per interval and reports how many were
// suppressed, so a sustained failure stays visible without flooding.
type throttledWarn struct {
	mu         sync.Mutex
	every      time.Duration
	last       time.Time
	suppressed int64
}

func (t *throttledWarn) warn(msg string, fields ...zap.Field) {
	t.mu.Lock()
	now := time.Now()
	if !t.last.IsZero() && now.Sub(t.last) < t.every {
		t.suppressed++
		t.mu.Unlock()
		return
	}
	suppressed := t.suppressed
	t.suppressed = 0
	t.last = now
	t.mu.Unlock()
	log.Warn(msg, append(fields, zap.Int64("suppressed", suppressed))...)
}

// Notify asks for a wake-up hint for robotID and returns immediately.
//
// It performs no I/O on the caller's goroutine and cannot block: the ring is
// handed to a bounded worker pool, and if that pool is saturated the ring is
// dropped rather than queued without limit. Producers therefore pay a mutex and
// a channel send — never a Redis round trip — no matter what state Redis is in.
//
// Call it after a successful ZADD and after the queue's own EXPIRE, so a
// producer crash cannot leave the authoritative key without a TTL. There is no
// error to handle: a producer cannot act on a failed hint, and failing an
// already-accepted enqueue over one would trade a bounded latency cost for a
// lost event.
func Notify(cfg *config.Config, robotID string) {
	if cfg == nil || strings.TrimSpace(robotID) == "" {
		return
	}
	ringStartOnce.Do(func() { activeBell.Store(startDoorbell(RingClient(cfg))) })
	if d := activeBell.Load(); d != nil {
		d.notify(robotID)
	}
}

// startDoorbell builds a doorbell and starts its workers. Separate from Notify
// so a test can install a fake Ringer without a mutable indirection sitting in
// the production call path.
func startDoorbell(r Ringer) *doorbell {
	d := &doorbell{
		queue:   make(chan string, ringQueueDepth),
		pending: make(map[string]struct{}, ringQueueDepth),
		failLog: &throttledWarn{every: ringLogInterval},
		dropLog: &throttledWarn{every: ringLogInterval},
	}
	for i := 0; i < ringWorkers; i++ {
		go d.work(r)
	}
	return d
}

func (d *doorbell) notify(robotID string) {
	// Coalesce. A second ring for a bot that already has one pending would push
	// the same single token onto the same list, so it is not merely wasteful —
	// it is indistinguishable from the first.
	d.mu.Lock()
	if _, queued := d.pending[robotID]; queued {
		d.mu.Unlock()
		return
	}
	d.pending[robotID] = struct{}{}
	d.mu.Unlock()

	select {
	case d.queue <- robotID:
	default:
		// Saturated. Drop the hint rather than block the producer or grow
		// without bound — the consumer's chunk-paced re-read is the floor, so
		// this costs latency, never an event.
		d.mu.Lock()
		delete(d.pending, robotID)
		d.mu.Unlock()
		d.dropLog.warn("bot event doorbell ring dropped: ring queue saturated",
			zap.String("robotID", robotID), zap.Int("queueDepth", ringQueueDepth))
	}
}

func (d *doorbell) work(r Ringer) {
	for robotID := range d.queue {
		// Clear the pending mark *before* ringing, not after. A message enqueued
		// while this ring is in flight must be able to schedule its own ring:
		// the in-flight one may have already pushed its token before that ZADD
		// landed. Erring toward one extra ring costs a wasted wake-up; erring
		// the other way costs a chunk of latency.
		d.mu.Lock()
		delete(d.pending, robotID)
		d.mu.Unlock()

		if err := Ring(r, robotID); err != nil {
			d.failLog.warn("bot event doorbell ring failed",
				zap.String("robotID", robotID), zap.Error(err))
		}
	}
}

func (d *doorbell) pendingCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}
