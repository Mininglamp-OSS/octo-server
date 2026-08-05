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
// The objection is durability: production runs `appendonly no` with only RDB
// snapshots (`save 3600 1 300 100 60 10000`), so a crash loses the last 60–300
// seconds and the counter moves backwards.
//
// # Co-recovery guarantees uniqueness, NOT monotonicity (review finding)
//
// Being in the same Redis instance as the queue does buy something real: if the
// snapshot is at T0 with counter `C0` and queue max `S0 <= C0`, a crash restores
// both, so the members that could have collided are gone along with the counter's
// advance. Resuming from `C0+1` therefore cannot produce a **duplicate**.
//
// It does NOT buy the other invariant, and an earlier revision of this comment
// wrongly treated the two as one. **Consumer cursors are external state and do not
// roll back with Redis.** A bot that read up to 49900 still holds 49900 after the
// counter regresses to 49000, so ids 49001..49900 are re-issued *below* a live
// cursor and are permanently invisible — #697 re-inflicted by a crash. On this
// axis a bare INCR counter is strictly worse than GenSeq, whose `min_seq` lives in
// MySQL and does not regress when Redis does.
//
// Two mechanisms close it, because neither suffices alone:
//
//   - **A durable high-water mark in MySQL** (`seq` row `botEventHigh:{robotID}`),
//     advanced roughly every `highWaterInterval` ids and folded into every seed's
//     floor. It does not share Redis's recovery domain, so it survives an RDB
//     rollback. Persisting every id would be a DB write per allocation; persisting
//     every ~1000 means the mark can trail the true maximum by at most that much,
//     which `seedSafetyMargin` (2 × step) already covers.
//   - **Rollback detection in the issuing process.** The durable mark only helps if
//     something re-seeds, and a long-running replica would otherwise keep INCRing
//     the regressed counter forever — its `seeded` entry is already set. So every
//     allocation is compared against the last id this process issued for that bot;
//     a value that did not advance means the counter regressed, and the allocation
//     re-seeds and retries. That makes recovery automatic rather than a runbook.
//
// The high-water write uses `GREATEST(min_seq, VALUES(min_seq))`. It must: an
// unconditional `ON DUPLICATE KEY UPDATE` is exactly how `GenSeq` lets a lagging
// replica move a floor backwards, which is the root cause of #697. Repeating that
// mistake in the mechanism meant to fix it would be quite something.
//
// The counter must still stay in the same Redis instance and db as the queue —
// splitting them costs the uniqueness half of the argument above.
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
	"os"
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

	// HighWaterKeyPrefix names the durable high-water row in the `seq` table.
	//
	// It reuses that table rather than adding one: the schema (`key`, `min_seq`) is
	// exactly right, the seed already reads that table for the legacy ceiling, and
	// it needs no migration. The key namespace is disjoint from GenSeq's
	// (`seq:robotEventSeq:…`), so neither reads the other's rows.
	HighWaterKeyPrefix = "botEventHigh:"

	// ExpectedModeEnv makes a lost or rolled-back mode key fail closed instead of
	// silently degrading to legacy.
	//
	// `ModeKey` lives in Redis, so it regresses under exactly the RDB rollback
	// described above — and dropping to legacy then issues *lower* ids beneath live
	// cursors, the same loss mirrored. Once activation is verified, roll this out as
	// `incr` and a missing mode refuses the enqueue instead. Same shape as #627's
	// OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE. Leave it unset until then: a replica
	// expecting `incr` before the flip fails every enqueue closed.
	ExpectedModeEnv = "OCTO_BOTEVENT_EXPECTED_MODE"

	// highWaterInterval is how many ids may be issued between durable writes. One
	// legacy block, so the mark trails the truth by less than seedSafetyMargin.
	highWaterInterval = legacyGenSeqStep

	// rollbackTolerance is how far below this process's high-water an allocation may
	// land before it is treated as a counter regression rather than ordinary
	// concurrency.
	//
	// A tolerance is unavoidable. `INCR` is atomic, but concurrent callers do not
	// *observe* their results in issue order: with N allocations in flight, the
	// caller holding 100 can reach the check after the caller holding 140. Comparing
	// against the running maximum with no slack would flag that as a regression,
	// re-seed, and jump the counter — which is what an earlier revision did, and the
	// concurrency test caught it as a 6000-wide gap across 200 allocations.
	//
	// One legacy block of slack is far above any real in-flight count (bounded in
	// practice by msgSem's 100 slots and the client pool) and far below a real
	// rollback, which loses 60–300 seconds of a live queue's ids. The gap it leaves
	// is a regression smaller than this bound: those go undetected here, and are
	// corrected by the next seed, since every seed's floor includes the durable mark.
	rollbackTolerance = legacyGenSeqStep

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

// HighWaterSeqKey returns the `seq` table key holding robotID's durable
// high-water mark. The `seq:` prefix matches how GenSeq namespaces its own rows.
func HighWaterSeqKey(robotID string) string { return "seq:" + HighWaterKeyPrefix + robotID }

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
// gateNotActivated is the sentinel gateSource returns when the mode does not match.
//
// It is distinguishable from any legitimate id because a seeded counter is never
// negative: every floor is >= 0 and INCR only rises. Callers match it **exactly**
// rather than testing `v < 0`, so a counter that somehow went negative (a manual
// SET, a corrupted restore) surfaces as an error instead of being mistaken for
// "not activated" and silently degrading to the legacy allocator.
const gateNotActivated = -1

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

	// lastIssued is the highest id this process has handed out per bot. It is the
	// rollback detector: Redis INCR cannot go backwards on its own, so an
	// allocation that fails to advance past this means the counter regressed
	// underneath us (an RDB-loss restart, or a flushed key).
	lastIssued sync.Map

	// seedLocks single-flights the seed per bot; see reseed.
	seedLocks sync.Map

	// lastPersisted tracks how far the durable high-water mark has been advanced,
	// so the DB write happens once per highWaterInterval rather than per id.
	lastPersisted sync.Map

	// rollbacksDetected counts regressions caught and self-healed. Non-zero means
	// Redis lost data; the events issued in the lost window are gone regardless,
	// but delivery recovers without a runbook.
	rollbacksDetected atomic.Int64

	// expectedMode is read once. Re-reading os.Getenv per allocation would put a
	// syscall on the hottest producer path for a value that cannot change.
	expectedMode = strings.TrimSpace(os.Getenv(ExpectedModeEnv))
)

// SeedFailures reports how many allocations failed at the seed step.
func SeedFailures() int64 { return seedFailures.Load() }

// RollbacksDetected reports how many counter regressions were caught and healed.
func RollbacksDetected() int64 { return rollbacksDetected.Load() }

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
		if v == gateNotActivated {
			return modeLost(ctx, robotID)
		}
		return afterIssue(ctx, client, robotID, v)
	}

	mode, err := client.Get(ModeKey).Result()
	if err != nil && err != rd.Nil {
		return 0, fmt.Errorf("botevent: read allocator mode: %w", err)
	}
	if mode != ModeIncr {
		return modeLost(ctx, robotID)
	}

	// Activated and this process has not seeded this bot yet. Seed before the
	// first INCR, never after: an id handed out below the floor is exactly the
	// unreachable-event bug.
	if err := reseed(ctx, client, robotID); err != nil {
		return 0, err
	}

	v, err := runGate(client, robotID)
	if err != nil {
		return 0, err
	}
	if v == gateNotActivated {
		return modeLost(ctx, robotID)
	}
	return afterIssue(ctx, client, robotID, v)
}

// afterIssue validates the freshly allocated id against what this process has
// already issued, then advances the durable high-water mark.
//
// The check is the rollback detector. INCR is monotonic while the key survives, so
// an id that does not advance can only mean the counter regressed beneath us — an
// RDB-loss restart, a FLUSHDB, a manual DEL. Without this, a long-running replica
// would keep INCRing the regressed counter and every id it issues would sit below
// live consumer cursors, invisible, until the counter climbed back on its own.
func afterIssue(ctx *config.Context, client Scripter, robotID string, v int64) (int64, error) {
	if prev, regressed := checkRegression(robotID, v); regressed {
		rollbacksDetected.Add(1)
		// Re-seed from the durable floor and retry once. One retry is enough: the
		// seed raises the counter above the high-water mark, so the next INCR
		// cannot land beneath it again unless Redis regresses a second time in
		// between, which the next allocation would catch in turn.
		seeded.Delete(robotID)
		lastPersisted.Delete(robotID)
		if err := reseed(ctx, client, robotID); err != nil {
			return 0, err
		}
		retried, err := runGate(client, robotID)
		if err != nil {
			return 0, err
		}
		if retried == gateNotActivated {
			return modeLost(ctx, robotID)
		}
		if retried <= prev {
			// The floor did not clear the ids this process already handed out, so
			// issuing would put an event below a cursor we know a client can hold.
			// Refuse rather than lose it silently.
			return 0, fmt.Errorf("botevent: counter for %q regressed to %d and re-seeding "+
				"only reached %d, which is still at or below the %d already issued by this "+
				"process; refusing to issue an id below a live consumer cursor",
				robotID, v, retried, prev)
		}
		v = retried
	}
	recordIssued(robotID, v)
	persistHighWater(ctx, robotID, v)
	return v, nil
}

// checkRegression reports this process's high-water for robotID and whether v is
// far enough below it to mean the counter regressed. See rollbackTolerance for why
// "far enough" rather than "at all".
func checkRegression(robotID string, v int64) (int64, bool) {
	prev, ok := lastIssued.Load(robotID)
	if !ok {
		return 0, false
	}
	high := prev.(int64)
	return high, v+rollbackTolerance <= high
}

// recordIssued advances this process's high-water monotonically.
//
// Compare-and-swap rather than a plain Store: concurrent callers arrive out of
// order, and a Store would let a later-arriving lower id overwrite the maximum,
// which would then make the *next* allocation look like a regression.
func recordIssued(robotID string, v int64) {
	for {
		prev, loaded := lastIssued.Load(robotID)
		if !loaded {
			if _, already := lastIssued.LoadOrStore(robotID, v); !already {
				return
			}
			continue
		}
		high := prev.(int64)
		if v <= high {
			return
		}
		if lastIssued.CompareAndSwap(robotID, high, v) {
			return
		}
	}
}

// reseed seeds the counter and marks the bot seeded, counting failures.
//
// Single-flighted per bot, with a double check inside the lock. Concurrent first
// allocations would otherwise each seed: the ones that lose the race run *after*
// the winner has already issued ids and advanced the durable mark, so their floor
// is computed from that mark and lands seedSafetyMargin above the live counter —
// jumping it forward and burning a block of ids per racing caller. The seed is
// still idempotent (the Lua only raises), so this is about not moving the counter
// needlessly, not about correctness of a single seed.
func reseed(ctx *config.Context, client Scripter, robotID string) error {
	mu := seedMutex(robotID)
	mu.Lock()
	defer mu.Unlock()
	if _, done := seeded.Load(robotID); done {
		return nil
	}
	if err := seedCounter(ctx, client, robotID); err != nil {
		seedFailures.Add(1)
		return fmt.Errorf("botevent: seed event id counter for %q: %w", robotID, err)
	}
	seeded.Store(robotID, struct{}{})
	return nil
}

// seedMutex returns the per-bot seed lock, creating it once.
func seedMutex(robotID string) *sync.Mutex {
	if mu, ok := seedLocks.Load(robotID); ok {
		return mu.(*sync.Mutex)
	}
	actual, _ := seedLocks.LoadOrStore(robotID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// modeLost handles a mode key that is absent when the caller may have expected it.
//
// Unset expectation means pre-activation, and legacy is correct. An explicit
// `incr` expectation means the mode was lost or rolled back after activation, and
// legacy would then issue ids from a lower GenSeq block — beneath live cursors, the
// same loss mirrored. Fail closed instead.
func modeLost(ctx *config.Context, robotID string) (int64, error) {
	if expectedMode == ModeIncr {
		return 0, fmt.Errorf("botevent: %s=%s but %s is not %q; refusing to fall back to the "+
			"legacy allocator, whose lower ids would land below live consumer cursors",
			ExpectedModeEnv, expectedMode, ModeKey, ModeIncr)
	}
	return legacyEventID(ctx, robotID)
}

// persistHighWater advances the durable mark at most once per highWaterInterval.
//
// Best-effort on purpose: the id is already issued and the enqueue must not fail
// because a bookkeeping write did. A missed write only shortens how far the mark
// trails, which seedSafetyMargin absorbs.
func persistHighWater(ctx *config.Context, robotID string, v int64) {
	if prev, ok := lastPersisted.Load(robotID); ok && v-prev.(int64) < highWaterInterval {
		return
	}
	// The mark records what has been issued, not a reservation above it. Writing
	// `v + interval` would compound: every seed adds seedSafetyMargin on top of the
	// mark, so a mark that already ran ahead would push the counter forward by
	// interval + margin each time it was consulted.
	mark := v
	// GREATEST, never a bare assignment. An unconditional overwrite is precisely how
	// GenSeq lets a lagging replica drag a floor backwards (the root cause of #697),
	// and this row is a floor.
	if _, err := ctx.DB().InsertBySql(
		"insert into `seq`(`key`,min_seq,step) values(?,?,0) "+
			"on duplicate key update min_seq = GREATEST(min_seq, VALUES(min_seq))",
		HighWaterSeqKey(robotID), mark).Exec(); err != nil {
		return
	}
	lastPersisted.Store(robotID, v)
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
	if id < gateNotActivated || id == 0 {
		return 0, fmt.Errorf("botevent: counter for %q returned %d, which is neither a valid id "+
			"nor the not-activated sentinel; refusing to issue it", robotID, id)
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
// Three sources are needed. The queue's max score covers ids already enqueued —
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
	// The durable mark is the only floor source that does not share Redis's
	// recovery domain, so it is what makes a post-activation RDB rollback
	// survivable. The other two both regress with the counter (queue) or stop
	// advancing at activation (legacy row).
	durableMax, err := highWaterCeiling(ctx, robotID)
	if err != nil {
		return err
	}
	floor := queueMax
	if legacyMax > floor {
		floor = legacyMax
	}
	if durableMax > floor {
		floor = durableMax
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

// highWaterCeiling returns the durable high-water mark for this bot, or 0.
//
// Unlike the queue ceiling and the legacy row, this keeps advancing after
// activation, which is what lets a seed recover from a Redis rollback.
func highWaterCeiling(ctx *config.Context, robotID string) (int64, error) {
	if ctx == nil {
		return 0, errors.New("nil ctx, cannot read high-water mark")
	}
	var mark int64
	if _, err := ctx.DB().Select("min_seq").From("`seq`").
		Where("`key`=?", HighWaterSeqKey(robotID)).Load(&mark); err != nil {
		return 0, fmt.Errorf("read high-water mark: %w", err)
	}
	return mark, nil
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
	for _, m := range []*sync.Map{&seeded, &lastIssued, &lastPersisted, &seedLocks} {
		m.Range(func(k, _ interface{}) bool {
			m.Delete(k)
			return true
		})
	}
	seedFailures.Store(0)
	rollbacksDetected.Store(0)
}

// SetExpectedModeForTest overrides the env-derived expectation and returns a
// restore function.
func SetExpectedModeForTest(mode string) func() {
	prev := expectedMode
	expectedMode = mode
	return func() { expectedMode = prev }
}
