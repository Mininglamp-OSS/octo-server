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
// Two mechanisms close it, and **neither is complete in this change** — see
// Mininglamp-OSS/octo-server#704, which gates activation on finishing them:
//
//   - **A durable high-water mark in MySQL** (`seq` row `botEventHigh:{robotID}`),
//     advanced roughly every `highWaterInterval` ids and folded into every seed's
//     floor. It does not share Redis's recovery domain, so it survives an RDB
//     rollback. Persisting every id would be a DB write per allocation; persisting
//     every ~1000 keeps the mark within one interval of the truth in healthy
//     operation, which `seedSafetyMargin` (2 × step) covers. **While durable writes
//     are failing it trails without bound**, and two attempts to bound that inside
//     this PR were both wrong — see persistHighWater for the arithmetic that says why
//     the fix is a change to what the recovery floor *is*, which is #704's.
//   - **Rollback detection in the issuing process.** The durable mark only helps if
//     something re-seeds, and a long-running replica would otherwise keep INCRing
//     the regressed counter forever — its `seeded` entry is already set. So every
//     allocation is compared against the last id this process issued for that bot;
//     a value that did not advance means the counter regressed, and the allocation
//     re-seeds and retries. That makes recovery automatic rather than a runbook —
//     but the comparison is process-local and tolerance-bounded, which is the other
//     half of #704.
//
// The high-water write uses `GREATEST(min_seq, VALUES(min_seq))`. It must: an
// unconditional `ON DUPLICATE KEY UPDATE` is exactly how `GenSeq` lets a lagging
// replica move a floor backwards, which is the root cause of #697. Repeating that
// mistake in the mechanism meant to fix it would be quite something.
//
// The counter must still stay in the same Redis instance and db as the queue —
// splitting them costs the uniqueness half of the argument above.
//
// That constraint has a second consequence worth writing down next to it: the gate and
// probe scripts are multi-key with no hash tag, so they would fail CROSSSLOT under Redis
// Cluster or a slot-enforcing proxy. Not new — `modules/bot_mention/idempotency.go` has the
// same shape — and not fixable independently, because a cluster that could place the
// counter and the queue in different slots is exactly the topology the co-recovery argument
// already rules out. The two move together or not at all.
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
// So the switch is gated on a state flag whose authority is a MySQL row (state.go),
// mirrored into Redis so the gate can check it inside the same script as the
// allocation (see gateSource, and mode.go for who is allowed to believe that mirror).
// A flip takes effect on the very next allocation everywhere, because the mirror the
// gate compares against is never cached. The one residual window is a request that
// has already read `legacy` and not yet ZADDed when the flip commits; drain writes for
// a few seconds around the flip to close it.

import (
	"context"
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
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
	//
	// Its value is `incr:{epoch}`, not a bare `incr`, and it is only ever believed
	// after the authority has confirmed that generation. mode.go has the rules; the
	// short version is that a mirror alone cannot activate anything.
	ModeKey = "botEventSeq:mode"

	// ModeIncr is the activated state: allocate from the monotonic counter. Also the
	// prefix of the mirror value, and an accepted ExpectedModeEnv value.
	ModeIncr = "incr"

	// ModeLegacy is the pre-activation state: delegate to GenSeq.
	//
	// It is an accepted ExpectedModeEnv value — a replica may assert that it expects
	// *not* to be activated, which fails closed if the authority says otherwise. It is
	// deliberately not a mirror value: the mirror's only job is to claim activation, and
	// "absent" already means "consult the authority" (an earlier revision claimed the
	// operator tool wrote this string so preflight could tell "not yet activated" from
	// "never configured" — it never did, and preflight reads the DB row for that).
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
	// `incr` and an unconfirmable mode refuses the enqueue instead. Same shape as
	// #627's OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE, including that a **malformed**
	// value fails closed rather than reading as "unset" — see mode.go. Accepted values
	// are `incr` and `legacy`; leave it unset until the flip, because a replica
	// expecting `incr` beforehand fails every enqueue closed.
	ExpectedModeEnv = "OCTO_BOTEVENT_EXPECTED_MODE"

	// highWaterInterval is how many ids may be issued between durable writes. One
	// legacy block, so in healthy operation the mark trails the truth by less than
	// seedSafetyMargin with a full interval to spare. What happens when the writes stop
	// landing is bounded separately, by span rather than by count — see persistHighWater.
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

// gateSource reads the mode mirror and allocates in one atomic step, returning -1
// when the mirror does not equal the value the caller validated.
//
// The atomicity is the point, and so is the absence of a cache *here*. If the gate
// decided from a per-process copy of the mode — even a one-second-old one — then for
// that second some replicas would allocate from the counter while others still
// allocated from GenSeq: two live id sources, which is the defect. Reading the mirror
// inside the same script as the INCR makes a flip take effect on the very next
// allocation, everywhere, with no window of divergence. (What *is* cached, one level
// up, is the authority behind the mirror, under rules designed to preserve exactly
// this property — see mode.go.)
//
// ARGV[1] is the full expected mirror value including the generation, so a mirror
// that was lost, hand-written as a bare `incr`, or replaced by a different epoch
// closes the gate instead of allocating.
//
// Sentinels returned by gateSource. Both are distinguishable from any legitimate id
// because a seeded counter is never negative: every floor is >= 0 and INCR only
// rises. runGate normalises the result before returning it — anything that is neither
// a positive id nor exactly one of these two becomes an error there — so callers may
// switch on them, and a counter that somehow went negative (a manual SET, a corrupted
// restore) surfaces as an error rather than being mistaken for a sentinel.
const (
	// gateNotActivated: the mode mirror is not the generation the caller validated.
	gateNotActivated = -1

	// gateCounterMissing: the mirror matches but the counter key is gone.
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

const dropMirrorSource = `
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`

var dropMirrorScript = rd.NewScript(dropMirrorSource)

// probeSource reads the mode mirror and whether this bot's counter already exists, in one
// round trip.
//
// The second half is cross-process evidence of activation, and it is the only such evidence
// available without a DB read. A counter key exists only because some replica allocated
// from it, which can only happen after the authority said `incr`. So a process that has
// just started, reads an authority saying `legacy`, and finds the counter present knows the
// authority regressed rather than that it is pre-activation — and can refuse instead of
// issuing a GenSeq id beneath everything the counter handed out (review P1-A).
//
// Folded into the existing mirror read rather than added beside it: the pre-activation path
// is supposed to cost one Redis round trip, and a second one would be the regression review
// P1-1 removed, in a smaller size.
const probeSource = `
local mode = redis.call('GET', KEYS[1])
if mode == false then mode = '' end
return {mode, redis.call('EXISTS', KEYS[2])}`

var probeScript = rd.NewScript(probeSource)

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
// is a second id source), and the mirror the gate compares against cannot be cached
// without recreating the mixed-source window (reviewer P0) — only the *authority
// behind* it can be, which is what keeps the pre-activation path free of DB round
// trips (mode.go). Load-test the hottest producer before activation, not after.
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

	// lastPersisted throttles the durable write to once per highWaterInterval. It
	// advances on a *failed* write too, so a DB outage costs one attempt per interval
	// rather than one per event, which is the half of review P1-7 that stays fixed. It is
	// therefore NOT a record of what landed — see persistHighWater for why this revision
	// does not try to bound the gap and #704 for who does.
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

	// counterFoundWithoutAuthority counts allocations where the authority said legacy but
	// the bot's counter already existed — i.e. the authority regressed underneath an
	// activated fleet. Unlike the other counters here this one does NOT self-heal: the
	// allocation is refused, so it is also visible as failed enqueues. It is a metric
	// because it names the cause, which the failure alone does not.
	counterFoundWithoutAuthority = newHealCounter("dmwork_bot_event_seq_counter_without_authority_total",
		"Allocations refused because a counter existed while the authority said not activated.")
)

// CounterFoundWithoutAuthority reports refusals caused by a regressed authority.
func CounterFoundWithoutAuthority() int64 { return counterFoundWithoutAuthority.load() }

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
	// Reject rather than normalise. Trimming here would make the allocator key off a
	// different string than the caller's own QueueKey(robotID) / Notify(robotID), so a
	// padded id would seed one counter and enqueue onto another queue. Unreachable
	// through today's call sites, and the divergence is what makes it worth an error
	// rather than a silent fix-up (review P2-12).
	if robotID == "" || strings.TrimSpace(robotID) != robotID {
		return 0, fmt.Errorf("botevent: robotID %q is empty or padded, cannot allocate event id; "+
			"the allocator, the queue key and the doorbell must all use the identical string", robotID)
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
	if attempt >= maxAllocAttempts {
		return 0, fmt.Errorf("botevent: gave up allocating for %q after %d self-healing attempts; "+
			"Redis state is changing faster than one allocation can follow", robotID, attempt)
	}

	// Steady state after activation: the authority is already resolved and this
	// process has already seeded this bot, so one round trip checks the mirror and
	// allocates atomically. No DB access, and no cached mode decides the gate — the
	// gate compares the live mirror against the validated generation itself.
	if b := activeBelief.Load(); b != nil && b.activated {
		if _, ok := seeded.Load(robotID); ok {
			return allocateFromCounter(ctx, client, robotID, b, attempt)
		}
	}

	// Otherwise the decision needs the uncached mirror. Pre-activation this is the
	// whole cost of an allocation before delegating: one Redis round trip, with the
	// authority read amortised by the belief cache (review P1-1). The same round trip
	// reports whether this bot's counter already exists, which is the only cross-process
	// evidence of activation available without a DB read (review P1-A).
	mirror, counterExists, err := probeAllocatorState(client, robotID)
	if err != nil {
		return 0, err
	}
	res, err := resolveMode(ctx, mirror)
	if err != nil {
		return 0, err
	}
	if res.unauthorizedMirror != "" {
		// The authority denied this mirror, so the correct state for the key is absent.
		// Best-effort and compare-and-delete: a failure only means the next allocation
		// re-reads the authority, bounded by repairCooldown.
		dropUnauthorizedMirror(client, res.unauthorizedMirror)
	}
	if res.decision == decideLegacy {
		return legacyDelegate(ctx, robotID, counterExists)
	}
	b := res.belief

	// Activated. Two orderings matter here and both were wrong before.
	rebuildMirror := mirror != b.mirrorValue()
	if rebuildMirror {
		// The mirror is absent, bare, or from an older generation, so Redis lost the
		// mode key (or never had this generation's). That is evidence Redis lost data,
		// and every counter this process believes it seeded is suspect — not just this
		// bot's. Dropping the whole seeded set makes the next allocation for each of
		// them re-seed from the durable floors. The seed is idempotent and only ever
		// raises, so the cost is one extra round trip per active bot.
		mirrorRebuilds.inc()
		invalidateSeeded()
	}
	// Seed before the first INCR, never after: an id handed out below the floor is
	// exactly the unreachable-event bug.
	if err := reseed(ctx, client, robotID); err != nil {
		return 0, err
	}
	if rebuildMirror {
		// Publish the mirror only now. Writing it first opens the gate for **every**
		// bot in every replica while only this one has been raised to its floor, so any
		// goroutine holding a stale seeded entry for another bot could pass the gate and
		// INCR a counter that has not been re-seeded yet (review P1-3). Note the
		// asymmetry the ordering exists to contain: the mirror is global, the seed is
		// per-bot.
		if err := client.Set(ModeKey, b.mirrorValue(), 0).Err(); err != nil {
			return 0, fmt.Errorf("botevent: rebuild mode mirror for epoch %d: %w", b.epoch, err)
		}
	}
	return allocateFromCounter(ctx, client, robotID, b, attempt)
}

// allocateFromCounter runs the gate for a bot this process has seeded under a
// validated belief, and handles the two ways Redis can have lost state underneath it.
func allocateFromCounter(ctx *config.Context, client Scripter, robotID string, b *belief, attempt int) (int64, error) {
	v, err := runGate(client, robotID, b.mirrorValue())
	if err != nil {
		return 0, err
	}
	switch v {
	case gateNotActivated:
		// The live mirror no longer equals the generation this process validated: lost,
		// overwritten, or replaced by a newer epoch. Re-resolve from the authority.
		return gateClosed(ctx, client, robotID, attempt+1)
	case gateCounterMissing:
		// The mirror is intact but the counter key is gone. Re-seed from the durable
		// floors and retry; issuing would renumber this bot's events from 1.
		countersLost.inc()
		seeded.Delete(robotID)
		lastPersisted.Delete(robotID)
		if err := reseed(ctx, client, robotID); err != nil {
			return 0, err
		}
		retried, err := runGate(client, robotID, b.mirrorValue())
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

// gateClosed re-resolves the authority after the gate refused an allocation this
// process believed was permitted, then retries the allocation from the top.
//
// The authority read is forced rather than served from the cached belief: the gate
// closing means the mirror changed underneath a belief this process was already acting
// on, which is exactly what the cache must not answer for. If the authority still says
// activated, the retry below rebuilds the mirror; if it says legacy, refreshAuthority
// refuses rather than downgrading a process that has already issued counter ids
// (review P1-4).
//
// Dropping the seeded marker is what makes the retry take the slow path. Without it,
// allocate's post-activation fast path would run the same closed gate again and the
// pair would spin to maxAllocAttempts — which is how the tests caught this.
func gateClosed(ctx *config.Context, client Scripter, robotID string, attempt int) (int64, error) {
	if attempt >= maxAllocAttempts {
		return 0, fmt.Errorf("botevent: gave up resolving the allocator mode for %q after %d "+
			"attempts; the mode mirror is being lost or rewritten repeatedly", robotID, attempt)
	}
	if _, err := refreshAuthority(ctx, false, "", true); err != nil {
		return 0, err
	}
	seeded.Delete(robotID)
	return allocate(ctx, client, robotID, attempt)
}

// legacyDelegate hands the allocation to GenSeq, refusing when anything says the counter
// era has already begun for this bot.
//
// Two independent pieces of evidence, because they cover different processes:
//
//   - `lastIssued` — this process has itself handed out a counter id. Unreachable while
//     the positive belief is terminal, and asserted rather than assumed because the
//     consequence is a permanently invisible event.
//   - `counterExists` — *some* replica has, which can only have happened after the
//     authority said `incr`. This is what a freshly started process has instead of
//     memory, and it is the difference between "pre-activation" and "the authority
//     regressed underneath an activated fleet" (review P1-A). A GenSeq id in the second
//     case lands arbitrarily far below the bot's live cursor: #697, self-inflicted.
//
// The residual this does NOT cover is stated rather than papered over: if the counter key
// is gone *as well* — a Redis flush or restore that took it, correlated with an authority
// that reads legacy — a cold process has no evidence at all and delegates to GenSeq. Only
// `OCTO_BOTEVENT_EXPECTED_MODE=incr` closes that, and the rollout arms it after activation
// is verified (step 5), so the window is open by design during the flip itself.
//
// Note what the env guard does and does not buy, because the refusal message below used to
// get it wrong: it is a fail-closed *assertion*, not a manual override. Setting it makes an
// unconfirmable mode refuse loudly on every replica; it never lets one proceed.
func legacyDelegate(ctx *config.Context, robotID string, counterExists bool) (int64, error) {
	if prev, issued := lastIssued.Load(robotID); issued {
		return 0, fmt.Errorf("botevent: refusing to allocate a legacy id for %q after this process "+
			"already issued counter id %v for it; a GenSeq id here would land below the bot's "+
			"consumer cursor and never be delivered", robotID, prev)
	}
	if counterExists {
		counterFoundWithoutAuthority.inc()
		return 0, fmt.Errorf("botevent: the authority says the allocator is not activated, but "+
			"%q already has a counter — which only exists because some replica allocated from it, "+
			"i.e. the authority was activated and has regressed. Refusing to issue a legacy id "+
			"below the counter's range. Fix the authority: restore %s to mode=1 with the epoch and "+
			"cutover_floor it had. Setting %s=%s does NOT let this replica proceed — it is a "+
			"fail-closed assertion, so it only makes every replica refuse loudly instead of some "+
			"of them degrading",
			SeqKey(robotID), stateTable, ExpectedModeEnv, ModeIncr)
	}
	return legacyEventID(ctx, robotID)
}

// probeAllocatorState reads the mode mirror and this bot's counter existence in one round
// trip. mirror is "" when the key is absent.
func probeAllocatorState(client Scripter, robotID string) (mirror string, counterExists bool, err error) {
	v, err := probeScript.Run(client, []string{ModeKey, SeqKey(robotID)}).Result()
	if err != nil {
		return "", false, fmt.Errorf("botevent: probe allocator state for %q: %w", robotID, err)
	}
	parts, ok := v.([]interface{})
	if !ok || len(parts) != 2 {
		return "", false, fmt.Errorf("botevent: probe allocator state for %q: unexpected result %T", robotID, v)
	}
	switch m := parts[0].(type) {
	case string:
		mirror = m
	case nil:
	default:
		return "", false, fmt.Errorf("botevent: probe allocator state for %q: unexpected mirror %T", robotID, parts[0])
	}
	exists, ok := parts[1].(int64)
	if !ok {
		return "", false, fmt.Errorf("botevent: probe allocator state for %q: unexpected counter flag %T", robotID, parts[1])
	}
	return mirror, exists != 0, nil
}

// dropUnauthorizedMirror removes a mode mirror the authority denied, if it still holds the
// exact value that was denied.
//
// Compare-and-delete rather than a bare DEL: between the authority read and this call an
// operator may have activated and written a *different* generation, which must survive.
//
// Best-effort. A failure is recorded on the belief so the next allocation is answered from
// the cached denial for a short cooldown instead of re-reading the authority per
// allocation — see belief.repairFailedFor. It is not an allocation error: the authority has
// spoken, legacy is the right answer, and a Redis that rejects this DEL would be failing
// the counter's own INCR anyway.
func dropUnauthorizedMirror(client Scripter, denied string) {
	if err := dropMirrorScript.Run(client, []string{ModeKey}, denied).Err(); err != nil {
		noteMirrorRepairFailed(denied)
		unauthorizedWarn.warn("botevent: could not remove the unauthorized mode mirror; "+
			"allocations stay on the legacy allocator and the authority will be re-read shortly",
			zap.String("mirrorKey", ModeKey), zap.String("mirrorValue", denied), zap.Error(err))
	}
}

// invalidateSeeded drops every bot's seeded marker. See the mirror-rebuild branch in
// allocate for why a lost mirror invalidates more than the bot being allocated for.
//
// `seeded` is cleared **last**, and the order is load-bearing (review P1-2). These are
// separate unlocked Range passes over every active bot, so a concurrent reseed — which
// stores its own per-bot state inside seedCounter and only then stores `seeded` — can
// interleave between them. Clearing `seeded` first would let that reseed's marker survive
// while its companion state was wiped, i.e. a bot treated as seeded whose bookkeeping this
// process no longer has. Clearing it last means the worst interleaving leaves a bot
// *unseeded* with stale bookkeeping, which the next allocation fixes by re-seeding.
func invalidateSeeded() {
	for _, m := range []*sync.Map{&lastPersisted, &seeded} {
		m.Range(func(k, _ interface{}) bool {
			m.Delete(k)
			return true
		})
	}
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
		b := activeBelief.Load()
		if b == nil || !b.activated {
			// The belief cannot have been downgraded (positive is terminal), so this
			// means it was cleared underneath us — only a test does that.
			return 0, fmt.Errorf("botevent: allocator belief lost mid-allocation for %q", robotID)
		}
		retried, err := runGate(client, robotID, b.mirrorValue())
		if err != nil {
			return 0, err
		}
		if retried == gateNotActivated {
			return gateClosed(ctx, client, robotID, attempt+1)
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
	// Best-effort, and deliberately not fatal: see persistHighWater for what that costs
	// and why bounding it is #704's rather than this function's.
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

// persistHighWater advances the durable mark, throttled to once per highWaterInterval.
//
// # It is best-effort, and this revision stops claiming otherwise
//
// Two previous revisions put a bound here and both were wrong, in opposite directions:
// a failure *count* that the throttle short-circuited before ever consulting (so only
// 1 allocation in `highWaterInterval` failed and 999 succeeded against a frozen mark),
// then a *span* measured from the durable mark — which is over its bound on the very
// first allocation after every seed, because `seedCounter` sets the counter to
// `base + seedSafetyMargin` while the mark stays at `base`. That made the refusal fire
// on the first failed write with no grace, drop the event, and turn the recovery probe
// into a storm inside `msgSem` slots.
//
// Working the arithmetic out properly says why a third local patch would fail too:
//
//	A seed sets the counter to S = max(sources) + seedSafetyMargin.
//	A recovery seed replays the *same* computation over the surviving MySQL sources, so
//	it lands at S again. Ids issued after a seed are S+1, S+2, …, all of them above S.
//
// So the first id after any seed is already outside what a recovery from the unchanged
// mark could reach — the exposure does not start after some grace, it starts
// immediately. Closing it requires the mark to advance *at seed time*, and a mark that
// records the seeded value feeds back into the next seed's floor, so every re-seed
// compounds it by a block (which also breaks the no-op property
// TestSeedIsIdempotentAndNeverLowers guards). That is a change to what the recovery
// floor *is*, not a tweak to a comparison — and it is exactly the question
// Mininglamp-OSS/octo-server#704 owns, together with rollback detection, which has the
// same shape ("what state can regress, who detects it, what re-establishes the floor").
// Both reviewers recommended moving it there in one piece rather than iterating here.
//
// # So what this function guarantees, plainly
//
// In healthy operation the mark trails the highest issued id by less than
// `highWaterInterval`, which is what makes a post-rollback seed land above what was
// issued before the rollback. **While durable writes are failing it trails without
// bound**, and an unbounded number of ids can be issued against a frozen mark. A Redis
// rollback plus a drained queue after that can then seed below a live consumer cursor.
// `dmwork_bot_event_seq_high_water_write_failure_total` is the signal, and #704 is the
// gate: it must be closed before an operator activates, so this exposure is not
// reachable by merging.
//
// The throttle re-arms on failure, so a DB outage costs one INSERT attempt per
// `highWaterInterval` rather than one per event. That much was never contested and is
// the half of the earlier finding (P1-7) that stays fixed here.
func persistHighWater(ctx *config.Context, robotID string, v int64) {
	if prev, ok := lastPersisted.Load(robotID); ok && v-prev.(int64) < highWaterInterval {
		return
	}
	// The mark records what has been issued, not a reservation above it. Writing
	// `v + interval` would compound: every seed adds seedSafetyMargin on top of the
	// mark, so a mark that already ran ahead would push the counter forward by
	// interval + margin each time it was consulted.
	mark := v
	deadline, cancel := context.WithTimeout(context.Background(), authorityTimeout)
	defer cancel()
	// GREATEST, never a bare assignment. An unconditional overwrite is precisely how
	// GenSeq lets a lagging replica drag a floor backwards (the root cause of #697),
	// and this row is a floor.
	if _, err := ctx.DB().InsertBySql(
		"insert into `seq`(`key`,min_seq,step) values(?,?,0) "+
			"on duplicate key update min_seq = GREATEST(min_seq, VALUES(min_seq))",
		HighWaterSeqKey(robotID), mark).ExecContext(deadline); err != nil {
		highWaterWriteFailures.inc()
		// Re-arm the throttle so the retry is one interval away, not one id away.
		storeMonotonic(&lastPersisted, robotID, v)
		return
	}
	storeMonotonic(&lastPersisted, robotID, v)
}

// storeMonotonic advances a per-bot int64 marker, never lowering it.
//
// CAS rather than a plain Store for the same reason recordIssued uses one: concurrent
// allocations for one bot arrive out of order, and a lower value overwriting a higher one
// costs a redundant durable write later (harmless against the GREATEST upsert, but the
// asymmetry reads like a bug — review P2-9).
func storeMonotonic(m *sync.Map, robotID string, v int64) {
	for {
		prev, loaded := m.Load(robotID)
		if !loaded {
			if _, already := m.LoadOrStore(robotID, v); !already {
				return
			}
			continue
		}
		high := prev.(int64)
		if v <= high {
			return
		}
		if m.CompareAndSwap(robotID, high, v) {
			return
		}
	}
}

// runGate performs the atomic mode-check-and-allocate.
//
// expectMirror is the exact ModeKey value this process validated against the
// authority, including the generation. A mirror that has been lost, downgraded to a
// bare `incr`, or replaced by another epoch therefore closes the gate rather than
// allocating — which is what makes a hand-written mirror unable to activate anything
// (review P1-2).
func runGate(client Scripter, robotID, expectMirror string) (int64, error) {
	v, err := gateScript.Run(client, []string{ModeKey, SeqKey(robotID)}, expectMirror).Result()
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
	deadline, cancel := context.WithTimeout(context.Background(), authorityTimeout)
	defer cancel()
	var mark int64
	if _, err := ctx.DB().Select("min_seq").From("`seq`").
		Where("`key`=?", HighWaterSeqKey(robotID)).LoadContext(deadline, &mark); err != nil {
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
	deadline, cancel := context.WithTimeout(context.Background(), authorityTimeout)
	defer cancel()
	var minSeq int64
	key := fmt.Sprintf("seq:%s%s", common.RobotEventSeqKey, robotID)
	if _, err := ctx.DB().Select("min_seq").From("`seq`").
		Where("`key`=?", key).LoadContext(deadline, &minSeq); err != nil {
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
	// The cached authority belief is process state exactly like `seeded`, and leaving
	// it behind would make one test's activation leak into the next one's
	// pre-activation assertions.
	ResetModeBeliefForTest()
}
