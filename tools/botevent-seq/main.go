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
	"database/sql"
	"errors"
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
			fatal("activate requires -yes, and three preconditions this tool CANNOT check:\n" +
				"  1. Mininglamp-OSS/octo-server#704 is closed. Counter-rollback detection is\n" +
				"     currently tolerance-bounded and process-local, and both gaps are only\n" +
				"     reachable AFTER activation. Merging #702 did not make activation safe.\n" +
				"  2. EVERY replica runs the post-#697 image. Activating while a pre-fix replica\n" +
				"     still allocates from GenSeq puts two id sources on one queue, which is the\n" +
				"     bug (#697), not the fix.\n" +
				"  3. Bot-event writes are paused for a few seconds around the flip. This design\n" +
				"     deliberately has no drain barrier (a robotEvent writer is INCR + ZADD with no\n" +
				"     transaction, so there is nothing to hold a lock until), so a request that has\n" +
				"     already resolved `legacy` and not yet ZADDed will land a low id after the\n" +
				"     flip. The pause is the only thing that closes that window.")
		}
		activate(ctx, client, *floor, *sample)
	default:
		fatal("unsupported -action %q", *action)
	}
}

// evidence is everything the floor has to clear, gathered from all three sources the
// brief requires: the queues, the legacy GenSeq rows, and the durable high-water marks.
//
// The queue scan alone is NOT enough, and an earlier revision of this tool made
// exactly that mistake (review P1-8). Bots are discovered by `SCAN robotEvent:*`, so a
// bot whose queue has fully drained — acked, or expired — has no key, contributes
// nothing, and its `seq` rows were never read. Such a bot can still have clients
// holding a high cursor and a `min_seq` that a lagging replica dragged backwards, so
// the global floor would land below its live cursor while its own per-bot seed, reading
// that same regressed row, could not catch it either. Both `seq` namespaces are
// therefore also swept table-wide, independently of Redis.
type evidence struct {
	queues          int
	inspected       int
	sampled         bool
	maxQueueScore   int64
	maxLegacyMinSeq int64
	maxHighWater    int64
	dupQueues       int
	dupMembers      int

	// failures counts evidence that could not be collected: an unreadable queue, or a
	// `seq` sweep that errored. Any failure makes the totals a lower bound, and a lower
	// bound is not something to validate an irreversible flip against.
	failures int
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

	// Table-wide first, so the floor covers bots the queue scan cannot see at all.
	// One query per namespace, no per-bot lookups.
	if v, err := maxSeqByPrefix(ctx, "seq:"+common.RobotEventSeqKey); err != nil {
		fmt.Fprintf(os.Stderr, "  sweep legacy seq rows failed: %v\n", err)
		ev.failures++
	} else {
		ev.maxLegacyMinSeq = v
	}
	if v, err := maxSeqByPrefix(ctx, botevent.HighWaterSeqKey("")); err != nil {
		fmt.Fprintf(os.Stderr, "  sweep high-water seq rows failed: %v\n", err)
		ev.failures++
	} else {
		ev.maxHighWater = v
	}

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
		members, err := client.ZRangeWithScores(k, 0, -1).Result()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: read failed: %v\n", k, err)
			ev.failures++
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
	}
	return ev
}

// maxSeqByPrefix returns the highest min_seq across every `seq` row in a namespace.
//
// This is the query the per-bot point lookups could not replace: it does not need to
// know which bots exist, so a drained queue cannot hide one. `_` and `%` are not
// special in either namespace (both end in `:` after a fixed identifier), so the LIKE
// pattern needs no escaping beyond the appended wildcard.
func maxSeqByPrefix(ctx *config.Context, prefix string) (int64, error) {
	var max sql.NullInt64
	if _, err := ctx.DB().SelectBySql(
		"SELECT MAX(min_seq) FROM `seq` WHERE `key` LIKE ?", prefix+"%").Load(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return max.Int64, nil
}

func report(ctx *config.Context, ev evidence) int64 {
	st, err := botevent.ReadState(ctx)
	switch {
	case errors.Is(err, botevent.ErrStateMissing):
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
	fmt.Printf("max legacy min_seq:   %d  (whole `seq` namespace, not just bots with a live queue)\n", ev.maxLegacyMinSeq)
	fmt.Printf("max durable high-water: %d  (whole `seq` namespace)\n", ev.maxHighWater)
	fmt.Printf("observed max (all three): %d\n", ev.observedMax())
	fmt.Printf("queues with duplicate scores: %d   duplicate members: %d\n", ev.dupQueues, ev.dupMembers)
	if ev.failures > 0 {
		fmt.Printf("\n** %d evidence source(s) could not be read — every total above is a LOWER BOUND **\n", ev.failures)
	}

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

// maxSafeFloor is the highest cutover floor this tool will accept.
//
// Sorted-set scores are float64, so above 2^53 distinct int64 ids stop having distinct
// scores — which recreates the pagination skip and the multi-member ack this whole
// change removes, from the one command meant to prevent them (review P2-3). 2^50 leaves
// three orders of magnitude of headroom above any plausible id and is still nowhere
// near the precision boundary.
const maxSafeFloor = 1 << 50

func activate(ctx *config.Context, client *rd.Client, floor int64, sample int) {
	ev := gather(ctx, client, sample)
	if ev.sampled {
		fatal("refusing to activate from sampled evidence; rerun without -sample")
	}
	if ev.failures > 0 {
		fatal("refusing to activate from incomplete evidence: %d source(s) could not be read, so "+
			"the observed maximum is a lower bound. A floor validated against a lower bound can "+
			"still land beneath a live consumer cursor, which is the loss this flip exists to stop. "+
			"Fix the read errors above and rerun.", ev.failures)
	}
	recommended := report(ctx, ev)
	if floor == 0 {
		floor = recommended
	}
	if floor <= 0 || floor > maxSafeFloor {
		fatal("refusing floor=%d: it must be positive and at most %d, above which int64 ids no "+
			"longer have distinct float64 sorted-set scores", floor, int64(maxSafeFloor))
	}
	refuseUnauthorizedMirror(ctx, client)
	fmt.Printf("\nactivating with floor=%d (observed max=%d)\n", floor, ev.observedMax())

	flipped, epoch, err := botevent.Activate(ctx, floor, ev.observedMax())
	if err != nil {
		fatal("activate: %v", err)
	}
	if !flipped {
		fmt.Printf("already activated at epoch %d; nothing to do\n", epoch)
		// Still reconcile the mirror: an already-flipped authority with a stale or
		// missing mirror is the state that makes every replica read the authority on
		// every allocation until one of them rewrites the key.
		writeMirror(client, epoch)
		return
	}
	// Mirror second: the authority is committed, and the allocator rebuilds the
	// mirror on its own if this write fails.
	writeMirror(client, epoch)
	fmt.Printf("activated at epoch %d. Replicas switch on their next allocation.\n\n", epoch)
	fmt.Printf("Verify now:\n")
	fmt.Printf("  - no `botevent: seed event id counter` errors in logs (a failed seed refuses\n")
	fmt.Printf("    the enqueue rather than issuing an unsafe id)\n")
	fmt.Printf("  - rerun preflight: the duplicate count must stop growing\n")
	fmt.Printf("  - bot events still flowing (POST /v1/bot/events returning results)\n")
	fmt.Printf("  - dmwork_bot_event_seq_mirror_unauthorized_total stays 0 (non-zero means some\n")
	fmt.Printf("    replica saw a mode mirror the authority did not confirm)\n")
	fmt.Printf("  - then roll out OCTO_BOTEVENT_EXPECTED_MODE=incr so a lost mirror AND a lost\n")
	fmt.Printf("    authority row fail closed instead of degrading\n\n")
	fmt.Printf("There is no online deactivate. Going back means every counter-issued id is above\n")
	fmt.Printf("what GenSeq would hand out next, so legacy ids would land below consumer cursors —\n")
	fmt.Printf("the same loss, in reverse. Roll forward.\n")
}

// writeMirror publishes the generation-stamped mirror value.
//
// It must be the exact spelling the allocator validates (botevent.MirrorValue): a bare
// `incr` is only a claim, and every replica would keep re-reading the authority until
// somebody wrote the real value.
// mirrorVerdict is what the pre-flip mirror check decided.
type mirrorVerdict int

const (
	// mirrorOK: nothing to say.
	mirrorOK mirrorVerdict = iota
	// mirrorNote: worth printing, does not stop the flip.
	mirrorNote
	// mirrorRefuse: stop.
	mirrorRefuse
)

// judgeMirror decides what a pre-flip mirror value means, given whether the authority says
// the allocator is already activated.
//
// Split out from the Redis/MySQL plumbing so the matrix is testable without either — this
// tool carries the whole activation procedure and had no tests at all, which is how the bug
// below shipped (review round 7 P2-1, found independently by two reviewers).
//
// `activated` is the authority's answer, and reading it first is the entire fix. The previous
// revision refused on *any* parseable mirror claim without consulting the state row, so
// re-running `activate -yes` on an already-activated system — a correct mirror, the normal
// state after a successful flip — died with "already holds incr:1 while the authority says
// legacy … Confirm which, then DEL the key and rerun". Every clause was false in the common
// case, it told the operator to delete the live mirror, and it made the documented
// "already activated; reconcile the mirror" branch unreachable whenever a mirror existed.
func judgeMirror(activated bool, mirror string, absent bool) (mirrorVerdict, string) {
	if absent {
		return mirrorOK, ""
	}
	epoch, claims := botevent.ParseMirrorEpoch(mirror)
	if !claims {
		// Not a claim of activation. Worth reporting, but it does not interact with the
		// flip: the allocator treats a malformed value as absent, and this run overwrites it.
		return mirrorNote, fmt.Sprintf("note: %s holds %q, which is not a valid mirror value; "+
			"allocators treat it as absent and it will be overwritten\n", botevent.ModeKey, mirror)
	}
	if activated {
		// The authority agrees. This is the ordinary re-run, and reconciling the mirror is
		// exactly what the operator wants — `activate` is documented as idempotent.
		return mirrorNote, fmt.Sprintf("note: %s already holds %q and the authority agrees "+
			"(epoch %d); the mirror will be reconciled\n", botevent.ModeKey, mirror, epoch)
	}
	return mirrorRefuse, fmt.Sprintf("refusing to activate: %s holds %q while the authority "+
		"says the allocator is NOT activated.\n"+
		"Nothing should have written that — it means this Redis was activated before, is shared "+
		"with another environment, or was restored from an activated snapshot. Confirm which, "+
		"then DEL the key and rerun.\n"+
		"Allocators are meanwhile ignoring it and staying on the legacy allocator "+
		"(dmwork_bot_event_seq_mirror_unauthorized_total is counting it). Note they do NOT "+
		"delete it: round 7 of #697's review showed that deleting a mirror on the authority's "+
		"word takes the fleet down when the authority is the party that regressed.",
		botevent.ModeKey, mirror)
}

// refuseUnauthorizedMirror stops an activation while a mirror claiming activation sits
// against an authority that says otherwise.
//
// That state means this Redis is not what the operator believes: someone wrote the key, it is
// shared with another environment, or a snapshot from an activated era was restored. Any of
// those wants a human look before an irreversible flip — and it is also the precondition for
// the one residual window in the allocator's denial cooldown (see pkg/botevent/mode.go), so
// checking it here is where that window gets closed in practice.
//
// Runs after the floor checks so the operator sees every other problem in one pass.
func refuseUnauthorizedMirror(ctx *config.Context, client *rd.Client) {
	v, err := client.Get(botevent.ModeKey).Result()
	absent := err == rd.Nil
	if err != nil && !absent {
		fatal("cannot read the mode mirror (%v); refusing to activate without knowing whether a "+
			"stale one is present", err)
	}

	// Read the authority before judging the mirror. Skipping this is what made a correct
	// mirror look like a forged one.
	activated := false
	switch st, sErr := botevent.ReadState(ctx); {
	case sErr == nil:
		activated = st.Activated()
	case errors.Is(sErr, botevent.ErrStateMissing):
		// The migration has not run. Not activated, and a mirror claiming otherwise is
		// exactly the case worth refusing on.
	default:
		fatal("cannot read the allocator state (%v); refusing to judge the mirror without it — "+
			"that is the mistake this check was added to fix", sErr)
	}

	verdict, msg := judgeMirror(activated, v, absent)
	switch verdict {
	case mirrorRefuse:
		fatal("%s", msg)
	case mirrorNote:
		fmt.Fprint(os.Stderr, msg)
	}
}

func writeMirror(client *rd.Client, epoch uint64) {
	if err := client.Set(botevent.ModeKey, botevent.MirrorValue(epoch), 0).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: authority is at epoch %d but the mirror write failed "+
			"(%v). Allocators will rebuild it from the DB on their next allocation.\n", epoch, err)
	}
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
