package botevent

// #697: the score allocator for `robotEvent:{robotID}`.
//
// # Why this package owns it
//
// Bot event delivery (`POST /v1/bot/events`) paginates the queue with an
// **exclusive** lower bound — `ZRANGEBYSCORE key (cursor +inf` — and acks by
// score — `ZRemRangeByScore(id, id)`. Both are only correct if scores are
// strictly monotonic and unique per queue.
//
// They were neither. Scores came from octo-lib `GenSeq`, a **per-process** HiLo
// block allocator (`config/seq.go`: `seqStep = 1000`, blocks cached in a
// package-level `seqMap` behind a process-local mutex, `min_seq` written back
// from process-local state with an unconditional
// `ON DUPLICATE KEY UPDATE min_seq=VALUES(min_seq)`). Two replicas whose
// cold-start reads both precede either write-back issue the *same* ids, and two
// replicas holding different live blocks issue ids whose order has nothing to do
// with time. Measured in production: 19 ordering inversions on block boundaries
// with a 6.7 day maximum regression, and 2624 colliding scores across three
// queues that were still accumulating. Reproduced end to end in
// `tools/genseq-repro`.
//
// The allocator lives here, next to the doorbell, because `pkg/botevent` is
// already the leaf package shared by every producer (`modules/robot`,
// `modules/group`) and the consumer (`modules/bot_api`). A copy in either side
// would drift, and a new package would need the same import-cycle escape.
//
// # Why Redis INCR, and the constraint that comes with it
//
// `INCR` on a single instance is strictly monotonic and never reuses a value, so
// both invariants hold by construction rather than by assumption.
//
// The obvious objection is durability: production runs `appendonly no` with only
// RDB snapshots (`save 3600 1 300 100 60 10000`), so a crash loses the last
// 60–300 seconds and the counter moves backwards. That is safe **here** for a
// specific reason: the counter and the queue it guards live in the same Redis
// instance and therefore the same RDB domain. If the snapshot is at T0 with
// counter `C0` and queue max score `S0 <= C0`, a crash restores both — the
// members enqueued after T0 are gone along with the counter's advance, so
// resuming from `C0+1` cannot collide with anything that survived.
//
// **That argument is why the counter must stay in the same Redis instance and db
// as the queue.** Moving it to a separate Redis, a separate db, or giving it its
// own AOF for "durability" breaks the co-recovery property and reintroduces
// exactly the collision class this package exists to remove. Adding replicas is
// fine (counter and queue lag together); splitting them is not.
//
// Production also runs `maxmemory 0` / `noeviction`, so the counter cannot be
// evicted under pressure — the failure mode that would otherwise make an `INCR`
// counter quietly unusable.
//
// # No fallback to GenSeq on failure — but there IS a legacy mode
//
// These are different things and the distinction is load-bearing.
//
// Before activation the allocator *deliberately* delegates to GenSeq, exactly as
// internal/msgextraseq does in `mode=legacy`. That is not a fallback, it is the
// pre-activation state: every replica behaves identically to the old binary, so
// deploying the new code changes nothing.
//
// After activation, a Redis failure returns an error and the caller fails the
// enqueue. It must NOT quietly reach for GenSeq, because two live id sources on
// one queue is the defect being removed.
//
// # Why activation cannot be skipped (reviewer P0)
//
// An earlier revision of this file seeded the counter above
// `max(queue ceiling, min_seq)` and switched immediately, on the theory that
// "new ids are always higher, so mixed old/new replicas are safe". That is
// backwards. A legacy replica issues ids from the *bottom* of its block upward,
// so while the new allocator hands out 7001, a legacy replica is still handing
// out 5001, 5002, … Once a consumer's cursor reaches 7001 every one of those is
// permanently invisible — the exact loss this change exists to remove, inflicted
// by the change itself. A larger safety margin makes it worse, not better: the
// margin *is* the gap that swallows legacy's ids.
//
// So the switch is gated on a DB-authoritative-style state flag and is only
// flipped once every legacy writer is gone. The flag is read atomically with the
// allocation itself (one script, see gateSource), so a flip takes effect on the
// very next allocation with no per-process cache window. The one residual window
// is a request that has already read `legacy` and not yet ZADDed when the flip
// commits; drain writes for a few seconds around the flip to close it.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	// SeqKeyPrefix namespaces the per-bot event id counter. Distinct from both
	// `robotEvent:` and `robotEventBell:` so the counter, the authoritative queue
	// and the hint can never collide.
	SeqKeyPrefix = "botEventSeq:"

	// ModeKey holds the process-wide allocator mode. Absent or anything other than
	// ModeIncr means legacy, so a deployment with no operator action behaves
	// exactly like the old binary.
	ModeKey = "botEventSeq:mode"

	// ModeIncr is the activated state: allocate from the monotonic counter.
	ModeIncr = "incr"

	// ModeLegacy is the pre-activation state, written explicitly by the operator
	// tool so preflight can tell "not yet activated" from "never configured".
	ModeLegacy = "legacy"

	// QueueKeyPrefix is the authoritative per-bot event queue. Defined here so the
	// producers and the consumer stop each spelling it with their own
	// fmt.Sprintf — the seed below has to read the very key the producers write.
	QueueKeyPrefix = "robotEvent:"

	// legacyGenSeqStep mirrors octo-lib `config.seqStep`. It is duplicated rather
	// than imported because octo-lib does not export it, and the seed's safety
	// margin is meaningless without it.
	legacyGenSeqStep = 1000

	// seedSafetyMargin is how far above the observed ceiling a first-time seed
	// starts.
	//
	// This only has to cover a block that legacy had *reserved* but not fully
	// issued before it went away — activation guarantees no legacy writer is still
	// running, so there is nothing left racing upward from below. `min_seq` may
	// itself have been moved backwards by a replica's unconditional write-back, so
	// it is a lower bound on what legacy could have handed out; two steps covers
	// one reserved block plus one such regression.
	seedSafetyMargin = 2 * legacyGenSeqStep
)

// SeqKey returns the event-id counter key for robotID. As with BellKey, robotID
// must be the identity resolved from the authenticated context, never a
// request-body value: the counter shares the queue's bot-ownership boundary.
func SeqKey(robotID string) string { return SeqKeyPrefix + robotID }

// QueueKey returns the authoritative event queue key for robotID.
func QueueKey(robotID string) string { return QueueKeyPrefix + robotID }

// seedSource raises the counter to floor if it is currently lower or unset, and
// returns the resulting value.
//
// Idempotent by construction: every replica seeds on its own first use for a given
// bot, in any order, any number of times, and they all converge. That is what lets
// activation be a single flip rather than a coordinated restart — but it does NOT
// make the *deploy* safe on its own; see the activation-gate note above. `SET` has no `GT` option in Redis
// (`GT`/`LT` belong to `EXPIRE` and `ZADD`), so the compare and the write have to
// share one script to be atomic against a concurrent seeder.
const seedSource = `
local cur = redis.call('GET', KEYS[1])
local floor = tonumber(ARGV[1])
if cur == false or tonumber(cur) < floor then
  redis.call('SET', KEYS[1], floor)
  return floor
end
return tonumber(cur)`

// seedScript sends EVALSHA and falls back to EVAL only on NOSCRIPT, matching
// bell.go and every other Lua site in this repo.
var seedScript = rd.NewScript(seedSource)

// gateSource reads the mode and allocates in one atomic step, returning -1 when
// the allocator is not activated.
//
// The atomicity is the point. Caching the mode per process — even for a second —
// would mean that during that second some replicas allocate from the counter
// while others still allocate from GenSeq: two live id sources, which is the
// defect. Reading it inside the same script as the INCR makes a flip take effect
// on the very next allocation, everywhere, with no window of divergence.
const gateSource = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return -1 end
return redis.call('INCR', KEYS[2])`

var gateScript = rd.NewScript(gateSource)

// Scripter is the go-redis surface the allocator needs: INCR, the sorted-set read
// used to observe the current ceiling, and the scripting set *rd.Script.Run
// requires. octo-lib's *redis.Conn exposes none of the scripting calls, so the
// allocator uses the instrumented raw client like the ring does.
//
// Declared as an interface so tests can substitute a fake — in particular one
// whose INCR succeeds while its seed fails, since the two have different
// consequences.
type Scripter interface {
	Get(key string) *rd.StringCmd
	Incr(key string) *rd.IntCmd
	ZRevRangeWithScores(key string, start, stop int64) *rd.ZSliceCmd
	Eval(script string, keys []string, args ...interface{}) *rd.Cmd
	EvalSha(sha1 string, keys []string, args ...interface{}) *rd.Cmd
	ScriptExists(hashes ...string) *rd.BoolSliceCmd
	ScriptLoad(script string) *rd.StringCmd
}

// Unlike the doorbell, this is not best-effort: a failed allocation fails the
// enqueue. But it cannot be patient either, and the reason is the one that forced
// the ring off the producer's goroutine in the first place.
//
// `saveRobotMessage` allocates inside a `msgSem` slot (capacity 100), held on the
// listener goroutine. Anything that slows it down holds a slot longer, and once all
// 100 are held, message fan-out stalls for **every bot in the process** — including
// bots that never send an interactive card. So a degraded Redis must cost this call
// ~1s, not ~3s.
//
// The trade is explicit: giving up fails one enqueue (one event, recoverable by a
// D8 re-tap or a resend), while waiting stalls fan-out for everyone. One retry, not
// two, for the same reason the ring uses none — a retried allocation is an
// allocation that took twice as long.
//
// This is also a real change in I/O shape versus GenSeq and belongs in the review
// record, not a footnote: GenSeq served 999 of every 1000 allocations from its
// process-local block with **no I/O at all**, while every allocation here is a Redis
// round trip. It cannot be batched back into blocks without recreating #697 (a block
// is a second id source), and the mode cannot be cached without recreating the
// mixed-source window (reviewer P0). Load-test the hottest producer before
// activation, not after.
const (
	seqDialTimeout = 500 * time.Millisecond
	seqReadTimeout = 300 * time.Millisecond
	seqPoolTimeout = 200 * time.Millisecond
	seqMaxRetries  = 1
)

var (
	seqClientOnce sync.Once
	seqClient     *rd.Client

	// seeded records which bots this process has already seeded. Purely an
	// optimisation: the seed is idempotent, so a cold cache costs one extra
	// round trip, never correctness.
	seeded sync.Map

	// seedFailures counts seeds that could not be established. Exposed for tests
	// and for the operator check; wiring it to Prometheus belongs to the card
	// observability work that owns that namespace, as bell.go notes for its own
	// counter.
	seedFailures atomic.Int64
)

// SeqClient returns the shared allocator client, built through octoredis so TLS
// options are honoured and pkg/redis's raw-client chokepoint guard stays
// satisfied.
//
// It is built from the same *config.Config as every other Redis user in the
// process, which is what keeps the counter in the queue's instance and db. See
// the co-recovery argument at the top of this file before changing that.
func SeqClient(cfg *config.Config) Scripter {
	seqClientOnce.Do(func() {
		seqClient = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = seqMaxRetries
			o.DialTimeout = seqDialTimeout
			o.ReadTimeout = seqReadTimeout
			o.WriteTimeout = seqReadTimeout
			o.PoolTimeout = seqPoolTimeout
		})
	})
	return seqClient
}

// SeedFailures reports how many allocations failed at the seed step.
func SeedFailures() int64 { return seedFailures.Load() }

// NextEventID allocates the next event id for robotID's queue.
//
// The returned value is used both as the sorted-set score and as the payload's
// `event_id`. Those two must stay equal: the consumer's cursor is a payload
// `event_id` while its reads and acks are bounded by score
// (`modules/bot_api/events.go`).
func NextEventID(ctx *config.Context, robotID string) (int64, error) {
	if ctx == nil {
		return 0, errors.New("botevent: nil ctx, cannot allocate event id")
	}
	robotID = strings.TrimSpace(robotID)
	if robotID == "" {
		return 0, errors.New("botevent: empty robotID, cannot allocate event id")
	}
	return nextEventID(ctx, SeqClient(ctx.GetConfig()), robotID)
}

// nextEventID is the testable core: it takes the client explicitly so a test can
// inject one whose seed or INCR fails.
func nextEventID(ctx *config.Context, client Scripter, robotID string) (int64, error) {
	if client == nil {
		return 0, errors.New("botevent: nil redis client, cannot allocate event id")
	}

	// Steady state after activation: one round trip that checks the mode and
	// allocates atomically.
	if _, ok := seeded.Load(robotID); ok {
		v, err := runGate(client, robotID)
		if err != nil {
			return 0, err
		}
		if v >= 0 {
			return v, nil
		}
		// Activated, then not: there is no supported deactivation, so this means
		// the mode key was cleared out from under us. Legacy is the safe answer —
		// it is what every other writer would now be doing too.
		return legacyEventID(ctx, robotID)
	}

	mode, err := client.Get(ModeKey).Result()
	if err != nil && err != rd.Nil {
		return 0, fmt.Errorf("botevent: read allocator mode: %w", err)
	}
	if mode != ModeIncr {
		return legacyEventID(ctx, robotID)
	}

	// Activated and this process has not seeded this bot yet. Seed before the
	// first INCR, never after: an id handed out below the floor is exactly the
	// unreachable-event bug.
	if err := seedCounter(ctx, client, robotID); err != nil {
		seedFailures.Add(1)
		return 0, fmt.Errorf("botevent: seed event id counter for %q: %w", robotID, err)
	}
	seeded.Store(robotID, struct{}{})

	v, err := runGate(client, robotID)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return legacyEventID(ctx, robotID)
	}
	return v, nil
}

// runGate performs the atomic mode-check-and-allocate. -1 means "not activated".
func runGate(client Scripter, robotID string) (int64, error) {
	v, err := gateScript.Run(client, []string{ModeKey, SeqKey(robotID)}, ModeIncr).Result()
	if err != nil {
		return 0, fmt.Errorf("botevent: allocate event id for %q: %w", robotID, err)
	}
	id, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("botevent: allocate event id for %q: unexpected script result %T", robotID, v)
	}
	return id, nil
}

// legacyEventID is the pre-activation allocator: the octo-lib GenSeq block
// allocator this change exists to retire.
//
// This is the ONLY permitted GenSeq call site for bot event ids, and the source
// guard in genseq_guard_test.go allows it here and nowhere else — the same shape
// internal/msgextraseq uses for its own legacy delegation. It is reached when the
// allocator has not been activated yet, which is the state every deployment
// starts in, so it is not dead code and its behaviour still matters: it is
// bug-compatible with the old binary on purpose, so that deploying the fix
// changes nothing until an operator flips the mode.
func legacyEventID(ctx *config.Context, robotID string) (int64, error) {
	if ctx == nil {
		return 0, errors.New("botevent: nil ctx, cannot allocate legacy event id")
	}
	return ctx.GenSeq(common.RobotEventSeqKey + robotID)
}

// seedCounter raises the counter above everything legacy could have handed out
// for this bot.
//
// Both sources are needed. The queue's max score covers ids already enqueued —
// including by an old replica whose block sits above a regressed `min_seq`. The
// `seq` row covers the opposite case: a bot whose queue has been fully acked (or
// has expired) still has clients holding a cursor near the last id it was ever
// issued, and seeding below that cursor would make every new event unreachable —
// the very failure this change exists to remove, self-inflicted at deploy time.
func seedCounter(ctx *config.Context, client Scripter, robotID string) error {
	queueMax, err := queueCeiling(client, robotID)
	if err != nil {
		return err
	}
	legacyMax, err := legacyCeiling(ctx, robotID)
	if err != nil {
		return err
	}
	floor := queueMax
	if legacyMax > floor {
		floor = legacyMax
	}
	if floor > 0 {
		// A bot with no queue and no seq row has never been issued an id, so it
		// has no cursor to clear and starts from 1. Adding the margin there would
		// only make the numbers less legible.
		floor += seedSafetyMargin
	}
	return seedScript.Run(client, []string{SeqKey(robotID)}, floor).Err()
}

// queueCeiling returns the highest score currently in the bot's queue, or 0.
func queueCeiling(client Scripter, robotID string) (int64, error) {
	top, err := client.ZRevRangeWithScores(QueueKey(robotID), 0, 0).Result()
	if err != nil {
		return 0, fmt.Errorf("read queue ceiling: %w", err)
	}
	if len(top) == 0 {
		return 0, nil
	}
	return int64(top[0].Score), nil
}

// legacyCeiling returns the legacy `GenSeq` block boundary for this bot's event
// sequence, or 0 when it never used one.
//
// This reads the `seq` row directly rather than calling GenSeq, which would
// allocate — and allocating from the very source being retired is how a "read"
// turns into a third live id source.
func legacyCeiling(ctx *config.Context, robotID string) (int64, error) {
	var minSeq int64
	key := fmt.Sprintf("seq:%s%s", common.RobotEventSeqKey, robotID)
	if _, err := ctx.DB().Select("min_seq").From("`seq`").
		Where("`key`=?", key).Load(&minSeq); err != nil {
		return 0, fmt.Errorf("read legacy seq row: %w", err)
	}
	return minSeq, nil
}

// ResetSeededForTest clears the process-local seed cache. Tests that seed against
// different fixtures need it; production never does.
func ResetSeededForTest() {
	seeded.Range(func(k, _ interface{}) bool {
		seeded.Delete(k)
		return true
	})
	seedFailures.Store(0)
}
