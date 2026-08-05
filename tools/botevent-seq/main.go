// Command botevent-seq is the operator side of the #697 fix: it reports whether
// the monotonic bot-event-id allocator is active and flips it on.
//
// The flip is a runtime state change, not a deploy. It takes effect on every
// replica at the moment the SET commits, because each allocation reads the mode
// inside the same Redis script that allocates (pkg/botevent/seq.go).
//
// Ordering matters and the tool cannot check it for you: activating while any
// pre-fix replica is still running is unsafe. Those replicas allocate from
// GenSeq blocks starting at the bottom, while activated replicas allocate above
// the ceiling — so once a consumer's cursor reaches the new range, every id a
// legacy replica issues behind it is permanently unreachable. Confirm every
// replica runs the new image, then activate.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

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
	sample := flag.Int("sample", 20, "queues to inspect in preflight (0 = all)")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fatal("%v", err)
	}
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) { o.MaxRetries = 2 })

	switch *action {
	case "preflight":
		preflight(client, *sample)
	case "activate":
		if !*yes {
			fatal("activate requires -yes. Confirm first that EVERY replica runs the post-#697 " +
				"image: activating while a pre-fix replica still allocates from GenSeq puts two id " +
				"sources on one queue, which is the bug (#697), not the fix.")
		}
		activate(client)
	default:
		fatal("unsupported -action %q", *action)
	}
}

func currentMode(client *rd.Client) string {
	v, err := client.Get(botevent.ModeKey).Result()
	if err == rd.Nil {
		return "(unset — legacy)"
	}
	if err != nil {
		fatal("read mode: %v", err)
	}
	return v
}

func preflight(client *rd.Client, sample int) {
	fmt.Printf("mode: %s\n", currentMode(client))
	fmt.Printf("mode key: %s   counter prefix: %s\n\n", botevent.ModeKey, botevent.SeqKeyPrefix)

	var keys []string
	iter := client.Scan(0, botevent.QueueKeyPrefix+"*", 500).Iterator()
	for iter.Next() {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		fatal("scan queues: %v", err)
	}
	sort.Strings(keys)
	fmt.Printf("bot event queues: %d\n", len(keys))

	inspect := keys
	if sample > 0 && len(inspect) > sample {
		inspect = inspect[:sample]
		fmt.Printf("inspecting the first %d (pass -sample 0 for all)\n", sample)
	}

	// Collision count is the #697 symptom and the number to watch after
	// activation: it must stop growing. Existing duplicates are not repaired by
	// this change, so it does not go to zero.
	totalDup, withDup := 0, 0
	for _, k := range inspect {
		members, err := client.ZRangeWithScores(k, 0, -1).Result()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: read failed: %v\n", k, err)
			continue
		}
		seen := make(map[float64]int, len(members))
		var maxScore float64
		for _, m := range members {
			seen[m.Score]++
			if m.Score > maxScore {
				maxScore = m.Score
			}
		}
		dup := len(members) - len(seen)
		if dup > 0 {
			withDup++
			totalDup += dup
			robotID := strings.TrimPrefix(k, botevent.QueueKeyPrefix)
			counter, _ := client.Get(botevent.SeqKey(robotID)).Int64()
			fmt.Printf("  COLLISIONS %s: members=%d distinct=%d dup=%d maxScore=%.0f counter=%d\n",
				k, len(members), len(seen), dup, maxScore, counter)
		}
	}
	fmt.Printf("\nqueues inspected: %d   with duplicate scores: %d   duplicate members: %d\n",
		len(inspect), withDup, totalDup)
	fmt.Printf("\nAfter activation this duplicate count must stop increasing. It will not drop:\n")
	fmt.Printf("existing duplicates are deliberately left alone — an ack deletes every member\n")
	fmt.Printf("sharing a score, and there is no record of which of a pair was ever delivered.\n")
}

func activate(client *rd.Client) {
	before := currentMode(client)
	if before == botevent.ModeIncr {
		fmt.Printf("already active (mode=%s), nothing to do\n", before)
		return
	}
	fmt.Printf("mode: %s -> %s\n", before, botevent.ModeIncr)
	if err := client.Set(botevent.ModeKey, botevent.ModeIncr, 0).Err(); err != nil {
		fatal("activate: %v", err)
	}
	fmt.Printf("activated. Every replica switches on its next allocation.\n\n")
	fmt.Printf("Verify now:\n")
	fmt.Printf("  - no `botevent: seed event id counter` errors in logs (a failed seed refuses\n")
	fmt.Printf("    the enqueue rather than issuing an unsafe id)\n")
	fmt.Printf("  - re-run preflight: the duplicate count must stop growing\n")
	fmt.Printf("  - bot events still flowing (POST /v1/bot/events returning results)\n\n")
	fmt.Printf("There is no online deactivate. Going back means every counter-issued id is\n")
	fmt.Printf("above what GenSeq would hand out next, so legacy ids would land below consumer\n")
	fmt.Printf("cursors — the same loss, in reverse. Roll forward.\n")
}

// loadConfig mirrors tools/msgextra-version so both operator tools resolve the
// same file plus the same TS_* environment overrides.
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
