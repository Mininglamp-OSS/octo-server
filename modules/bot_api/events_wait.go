package bot_api

import (
	"os"
	"strconv"
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
// do one). Instead producers ring a per-bot doorbell list at the two enqueue
// chokepoints and the waiter blocks on it with BLPOP, so the blocking happens
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
// Producers keep ringing on the shared pool: LPUSH/LTRIM/EXPIRE never block.

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

// maxEventHolds resolves the concurrent-hold cap, overridable for deployments
// with unusual bot counts. A non-positive or unparseable value falls back to
// the default rather than disabling the cap.
func maxEventHolds() int {
	if raw := os.Getenv("OCTO_BOT_EVENTS_MAX_HOLDS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxEventHolds
}

var (
	eventWaitRedisOnce sync.Once
	eventWaitRedis     *rd.Client

	eventHoldOnce sync.Once
	eventHoldSem  chan struct{}

	eventHoldPerBotMu sync.Mutex
	eventHoldPerBot   = map[string]struct{}{}
)

// sharedEventWaitRedis builds the process-wide dedicated client for BLPOP.
// Built through octoredis so TLS options are honoured and the redis chokepoint
// guard (pkg/redis TestNoRawRedisClientOutsideChokepoint) stays satisfied.
func sharedEventWaitRedis(cfg *config.Config) *rd.Client {
	eventWaitRedisOnce.Do(func() {
		holds := maxEventHolds()
		eventWaitRedis = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
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
// false when either is unavailable; callers then answer immediately with an
// empty batch, which is exactly the pre-long-poll behavior. Refusing to hold is
// a degradation, never an error — matching the fail-open posture of the shared
// rate-limit middleware.
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

// waitForEvents parks until an event lands for robotID, the hold expires, or the
// caller goes away. It returns the same shape the immediate path returns; an
// expired hold yields an empty batch, not an error, because "nothing happened"
// is a normal outcome and not a failure the caller can act on.
//
// The doorbell only decides *when* to look. Every wake-up re-reads the
// authoritative sorted set from the caller's cursor, so a bell that was lost,
// stolen by another waiter, or left over from an already-consumed event costs
// at most one wasted wake-up — never an event.
func (ba *BotAPI) waitForEvents(
	c *wkhttp.Context,
	robotID string,
	sinceEventID int64,
	limit int64,
	botKind string,
	wait time.Duration,
) ([]*eventResp, error) {
	empty := make([]*eventResp, 0)

	release, ok := acquireEventHold(robotID)
	if !ok {
		// At capacity, or this bot is already parked elsewhere. Degrade to the
		// immediate answer rather than erroring.
		return empty, nil
	}
	defer release()

	client := sharedEventWaitRedis(ba.ctx.GetConfig())
	if client == nil {
		return empty, nil
	}

	reqCtx := c.Request.Context()
	bellKey := botevent.BellKey(robotID)
	deadline := time.Now().Add(wait)

	for {
		if reqCtx.Err() != nil {
			return empty, nil
		}
		chunk := eventWaitChunkFor(time.Until(deadline))
		if chunk == 0 {
			return empty, nil
		}

		if _, err := client.BLPop(chunk, bellKey).Result(); err != nil && err != rd.Nil {
			// A doorbell failure must not fail the request: the queue is the
			// authority and the caller can simply poll again. Log and answer
			// empty, which is indistinguishable from an idle hold.
			ba.Warn("bot event doorbell wait failed",
				zap.String("robotID", robotID), zap.Error(err))
			return empty, nil
		}

		if reqCtx.Err() != nil {
			return empty, nil
		}

		results, err := ba.getEventsResult(robotID, sinceEventID, limit)
		if err != nil {
			return nil, err
		}
		results = ba.filterAppBotEvents(botKind, robotID, results)
		if len(results) > 0 {
			return results, nil
		}
		// Nothing visible to this caller. That includes the case where the
		// batch was non-empty but the App Bot filter emptied it: those events
		// were auto-ACK'd out of the queue, so looping cannot spin on them, and
		// answering "here is nothing" early would waste the hold the caller
		// asked for.
	}
}
