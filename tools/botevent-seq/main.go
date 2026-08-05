// Command botevent-seq is the operator side of the #697 fix: it reports the
// allocator's authoritative state and flips it on.
//
// The authority is the `octo_bot_event_seq_state` row in MySQL, not the Redis key.
// The Redis key is a mirror that lets the hot path check the mode inside the same Lua
// script as the allocation; a lost mirror is rebuilt from this row by the allocator
// itself. Writing only Redis would leave activation state in an instance running
// `appendonly no`, where an RDB rollback would silently drop every replica back to
// the legacy allocator — whose lower ids land beneath live consumer cursors.
//
// Ordering matters and the tool cannot check it for you: activating while any pre-fix
// replica is still running is unsafe, because those replicas allocate from GenSeq
// blocks starting at the bottom while activated replicas allocate above the ceiling.
// Once a consumer's cursor reaches the new range, every id a legacy replica issues
// behind it is permanently unreachable.
//
// This design deliberately does NOT have #627's `FOR SHARE` drain barrier: a
// robotEvent writer is INCR + ZADD with no transaction, so there is nothing to hold a
// lock until. Confirm every replica runs the new image, pause writes briefly, then
// activate.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/spf13/viper"
)

func main() {
	configFile := flag.String("config", "configs/tsdd.yaml", "octo-server config file")
	action := flag.String("action", "preflight", "preflight | activate")
	yes := flag.Bool("yes", false, "required for activate")
	// Defaults to every queue: the rollout treats these totals as the activation
	// floor evidence, so a sampled default would be a validation that silently
	// checked 1% of production. Sampling stays available for a quick look.
	sample := flag.Int("sample", 0, "inspect only the first N queues (0 = all; sampled output is NOT activation evidence)")
	floor := flag.Int64("floor", 0, "explicit cutover floor (0 = use the computed recommendation)")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fatal("%v", err)
	}
	ctx := config.NewContext(cfg)
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) { o.MaxRetries = 2 })

	switch *action {
	case "preflight":
		preflight(ctx, client, *sample)
	case "activate":
		if !*yes {
			fatal("activate requires -yes. Confirm first that EVERY replica runs the post-#697 " +
				"image: activating while a pre-fix replica still allocates from GenSeq puts two id " +
				"sources on one queue, which is the bug (#697), not the fix.")
		}
		activate(ctx, client, *floor, *sample)
	default:
		fatal("unsupported -action %q", *action)
	}
}

// evidence is everything the floor has to clear, gathered from all three sources the
// brief requires: the queues, the legacy GenSeq rows, and the durable high-water marks.
type evidence struct {
	queues          int
	inspected       int
	sampled         bool
	maxQueueScore   int64
	maxLegacyMinSeq int64
	maxHighWater    int64
	dupQueues       int
	dupMembers      int
}

func (e evidence) observedMax() int64 {
	m := e.maxQueueScore
	if e.maxLegacyMinSeq > m {
		m = e.maxLegacyMinSeq
	}
	if e.maxHighWater > m {
		m = e.maxHighWater
	}
	return m
}

func gather(ctx *config.Context, client *rd.Client, sample int) evidence {
	var ev evidence

	var keys []string
	iter := client.Scan(0, botevent.QueueKeyPrefix+"*", 500).Iterator()
	for iter.Next() {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		fatal("scan queues: %v", err)
	}
	sort.Strings(keys)
	ev.queues = len(keys)

	inspect := keys
	if sample > 0 && len(inspect) > sample {
		inspect, ev.sampled = inspect[:sample], true
	}
	ev.inspected = len(inspect)

	for _, k := range inspect {
		robotID := strings.TrimPrefix(k, botevent.QueueKeyPrefix)

		members, err := client.ZRangeWithScores(k, 0, -1).Result()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: read failed: %v\n", k, err)
			continue
		}
		seen := make(map[float64]struct{}, len(members))
		for _, m := range members {
			seen[m.Score] = struct{}{}
			if int64(m.Score) > ev.maxQueueScore {
				ev.maxQueueScore = int64(m.Score)
			}
		}
		if dup := len(members) - len(seen); dup > 0 {
			ev.dupQueues++
			ev.dupMembers += dup
			fmt.Printf("  COLLISIONS %s: members=%d distinct=%d dup=%d\n", k, len(members), len(seen), dup)
		}

		// The legacy GenSeq row is the upper bound on what the old allocator could
		// have handed out for this bot, and the durable mark is what the counter had
		// reached. Both are in MySQL, so neither regresses when Redis does.
		if v := scalarSeq(ctx, "seq:"+common.RobotEventSeqKey+robotID); v > ev.maxLegacyMinSeq {
			ev.maxLegacyMinSeq = v
		}
		if v := scalarSeq(ctx, botevent.HighWaterSeqKey(robotID)); v > ev.maxHighWater {
			ev.maxHighWater = v
		}
	}
	return ev
}

func scalarSeq(ctx *config.Context, key string) int64 {
	var v int64
	if _, err := ctx.DB().SelectBySql("SELECT min_seq FROM `seq` WHERE `key`=?", key).Load(&v); err != nil {
		return 0
	}
	return v
}

func report(ctx *config.Context, ev evidence) int64 {
	st, err := botevent.ReadState(ctx)
	switch {
	case err == botevent.ErrStateMissing:
		fmt.Printf("state: MISSING — the migration has not run. Readers treat this as legacy.\n")
	case err != nil:
		fatal("read state: %v", err)
	default:
		mode := "legacy"
		if st.Activated() {
			mode = "incr (ACTIVATED)"
		}
		fmt.Printf("state: mode=%s epoch=%d cutover_floor=%d\n", mode, st.Epoch, st.CutoverFloor)
	}

	mirror, mErr := readMirror(ctx)
	fmt.Printf("mirror (%s): %s\n\n", botevent.ModeKey, mirror)
	if mErr != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", mErr)
	}

	fmt.Printf("queues: %d   inspected: %d%s\n", ev.queues, ev.inspected,
		map[bool]string{true: "  ** SAMPLED — not activation evidence, rerun with -sample 0 **"}[ev.sampled])
	fmt.Printf("max queue score:      %d\n", ev.maxQueueScore)
	fmt.Printf("max legacy min_seq:   %d\n", ev.maxLegacyMinSeq)
	fmt.Printf("max durable high-water: %d\n", ev.maxHighWater)
	fmt.Printf("observed max (all three): %d\n", ev.observedMax())
	fmt.Printf("queues with duplicate scores: %d   duplicate members: %d\n", ev.dupQueues, ev.dupMembers)

	recommended := ev.observedMax() + 2000
	fmt.Printf("\nrecommended cutover floor: %d  (observed max + one reserved block + margin)\n", recommended)
	fmt.Printf("\nAfter activation the duplicate count must stop increasing. It will not drop:\n")
	fmt.Printf("existing duplicates are deliberately left alone — an ack deletes every member\n")
	fmt.Printf("sharing a score, and there is no record of which of a pair was ever delivered.\n")
	return recommended
}

func preflight(ctx *config.Context, client *rd.Client, sample int) {
	ev := gather(ctx, client, sample)
	report(ctx, ev)
}

func activate(ctx *config.Context, client *rd.Client, floor int64, sample int) {
	ev := gather(ctx, client, sample)
	if ev.sampled {
		fatal("refusing to activate from sampled evidence; rerun without -sample")
	}
	recommended := report(ctx, ev)
	if floor == 0 {
		floor = recommended
	}
	fmt.Printf("\nactivating with floor=%d (observed max=%d)\n", floor, ev.observedMax())

	flipped, epoch, err := botevent.Activate(ctx, floor, ev.observedMax())
	if err != nil {
		fatal("activate: %v", err)
	}
	if !flipped {
		fmt.Printf("already activated at epoch %d; nothing to do\n", epoch)
		return
	}
	// Mirror second: the authority is committed, and the allocator rebuilds the
	// mirror on its own if this write fails.
	if err := client.Set(botevent.ModeKey, botevent.ModeIncr, 0).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: authority flipped (epoch %d) but the mirror write failed "+
			"(%v). Allocators will rebuild it from the DB on their next allocation.\n", epoch, err)
	}
	fmt.Printf("activated at epoch %d. Replicas switch on their next allocation.\n\n", epoch)
	fmt.Printf("Verify now:\n")
	fmt.Printf("  - no `botevent: seed event id counter` errors in logs (a failed seed refuses\n")
	fmt.Printf("    the enqueue rather than issuing an unsafe id)\n")
	fmt.Printf("  - rerun preflight: the duplicate count must stop growing\n")
	fmt.Printf("  - bot events still flowing (POST /v1/bot/events returning results)\n")
	fmt.Printf("  - then roll out OCTO_BOTEVENT_EXPECTED_MODE=incr so a lost mirror AND a lost\n")
	fmt.Printf("    authority row fail closed instead of degrading\n\n")
	fmt.Printf("There is no online deactivate. Going back means every counter-issued id is above\n")
	fmt.Printf("what GenSeq would hand out next, so legacy ids would land below consumer cursors —\n")
	fmt.Printf("the same loss, in reverse. Roll forward.\n")
}

func readMirror(ctx *config.Context) (string, error) {
	v, err := ctx.GetRedisConn().GetString(botevent.ModeKey)
	if err != nil {
		return "(unreadable)", fmt.Errorf("read mirror: %w", err)
	}
	if v == "" {
		return "(absent — allocators will rebuild it from the authority)", nil
	}
	return v, nil
}

// loadConfig mirrors tools/msgextra-version so both operator tools resolve the same
// file plus the same TS_* environment overrides.
func loadConfig(path string) (*config.Config, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	return cfg, nil
}

func fatal(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "botevent-seq: "+f+"\n", a...)
	os.Exit(1)
}
