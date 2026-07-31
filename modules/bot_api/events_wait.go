package bot_api

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// Long-poll support for POST /v1/bot/events (card-message-interaction D5 / P3-2).
//
// # Why a doorbell and not a tighter poll
//
// Re-running ZRangeByScore on a timer inside the hold would multiply Redis
// reads (a 30s hold at 300ms granularity is 100 reads where the caller used to
// do one). Instead **every** producer that ZADDs into robotEvent:{robotID}
// rings a per-bot doorbell list — five sites today, enforced by
// pkg/botevent's TestEveryBotEventQueueWriterRingsTheDoorbell rather than by a
// comment — and the waiter blocks on it with BLPOP, so the blocking happens
// inside Redis and is correct across replicas — the in-process channel map used
// by modules/robot's inlineQuery long-poll only works on a single replica, and
// this queue is consumed by whichever replica the bot happens to reach.
//
// # Why an isolated connection pool
//
// BLPOP occupies its connection for the whole block. The shared pool from
// ctx.GetRedisConn() is built with no explicit PoolSize (go-redis default
// 10*NumCPU) and is used by every other Redis call in the process, so parking
// holds there would let a handful of long-polling bots starve ordinary traffic.
// The waiter therefore gets its own instrumented client sized to the hold cap,
// following the same explicit-PoolSize convention as the rate-limit clients in
// modules/user, modules/incomingwebhook, modules/file and modules/group.
//
// Producers do not share this pool. They ring through pkg/botevent's own small
// client (ringPoolSize = 10), because a ring is one EVALSHA that returns
// immediately and must never queue behind a parked hold. Three pools per replica
// therefore exist: shared, wait (maxEventHolds + 4), ring (10) — the number an
// operator sizes Redis `maxclients` from.
//
// # How the hold loop guarantees it terminates
//
// Every iteration burns at least one chunk of wall clock, or advances the queue
// cursor, or consumes a doorbell token — and never none of the three:
//   - BLPOP timed out (redis.Nil): the chunk was spent.
//   - BLPOP failed: the error path explicitly pays out the rest of the chunk
//     (go-redis returns dial/protocol errors in ~8ms, so nothing else would).
//   - the block was skipped: only permitted when the previous read actually
//     moved the cursor — `eventPage.advanced`, applied to the entry page and to
//     every in-loop page alike.
//   - BLPOP returned a token: a producer rang, so a ZADD landed.
//
// The third disjunct is why that last case is stated as "consumed a token" and
// not "advances the cursor". A ring whose event lands *below* this caller's
// cursor wakes the hold and the read finds nothing — reachable when the caller
// sends an `event_id` above the queue's maximum, and by the cross-replica
// `GenSeq` id ordering. That costs one unpaced ZRangeByScore per enqueue, which
// is bounded by that bot's own producer rate and by the one-hold-per-bot rule,
// so it is not a storm; but it is not cursor progress either, and an invariant
// that quietly excluded it would be the same shape of false written premise that
// caused rounds 1 through 3.
//
// The cursor is monotonic and bounded by the queue's maximum event id, the token
// count is bounded by producer rate, and the chunk count is bounded by the
// deadline. Nothing in that argument depends on a write succeeding — in
// particular not on the filter's auto-ACK ZREM, whose error is only logged.
//
// What the invariant does NOT bound is *rate*: a drain that keeps advancing (a
// filtered backlog read at a small `limit`) legitimately re-reads without pacing
// until the backlog is gone or the deadline hits. That loop is bounded by the
// backlog rather than by the deadline, and it is the drain doing its job — but
// if a future change needs a ceiling on it, that is where to put one.

const (
	// maxEventWaitSeconds caps the hold. 30s stays well under the 60s idle
	// timeout common to reverse proxies; lowering it is a pure config change
	// with no contract impact.
	maxEventWaitSeconds = 30

	// eventWaitChunk bounds one BLPOP call. Two independent reasons it must
	// stay small:
	//   - octo-lib's Conn/go-redis v6 BLPOP takes no context, so a client that
	//     hung up can only be noticed between chunks; the chunk size is the
	//     upper bound on how long a dead connection keeps its slot.
	//   - the same bound caps how much an in-flight hold can extend process
	//     drain at shutdown (one chunk, not a whole 30s hold).
	eventWaitChunk = 5 * time.Second

	// defaultMaxEventHolds bounds concurrent holds process-wide. Each hold
	// pins one connection of the dedicated pool, so this and the pool size
	// move together — see eventWaitPoolSize.
	defaultMaxEventHolds = 64

	// eventWaitPoolHeadroom leaves room for reconnects so a hold at the cap
	// cannot wedge the pool it lives in.
	eventWaitPoolHeadroom = 4

	// maxAllowedEventHolds bounds the operator override. Every other limit on
	// this endpoint is clamped (wait -> 30, limit -> 100); this one is the only
	// one that *allocates* — it sizes both a channel and a Redis pool — so an
	// unclamped value would ask go-redis for a million-connection pool, and a
	// value near MaxInt would overflow `holds + eventWaitPoolHeadroom`.
	maxAllowedEventHolds = 4096

	// eventHoldOffFactor sizes the budget for *refused* holds relative to the
	// granted-hold cap.
	//
	// A refused hold sleeps out a chunk instead of answering instantly, which is
	// deliberate back-pressure (see holdOffRefusedWait). But that sleeper holds
	// an HTTP handler, a goroutine and a timer while being counted by neither
	// the per-bot map nor the global semaphore — so without a budget of its own
	// the refusal path could occupy more of the process than the success path
	// it protects, which is the wrong way round. /v1/bot has no per-bot rate
	// middleware (bots have no login uid), so nothing upstream bounds it either.
	//
	// Four times the hold cap, rather than one: a legitimate bot fleet doubles
	// its pollers briefly during a rolling restart, and capacity refusals are
	// expected to outnumber granted holds precisely when the cap is doing its
	// job. Beyond the budget the answer reverts to the instant empty batch,
	// which is what every caller that does not send `wait` already gets — a safe
	// floor, not a new failure mode.
	eventHoldOffFactor = 4

	// maxEventHoldsEnv overrides defaultMaxEventHolds. Documented in
	// docs/bot-events-longpoll.md; resolved and validated once at boot
	// (NewBotAPI) so a bad value surfaces at deploy rather than on the first
	// long-poll request.
	maxEventHoldsEnv = "OCTO_BOT_EVENTS_MAX_HOLDS"
)

// clampEventWait converts the caller's `wait` (seconds) into a hold duration.
// Out-of-range values are clamped rather than rejected: `wait` is a hint about
// how long the caller is willing to park, not a correctness input, and a 400
// here would break callers for no protective benefit.
func clampEventWait(waitSeconds int64) time.Duration {
	if waitSeconds <= 0 {
		return 0
	}
	if waitSeconds > maxEventWaitSeconds {
		waitSeconds = maxEventWaitSeconds
	}
	return time.Duration(waitSeconds) * time.Second
}

// eventWaitChunkFor sizes the next BLPOP from the time left in the hold.
//
// BLPOP's timeout is whole seconds and go-redis truncates toward zero, so a
// sub-second argument becomes 0 — which Redis reads as "block forever". Every
// chunk is therefore rounded **up** to a whole second, never down.
//
// Rounding up rather than down is the deliberate choice: truncating drops the
// sub-second remainder on every iteration, so a 2s hold would spend one second
// blocking and then bail out with ~0.99s left — returning empty in half the
// time the caller asked for. Overshooting the deadline by less than a second is
// harmless; systematically under-serving the requested hold defeats the point
// of asking for one.
//
// Returns 0 only when the hold is already over, which callers treat as "stop".
func eventWaitChunkFor(remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if remaining >= eventWaitChunk {
		return eventWaitChunk
	}
	// Round up to the next whole second; the floor of one second is what keeps
	// the BLPOP argument from truncating to an unbounded block.
	chunks := (remaining + time.Second - 1) / time.Second
	return chunks * time.Second
}

// parseMaxEventHolds interprets the OCTO_BOT_EVENTS_MAX_HOLDS override.
//
// Pure, so the boot-time resolution and its tests exercise the same code. The
// second return value is the reason the raw value was not used verbatim, empty
// when it was (or when none was set); callers log it. A non-positive or
// unparseable value falls back to the default rather than disabling the cap —
// an unbounded cap would let holds exhaust the pool they live in.
func parseMaxEventHolds(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMaxEventHolds, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMaxEventHolds, "not an integer"
	}
	if n <= 0 {
		return defaultMaxEventHolds, "must be positive"
	}
	if n > maxAllowedEventHolds {
		return maxAllowedEventHolds, "above the maximum"
	}
	return n, ""
}

var (
	eventWaitRedisOnce sync.Once
	eventWaitRedis     *rd.Client

	eventHoldsOnce sync.Once
	eventHoldsN    int
	eventHoldsNote string

	eventHoldOnce sync.Once
	eventHoldSem  chan struct{}

	eventHoldOffOnce sync.Once
	eventHoldOffSem  chan struct{}

	eventHoldPerBotMu sync.Mutex
	eventHoldPerBot   = map[string]struct{}{}
)

// maxEventHolds resolves the concurrent-hold cap once per process.
//
// Resolved once rather than per call because two independent things are sized
// from it — the semaphore and the dedicated pool — and an environment that
// changed between those two reads would leave them disagreeing.
func maxEventHolds() int {
	eventHoldsOnce.Do(func() {
		eventHoldsN, eventHoldsNote = parseMaxEventHolds(os.Getenv(maxEventHoldsEnv))
	})
	return eventHoldsN
}

// resolveEventHoldBudget forces that resolution at boot and reports a rejected
// or clamped override, so an operator typo shows up in the startup log rather
// than silently taking effect on the first long-poll request. Modelled on
// ParseRPSFromEnv's warn-and-fall-back in main.go.
func (ba *BotAPI) resolveEventHoldBudget() {
	holds := maxEventHolds()
	if eventHoldsNote != "" {
		ba.Warn("bot event hold cap override not used as given",
			zap.String("env", maxEventHoldsEnv),
			zap.String("value", os.Getenv(maxEventHoldsEnv)),
			zap.String("reason", eventHoldsNote),
			zap.Int("using", holds))
	}
}

// sharedEventWaitRedis builds the process-wide dedicated client for BLPOP.
// Built through octoredis so TLS options are honoured and the redis chokepoint
// guard (pkg/redis TestNoRawRedisClientOutsideChokepoint) stays satisfied.
func sharedEventWaitRedis(cfg *config.Config) *rd.Client {
	eventWaitRedisOnce.Do(func() {
		holds := maxEventHolds()
		eventWaitRedis = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			// No retries. go-redis does not retry a *read timeout* on a blocking
			// command (defaultProcess passes `cmd.readTimeout() == nil` as the
			// retryTimeout flag), but it does retry io.EOF, non-timeout
			// net.Errors, `ERR max number of clients reached`, LOADING, READONLY
			// and CLUSTERDOWN. A failover that closes the connection late in a
			// chunk would therefore run a second full BLPOP inside the same
			// call, adding roughly one chunk beyond the documented worst case
			// and eating a third of the margin against a proxy idle timeout.
			// The chunk-paced re-read already is the retry, so this one only
			// costs latency.
			o.MaxRetries = 0
			// Sized to the hold cap: a full house of waiters must not have to
			// queue for a connection, and must not be able to borrow one from
			// anywhere else.
			o.PoolSize = holds + eventWaitPoolHeadroom
			// BLPOP overrides the per-command read deadline itself (go-redis
			// sets it to block duration + 10s), so the pool-level ReadTimeout
			// does not truncate a hold. Left at the default deliberately.
		})
	})
	return eventWaitRedis
}

// acquireEventHold takes both the global slot and the per-bot slot. It returns
// false when either is unavailable; the caller then hands the request to
// holdOffRefusedWait, which answers with an empty batch after a bounded pause
// rather than instantly. Refusing to hold is a degradation, never an error —
// matching the fail-open posture of the shared rate-limit middleware.
//
// Scope: both budgets are **per process**, which is the scope that matters
// because the resource being protected — this replica's dedicated Redis pool —
// is also per process. It is NOT a cluster-wide guarantee: with N replicas a
// single bot can park one hold on each, and the fleet-wide ceiling is
// maxEventHolds × N. That is bounded and intended; do not read "one hold per
// bot" as a distributed invariant.
func acquireEventHold(robotID string) (release func(), ok bool) {
	eventHoldOnce.Do(func() {
		eventHoldSem = make(chan struct{}, maxEventHolds())
	})

	// Per-bot first: one hold per bot, so a bot that reconnects in a loop
	// cannot occupy several pool connections at once.
	eventHoldPerBotMu.Lock()
	if _, held := eventHoldPerBot[robotID]; held {
		eventHoldPerBotMu.Unlock()
		return nil, false
	}
	eventHoldPerBot[robotID] = struct{}{}
	eventHoldPerBotMu.Unlock()

	select {
	case eventHoldSem <- struct{}{}:
	default:
		eventHoldPerBotMu.Lock()
		delete(eventHoldPerBot, robotID)
		eventHoldPerBotMu.Unlock()
		return nil, false
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-eventHoldSem
			eventHoldPerBotMu.Lock()
			delete(eventHoldPerBot, robotID)
			eventHoldPerBotMu.Unlock()
		})
	}, true
}

// acquireHoldOff takes a slot in the refused-hold budget. It returns false when
// the budget is exhausted, and the caller then answers instantly — the shape
// every caller that omits `wait` already gets.
func acquireHoldOff() (release func(), ok bool) {
	eventHoldOffOnce.Do(func() {
		eventHoldOffSem = make(chan struct{}, maxEventHolds()*eventHoldOffFactor)
	})
	select {
	case eventHoldOffSem <- struct{}{}:
	default:
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { <-eventHoldOffSem }) }, true
}

// holdOffRefusedWait answers a refused hold after a bounded pause.
//
// A refused hold answered immediately is indistinguishable on the wire from a
// served one, and a long-poll client has no interval to fall back on, so an
// instant empty batch invites an instant re-request. Each of those still costs a
// full ZRangeByScore before it gets here, so the endpoint would answer capacity
// exhaustion by *amplifying* load on the replica that is already saturated.
// Making refusal cost the caller one chunk makes it self-limiting and shaped
// like an idle hold.
//
// The pause is itself budgeted (see eventHoldOffFactor): back-pressure that can
// occupy the process without bound is not back-pressure.
func (ba *BotAPI) holdOffRefusedWait(ctx context.Context, wait time.Duration) {
	release, ok := acquireHoldOff()
	if !ok {
		return
	}
	defer release()
	holdOff := wait
	if holdOff > eventWaitChunk {
		holdOff = eventWaitChunk
	}
	_ = sleepOrDone(ctx, holdOff)
}

// sleepOrDone waits out d unless the request ends first. It reports whether the
// pause completed; false means the caller went away, which every call site
// treats as "stop".
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForEvents parks until an event lands for robotID, the hold expires, or the
// caller goes away. It returns the same shape the immediate path returns; an
// expired hold yields an empty batch, not an error, because "nothing happened"
// is a normal outcome and not a failure the caller can act on.
//
// The doorbell only decides *when* to look. Every wake-up re-reads the
// authoritative sorted set, so a bell that was lost, stolen by another waiter,
// or left over from an already-consumed event costs at most one wasted wake-up
// — never an event.
//
// `entry` is the read getEvents already performed. Threading it in rather than
// starting over is what lets the loop begin from an advanced cursor and, when
// that read filled its page, re-read immediately instead of blocking for a
// backlog no new bell will announce. See the file header for why the loop
// terminates.
func (ba *BotAPI) waitForEvents(
	c *wkhttp.Context,
	robotID string,
	botKind string,
	limit int64,
	wait time.Duration,
	entry eventPage,
) ([]*eventResp, error) {
	empty := make([]*eventResp, 0)

	reqCtx := c.Request.Context()
	deadline := time.Now().Add(wait)

	release, ok := acquireEventHold(robotID)
	if !ok {
		// At capacity, or this bot is already parked elsewhere.
		ba.holdOffRefusedWait(reqCtx, wait)
		return empty, nil
	}
	defer release()

	client := sharedEventWaitRedis(ba.ctx.GetConfig())
	if client == nil {
		return empty, nil
	}

	bellKey := botevent.BellKey(robotID)
	cursor := entry.cursor
	// A full page is evidence that more may be queued behind it, and no *new*
	// bell will ring for events that were already enqueued when this request
	// arrived. Carrying the entry read's fullness in means the first iteration
	// re-reads rather than blocking; without it an App Bot whose queue holds a
	// page of filtered events followed by a deliverable DM waits a whole chunk
	// for a message that was already there.
	//
	// Gated on `advanced` exactly as the in-loop decision is: a full entry page
	// that moved nothing would be re-read identically, so skipping the block buys
	// one wasted round trip and no progress.
	reread := entry.full && entry.advanced
	warned := false

	for {
		if reqCtx.Err() != nil {
			return empty, nil
		}
		chunk := eventWaitChunkFor(time.Until(deadline))
		if chunk == 0 {
			return empty, nil
		}

		if !reread {
			blockStarted := time.Now()
			if _, err := client.BLPop(chunk, bellKey).Result(); err != nil && err != rd.Nil {
				// A doorbell failure must not fail the request — the queue is the
				// authority — and must not collapse the hold either: returning
				// here would turn one transient blip into a ~0s answer for a 30s
				// hold. So fall through to the authoritative read and retry.
				//
				// But a failing BLPOP provides no pacing of its own. redis.Nil is
				// excluded above, so every error reaching here is a dial, write,
				// protocol or server error, and go-redis returns those after
				// MinRetryBackoff (~8ms), not after `chunk`. Left to itself this
				// loop would run at ~50-100Hz for the rest of the hold, issuing a
				// full ZRangeByScore and a log line each time — answering a Redis
				// capacity incident (`ERR max number of clients reached` is
				// classified retryable) with read and log amplification. Pay out
				// the rest of the chunk explicitly, and log once per hold.
				if !warned {
					warned = true
					ba.Warn("bot event doorbell wait failed; falling back to chunk-paced re-reads",
						zap.String("robotID", robotID), zap.Error(err))
				}
				if rest := chunk - time.Since(blockStarted); rest > 0 {
					if !sleepOrDone(reqCtx, rest) {
						return empty, nil
					}
				}
			}
		}
		reread = false

		if reqCtx.Err() != nil {
			return empty, nil
		}

		page, err := ba.readEventPage(robotID, botKind, cursor, limit)
		if err != nil {
			// Unlike a BLPOP failure, this exits immediately rather than paying
			// out the chunk. The asymmetry is deliberate: the sorted set is the
			// authority, so a read failure is a real error the caller must see,
			// and — unlike an empty batch — the client does back off on it. The
			// companion plugin's poller applies exponential backoff capped at
			// 30s for an `error` outcome and *no* backoff for `empty`
			// (openclaw-channel-octo `src/events-poll.ts`), which is exactly why
			// an instant empty answer amplifies load and an instant error does
			// not. Pacing this exit would delay a real error for no gain.
			return nil, err
		}
		if len(page.visible) > 0 {
			return page.visible, nil
		}
		// Nothing this caller may see — the page was empty, or the App Bot filter
		// emptied it. Advance past everything the read *decoded*, including the
		// events the filter dropped, so the next iteration cannot be handed the
		// same page again. This does not depend on the filter's auto-ACK ZREM
		// having succeeded (it only warns on failure), which is what makes
		// progress here structural rather than circumstantial.
		//
		// A member that failed to decode is cleared only incidentally — when a
		// decodable member with a higher id shares its page. A page in which
		// nothing decodes cannot move the cursor at all, and that residual is
		// pre-existing: the immediate path has it too, and such a queue head
		// starves that bot until the events expire.
		//
		// Skipping the block is therefore gated on the cursor having moved. A full
		// page that advanced nothing would otherwise spin unpaced for the rest of
		// the hold; falling back to the block costs one chunk and keeps it bounded.
		cursor = page.cursor
		reread = page.full && page.advanced
	}
}
