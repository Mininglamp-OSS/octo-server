package botevent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

type gateMutationClient struct {
	*rd.Client
	gateCalls  int
	beforeGate func(call int)
}

func (c *gateMutationClient) EvalSha(hash string, keys []string, args ...interface{}) *rd.Cmd {
	if hash == gateScript.Hash() {
		c.gateCalls++
		if c.beforeGate != nil {
			c.beforeGate(c.gateCalls)
		}
	}
	return c.Client.EvalSha(hash, keys, args...)
}

// These tests need a real MySQL and Redis: the contract is about what two
// concurrent processes and one exclusive cursor do to each other, and a fake would
// only restate the code.
//
// They use the standard `test` database and create the `seq` table themselves.
// `seq` is migration-owned (modules/common/sql/20211108000001_common_legacy01.sql)
// and this package does not go through testutil.NewTestServer, so there is no
// sql-migrate ledger here — a bare CREATE would normally leave a table that
// `gorp_migrations` has no row for, and the next package's NewTestServer would die
// with `Table 'seq' already exists`.
//
// That is safe here specifically because CI drops and recreates `test` before
// **every** package (.github/workflows/ci.yml), so nothing this package creates is
// visible to the next one. Running several packages locally without that reset will
// reproduce the `already exists` failure — reset between packages, as CI does.
//
// An earlier revision used a dedicated `botevent_test` database to dodge the issue.
// That was worse: CI never creates it, so every one of these integration tests
// silently Skipped there, which is indistinguishable from passing.
//
// tools/genseq-repro holds the mirror image of these assertions against the legacy
// GenSeq allocator, where they fail. Neither is meaningful without the other.

func testAddrs() (mysqlAddr, redisAddr string) {
	mysqlAddr = os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if mysqlAddr == "" {
		mysqlAddr = config.New().DB.MySQLAddr
	}
	redisAddr = os.Getenv("OCTO_TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	return
}

// seqTableDDL creates the migration-owned `seq` table. The row is the load-bearing half
// of the seed: it is the only record of how high the legacy allocator ever went for a bot
// whose queue is now empty.
//
// Shared with probeTestDependencies in main_test.go, which runs the identical statements
// so a CI environment that cannot execute them fails loudly instead of skipping.
const seqTableDDL = "CREATE TABLE IF NOT EXISTS `seq` (" +
	"`key` varchar(100) NOT NULL, `min_seq` bigint NOT NULL DEFAULT 0, " +
	"`step` int NOT NULL DEFAULT 0, PRIMARY KEY (`key`)) " +
	"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

// stateTableDDL matches modules/robot/sql/20260805000001_bot_event_seq_state.sql,
// including the singleton CHECK and the ON UPDATE clause. An abbreviated copy would mean
// the tests never exercise the shipped schema — and the CHECK is what the migration's Down
// relies on to refuse dropping an activated authority (review P2-11).
const stateTableDDL = "CREATE TABLE IF NOT EXISTS `" + stateTable + "` (" +
	"`singleton_id` tinyint unsigned NOT NULL, `mode` tinyint NOT NULL DEFAULT 0, " +
	"`epoch` bigint unsigned NOT NULL DEFAULT 0, `cutover_floor` bigint NOT NULL DEFAULT 0, " +
	"`updated_at` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, " +
	"PRIMARY KEY (`singleton_id`), " +
	"CONSTRAINT `chk_bot_event_seq_singleton` CHECK (`singleton_id` = 1)) " +
	"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

func seqTestCtx(t *testing.T) (*config.Context, *rd.Client) {
	t.Helper()
	mysqlAddr, redisAddr := testAddrs()

	cfg := config.New()
	cfg.DB.MySQLAddr = mysqlAddr
	ctx := config.NewContext(cfg)

	if _, err := ctx.DB().Exec(seqTableDDL); err != nil {
		t.Skipf("no usable MySQL at %s: %v", mysqlAddr, err)
	}
	if _, err := ctx.DB().Exec(stateTableDDL); err != nil {
		t.Skipf("cannot create %s at %s: %v", stateTable, mysqlAddr, err)
	}

	client := rd.NewClient(&rd.Options{Addr: redisAddr})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("no usable Redis at %s: %v", redisAddr, err)
	}

	ResetSeededForTest()
	t.Cleanup(func() {
		ResetSeededForTest()
		_ = client.Close()
	})
	return ctx, client
}

// fixture clears every trace of a bot so each test starts from a known floor, and
// puts the allocator in the ACTIVATED state — most tests are about post-activation
// behaviour. The pre-activation path has its own tests below.
func fixture(t *testing.T, ctx *config.Context, client *rd.Client, robotID string) {
	t.Helper()
	clean := func() {
		client.Del(SeqKey(robotID), QueueKey(robotID), ModeKey)
		_, _ = ctx.DB().DeleteFrom("seq").Where("`key`=?", "seq:robotEventSeq:"+robotID).Exec()
		_, _ = ctx.DB().DeleteFrom("seq").Where("`key`=?", HighWaterSeqKey(robotID)).Exec()
		setStateMode(t, ctx, StateModeLegacy, 0)
	}
	clean()
	// Activated state: the DB row is the authority, the Redis key only its mirror.
	setStateMode(t, ctx, StateModeIncr, 0)
	if err := client.Set(ModeKey, MirrorValue(0), 0).Err(); err != nil {
		t.Fatalf("write mode mirror: %v", err)
	}
	t.Cleanup(clean)
}

// setStateMode forces the authoritative state row, bypassing Activate's floor
// validation. Tests that exercise Activate itself call it directly.
func setStateMode(t *testing.T, ctx *config.Context, mode int, floor int64) {
	t.Helper()
	// epoch is pinned to 0 so the mirror value tests write (MirrorValue(0)) matches.
	// Without resetting it, a test that ran the real Activate leaves epoch=1 behind and
	// the next fixture's mirror would be a stale generation.
	if _, err := ctx.DB().InsertBySql(
		"insert into `"+stateTable+"`(`singleton_id`,`mode`,`epoch`,`cutover_floor`) values(?,?,0,?) "+
			"on duplicate key update `mode`=VALUES(`mode`), `epoch`=0, `cutover_floor`=VALUES(`cutover_floor`)",
		stateSingletonID, mode, floor).Exec(); err != nil {
		t.Fatalf("set state mode: %v", err)
	}
}

// TestNotActivatedDelegatesToLegacy is what makes deploying this change a no-op.
//
// Until an operator activates, every replica must behave exactly like the old
// binary. If a deploy switched allocators on its own, the new replicas would issue
// ids above the block that legacy replicas are still issuing from the bottom of,
// and once a consumer's cursor reached the new range every legacy id behind it
// would be permanently invisible — the loss of #697, caused by its fix.
func TestNotActivatedDelegatesToLegacy(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_notactivated_bot"
	fixture(t, ctx, client, robotID)
	// Pre-activation: authority says legacy, and the mirror is absent.
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)

	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// GenSeq's first block for a brand-new key starts at 1000001; the counter would
	// have produced 1. Asserting the shape rather than the exact number keeps this
	// from breaking if octo-lib changes its base.
	if first < 1000000 {
		t.Fatalf("first id %d looks like a counter allocation; before activation it must come "+
			"from GenSeq so the deploy is behaviour-neutral", first)
	}
	if n, _ := client.Exists(SeqKey(robotID)).Result(); n != 0 {
		t.Error("the counter must not even be created before activation")
	}
}

// TestActivationTakesEffectWithoutAProcessCacheWindow pins why the mode is read
// inside the allocation script rather than cached.
//
// A per-process cache — even a one-second one — means that for that second some
// replicas allocate from the counter while others still allocate from GenSeq. Two
// live sources is the defect, so the flip has to be visible on the very next
// allocation.
func TestActivationTakesEffectWithoutAProcessCacheWindow(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_flip_bot"
	fixture(t, ctx, client, robotID)
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)

	legacyID, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("pre-activation allocate: %v", err)
	}

	// Activate, with no restart and no cache invalidation of any kind.
	setStateMode(t, ctx, StateModeIncr, 0)
	if err := client.Set(ModeKey, MirrorValue(0), 0).Err(); err != nil {
		t.Fatalf("activate: %v", err)
	}

	activatedID, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("post-activation allocate: %v", err)
	}
	if n, _ := client.Exists(SeqKey(robotID)).Result(); n == 0 {
		t.Fatal("the very next allocation after activation still used the legacy path")
	}
	if activatedID <= legacyID {
		t.Fatalf("post-activation id %d must be above the last legacy id %d, or the events "+
			"issued just before the flip land below the consumer's cursor", activatedID, legacyID)
	}
}

// TestSeedClearsTheLegacyCeilingAtActivation is the deploy-time safety property:
// the first activated id must clear everything legacy could have issued, including
// a block it had reserved but not finished.
func TestSeedClearsTheLegacyCeilingAtActivation(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_clearceiling_bot"
	fixture(t, ctx, client, robotID)
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)

	// Let legacy issue a few ids and enqueue them, as it would have in production.
	var lastLegacy int64
	for i := 0; i < 3; i++ {
		id, err := nextEventID(ctx, client, robotID)
		if err != nil {
			t.Fatalf("legacy allocate: %v", err)
		}
		client.ZAdd(QueueKey(robotID), rd.Z{Score: float64(id), Member: fmt.Sprintf(`{"event_id":%d}`, id)})
		lastLegacy = id
	}

	setStateMode(t, ctx, StateModeIncr, 0)
	if err := client.Set(ModeKey, MirrorValue(0), 0).Err(); err != nil {
		t.Fatalf("activate: %v", err)
	}
	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("activated allocate: %v", err)
	}
	if first <= lastLegacy+legacyGenSeqStep {
		t.Fatalf("first activated id %d does not clear the legacy reserved block above %d; "+
			"a block legacy reserved but did not finish issuing would overlap", first, lastLegacy)
	}
}

// TestValidatedBeliefRetainsTheCutoverFloor covers the stateFloorOrZero hole.
//
// Once a process has validated an activated authority, a later seed for another bot must
// use the cutover floor carried by that positive belief. Re-reading the state row during
// seed and turning any error/missing row into zero silently discards the one global floor
// that was validated at activation, allowing a new counter to restart at 1.
func TestValidatedBeliefRetainsTheCutoverFloor(t *testing.T) {
	ctx, client := seqTestCtx(t)
	resolverBot := "seqtest_floor_resolver_bot"
	newBot := "seqtest_floor_new_bot"
	fixture(t, ctx, client, resolverBot)

	const cutoverFloor = int64(8_000_000)
	setStateMode(t, ctx, StateModeIncr, cutoverFloor)
	ResetSeededForTest()
	if _, err := nextEventID(ctx, client, resolverBot); err != nil {
		t.Fatalf("resolve activated authority: %v", err)
	}

	client.Del(SeqKey(newBot), QueueKey(newBot))
	_, _ = ctx.DB().DeleteFrom("seq").Where("`key` in ?", []string{
		"seq:robotEventSeq:" + newBot,
		HighWaterSeqKey(newBot),
	}).Exec()
	if _, err := ctx.DB().DeleteFrom(stateTable).Where("singleton_id=?", stateSingletonID).Exec(); err != nil {
		t.Fatalf("remove authority after positive resolution: %v", err)
	}

	got, err := nextEventID(ctx, client, newBot)
	if err != nil {
		t.Fatalf("allocate from retained positive belief: %v", err)
	}
	if got <= cutoverFloor {
		t.Fatalf("id %d did not retain validated cutover floor %d after the authority row became unreadable; "+
			"re-reading the floor during seed and swallowing the error can restart a bot below live cursors",
			got, cutoverFloor)
	}
}

// TestSeedLoadsBothSeqFloorsWithOneQuery is an I/O-shape guard. A mirror rebuild
// invalidates every active bot's seed marker, so two point SELECTs per bot amplify an
// already expensive recovery burst. The legacy and durable rows share one table and must
// be loaded by one conditional aggregate query.
func TestSeedLoadsBothSeqFloorsWithOneQuery(t *testing.T) {
	source, err := os.ReadFile("seq.go")
	if err != nil {
		t.Fatalf("read seq.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func seedCounter(")
	if start < 0 {
		t.Fatal("could not locate seedCounter body")
	}
	end := strings.Index(text[start:], "\nfunc queueCeiling(")
	if end < 0 {
		t.Fatal("could not locate seedCounter body")
	}
	body := text[start : start+end]
	if !strings.Contains(body, "seqCeilings(ctx, robotID)") {
		t.Error("seedCounter must load legacy and durable seq floors through one seqCeilings query")
	}
	for _, old := range []string{"legacyCeiling(ctx, robotID)", "highWaterCeiling(ctx, robotID)"} {
		if strings.Contains(body, old) {
			t.Errorf("seedCounter still calls %s; mirror recovery would issue multiple MySQL reads per bot", old)
		}
	}
}

func TestSeqCeilingsReturnsLegacyAndDurableRows(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_combined_ceilings_bot"
	fixture(t, ctx, client, robotID)

	const legacy, durable = int64(12_000), int64(34_000)
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO `seq`(`key`, min_seq, step) VALUES(?,?,?)",
		"seq:robotEventSeq:"+robotID, legacy, legacyGenSeqStep).Exec(); err != nil {
		t.Fatalf("insert legacy ceiling: %v", err)
	}
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO `seq`(`key`, min_seq, step) VALUES(?,?,?)",
		HighWaterSeqKey(robotID), durable, highWaterInterval).Exec(); err != nil {
		t.Fatalf("insert durable ceiling: %v", err)
	}

	gotLegacy, gotDurable, err := seqCeilings(ctx, robotID)
	if err != nil {
		t.Fatalf("load combined ceilings: %v", err)
	}
	if gotLegacy != legacy || gotDurable != durable {
		t.Fatalf("seqCeilings = (%d, %d), want (%d, %d)",
			gotLegacy, gotDurable, legacy, durable)
	}
}

func TestNextEventIDIsStrictlyMonotonicUnderConcurrency(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_monotonic_bot"
	fixture(t, ctx, client, robotID)

	const goroutines, each = 8, 25
	got := make([][]int64, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				v, err := nextEventID(ctx, client, robotID)
				if err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
				got[g] = append(got[g], v)
			}
		}(g)
	}
	wg.Wait()

	var all []int64
	for _, ids := range got {
		// Within one caller, ids must also be increasing — INCR never reorders.
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Fatalf("ids went backwards within one caller: %d then %d", ids[i-1], ids[i])
			}
		}
		all = append(all, ids...)
	}
	sort.Slice(all, func(a, b int) bool { return all[a] < all[b] })
	if len(all) != goroutines*each {
		t.Fatalf("expected %d ids, got %d", goroutines*each, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("duplicate id %d issued to two callers", all[i])
		}
	}
	// Contiguity is not contractual, but a gap would mean something else is
	// sharing this counter, which is worth knowing about.
	if all[len(all)-1]-all[0] != int64(len(all)-1) {
		high, _ := highWaterCeiling(ctx, robotID)
		t.Errorf("ids are not contiguous (%d..%d for %d allocations); rollbacksDetected=%d, "+
			"durable high-water=%d — a gap means something re-seeded mid-run, which under "+
			"pure concurrency it must not (see rollbackTolerance)",
			all[0], all[len(all)-1], len(all), RollbacksDetected(), high)
	}
}

// TestSeedRaisesAboveQueueCeiling covers the ids legacy already put in the queue.
func TestSeedRaisesAboveQueueCeiling(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_queueceiling_bot"
	fixture(t, ctx, client, robotID)

	const legacyHigh = 1035010
	client.ZAdd(QueueKey(robotID), rd.Z{Score: legacyHigh, Member: `{"event_id":1035010}`})

	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first <= legacyHigh {
		t.Fatalf("first id %d is not above the queue ceiling %d — every new event would be "+
			"born below a client cursor sitting at the ceiling", first, legacyHigh)
	}
}

// TestSeedRaisesAboveLegacySeqRowWithEmptyQueue is the self-inflicted-outage case:
// the queue is empty (fully acked or expired) so it carries no evidence, but
// clients still hold cursors near the last id legacy ever issued. Seeding from the
// queue alone would put every new event below those cursors.
func TestSeedRaisesAboveLegacySeqRowWithEmptyQueue(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_legacyrow_bot"
	fixture(t, ctx, client, robotID)

	const legacyMinSeq = 2040000
	if _, err := ctx.DB().InsertBySql(
		"insert into `seq`(`key`,min_seq,step) values(?,?,?)",
		"seq:robotEventSeq:"+robotID, legacyMinSeq, 1000).Exec(); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if n, err := client.ZCard(QueueKey(robotID)).Result(); err != nil || n != 0 {
		t.Fatalf("queue should be empty for this case (n=%d err=%v)", n, err)
	}

	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first <= legacyMinSeq {
		t.Fatalf("first id %d is not above the legacy seq row %d — clients holding a cursor "+
			"from before the queue drained would never see another event", first, legacyMinSeq)
	}
}

// TestSeedIsIdempotentAndNeverLowers is what licenses the single-stage deploy: any
// replica may seed at any time, any number of times, in any order.
func TestSeedIsIdempotentAndNeverLowers(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_idempotent_bot"
	fixture(t, ctx, client, robotID)

	client.ZAdd(QueueKey(robotID), rd.Z{Score: 5000, Member: `{"event_id":5000}`})

	if err := seedCounter(ctx, client, robotID, 0); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	afterFirst, _ := client.Get(SeqKey(robotID)).Int64()

	// Advance well past the floor, then re-seed: a second seeder must not undo it.
	for i := 0; i < 50; i++ {
		client.Incr(SeqKey(robotID))
	}
	advanced, _ := client.Get(SeqKey(robotID)).Int64()

	for i := 0; i < 3; i++ {
		if err := seedCounter(ctx, client, robotID, 0); err != nil {
			t.Fatalf("re-seed %d: %v", i, err)
		}
	}
	afterReseed, _ := client.Get(SeqKey(robotID)).Int64()
	if afterReseed != advanced {
		t.Fatalf("re-seeding moved the counter from %d to %d; it must be a no-op once above "+
			"the floor, or a late-starting replica would rewind live ids", advanced, afterReseed)
	}
	if afterFirst <= 5000 {
		t.Fatalf("first seed left the counter at %d, not above the ceiling 5000", afterFirst)
	}
}

// TestNextEventIDFailsClosedWhenSeedFails pins the no-fallback rule. Returning an
// unseeded id would be worse than returning an error: the error fails one enqueue,
// an unseeded id silently puts every event for that bot below its client's cursor.
//
// The failure is induced with a real Redis rather than a fake, and induced
// specifically on the *seed* while leaving INCR perfectly usable: the queue key is
// made a string, so the ceiling read fails with WRONGTYPE while the counter key is
// untouched. That is the split that matters — a fake whose every call fails could
// not tell "refused to allocate" from "could not reach Redis at all".
func TestNextEventIDFailsClosedWhenSeedFails(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_failclosed_bot"
	fixture(t, ctx, client, robotID)

	if err := client.Set(QueueKey(robotID), "not-a-zset", 0).Err(); err != nil {
		t.Fatalf("plant WRONGTYPE: %v", err)
	}
	// Prove INCR on the counter key is genuinely available, so the failure below
	// can only come from the seed.
	if err := client.Incr(SeqKey(robotID)).Err(); err != nil {
		t.Fatalf("counter key should be usable: %v", err)
	}
	client.Del(SeqKey(robotID))

	before := SeedFailures()
	if _, err := nextEventID(ctx, client, robotID); err == nil {
		t.Fatal("expected a failed seed to fail the allocation, got nil error")
	}
	if SeedFailures() != before+1 {
		t.Errorf("seed failure was not counted (%d -> %d)", before, SeedFailures())
	}
	if n, _ := client.Exists(SeqKey(robotID)).Result(); n != 0 {
		t.Error("a failed seed must not leave a counter behind")
	}
	// And it must not have been marked seeded, or a retry would skip the seed and
	// hand out an unsafe id.
	if _, ok := seeded.Load(robotID); ok {
		t.Error("a failed seed must not mark the bot as seeded")
	}
}

// TestExclusiveCursorIsLosslessWithMonotonicIDs is the end-to-end assertion, run
// against the same read shape as modules/bot_api/events.go. Its mirror in
// tools/genseq-repro fails under the legacy allocator: same consumer, same page
// size, ids from two blocks instead of one counter.
func TestExclusiveCursorIsLosslessWithMonotonicIDs(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_lossless_bot"
	fixture(t, ctx, client, robotID)

	// Two concurrent producers, interleaved, as two replicas would be.
	const perProducer = 40
	var wg sync.WaitGroup
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				id, err := nextEventID(ctx, client, robotID)
				if err != nil {
					t.Errorf("producer %d: %v", p, err)
					return
				}
				// score == payload event_id, the equality events.go's cursor and
				// ack both depend on.
				client.ZAdd(QueueKey(robotID), rd.Z{
					Score:  float64(id),
					Member: fmt.Sprintf(`{"event_id":%d,"producer":%d}`, id, p),
				})
			}
		}(p)
	}
	wg.Wait()

	total, _ := client.ZCard(QueueKey(robotID)).Result()
	if total != 2*perProducer {
		t.Fatalf("expected %d members, got %d — a collision would collapse two into one",
			2*perProducer, total)
	}

	// Consume with limit 1, the page size that makes a shared score fatal.
	delivered := map[string]bool{}
	var cursor int64
	for {
		page, err := client.ZRangeByScore(QueueKey(robotID), rd.ZRangeBy{
			Min: fmt.Sprintf("(%d", cursor), Max: "+inf", Count: 1,
		}).Result()
		if err != nil || len(page) == 0 {
			break
		}
		moved := false
		for _, m := range page {
			delivered[m] = true
			var id int64
			if _, err := fmt.Sscanf(m, `{"event_id":%d`, &id); err == nil && id > cursor {
				cursor, moved = id, true
			}
		}
		if !moved {
			break
		}
	}
	if len(delivered) != int(total) {
		t.Fatalf("exclusive cursor delivered %d of %d members; monotonic ids must make it "+
			"lossless at any page size", len(delivered), total)
	}
}

// TestCounterRollbackIsDetectedAndHealed is the review finding: co-recovery makes
// ids unique across an RDB rollback but says nothing about consumer cursors, which
// are external and do not roll back.
//
// Simulated by setting the counter back to an earlier value, which is what
// restoring an older RDB snapshot does to it. The bug this locks is not the
// regression itself — it is that a replica which has already seeded would keep
// INCRing the regressed counter and quietly issue every subsequent id below the
// cursor a client already holds.
func TestCounterRollbackIsDetectedAndHealed(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_rollback_bot"
	fixture(t, ctx, client, robotID)

	// Issue enough ids to advance the durable mark at least once.
	var highest int64
	for i := 0; i < highWaterInterval+5; i++ {
		v, err := nextEventID(ctx, client, robotID)
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		highest = v
	}
	mark, err := highWaterCeiling(ctx, robotID)
	if err != nil {
		t.Fatalf("read high-water: %v", err)
	}
	if mark == 0 {
		t.Fatalf("durable high-water mark was never written after %d allocations; "+
			"without it a rollback cannot be recovered from", highWaterInterval+5)
	}

	// The client has read up to `highest`. Now Redis loses data and comes back with
	// an older counter — the queue would come back older too, but the client's
	// cursor does not.
	// Regress by more than rollbackTolerance: smaller regressions are deliberately
	// indistinguishable from concurrent out-of-order arrival (see that constant).
	regressedTo := highest - 3*rollbackTolerance
	if regressedTo < 1 {
		regressedTo = 1 // a counter is never negative; see gateNotActivated
	}
	if err := client.Set(SeqKey(robotID), regressedTo, 0).Err(); err != nil {
		t.Fatalf("simulate rollback: %v", err)
	}
	before := RollbacksDetected()

	next, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate after rollback: %v", err)
	}
	if RollbacksDetected() != before+1 {
		t.Errorf("rollback was not detected (%d -> %d)", before, RollbacksDetected())
	}
	if next <= highest {
		t.Fatalf("id %d after rollback is not above the %d already issued and possibly read; "+
			"it would land below a live consumer cursor", next, highest)
	}
}

// TestHighWaterNeverMovesBackwards pins the GREATEST in the durable write. An
// unconditional overwrite is exactly how GenSeq lets a lagging replica drag a floor
// backwards, which is the root cause of #697 — repeating it here would undo the fix.
func TestHighWaterNeverMovesBackwards(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_highwater_bot"
	fixture(t, ctx, client, robotID)

	persistHighWater(ctx, robotID, 900000)
	high, err := highWaterCeiling(ctx, robotID)
	if err != nil || high == 0 {
		t.Fatalf("first write did not land (high=%d err=%v)", high, err)
	}

	// A replica that is behind tries to persist a lower mark, as one would after a
	// rollback or when running with a stale block.
	lastPersisted.Delete(robotID)
	persistHighWater(ctx, robotID, 1000)

	after, err := highWaterCeiling(ctx, robotID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after < high {
		t.Fatalf("high-water mark moved backwards from %d to %d; a lagging writer must not "+
			"be able to lower a floor (this is the #697 root cause)", high, after)
	}
}

// TestSeedRecoversFromTheDurableMarkAlone isolates the durable source: no queue, no
// legacy row, no counter — only the MySQL mark, which is the state left behind by a
// Redis that lost everything.
func TestSeedRecoversFromTheDurableMarkAlone(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_durableonly_bot"
	fixture(t, ctx, client, robotID)

	const mark = 777000
	persistHighWater(ctx, robotID, mark)
	// Everything Redis knew is gone.
	client.Del(SeqKey(robotID), QueueKey(robotID))
	ResetSeededForTest()
	setStateMode(t, ctx, StateModeIncr, 0)
	if err := client.Set(ModeKey, MirrorValue(0), 0).Err(); err != nil {
		t.Fatalf("activate: %v", err)
	}

	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first <= mark {
		t.Fatalf("first id %d after total Redis loss is not above the durable mark %d", first, mark)
	}
}

// TestExpectedModeFailsClosedWhenModeIsLost covers the other half of the same
// rollback: ModeKey also lives in Redis, and dropping to legacy would issue ids from
// a lower GenSeq block — beneath live cursors, the same loss mirrored.
func TestExpectedModeFailsClosedWhenModeIsLost(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_expectedmode_bot"
	fixture(t, ctx, client, robotID)

	// A fresh process (no cached belief, nothing issued) meeting a legacy authority
	// with the mirror gone: that is what a pre-migration deploy looks like, so the
	// guard is the only thing that distinguishes it from a lost activation.
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)
	ResetSeededForTest()
	restore := SetExpectedModeForTest(ModeIncr)

	if _, err := nextEventID(ctx, client, robotID); err == nil {
		restore()
		t.Fatal("expected a lost mode key to fail closed when the replica expects incr, " +
			"got nil error — falling back to legacy would issue ids below live cursors")
	}

	// With no expectation set, the same state is pre-activation and legacy is right.
	restore()
	ResetSeededForTest()
	v, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("without an expectation, a missing mode must mean legacy: %v", err)
	}
	if v == 0 {
		t.Fatal("legacy allocation returned 0")
	}
}

// TestMalformedExpectedModeFailsClosed pins the other half of that guard.
//
// The previous revision compared a free-form env string against ModeIncr, so
// `OCTO_BOTEVENT_EXPECTED_MODE=inrc` was indistinguishable from unset: a typo silently
// disarmed the one thing standing between a lost mode and a downgrade (review P1-4).
// Same shape as #627's internal/msgextraseq, which fails closed on a malformed value.
func TestMalformedExpectedModeFailsClosed(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_badexpectedmode_bot"
	fixture(t, ctx, client, robotID)
	ResetSeededForTest()

	restore := SetExpectedModeForTest("inrc")
	defer restore()

	if _, err := nextEventID(ctx, client, robotID); err == nil {
		t.Fatal("a malformed expected-mode value must fail closed; reading it as \"unset\" is " +
			"how a typo turns a deployment guard into decoration")
	}
}

// TestExpectedModeRefusalDoesNotReadAuthorityPerAllocation is the regression
// from the post-rebase review on head 6a5cd9a6.
//
// A deployment guard mismatch is stable for the lifetime of the process. Once
// the authority has been resolved, refusing the allocation must not re-read the
// same row for every message while holding a msgSem slot. A malformed guard is
// even cheaper: it is process-local configuration and must fail before any DB
// read at all.
func TestExpectedModeRefusalDoesNotReadAuthorityPerAllocation(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		maxReads int64
	}{
		{name: "expects incr while authority is legacy", expected: ModeIncr, maxReads: 1},
		{name: "malformed expectation", expected: "inrc", maxReads: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client := seqTestCtx(t)
			robotID := "seqtest_expected_refusal_" + strings.ReplaceAll(tc.name, " ", "_")
			fixture(t, ctx, client, robotID)
			setStateMode(t, ctx, StateModeLegacy, 0)
			client.Del(ModeKey)
			ResetSeededForTest()

			restore := SetExpectedModeForTest(tc.expected)
			defer restore()

			before := AuthorityReads()
			const allocations = 20
			for i := 0; i < allocations; i++ {
				if id, err := nextEventID(ctx, client, robotID); err == nil {
					t.Fatalf("allocation %d returned id %d; the expected-mode guard must fail closed", i, id)
				}
			}
			if reads := AuthorityReads() - before; reads > tc.maxReads {
				t.Fatalf("%d refused allocations issued %d authority reads, want at most %d; "+
					"a stable deployment error must not serialize every message on MySQL",
					allocations, reads, tc.maxReads)
			}
		})
	}
}

// TestMirrorAloneCannotActivateTheCounter is the review P1-2 / Jerry-Xin 🔴 case.
//
// The hot path used to trust a positive mirror outright and never read the authority,
// so a stray `SET botEventSeq:mode incr` performed the activation the operator tool
// exists to gate: no floor validation, no `-yes`, no confirmation that every pre-fix
// replica is gone. That is the mixed-source loss this whole change removes — an
// activated replica issuing above the ceiling while a legacy one issues from the
// bottom of a block.
//
// Reachable by hand, by a Redis shared with another environment, by a snapshot
// restored from an activated era, or by rolling the migration back and leaving the
// mirror behind.
func TestMirrorAloneCannotActivateTheCounter(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_forgedmirror_bot"
	fixture(t, ctx, client, robotID)

	for _, mirror := range []string{ModeIncr, MirrorValue(0), MirrorValue(99)} {
		t.Run(mirror, func(t *testing.T) {
			// Authority says legacy. Somebody wrote the mirror anyway.
			setStateMode(t, ctx, StateModeLegacy, 0)
			ResetSeededForTest()
			client.Del(SeqKey(robotID))
			if err := client.Set(ModeKey, mirror, 0).Err(); err != nil {
				t.Fatalf("plant mirror: %v", err)
			}

			before := MirrorUnauthorized()
			id, err := nextEventID(ctx, client, robotID)
			if err != nil {
				t.Fatalf("allocate: %v", err)
			}
			if n, _ := client.Exists(SeqKey(robotID)).Result(); n != 0 {
				t.Fatalf("mirror %q activated the counter without the authority; the gate the "+
					"operator tool implements was bypassed entirely", mirror)
			}
			if id < 1000000 {
				t.Errorf("id %d looks like a counter allocation; with the authority saying legacy "+
					"it must come from GenSeq", id)
			}
			if MirrorUnauthorized() == before {
				t.Error("an unconfirmed mirror must be counted; the allocator leaves the " +
					"conflict intact for operator diagnosis, so without the metric it is invisible")
			}
		})
	}
}

// TestNotActivatedDoesNotQueryTheAuthorityPerAllocation is review P1-1.
//
// The pre-activation path ran `SELECT ... FROM octo_bot_event_seq_state` on **every**
// allocation, inside a held msgSem slot and with no deadline, which made the PR's
// central claim ("until an operator activates, every replica delegates to GenSeq
// exactly as before") false. Nothing pinned it, because the other pre-activation test
// asserts the id's shape and this is a property of the I/O shape.
func TestNotActivatedDoesNotQueryTheAuthorityPerAllocation(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_noauthorityspam_bot"
	fixture(t, ctx, client, robotID)
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)
	ResetSeededForTest()

	before := AuthorityReads()
	const allocations = 20
	for i := 0; i < allocations; i++ {
		if _, err := nextEventID(ctx, client, robotID); err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
	}
	reads := AuthorityReads() - before
	// One read for the cold belief. The negative belief is trusted while the mirror
	// agrees with it, and the mirror is absent throughout, so nothing forces another.
	if reads > 1 {
		t.Errorf("%d allocations issued %d authority reads; the merged-but-not-activated build "+
			"must not put a DB round trip on the fan-out path (it runs inside a msgSem slot)",
			allocations, reads)
	}

	// And the cache must expire rather than pin a stale answer forever: a replica whose
	// mirror write was lost has to notice the flip on its own.
	AgeModeBeliefForTest()
	if _, err := nextEventID(ctx, client, robotID); err != nil {
		t.Fatalf("allocate after the belief aged out: %v", err)
	}
	if AuthorityReads()-before <= reads {
		t.Error("an aged-out negative belief must be re-read; otherwise a replica that missed " +
			"the mirror write never notices the activation")
	}
}

// TestCounterKeyCannotCollideWithModeKey pins the key-namespace fix.
//
// `SeqKeyPrefix` was `botEventSeq:` while `ModeKey` is `botEventSeq:mode`, so
// SeqKey("mode") *was* ModeKey: a bot with that id would have seeded and INCRed the
// global activation flag as its own counter. Unreachable with UUID-hex robot ids, but
// a namespace where a legal input collides with a global key does not get to stay.
func TestCounterKeyCannotCollideWithModeKey(t *testing.T) {
	for _, robotID := range []string{"mode", "counter:mode", "", "counter", "mode:"} {
		if got := SeqKey(robotID); got == ModeKey {
			t.Errorf("SeqKey(%q) == ModeKey (%q): one bot's counter would be the global "+
				"activation flag", robotID, ModeKey)
		}
		if got := SeqKey(robotID); got == HighWaterSeqKey(robotID) {
			t.Errorf("SeqKey(%q) collides with its own durable mark key", robotID)
		}
	}
	// And the inverse: the mode key must not be reachable as any counter key.
	if strings.HasPrefix(ModeKey, SeqKeyPrefix) {
		t.Errorf("ModeKey %q lives under the counter prefix %q, so some robotID reaches it",
			ModeKey, SeqKeyPrefix)
	}
}

// TestMissingCounterFailsBeforeIssuingFromOne is the EXISTS guard.
//
// INCR on a missing key returns 1. Before the guard, the only thing preventing an
// unseeded INCR was `seeded` — a process-local map guarding Redis state — so a
// partial restore or a stray DEL that took the counter but left the mirror, with the
// process still up, renumbered that bot's events from 1: arbitrarily far below its
// consumer's cursor, and needing the entire historical range re-issued before one
// event became visible. Failing before issuing beats detecting afterwards.
func TestMissingCounterFailsBeforeIssuingFromOne(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_counterlost_bot"
	fixture(t, ctx, client, robotID)

	var highest int64
	for i := 0; i < 3; i++ {
		v, err := nextEventID(ctx, client, robotID)
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		highest = v
	}

	// The counter vanishes; the mirror and this process's `seeded` entry survive.
	if err := client.Del(SeqKey(robotID)).Err(); err != nil {
		t.Fatalf("drop counter: %v", err)
	}
	before := CountersLost()

	next, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate after counter loss: %v", err)
	}
	if CountersLost() != before+1 {
		t.Errorf("counter loss was not detected (%d -> %d)", before, CountersLost())
	}
	if next <= highest {
		t.Fatalf("id %d after counter loss is not above the %d already issued; INCR on a "+
			"missing key returns 1, which is what the EXISTS guard exists to prevent", next, highest)
	}
}

// TestRetryReResolvesWhenTheMirrorChangesAfterReseed covers the retry-specific
// gateNotActivated sentinel. The mirror can disappear between a successful re-seed and
// the retrying gate. Treating every negative value as a generic "counter unusable" error
// loses the existing authority-recovery path and reports the wrong fault.
func TestRetryReResolvesWhenTheMirrorChangesAfterReseed(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_retry_mirror_bot"
	fixture(t, ctx, client, robotID)

	highest, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("initial allocation: %v", err)
	}
	if err := client.Del(SeqKey(robotID)).Err(); err != nil {
		t.Fatalf("drop counter: %v", err)
	}

	wrapped := &gateMutationClient{Client: client}
	wrapped.beforeGate = func(call int) {
		if call == 2 {
			_ = client.Del(ModeKey).Err()
		}
	}
	next, err := nextEventID(ctx, wrapped, robotID)
	if err != nil {
		t.Fatalf("mirror changed between re-seed and retry: %v", err)
	}
	if next <= highest {
		t.Fatalf("recovered id %d is not above prior id %d", next, highest)
	}
}

// TestRegressionRetryNamesACounterThatDisappearsAgain covers gateCounterMissing on
// the other retry site. Reporting sentinel -2 as a numeric floor reached by re-seeding
// sends an operator toward the wrong recovery action.
func TestRegressionRetryNamesACounterThatDisappearsAgain(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_retry_counter_bot"
	fixture(t, ctx, client, robotID)
	if err := client.Set(SeqKey(robotID), 10_000, 0).Err(); err != nil {
		t.Fatalf("raise initial counter: %v", err)
	}

	highest, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("initial allocation: %v", err)
	}
	if err := client.Set(SeqKey(robotID), highest-3*rollbackTolerance, 0).Err(); err != nil {
		t.Fatalf("regress counter: %v", err)
	}

	wrapped := &gateMutationClient{Client: client}
	wrapped.beforeGate = func(call int) {
		if call == 2 {
			_ = client.Del(SeqKey(robotID)).Err()
		}
	}
	_, err = nextEventID(ctx, wrapped, robotID)
	if err == nil {
		t.Fatal("counter disappearing again during rollback recovery must fail closed")
	}
	if !strings.Contains(err.Error(), "counter") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error must identify the retry sentinel as a missing counter, got: %v", err)
	}
	if strings.Contains(err.Error(), "only reached -2") {
		t.Fatalf("error misreports gateCounterMissing as a numeric re-seed floor: %v", err)
	}
}

// TestMirrorRebuiltFromDBAuthority is why the state row exists.
//
// The mode mirror is in the same Redis as everything else, so it regresses with an
// RDB snapshot. Before the state table, losing it dropped the allocator to legacy —
// whose ids sit below everything the counter had issued, i.e. #697 mirrored. The DB
// row now decides, and a lost mirror is rebuilt rather than obeyed.
func TestMirrorRebuiltFromDBAuthority(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_mirror_bot"
	fixture(t, ctx, client, robotID)

	highest, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Redis loses the mirror. The authority still says activated.
	if err := client.Del(ModeKey).Err(); err != nil {
		t.Fatalf("drop mirror: %v", err)
	}
	before := MirrorRebuilds()

	next, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate after mirror loss: %v", err)
	}
	if MirrorRebuilds() != before+1 {
		t.Errorf("mirror was not rebuilt (%d -> %d)", before, MirrorRebuilds())
	}
	if next <= highest {
		t.Fatalf("id %d after mirror loss is not above the %d already issued — the allocator "+
			"degraded to legacy instead of consulting the authority", next, highest)
	}
	// Rebuilt with the generation, not a bare `incr`: the gate compares the mirror
	// against the exact value the authority confirmed, so a bare value would be a claim
	// the allocator then has to go re-verify on every allocation.
	if v, _ := client.Get(ModeKey).Result(); v != MirrorValue(0) {
		t.Errorf("mirror was not restored to the generation-stamped value (got %q, want %q)",
			v, MirrorValue(0))
	}
}

// TestHigherMirrorEpochForcesAuthorityRefresh prevents an old process from overwriting
// a newer generation. Positive beliefs are terminal with respect to legacy, but not with
// respect to a mirror that names a strictly higher epoch.
func TestHigherMirrorEpochForcesAuthorityRefresh(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_higher_epoch_bot"
	fixture(t, ctx, client, robotID)

	if _, err := ctx.DB().Update(stateTable).
		Set("mode", StateModeIncr).
		Set("epoch", 2).
		Set("cutover_floor", 9000).
		Where("singleton_id=?", stateSingletonID).Exec(); err != nil {
		t.Fatalf("advance authority epoch: %v", err)
	}
	if err := client.Set(ModeKey, MirrorValue(2), 0).Err(); err != nil {
		t.Fatalf("write newer mirror: %v", err)
	}
	activeBelief.Store(&belief{activated: true, epoch: 1, resolvedAt: beliefNow()})
	seeded.Delete(robotID)
	client.Del(SeqKey(robotID), QueueKey(robotID))

	if _, err := nextEventID(ctx, client, robotID); err != nil {
		t.Fatalf("allocate after observing a higher mirror epoch: %v", err)
	}
	if got, err := client.Get(ModeKey).Result(); err != nil || got != MirrorValue(2) {
		t.Fatalf("older belief overwrote newer mirror: got %q err=%v, want %q", got, err, MirrorValue(2))
	}
	if b := activeBelief.Load(); b == nil || b.epoch != 2 {
		t.Fatalf("authority was not refreshed upward (belief=%+v)", b)
	}
}

// TestRunGateRejectsIDsOutsideFloat64ExactIntegerRange protects the wire invariant
// score == event_id. At 2^53, the next integer cannot be represented as a distinct ZSET
// score, recreating collision, cursor skip, and multi-member ack.
func TestRunGateRejectsIDsOutsideFloat64ExactIntegerRange(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_float64_limit_bot"
	fixture(t, ctx, client, robotID)

	const largestSafeID = int64(1<<53 - 1)
	if err := client.Set(SeqKey(robotID), largestSafeID, 0).Err(); err != nil {
		t.Fatalf("set counter at exact-integer boundary: %v", err)
	}
	if id, err := runGate(client, robotID, MirrorValue(0)); err == nil {
		t.Fatalf("runGate returned id %d at/above 2^53; adjacent ids no longer have distinct float64 scores", id)
	} else if !strings.Contains(err.Error(), "float64") {
		t.Fatalf("boundary refusal must explain the score precision invariant, got: %v", err)
	}
}

// TestActivateValidatesFloorAndIsIdempotent covers the operator-facing flip.
func TestActivateValidatesFloorAndIsIdempotent(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_activate_bot"
	fixture(t, ctx, client, robotID)
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)

	// A floor at or below what has already been issued would put the first activated
	// ids under cursors clients already hold.
	if _, _, err := Activate(context.Background(), ctx, 5000, 5000); !errors.Is(err, ErrFloorTooLow) {
		t.Fatalf("expected ErrFloorTooLow for floor == observed max, got %v", err)
	}
	if _, _, err := Activate(context.Background(), ctx, 4999, 5000); !errors.Is(err, ErrFloorTooLow) {
		t.Fatalf("expected ErrFloorTooLow for floor < observed max, got %v", err)
	}

	flipped, epoch, err := Activate(context.Background(), ctx, 7001, 5000)
	if err != nil || !flipped {
		t.Fatalf("activate: flipped=%v epoch=%d err=%v", flipped, epoch, err)
	}
	st, err := ReadState(ctx)
	if err != nil || !st.Activated() || st.CutoverFloor != 7001 {
		t.Fatalf("state after activate: %+v err=%v", st, err)
	}

	// Idempotent: a second run reports no flip and no error.
	again, sameEpoch, err := Activate(context.Background(), ctx, 9001, 5000)
	if err != nil || again {
		t.Fatalf("second activate should be a no-op: again=%v err=%v", again, err)
	}
	if sameEpoch != epoch {
		t.Errorf("epoch changed on a no-op activate: %d -> %d", epoch, sameEpoch)
	}
	if st2, _ := ReadState(ctx); st2.CutoverFloor != 7001 {
		t.Errorf("a no-op activate must not rewrite the floor (got %d)", st2.CutoverFloor)
	}
}

// TestKnownResidualZaddReorderingCanStillSkip records a gap this change does NOT
// close, so it is a fact in the suite rather than a claim in a PR description.
//
// Allocation and publication are two operations. A producer can allocate N, stall
// (marshal, GC, a slow round trip), while another allocates N+1 and ZADDs — and the
// doorbell wakes the consumer on precisely that ZADD. The consumer reads N+1, moves
// its cursor there, and the first producer's ZADD then lands at N, below the cursor,
// unreachable. Monotonic ids remove collisions and cross-restart inversions; they do
// not remove this window, which needs a re-delivery window below the cursor
// (see modules/bot_api/events.go's own note) and is tracked separately.
//
// This test asserts the loss still happens. If it starts failing, the residual has
// been fixed and this test should become the regression test for that fix.
func TestKnownResidualZaddReorderingCanStillSkip(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_residual_bot"
	fixture(t, ctx, client, robotID)

	stalled, err := nextEventID(ctx, client, robotID) // producer A allocates, then stalls
	if err != nil {
		t.Fatalf("allocate A: %v", err)
	}
	ahead, err := nextEventID(ctx, client, robotID) // producer B allocates and publishes
	if err != nil {
		t.Fatalf("allocate B: %v", err)
	}
	client.ZAdd(QueueKey(robotID), rd.Z{
		Score: float64(ahead), Member: fmt.Sprintf(`{"event_id":%d,"from":"B"}`, ahead)})

	// Consumer wakes on B's doorbell and advances its cursor.
	page, err := client.ZRangeByScore(QueueKey(robotID), rd.ZRangeBy{
		Min: "(0", Max: "+inf", Count: 20}).Result()
	if err != nil || len(page) != 1 {
		t.Fatalf("consumer read: page=%v err=%v", page, err)
	}
	cursor := ahead

	// A's ZADD finally lands, below the cursor.
	client.ZAdd(QueueKey(robotID), rd.Z{
		Score: float64(stalled), Member: fmt.Sprintf(`{"event_id":%d,"from":"A"}`, stalled)})

	after, err := client.ZRangeByScore(QueueKey(robotID), rd.ZRangeBy{
		Min: fmt.Sprintf("(%d", cursor), Max: "+inf", Count: 20}).Result()
	if err != nil {
		t.Fatalf("consumer re-read: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("expected A's event to be unreachable below the cursor (the known residual), "+
			"but the consumer saw %v — if the re-delivery window has landed, invert this test",
			after)
	}
}
