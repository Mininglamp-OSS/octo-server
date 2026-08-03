package bot_api

import (
	"context"
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
	clear := func() {
		eventHoldsOnce = sync.Once{}
		eventHoldsN, eventHoldsNote = 0, ""
		eventHoldOnce = sync.Once{}
		eventHoldSem = nil
		eventHoldOffOnce = sync.Once{}
		eventHoldOffSem = nil
		eventHoldPerBotMu.Lock()
		eventHoldPerBot = map[string]struct{}{}
		eventHoldPerBotMu.Unlock()
	}
	clear()
	t.Setenv(maxEventHoldsEnv, itoa(int64(cap)))
	t.Cleanup(clear)
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
	resetEventHoldState(t, 32)
	if got := maxEventHolds(); got != 32 {
		t.Fatalf("maxEventHolds() = %d, want 32", got)
	}
	if eventWaitPoolHeadroom <= 0 {
		t.Fatal("pool must carry headroom above the hold cap")
	}
}

func TestParseMaxEventHoldsHandlesEveryOverrideShape(t *testing.T) {
	cases := []struct {
		raw      string
		want     int
		wantNote bool
	}{
		// A bad override falls back to the default, never disabling the cap: an
		// unbounded cap would let holds exhaust the pool they live in.
		{"", defaultMaxEventHolds, false},
		{"   ", defaultMaxEventHolds, false},
		{"0", defaultMaxEventHolds, true},
		{"-1", defaultMaxEventHolds, true},
		{"abc", defaultMaxEventHolds, true},
		{"32", 32, false},
		{" 32 ", 32, false},
		// The clamp is the one that *allocates*: it sizes both a channel and a
		// Redis pool, so an unclamped value would ask go-redis for a
		// million-connection pool.
		{"1000000", maxAllowedEventHolds, true},
		{"4097", maxAllowedEventHolds, true},
		{"4096", maxAllowedEventHolds, false},
	}
	for _, tc := range cases {
		got, note := parseMaxEventHolds(tc.raw)
		if got != tc.want {
			t.Fatalf("parseMaxEventHolds(%q) = %d, want %d", tc.raw, got, tc.want)
		}
		// Every value that is not used verbatim must explain itself, so the
		// boot-time Warn can name the reason instead of silently substituting.
		if (note != "") != tc.wantNote {
			t.Fatalf("parseMaxEventHolds(%q) note = %q, wantNote=%v", tc.raw, note, tc.wantNote)
		}
	}
}

func TestMaxEventHoldsResolvesOnceForTheWholeProcess(t *testing.T) {
	// The cap sizes two independent things — the semaphore and the dedicated
	// Redis pool — which are built at different moments. Re-reading the
	// environment for each would let them disagree.
	resetEventHoldState(t, 12)
	if got := maxEventHolds(); got != 12 {
		t.Fatalf("maxEventHolds() = %d, want 12", got)
	}
	t.Setenv(maxEventHoldsEnv, "99")
	if got := maxEventHolds(); got != 12 {
		t.Fatalf("maxEventHolds() = %d after the environment changed, want the resolved 12", got)
	}
}

func TestSleepOrDoneReportsCancellationRatherThanWaitingItOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepOrDone(ctx, 5*time.Second) {
		t.Fatal("a cancelled request must not be reported as a completed pause")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v to notice", elapsed)
	}
	// A non-positive pause is a no-op, not an infinite one.
	if !sleepOrDone(context.Background(), 0) {
		t.Fatal("a zero pause must complete")
	}
}

func TestHoldOffRefusedWaitPausesTheCallerButNotBeyondOneChunk(t *testing.T) {
	resetEventHoldState(t, 4)
	ba := &BotAPI{}

	// A refused hold answered instantly is indistinguishable on the wire from a
	// served one, and a long-poll client has no interval to fall back on — so it
	// re-requests immediately, amplifying load on the replica already at
	// capacity. The refusal must cost the caller something.
	start := time.Now()
	ba.holdOffRefusedWait(context.Background(), time.Second)
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("a refused hold returned in %v; it must pace the caller", elapsed)
	}

	// But never more than one chunk, however long a hold the caller asked for:
	// the request is not being served, so it must not occupy the process for the
	// full 30s.
	start = time.Now()
	ba.holdOffRefusedWait(context.Background(), maxEventWaitSeconds*time.Second)
	elapsed := time.Since(start)
	if elapsed > eventWaitChunk+time.Second {
		t.Fatalf("a refused 30s hold paused for %v, want at most one %v chunk", elapsed, eventWaitChunk)
	}
	if elapsed < eventWaitChunk-time.Second {
		t.Fatalf("a refused 30s hold paused only %v, want about one %v chunk", elapsed, eventWaitChunk)
	}
}

func TestHoldOffRefusedWaitReleasesOnClientDisconnect(t *testing.T) {
	resetEventHoldState(t, 4)
	ba := &BotAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	ba.holdOffRefusedWait(ctx, maxEventWaitSeconds*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a disconnected caller was paced for %v; the pause must end with the request", elapsed)
	}
}

func TestHoldOffBudgetFallsBackToTheInstantAnswer(t *testing.T) {
	// The pause is back-pressure, and back-pressure that can occupy the process
	// without bound is not back-pressure: a refused hold sits in neither the
	// per-bot map nor the global semaphore while holding a handler, a goroutine
	// and a timer. Past its own budget the answer reverts to the instant empty
	// batch — exactly what a caller that omits `wait` already gets.
	resetEventHoldState(t, 1)
	budget := maxEventHolds() * eventHoldOffFactor

	held := make([]func(), 0, budget)
	for i := 0; i < budget; i++ {
		release, ok := acquireHoldOff()
		if !ok {
			t.Fatalf("hold-off %d of %d refused; the budget is smaller than advertised", i, budget)
		}
		held = append(held, release)
	}
	if _, ok := acquireHoldOff(); ok {
		t.Fatal("hold-off beyond the budget must be refused")
	}

	ba := &BotAPI{}
	start := time.Now()
	ba.holdOffRefusedWait(context.Background(), maxEventWaitSeconds*time.Second)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("an over-budget refusal paused for %v; it must answer instantly", elapsed)
	}

	for _, release := range held {
		release()
	}
	// And the budget is reusable, not one-shot.
	release, ok := acquireHoldOff()
	if !ok {
		t.Fatal("hold-off slots must be reusable once released")
	}
	release()
	release() // a double defer must not free a slot twice
	for i := 0; i < budget; i++ {
		if _, ok := acquireHoldOff(); !ok {
			t.Fatalf("double release leaked a slot: only %d of %d available", i, budget)
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
	// connection; it is refused here and paced by holdOffRefusedWait instead.
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
	// to a paced empty response rather than a failure.
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
