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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// SeqKeyPrefix namespaces the per-bot event id counter.
	//
	// The `counter:` segment is load-bearing, not decoration. It was `botEventSeq:`
	// while ModeKey was `botEventSeq:mode`, which made SeqKey("mode") *equal* to
	// ModeKey: a bot whose id was `mode` would have seeded and INCRed the global
	// activation flag as its own counter, coupling one bot's data path to the gate
	// for every bot. Robot ids are UUID-hex in practice, so it was unreachable — and
	// a namespace where one legal input collides with a global key is not something
	// to leave standing on that basis. TestCounterKeyCannotCollideWithModeKey pins it.
	SeqKeyPrefix = "botEventSeq:counter:"

	// ModeKey is the Redis **mirror** of the authoritative mode in
	// octo_bot_event_seq_state (see state.go).
	//
	// The mirror exists so the mode can be checked inside the same Lua script as the
	// INCR — atomically, without a DB round trip on the hot path. It is not the
	// authority: it lives in a Redis running `appendonly no`, so it regresses with an
	// RDB snapshot, and an absent mirror must therefore mean "consult the DB", never
	// "fall back to legacy". Falling back would issue GenSeq ids below everything the
	// counter already handed out — #697 mirrored.
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

	// maxAllocAttempts bounds the self-healing retries within one allocation.
	//
	// afterIssue and mirrorMissing call each other: a re-seed can land on a gate that
	// has just lost its mirror, and rebuilding the mirror ends in afterIssue again.
	// Each hop is legitimate, but the pair is mutually recursive, so a Redis that keeps
	// losing state (a FLUSHDB loop, a flapping restore) could recurse until the stack
	// gives out — a process crash, which is strictly worse than failing one enqueue.
	// Three attempts covers every single-fault recovery the tests exercise; beyond that
	// the state is changing faster than one allocation can track and the caller should
	// see an error.
	maxAllocAttempts = 3

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
// Sentinels returned by gateSource. Both are distinguishable from any legitimate id
// because a seeded counter is never negative: every floor is >= 0 and INCR only
// rises. Callers match them **exactly** rather than testing `v < 0`, so a counter
// that somehow went negative (a manual SET, a corrupted restore) surfaces as an
// error instead of being mistaken for a sentinel and silently degrading.
const (
	// gateNotActivated: the mode mirror does not say incr.
	gateNotActivated = -1

	// gateCounterMissing: the mirror says incr but the counter key is gone.
	//
	// This guard is why the script does an EXISTS. `INCR` on a missing key returns
	// **1**, and the only thing that would otherwise stop an unseeded INCR is
	// `seeded` — a process-local map guarding Redis state. So a partial restore, a
	// stray DEL, or a FLUSHDB that takes the counter but leaves the mirror, with the
	// process still up, would renumber that bot's events from 1: arbitrarily far
	// below its consumer's cursor, and worse than a snapshot rollback because
	// recovery would require re-issuing the bot's entire historical range before one
	// event became visible again. Failing before issuing is strictly better than
	// detecting it afterwards. (Review finding 2.2.)
	gateCounterMissing = -2
)

const gateSource = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return -1 end
if redis.call('EXISTS', KEYS[2]) == 0 then return -2 end
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
	Set(key string, value interface{}, expiration time.Duration) *rd.StatusCmd
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

// healCounter is a counter that is both visible to Prometheus in production and
// readable by a test.
//
// Both halves are needed. The Prometheus counter is the only way an operator learns
// that Redis lost state — these four events all *self-heal*, so without a metric they
// produce no error, no failed enqueue and no log an alert could key on, and the RDB
// safety margin degrades silently. The atomic is what lets the tests assert the heal
// actually fired, without pulling prometheus/testutil into production code.
//
// Registered through promauto on the default registry, matching pkg/i18n/details.go
// and modules/oidc/metrics.go. No labels: four distinct names beat one metric with a
// reason label, and the brief asks for low cardinality.
type healCounter struct {
	n atomic.Int64
	m prometheus.Counter
}

func newHealCounter(name, help string) *healCounter {
	return &healCounter{m: promauto.NewCounter(prometheus.CounterOpts{Name: name, Help: help})}
}

func (c *healCounter) inc()        { c.n.Add(1); c.m.Inc() }
func (c *healCounter) load() int64 { return c.n.Load() }

// resetForTest zeroes only the test-facing half; a Prometheus counter cannot decrease.
func (c *healCounter) resetForTest() { c.n.Store(0) }

var (
	seqClientOnce sync.Once
	seqClient     *rd.Client

	// seeded records which bots this process has already seeded. Purely an
	// optimisation: the seed is idempotent, so a cold cache costs one extra
	// round trip, never correctness.
	seeded sync.Map

	// seedFailures counts seeds that could not be established.
	seedFailures = newHealCounter("dmwork_bot_event_seq_seed_failure_total",
		"Bot event id seeds that could not be established; the enqueue was refused.")

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

	// countersLost counts allocations that found the mirror set but the counter key
	// gone, and mirrorRebuilds counts mirrors rebuilt from the DB authority. Both
	// mean Redis lost data; both are self-healed, and both are worth alerting on.
	countersLost = newHealCounter("dmwork_bot_event_seq_counter_lost_total",
		"Allocations that found the mode mirror set but the per-bot counter key gone.")
	mirrorRebuilds = newHealCounter("dmwork_bot_event_seq_mirror_rebuild_total",
		"Times the Redis mode mirror was rebuilt from the authoritative DB row.")

	// highWaterWriteFailures counts durable high-water writes that did not land.
	// The rollback recovery story depends on that mark, so a rising count means the
	// RDB safety margin has degraded even though nothing else looks wrong.
	highWaterWriteFailures = newHealCounter("dmwork_bot_event_seq_high_water_write_failure_total",
		"Durable high-water writes that failed; the RDB rollback safety margin has degraded.")

	// rollbacksDetected counts regressions caught and self-healed. Non-zero means
	// Redis lost data; the events issued in the lost window are gone regardless,
	// but delivery recovers without a runbook.
	rollbacksDetected = newHealCounter("dmwork_bot_event_seq_rollback_total",
		"Counter regressions detected and self-healed; non-zero means Redis lost data.")

	// expectedMode is read once. Re-reading os.Getenv per allocation would put a
	// syscall on the hottest producer path for a value that cannot change.
	expectedMode = strings.TrimSpace(os.Getenv(ExpectedModeEnv))
)

// SeedFailures reports how many allocations failed at the seed step.
func SeedFailures() int64 { return seedFailures.load() }

// RollbacksDetected reports how many counter regressions were caught and healed.
func RollbacksDetected() int64 { return rollbacksDetected.load() }

// CountersLost reports how many allocations found the counter key missing.
func CountersLost() int64 { return countersLost.load() }

// MirrorRebuilds reports how many times the mode mirror was rebuilt from the DB.
func MirrorRebuilds() int64 { return mirrorRebuilds.load() }

// HighWaterWriteFailures reports durable high-water writes that failed.
func HighWaterWriteFailures() int64 { return highWaterWriteFailures.load() }

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
	return allocate(ctx, client, robotID, 0)
}

// allocate is nextEventID with the self-healing attempt counter threaded through.
func allocate(ctx *config.Context, client Scripter, robotID string, attempt int) (int64, error) {
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
		switch v {
		case gateNotActivated:
			return mirrorMissing(ctx, client, robotID, attempt+1)
		case gateCounterMissing:
			// The mirror says activated but the counter is gone. Re-seed from the
			// durable floors and retry; issuing would renumber from 1.
			countersLost.inc()
			seeded.Delete(robotID)
			lastPersisted.Delete(robotID)
			if err := reseed(ctx, client, robotID); err != nil {
				return 0, err
			}
			retried, err := runGate(client, robotID)
			if err != nil {
				return 0, err
			}
			if retried < 0 {
				return 0, fmt.Errorf("botevent: counter for %q still unusable after re-seed (gate=%d)", robotID, retried)
			}
			return afterIssue(ctx, client, robotID, retried, attempt+1)
		}
		return afterIssue(ctx, client, robotID, v, attempt)
	}

	mode, err := client.Get(ModeKey).Result()
	if err != nil && err != rd.Nil {
		return 0, fmt.Errorf("botevent: read allocator mode: %w", err)
	}
	if mode != ModeIncr {
		return mirrorMissing(ctx, client, robotID, attempt+1)
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
	if v < 0 {
		return 0, fmt.Errorf("botevent: gate returned %d for %q immediately after a "+
			"successful seed; the mode mirror or counter is being mutated concurrently", v, robotID)
	}
	return afterIssue(ctx, client, robotID, v, attempt)
}

// afterIssue validates the freshly allocated id against what this process has
// already issued, then advances the durable high-water mark.
//
// The check is the rollback detector. INCR is monotonic while the key survives, so
// an id that does not advance can only mean the counter regressed beneath us — an
// RDB-loss restart, a FLUSHDB, a manual DEL. Without this, a long-running replica
// would keep INCRing the regressed counter and every id it issues would sit below
// live consumer cursors, invisible, until the counter climbed back on its own.
func afterIssue(ctx *config.Context, client Scripter, robotID string, v int64, attempt int) (int64, error) {
	if attempt >= maxAllocAttempts {
		return 0, fmt.Errorf("botevent: gave up allocating for %q after %d self-healing "+
			"attempts; Redis state is changing faster than one allocation can follow",
			robotID, attempt)
	}
	if prev, regressed := checkRegression(robotID, v); regressed {
		rollbacksDetected.inc()
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
			return mirrorMissing(ctx, client, robotID, attempt+1)
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
		seedFailures.inc()
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

// mirrorMissing handles a mode mirror that does not say incr.
//
// This is where the Redis key stops being an authority. Before the state table
// existed, an absent mirror meant "fall back to legacy" — which after activation
// issues GenSeq ids below everything the counter has handed out, i.e. #697 mirrored.
// Now the DB row decides:
//
//   - row says activated  → the mirror was lost or rolled back. Rebuild it from the
//     authority, re-seed, and carry on. Self-healing, no runbook.
//   - row says legacy     → genuinely pre-activation. Legacy is correct.
//   - row unreadable      → cannot tell. If this replica was told to expect incr,
//     fail closed; otherwise assume pre-activation (a missing table is what a
//     pre-migration deploy looks like).
func mirrorMissing(ctx *config.Context, client Scripter, robotID string, attempt int) (int64, error) {
	if attempt >= maxAllocAttempts {
		return 0, fmt.Errorf("botevent: gave up resolving the allocator mode for %q after %d "+
			"attempts; the mode mirror is being lost repeatedly", robotID, attempt)
	}
	st, err := ReadState(ctx)
	if err != nil {
		if expectedMode == ModeIncr {
			return 0, fmt.Errorf("botevent: %s=%s but the allocator state is unreadable (%v); "+
				"refusing to fall back to the legacy allocator, whose lower ids would land "+
				"below live consumer cursors", ExpectedModeEnv, expectedMode, err)
		}
		return legacyEventID(ctx, robotID)
	}
	if !st.Activated() {
		if expectedMode == ModeIncr {
			return 0, fmt.Errorf("botevent: %s=%s but the allocator state says legacy "+
				"(epoch=%d); refusing to fall back", ExpectedModeEnv, expectedMode, st.Epoch)
		}
		return legacyEventID(ctx, robotID)
	}

	// Authoritative state says activated, so the mirror is stale. Rebuild and retry.
	mirrorRebuilds.inc()
	if err := client.Set(ModeKey, ModeIncr, 0).Err(); err != nil {
		return 0, fmt.Errorf("botevent: rebuild mode mirror for epoch %d: %w", st.Epoch, err)
	}
	seeded.Delete(robotID)
	lastPersisted.Delete(robotID)
	if err := reseed(ctx, client, robotID); err != nil {
		return 0, err
	}
	v, err := runGate(client, robotID)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("botevent: gate still refuses (%d) for %q after rebuilding the "+
			"mirror at epoch %d", v, robotID, st.Epoch)
	}
	return afterIssue(ctx, client, robotID, v, attempt+1)
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
		highWaterWriteFailures.inc()
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
	// Valid results are a positive id, or one of the two sentinels. Anything else --
	// zero, or a value below the lowest sentinel -- means the counter holds something
	// it cannot have produced, and issuing it could put an event under a live cursor.
	if id == 0 || id < gateCounterMissing {
		return 0, fmt.Errorf("botevent: counter for %q returned %d, which is neither a valid id "+
			"nor a known sentinel; refusing to issue it", robotID, id)
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
	// The floor recorded at activation, validated against the observed maximum at
	// that time. A backstop for the case where every Redis-side source has been lost
	// at once and the durable mark has not caught up yet.
	if stateFloor := stateFloorOrZero(ctx); stateFloor > floor {
		floor = stateFloor
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
	seedFailures.resetForTest()
	rollbacksDetected.resetForTest()
	countersLost.resetForTest()
	mirrorRebuilds.resetForTest()
	highWaterWriteFailures.resetForTest()
}

// SetExpectedModeForTest overrides the env-derived expectation and returns a
// restore function.
func SetExpectedModeForTest(mode string) func() {
	prev := expectedMode
	expectedMode = mode
	return func() { expectedMode = prev }
}
