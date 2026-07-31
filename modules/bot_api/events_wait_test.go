package bot_api

import (
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// resetEventHoldState clears the process-wide hold bookkeeping so each test
// starts from a known cap. The production singletons are intentionally
// package-level (one budget per process), so tests must reset them explicitly
// rather than construct their own.
func resetEventHoldState(t *testing.T, cap int) {
	t.Helper()
	eventHoldOnce = sync.Once{}
	eventHoldSem = nil
	eventHoldPerBotMu.Lock()
	eventHoldPerBot = map[string]struct{}{}
	eventHoldPerBotMu.Unlock()
	t.Setenv("OCTO_BOT_EVENTS_MAX_HOLDS", itoa(int64(cap)))
	t.Cleanup(func() {
		eventHoldOnce = sync.Once{}
		eventHoldSem = nil
		eventHoldPerBotMu.Lock()
		eventHoldPerBot = map[string]struct{}{}
		eventHoldPerBotMu.Unlock()
	})
}

func TestClampEventWaitPreservesLegacyBehaviorByDefault(t *testing.T) {
	// The whole backward-compatibility guarantee rests on this: a request that
	// does not mention `wait` (zero value) must produce a zero hold, i.e. the
	// pre-long-poll code path.
	if got := clampEventWait(0); got != 0 {
		t.Fatalf("absent wait produced a hold of %v, want 0", got)
	}
	if got := clampEventWait(-5); got != 0 {
		t.Fatalf("negative wait produced a hold of %v, want 0", got)
	}
}

func TestClampEventWaitClampsRatherThanRejects(t *testing.T) {
	cases := map[int64]time.Duration{
		1:                       1 * time.Second,
		maxEventWaitSeconds:     maxEventWaitSeconds * time.Second,
		maxEventWaitSeconds + 1: maxEventWaitSeconds * time.Second,
		1 << 40:                 maxEventWaitSeconds * time.Second,
	}
	for in, want := range cases {
		if got := clampEventWait(in); got != want {
			t.Fatalf("clampEventWait(%d) = %v, want %v", in, got, want)
		}
	}
}

func TestEventWaitChunkForNeverTruncatesToZero(t *testing.T) {
	// Regression guard for a bug the integration suite caught: rounding the
	// remainder *down* made a 2s hold return after ~1s, i.e. half the requested
	// wait. Every non-expired remainder must yield at least a full second, and
	// a sub-second remainder must round up rather than vanish.
	for _, remaining := range []time.Duration{
		time.Nanosecond, 100 * time.Millisecond, 999 * time.Millisecond,
		time.Second, 1500 * time.Millisecond, 1999 * time.Millisecond,
	} {
		got := eventWaitChunkFor(remaining)
		if got < time.Second {
			t.Fatalf("eventWaitChunkFor(%v) = %v; anything below 1s truncates to a 0s BLPOP (infinite block)", remaining, got)
		}
		if got < remaining {
			t.Fatalf("eventWaitChunkFor(%v) = %v; must not under-serve the requested hold", remaining, got)
		}
		if got.Truncate(time.Second) != got {
			t.Fatalf("eventWaitChunkFor(%v) = %v is not whole seconds", remaining, got)
		}
	}
}

func TestEventWaitChunkForCapsAtTheChunkSize(t *testing.T) {
	if got := eventWaitChunkFor(time.Hour); got != eventWaitChunk {
		t.Fatalf("eventWaitChunkFor(1h) = %v, want the %v cap", got, eventWaitChunk)
	}
	// An expired hold is the only case that may return zero, and callers read
	// that as "stop", never as "block forever".
	for _, expired := range []time.Duration{0, -time.Second} {
		if got := eventWaitChunkFor(expired); got != 0 {
			t.Fatalf("eventWaitChunkFor(%v) = %v, want 0", expired, got)
		}
	}
}

func TestEventWaitChunkNeverDegeneratesToInfiniteBlock(t *testing.T) {
	// BLPOP's timeout is whole seconds and go-redis truncates toward zero;
	// BLPOP with timeout 0 blocks forever. Any chunk the wait loop can produce
	// must therefore be at least one second.
	if eventWaitChunk < time.Second {
		t.Fatalf("eventWaitChunk %v truncates to a 0s BLPOP (infinite block)", eventWaitChunk)
	}
	if eventWaitChunk.Truncate(time.Second) != eventWaitChunk {
		t.Fatalf("eventWaitChunk %v is not a whole number of seconds", eventWaitChunk)
	}
	// The chunk also bounds how long a hung client keeps its slot and how long
	// an in-flight hold can extend shutdown drain.
	if eventWaitChunk > 5*time.Second {
		t.Fatalf("eventWaitChunk %v is too coarse to notice a disconnect promptly", eventWaitChunk)
	}
}

func TestEventWaitPoolCoversTheHoldCap(t *testing.T) {
	// Each hold pins one connection for its whole duration. If the cap could
	// exceed the pool, waiters would queue for connections inside the pool and
	// the isolation the dedicated client exists to provide would be defeated.
	t.Setenv("OCTO_BOT_EVENTS_MAX_HOLDS", "32")
	if got := maxEventHolds(); got != 32 {
		t.Fatalf("maxEventHolds() = %d, want 32", got)
	}
	if eventWaitPoolHeadroom <= 0 {
		t.Fatal("pool must carry headroom above the hold cap")
	}
}

func TestMaxEventHoldsRejectsUnusableOverrides(t *testing.T) {
	for _, raw := range []string{"", "0", "-1", "abc"} {
		t.Setenv("OCTO_BOT_EVENTS_MAX_HOLDS", raw)
		// A bad override must fall back to the default, never disable the cap
		// (an unbounded cap would let holds exhaust the pool).
		if got := maxEventHolds(); got != defaultMaxEventHolds {
			t.Fatalf("override %q gave %d, want the default %d", raw, got, defaultMaxEventHolds)
		}
	}
}

func TestAcquireEventHoldAllowsOneHoldPerBot(t *testing.T) {
	resetEventHoldState(t, 8)

	release, ok := acquireEventHold("bot1")
	if !ok {
		t.Fatal("first hold must be granted")
	}
	// A bot that opens a second concurrent poll must not pin a second
	// connection; it degrades to the immediate answer instead.
	if _, ok := acquireEventHold("bot1"); ok {
		t.Fatal("second concurrent hold for the same bot must be refused")
	}
	// A different bot is unaffected.
	release2, ok := acquireEventHold("bot2")
	if !ok {
		t.Fatal("a different bot must still be able to hold")
	}
	release2()

	release()
	release3, ok := acquireEventHold("bot1")
	if !ok {
		t.Fatal("hold must be re-grantable after release")
	}
	release3()
}

func TestAcquireEventHoldFailsOpenAtCapacity(t *testing.T) {
	resetEventHoldState(t, 2)

	r1, ok := acquireEventHold("bot1")
	if !ok {
		t.Fatal("hold 1 must be granted")
	}
	r2, ok := acquireEventHold("bot2")
	if !ok {
		t.Fatal("hold 2 must be granted")
	}
	// At capacity the answer is "no hold", not an error: the caller falls back
	// to the immediate empty response, which is exactly the legacy behavior.
	if _, ok := acquireEventHold("bot3"); ok {
		t.Fatal("hold beyond the cap must be refused")
	}
	r1()
	r3, ok := acquireEventHold("bot3")
	if !ok {
		t.Fatal("slot must be reusable once released")
	}
	r3()
	r2()
}

func TestAcquireEventHoldReleasesPerBotSlotWhenCapacityRefuses(t *testing.T) {
	// Regression guard for the ordering bug this code is prone to: the per-bot
	// marker is taken before the global semaphore, so a capacity refusal must
	// unwind it. Otherwise a bot that once hit a full house could never hold
	// again for the process lifetime.
	resetEventHoldState(t, 1)

	r1, ok := acquireEventHold("bot1")
	if !ok {
		t.Fatal("hold 1 must be granted")
	}
	if _, ok := acquireEventHold("bot2"); ok {
		t.Fatal("bot2 must be refused at capacity")
	}
	r1()

	r2, ok := acquireEventHold("bot2")
	if !ok {
		t.Fatal("bot2 must be able to hold after the capacity refusal unwound its marker")
	}
	r2()
}

func TestReleaseEventHoldIsIdempotent(t *testing.T) {
	resetEventHoldState(t, 1)
	release, ok := acquireEventHold("bot1")
	if !ok {
		t.Fatal("hold must be granted")
	}
	release()
	release() // a double defer must not free a slot twice
	if _, ok := acquireEventHold("bot2"); !ok {
		t.Fatal("bot2 must get the single slot")
	}
	if _, ok := acquireEventHold("bot3"); ok {
		t.Fatal("double release leaked a slot: cap is no longer enforced")
	}
}

func TestAcquireEventHoldIsRaceSafe(t *testing.T) {
	resetEventHoldState(t, 4)
	var wg sync.WaitGroup
	granted := make(chan func(), 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "bot" + itoa(int64(i%8))
			if release, ok := acquireEventHold(id); ok {
				granted <- release
			}
		}(i)
	}
	wg.Wait()
	close(granted)
	n := 0
	for release := range granted {
		n++
		release()
	}
	if n > 4 {
		t.Fatalf("%d holds granted, cap is 4", n)
	}
}

func TestFilterAppBotEventsLeavesNonAppBotsAlone(t *testing.T) {
	ba := &BotAPI{}
	in := []*eventResp{
		{EventID: 1, Message: &messageResp{ChannelType: 2}},
		{EventID: 2},
	}
	out := ba.filterAppBotEvents(BotKindUser, "bot1", in)
	if len(out) != 2 {
		t.Fatalf("non-App bot events were filtered: %d of 2 survived", len(out))
	}
}

func TestFilterAppBotEventsKeepsDMEvents(t *testing.T) {
	ba := &BotAPI{}
	in := []*eventResp{
		{EventID: 1, Message: &messageResp{ChannelType: common.ChannelTypePerson.Uint8()}},
		{EventID: 2, Message: &messageResp{ChannelType: 0}},
		{EventID: 3},
	}
	// All three are DM-shaped, so nothing is dropped and no ZREM is issued —
	// which is what lets this run without Redis.
	out := ba.filterAppBotEvents(BotKindApp, "bot1", in)
	if len(out) != 3 {
		t.Fatalf("DM events were dropped: %d of 3 survived", len(out))
	}
}

func TestFilterAppBotEventsHandlesEmptyBatch(t *testing.T) {
	ba := &BotAPI{}
	if out := ba.filterAppBotEvents(BotKindApp, "bot1", nil); len(out) != 0 {
		t.Fatalf("nil batch produced %d events", len(out))
	}
}
