// Command genseq-repro reproduces the #697 defect against a real MySQL and Redis:
// octo-lib's GenSeq is a per-process HiLo block allocator, so two replicas that
// cold-start concurrently hand out the SAME sequence numbers — and those numbers
// are used as sorted-set scores for bot event delivery, which paginates with an
// exclusive cursor and deletes by score.
//
// It calls the real config.Context.GenSeq (no reimplementation) from two child
// processes, because seqMap/seqLock are package-level in octo-lib and a single
// process therefore cannot hold two independent block states.
//
// Read-only against production is not involved; this talks to local containers.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

var (
	mysqlAddr = flag.String("mysql", "root:demo@tcp(host.docker.internal:3306)/genseq_repro?charset=utf8mb4&parseTime=true", "")
	redisAddr = flag.String("redis", "host.docker.internal:6379", "")
	child     = flag.Bool("child", false, "run as one replica")
	label     = flag.String("label", "", "replica label")
	key       = flag.String("key", "reproRobotEvent", "seq flag")
	count     = flag.Int("n", 12, "ids per replica")
	startAt   = flag.Int64("start-at", 0, "unix milli barrier")
	seedFloor = flag.Int64("seed", 5000, "pre-existing seq.min_seq")
)

func newCfg() *config.Config {
	cfg := config.New()
	cfg.DB.MySQLAddr = *mysqlAddr
	cfg.DB.RedisAddr = *redisAddr
	return cfg
}

func newCtx() *config.Context { return config.NewContext(newCfg()) }

// newRedis goes through the octoredis chokepoint rather than rd.NewClient.
// pkg/redis's TestNoRawRedisClientOutsideChokepoint scans the whole repo, tools
// included, so a raw client here fails CI — and the instrumentation it enforces
// (dependency="redis" on every command) is worth having even in a repro tool.
func newRedis() *rd.Client {
	return octoredis.NewInstrumentedClient(newCfg(), func(o *rd.Options) { o.MaxRetries = 1 })
}

// runChild is one replica: cold-start GenSeq, wait for the shared barrier so both
// replicas read seq.min_seq before either writes it back, then allocate.
func runChild() {
	ctx := newCtx()
	if *startAt > 0 {
		time.Sleep(time.Until(time.UnixMilli(*startAt)))
	}
	out := make([]string, 0, *count)
	for i := 0; i < *count; i++ {
		v, err := ctx.GenSeq(*key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: GenSeq: %v\n", *label, err)
			os.Exit(1)
		}
		out = append(out, strconv.FormatInt(v, 10))
	}
	fmt.Println(strings.Join(out, ","))
}

func main() {
	flag.Parse()
	if *child {
		runChild()
		return
	}

	ctx := newCtx()
	db := ctx.DB()
	if _, err := db.DeleteFrom("seq").Where("`key`=?", "seq:"+*key).Exec(); err != nil {
		fatal("clean seq row: %v", err)
	}
	if _, err := db.InsertBySql(
		"insert into `seq`(`key`,min_seq,step) values(?,?,?)",
		"seq:"+*key, *seedFloor, 1000).Exec(); err != nil {
		fatal("seed seq row: %v", err)
	}
	fmt.Printf("seeded seq:%s min_seq=%d step=1000\n\n", *key, *seedFloor)

	barrier := time.Now().Add(3 * time.Second).UnixMilli()
	self, err := os.Executable()
	if err != nil {
		fatal("executable: %v", err)
	}

	var wg sync.WaitGroup
	results := make([][]int64, 2)
	labels := []string{"replica-A", "replica-B"}
	for i := range labels {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(self,
				"-child", "-label", labels[i],
				"-mysql", *mysqlAddr, "-key", *key,
				"-n", strconv.Itoa(*count),
				"-start-at", strconv.FormatInt(barrier, 10))
			cmd.Stderr = os.Stderr
			raw, err := cmd.Output()
			if err != nil {
				fatal("%s: %v", labels[i], err)
			}
			for _, f := range strings.Split(strings.TrimSpace(string(raw)), ",") {
				n, _ := strconv.ParseInt(f, 10, 64)
				results[i] = append(results[i], n)
			}
		}(i)
	}
	wg.Wait()

	for i, l := range labels {
		fmt.Printf("%s issued: %v\n", l, results[i])
	}

	seen := map[int64][]string{}
	for i, l := range labels {
		for _, v := range results[i] {
			seen[v] = append(seen[v], l)
		}
	}
	var dupes []int64
	for v, who := range seen {
		if len(who) > 1 {
			dupes = append(dupes, v)
		}
	}
	sort.Slice(dupes, func(a, b int) bool { return dupes[a] < dupes[b] })

	fmt.Printf("\n--- finding 1: are ids unique across replicas? ---\n")
	if len(dupes) == 0 {
		fmt.Printf("UNIQUE — no collision this run (retry; the barrier is best-effort)\n")
	} else {
		fmt.Printf("COLLISION: %d ids issued by BOTH replicas: %v\n", len(dupes), dupes)
	}

	var mysqlAfter int64
	if _, err := db.Select("min_seq").From("seq").
		Where("`key`=?", "seq:"+*key).Load(&mysqlAfter); err != nil {
		fatal("read back min_seq: %v", err)
	}
	fmt.Printf("seq.min_seq after both replicas: %d (unconditional ON DUPLICATE KEY UPDATE)\n", mysqlAfter)

	if len(dupes) > 0 {
		proveDeliveryLoss(results, labels, dupes)
	}
}

// proveDeliveryLoss replays the collision through the exact read and ack shape of
// modules/bot_api/events.go: ZRANGEBYSCORE with an exclusive Min, and
// ZREMRANGEBYSCORE(id, id) on ack.
func proveDeliveryLoss(results [][]int64, labels []string, dupes []int64) {
	client := newRedis()
	qkey := "reproRobotEvent:zset"
	client.Del(qkey)

	type ev struct {
		id  int64
		who string
	}
	var all []ev
	for i, l := range labels {
		for _, v := range results[i] {
			all = append(all, ev{v, l})
		}
	}
	for _, e := range all {
		// distinct member payload, same score — exactly what the five ZADD
		// producers do: ZAdd(key, float64(seq), payload{EventID: seq})
		client.ZAdd(qkey, rd.Z{Score: float64(e.id), Member: fmt.Sprintf(`{"event_id":%d,"from":"%s"}`, e.id, e.who)})
	}
	stored, _ := client.ZCard(qkey).Result()
	fmt.Printf("\n--- finding 2: exclusive-cursor delivery over the collided queue ---\n")
	fmt.Printf("enqueued %d events, ZSET holds %d members\n", len(all), stored)

	// Consume the way a bot does: cursor advances to max event_id observed.
	delivered := map[string]bool{}
	var cursor int64
	for {
		page, err := client.ZRangeByScore(qkey, rd.ZRangeBy{
			Min: fmt.Sprintf("(%d", cursor), Max: "+inf", Count: 20,
		}).Result()
		if err != nil || len(page) == 0 {
			break
		}
		for _, m := range page {
			delivered[m] = true
			if id := eventIDOf(m); id > cursor {
				cursor = id
			}
		}
	}
	fmt.Printf("delivered %d of %d members (cursor ended at %d)\n", len(delivered), stored, cursor)

	var lost []string
	everything, _ := client.ZRange(qkey, 0, -1).Result()
	for _, m := range everything {
		if !delivered[m] {
			lost = append(lost, m)
		}
	}
	if len(lost) == 0 {
		fmt.Printf("(a whole-queue read in one pass loses nothing — the skip needs a page\n")
		fmt.Printf(" boundary on the shared score, or a late low-block enqueue; both below)\n")
	} else {
		fmt.Printf("SKIPPED FOREVER (%d): %v\n", len(lost), lost)
	}

	// finding 3 runs before the two skip scenarios, which rebuild the queue.
	victim := strconv.FormatInt(dupes[0], 10)
	before, _ := client.ZRangeByScore(qkey, rd.ZRangeBy{Min: victim, Max: victim}).Result()
	client.ZRemRangeByScore(qkey, victim, victim)
	after, _ := client.ZRangeByScore(qkey, rd.ZRangeBy{Min: victim, Max: victim}).Result()
	fmt.Printf("\n--- finding 3: ack collateral damage ---\n")
	fmt.Printf("score %s held %d members before ack: %v\n", victim, len(before), before)
	fmt.Printf("after ZRemRangeByScore(%s,%s): %d remain\n", victim, victim, len(after))
	if len(before) > 1 {
		fmt.Printf("ACK DESTROYED %d event(s) while acking 1\n", len(before)-1)
	}

	proveSkipAtPageBoundary(client, qkey, dupes[0])
	proveSkipOnLateLowBlock(client, qkey)
	client.Del(qkey)
}

// proveSkipAtPageBoundary shows the loss that needs no time inversion at all: when a
// page ends on the first of two members sharing a score, the exclusive `(cursor` on
// the next read excludes the second one permanently. events.go uses limit 20..100, so
// on a queue with >1000 duplicate scores this boundary is hit routinely.
func proveSkipAtPageBoundary(client *rd.Client, qkey string, shared int64) {
	client.Del(qkey)
	client.ZAdd(qkey, rd.Z{Score: float64(shared), Member: fmt.Sprintf(`{"event_id":%d,"from":"replica-A"}`, shared)})
	client.ZAdd(qkey, rd.Z{Score: float64(shared), Member: fmt.Sprintf(`{"event_id":%d,"from":"replica-B"}`, shared)})
	client.ZAdd(qkey, rd.Z{Score: float64(shared + 1), Member: fmt.Sprintf(`{"event_id":%d,"from":"replica-A"}`, shared+1)})

	fmt.Printf("\n--- finding 2a: page boundary lands on a shared score ---\n")
	total, _ := client.ZCard(qkey).Result()

	delivered, cursor := consume(client, qkey, 1) // limit 1 puts the boundary on the shared score
	fmt.Printf("queue has %d members; consumed with limit=1, cursor ended at %d\n", total, cursor)
	fmt.Printf("delivered %d:\n", len(delivered))
	for _, m := range delivered {
		fmt.Printf("    %s\n", m)
	}
	all, _ := client.ZRange(qkey, 0, -1).Result()
	seen := map[string]bool{}
	for _, m := range delivered {
		seen[m] = true
	}
	for _, m := range all {
		if !seen[m] {
			fmt.Printf("  SKIPPED FOREVER: %s\n", m)
		}
	}
}

// proveSkipOnLateLowBlock reproduces what was actually measured in production: a
// replica holding a lower block enqueues AFTER the cursor has advanced into a higher
// block, so its events are born already below the cursor.
func proveSkipOnLateLowBlock(client *rd.Client, qkey string) {
	client.Del(qkey)
	fmt.Printf("\n--- finding 2b: late enqueue from a lower block (the production shape) ---\n")

	// replica-B is on block 6xxx and enqueues first.
	for _, id := range []int64{6001, 6002} {
		client.ZAdd(qkey, rd.Z{Score: float64(id), Member: fmt.Sprintf(`{"event_id":%d,"from":"replica-B"}`, id)})
	}
	delivered, cursor := consume(client, qkey, 20)
	fmt.Printf("bot polls: delivered %d, cursor=%d\n", len(delivered), cursor)

	// replica-A is still on block 5xxx and enqueues later in wall-clock time.
	late := []int64{5013, 5014}
	for _, id := range late {
		client.ZAdd(qkey, rd.Z{Score: float64(id), Member: fmt.Sprintf(`{"event_id":%d,"from":"replica-A"}`, id)})
	}
	fmt.Printf("replica-A then enqueues %v (later in time, lower score)\n", late)

	page, _ := client.ZRangeByScore(qkey, rd.ZRangeBy{
		Min: fmt.Sprintf("(%d", cursor), Max: "+inf", Count: 20,
	}).Result()
	fmt.Printf("bot polls again from (%d: got %d events\n", cursor, len(page))
	if len(page) == 0 {
		fmt.Printf("  SKIPPED FOREVER: %v — born below the cursor, never deliverable\n", late)
	}
}

// consume mimics readEventPage: exclusive cursor, advanced to the max event_id seen.
func consume(client *rd.Client, qkey string, limit int64) ([]string, int64) {
	var out []string
	var cursor int64
	for {
		page, err := client.ZRangeByScore(qkey, rd.ZRangeBy{
			Min: fmt.Sprintf("(%d", cursor), Max: "+inf", Count: limit,
		}).Result()
		if err != nil || len(page) == 0 {
			break
		}
		moved := false
		for _, m := range page {
			out = append(out, m)
			if id := eventIDOf(m); id > cursor {
				cursor = id
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	return out, cursor
}

func eventIDOf(member string) int64 {
	const p = `{"event_id":`
	i := strings.Index(member, p)
	if i < 0 {
		return 0
	}
	rest := member[i+len(p):]
	j := strings.IndexAny(rest, ",}")
	n, _ := strconv.ParseInt(rest[:j], 10, 64)
	return n
}

func fatal(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "genseq-repro: "+f+"\n", a...)
	os.Exit(1)
}
