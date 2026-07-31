package botevent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

// PR#685 review (yujiawei, P1): "Add a test that holds the ring pool and asserts
// producer latency stays bounded — the existing tests all exercise a healthy
// ring." The findings that reached production in rounds 1-3 were all degradation
// paths that cost more than the design intended, and every test written for them
// exercised the healthy path. So the tests below run the ring against a Redis
// that never answers.

// blockingRinger stands in for a Redis that accepts work and never completes it:
// a saturated pool, a blackholed connection, a failover in progress. Every
// script call parks until released.
type blockingRinger struct {
	release chan struct{}
	calls   int64
}

func newBlockingRinger() *blockingRinger {
	return &blockingRinger{release: make(chan struct{})}
}

func (b *blockingRinger) block() *rd.Cmd {
	atomic.AddInt64(&b.calls, 1)
	<-b.release
	return rd.NewCmdResult(int64(1), nil)
}

func (b *blockingRinger) Eval(string, []string, ...interface{}) *rd.Cmd    { return b.block() }
func (b *blockingRinger) EvalSha(string, []string, ...interface{}) *rd.Cmd { return b.block() }
func (b *blockingRinger) ScriptExists(...string) *rd.BoolSliceCmd {
	return rd.NewBoolSliceResult([]bool{true}, nil)
}
func (b *blockingRinger) ScriptLoad(string) *rd.StringCmd {
	return rd.NewStringResult(ringScript.Hash(), nil)
}

// installRinger wins the start-up sync.Once so Notify drives the given Ringer
// instead of a real client, and hands back the doorbell it installed. Each
// worker closes over its own *doorbell, so swapping the active one out in
// Cleanup cannot race with workers still draining the old one.
func installRinger(t *testing.T, r Ringer) *doorbell {
	t.Helper()
	ringStartOnce = sync.Once{}
	activeBell.Store(nil)
	ringStartOnce.Do(func() { activeBell.Store(startDoorbell(r)) })
	d := activeBell.Load()
	t.Cleanup(func() {
		close(d.queue) // let the workers exit rather than leak into the next test
		ringStartOnce = sync.Once{}
		activeBell.Store(nil)
	})
	return d
}

// TestNotifyNeverBlocksTheProducerOnAStalledRedis is the regression test for the
// P1 this redesign exists to remove.
//
// The ring used to run inline in saveRobotMessage, which executes inside a
// msgSem slot (capacity 100, acquired by a *blocking* send on the message
// listener goroutine). So seconds of ring latency became held slots, and 100
// held slots stop bot message fan-out process-wide — for every bot, including
// bots that never long-poll. Producers must therefore pay no Redis latency at
// all, not merely a bounded amount.
func TestNotifyNeverBlocksTheProducerOnAStalledRedis(t *testing.T) {
	blocked := newBlockingRinger()
	d := installRinger(t, blocked)
	defer close(blocked.release)

	cfg := &config.Config{}

	// Park every worker on the stalled Redis first.
	for i := 0; i < ringWorkers; i++ {
		Notify(cfg, "stalled-bot-"+itoaGuard(i))
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&blocked.calls) >= int64(ringWorkers) },
		"workers never reached the ringer")

	// With every worker stuck, a producer must still return immediately. The
	// budget is deliberately tiny: this call may not touch the network at all,
	// so anything above a mutex and a channel send is a regression.
	const producers = 500
	start := time.Now()
	for i := 0; i < producers; i++ {
		Notify(cfg, "hot-bot-"+itoaGuard(i))
	}
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("%d Notify calls took %v against a stalled Redis; producers must not wait on the ring",
			producers, elapsed)
	}

	// And the queue must be bounded — a stalled Redis may cost bells, never
	// unbounded memory or goroutines.
	if got := len(d.queue); got > ringQueueDepth {
		t.Fatalf("ring queue holds %d entries, above its %d bound", got, ringQueueDepth)
	}
}

// TestNotifyCoalescesPerBot pins the property that lets the queue be bounded by
// bot count rather than by message rate: Ring trims the bell to one element, so
// a second pending ring for the same bot would push the same token onto the same
// list. Without coalescing, a bot receiving a burst of N messages would enqueue
// N rings and could evict other bots' rings from a full queue.
func TestNotifyCoalescesPerBot(t *testing.T) {
	blocked := newBlockingRinger()
	d := installRinger(t, blocked)
	defer close(blocked.release)

	cfg := &config.Config{}
	// Fill every worker with other bots so nothing drains "burst-bot" while the
	// coalescing is being observed.
	for i := 0; i < ringWorkers; i++ {
		Notify(cfg, "filler-"+itoaGuard(i))
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&blocked.calls) >= int64(ringWorkers) },
		"workers never reached the ringer")

	before := len(d.queue)
	for i := 0; i < 1000; i++ {
		Notify(cfg, "burst-bot")
	}
	if got := len(d.queue) - before; got != 1 {
		t.Fatalf("1000 notifies for one bot queued %d rings, want exactly 1", got)
	}
}

// TestNotifyDropsRatherThanGrowsWhenSaturated: the queue is a backstop, and when
// it is full the answer is to lose a hint, never to block a producer or allocate
// without bound. Losing a hint costs a waiter one chunk of latency because the
// hold falls back to a chunk-paced authoritative re-read; blocking a producer
// costs bot delivery process-wide.
func TestNotifyDropsRatherThanGrowsWhenSaturated(t *testing.T) {
	blocked := newBlockingRinger()
	d := installRinger(t, blocked)
	defer close(blocked.release)

	cfg := &config.Config{}
	for i := 0; i < ringWorkers; i++ {
		Notify(cfg, "filler-"+itoaGuard(i))
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&blocked.calls) >= int64(ringWorkers) },
		"workers never reached the ringer")

	// Two full queues' worth of distinct bots, so the second half must be dropped.
	start := time.Now()
	for i := 0; i < ringQueueDepth*2; i++ {
		Notify(cfg, "bot-"+itoaGuard(i))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("saturating the queue took %v; the drop path must not block", elapsed)
	}
	if got := len(d.queue); got != ringQueueDepth {
		t.Fatalf("queue holds %d, want exactly its %d bound", got, ringQueueDepth)
	}
	// A dropped ring must not leave its pending mark behind, or that bot could
	// never be notified again for the process lifetime.
	if got := d.pendingCount(); got > ringQueueDepth {
		t.Fatalf("pending set holds %d entries after %d drops; drops must unwind their mark",
			got, ringQueueDepth)
	}
}

// TestNotifyClearsPendingBeforeRinging: a message enqueued while a ring is in
// flight must be able to schedule its own ring. The in-flight ring may have
// already pushed its token before that ZADD landed, so coalescing against it
// would cost the new event a full chunk of latency. Erring toward one extra ring
// costs a wasted wake-up, which the design already tolerates.
func TestNotifyClearsPendingBeforeRinging(t *testing.T) {
	blocked := newBlockingRinger()
	d := installRinger(t, blocked)
	defer close(blocked.release)

	cfg := &config.Config{}
	Notify(cfg, "bot1")
	// The worker has picked it up and is parked inside Ring.
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&blocked.calls) >= 1 },
		"the worker never reached the ringer")
	waitFor(t, time.Second, func() bool { return d.pendingCount() == 0 },
		"the pending mark must be cleared before the ring, not after")

	// So a second notify for the same bot is accepted rather than swallowed. It
	// must reach the ringer — asserting on queue depth would be wrong, since a
	// free worker drains it immediately (only one of the eight is parked).
	Notify(cfg, "bot1")
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&blocked.calls) >= 2 },
		"a notify issued during an in-flight ring was swallowed by coalescing")
	_ = d
}

// TestNotifyIsANoOpOnEmptyInput keeps the guard cheap enough to sit on the
// hottest producer path.
func TestNotifyIsANoOpOnEmptyInput(t *testing.T) {
	blocked := newBlockingRinger()
	d := installRinger(t, blocked)
	defer close(blocked.release)

	Notify(nil, "bot1")
	Notify(&config.Config{}, "   ")
	if got := len(d.queue); got != 0 {
		t.Fatalf("empty input queued %d rings", got)
	}
}

// TestRingPoolIsSizedFromItsOwnConcurrency pins the derivation rather than the
// number. An earlier revision set the pool to 10 by copying the convention of
// unrelated short-transaction clients while the path it served admitted 100
// concurrent callers — a 10:1 contention ratio dressed up as a convention
// (PR#685 review, yujiawei). Workers must never queue for a connection, or the
// pool timeout becomes part of the ring's tail.
func TestRingPoolIsSizedFromItsOwnConcurrency(t *testing.T) {
	if ringPoolSize < ringWorkers {
		t.Fatalf("ringPoolSize %d is below ringWorkers %d: workers would contend inside the pool",
			ringPoolSize, ringWorkers)
	}
	// The timeouts must stay far below the consumer's chunk, since a ring that
	// outlives the fallback re-read it exists to pre-empt has no value left.
	if ringDialTimeout >= time.Second || ringReadTimeout >= time.Second || ringPoolTimeout >= time.Second {
		t.Fatalf("ring timeouts must stay sub-second: dial=%v read=%v pool=%v",
			ringDialTimeout, ringReadTimeout, ringPoolTimeout)
	}
}

func waitFor(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
