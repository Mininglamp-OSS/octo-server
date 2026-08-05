package botevent

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

// The allocator's contract is about what two concurrent processes and one
// exclusive cursor do to each other, so these tests run against a real Redis and
// a real MySQL `seq` row rather than fakes. Fakes would restate the code.
//
// tools/genseq-repro holds the mirror image: the same assertions against the
// legacy GenSeq allocator, which fail. Neither is meaningful without the other —
// a lossless-delivery test that passes for both allocators is testing nothing.

// These tests use a **dedicated database**, not the shared `test` one, and create
// the `seq` table themselves.
//
// That is not fastidiousness. `seq` is owned by a migration
// (modules/common/sql/20211108000001_common_legacy01.sql), and this package does
// not go through testutil.NewTestServer, so it has no sql-migrate ledger. Creating
// the table bare in `test` would leave a table that `gorp_migrations` has no row
// for, and the next package whose NewTestServer runs that migration would die with
// `Table 'seq' already exists` — the exact failure mode that has most of
// modules/message's DB tests skipped today.
const defaultSeqTestDB = "root:demo@tcp(127.0.0.1:3306)/botevent_test?charset=utf8mb4&parseTime=true"

func testAddrs() (mysqlAddr, redisAddr string) {
	mysqlAddr = os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if mysqlAddr == "" {
		mysqlAddr = defaultSeqTestDB
	}
	redisAddr = os.Getenv("OCTO_TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	return
}

func seqTestCtx(t *testing.T) (*config.Context, *rd.Client) {
	t.Helper()
	mysqlAddr, redisAddr := testAddrs()

	cfg := config.New()
	cfg.DB.MySQLAddr = mysqlAddr
	ctx := config.NewContext(cfg)

	// The `seq` row is the load-bearing half of the seed: it is the only record of
	// how high the legacy allocator ever went for a bot whose queue is now empty.
	if _, err := ctx.DB().Exec("CREATE TABLE IF NOT EXISTS `seq` (" +
		"`key` varchar(100) NOT NULL, `min_seq` bigint NOT NULL DEFAULT 0, " +
		"`step` int NOT NULL DEFAULT 0, PRIMARY KEY (`key`)) " +
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"); err != nil {
		t.Skipf("no usable MySQL at %s (create the database first: "+
			"CREATE DATABASE botevent_test): %v", mysqlAddr, err)
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

// fixture clears every trace of a bot so each test starts from a known floor.
func fixture(t *testing.T, ctx *config.Context, client *rd.Client, robotID string) {
	t.Helper()
	clean := func() {
		client.Del(SeqKey(robotID), QueueKey(robotID))
		_, _ = ctx.DB().DeleteFrom("seq").Where("`key`=?", "seq:robotEventSeq:"+robotID).Exec()
	}
	clean()
	t.Cleanup(clean)
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
		t.Errorf("ids are not contiguous (%d..%d for %d allocations) — is another writer sharing %s?",
			all[0], all[len(all)-1], len(all), SeqKey(robotID))
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

	if err := seedCounter(ctx, client, robotID); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	afterFirst, _ := client.Get(SeqKey(robotID)).Int64()

	// Advance well past the floor, then re-seed: a second seeder must not undo it.
	for i := 0; i < 50; i++ {
		client.Incr(SeqKey(robotID))
	}
	advanced, _ := client.Get(SeqKey(robotID)).Int64()

	for i := 0; i < 3; i++ {
		if err := seedCounter(ctx, client, robotID); err != nil {
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
