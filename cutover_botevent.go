package main

// `app cutover botevent` — the operator side of the #697 fix, moved here from
// tools/botevent-seq so it ships inside the image. It reports the allocator's
// authoritative state and flips it on.
//
// The authority is the `octo_bot_event_seq_state` row in MySQL, not the Redis key.
// The Redis key is a mirror that lets the hot path check the mode inside the same Lua
// script as the allocation. A lost mirror can eventually be rebuilt from this row by
// an allocator after its negative belief expires, but publishing the mirror is still a
// required cutover step: this command fails and writes must stay paused if that write
// does not complete. Writing only Redis would leave activation state in an instance running
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

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const activationPreconditions = `activate preconditions this tool CANNOT check:
  1. Mininglamp-OSS/octo-server#704 is closed, including an independently reviewed
     cutover floor that cannot inherit a regressed legacy min_seq and land below a
     cursor already held by a consumer. Merging or deploying #702 is not activation.
  2. EVERY replica runs the post-#697 image. Activating while a pre-fix replica still
     allocates from GenSeq puts two id sources on one queue, which is the bug (#697),
     not the fix.
  3. Bot-event writes are paused for a few seconds around the flip. This design has no
     drain barrier (a robotEvent writer is INCR + ZADD with no transaction), so a
     request that resolved legacy before the flip can otherwise publish a low id after it.

`

func boteventCutoverDomain() *cutoverDomain {
	d := &cutoverDomain{
		name:    "botevent",
		summary: "#697 bot event id allocator (legacy GenSeq → monotonic Redis counter)",
	}
	// Defaults to every queue: the rollout treats these totals as the activation
	// floor evidence, so a sampled default would be a validation that silently
	// checked 1% of production. Sampling stays available for a quick look.
	var sample int
	d.registerFlags = func(fs *flag.FlagSet) {
		fs.IntVar(&sample, "sample", 0, "inspect only the first N queues (0 = all; sampled output is NOT activation evidence)")
	}
	d.preflight = func(rt *cutoverRuntime, out io.Writer) error {
		return boteventCutoverPreflight(rt, out, sample)
	}
	d.activate = func(rt *cutoverRuntime, out io.Writer) error {
		return boteventCutoverActivate(rt, out, sample)
	}
	d.status = boteventCutoverStatus
	return d
}

func boteventRedisClient(cfg *config.Config) *rd.Client {
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) { o.MaxRetries = 2 })
	fmt.Fprintf(os.Stderr, "redis: %s  db=%d\n", client.Options().Addr, client.Options().DB)
	return client
}

func boteventCutoverPreflight(rt *cutoverRuntime, out io.Writer, sample int) error {
	client := boteventRedisClient(rt.cfg)
	defer client.Close()
	ev, err := gatherBoteventEvidence(rt.ctx, client, out, sample)
	if err != nil {
		return err
	}
	_, err = reportBoteventEvidence(rt.ctx, out, ev)
	return err
}

func boteventCutoverStatus(rt *cutoverRuntime, out io.Writer) error {
	// The mirror is read from Redis, so name the server first — a status that
	// silently reads the wrong instance reports the wrong activation state.
	fprintRedisEndpoint(rt.cfg)
	if err := fprintBoteventState(rt.ctx, out); err != nil {
		return err
	}
	mirror, mErr := readBoteventMirror(rt.ctx)
	fmt.Fprintf(out, "mirror (%s): %s\n", botevent.ModeKey, mirror)
	if mErr != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", mErr)
	}
	fprintCutoverGuard(out, botevent.ExpectedModeEnv, botevent.ExpectedModeSpellings(), boteventModeName)
	return nil
}

// boteventModeName renders a mode with the domain's own spelling table, so the
// name shown here and the guard values the allocator accepts have one source.
func boteventModeName(mode int) string {
	return cutoverModeName(botevent.ExpectedModeSpellings(), mode)
}

// boteventEvidence is everything the floor has to clear, gathered from all three
// sources the brief requires: the queues, the legacy GenSeq rows, and the durable
// high-water marks.
//
// The queue scan alone is NOT enough, and an earlier revision of the standalone
// tool made exactly that mistake (review P1-8). Bots are discovered by
// `SCAN robotEvent:*`, so a bot whose queue has fully drained — acked, or expired —
// has no key, contributes nothing, and its `seq` rows were never read. Such a bot
// can still have clients holding a high cursor and a `min_seq` that a lagging
// replica dragged backwards, so the global floor would land below its live cursor
// while its own per-bot seed, reading that same regressed row, could not catch it
// either. Both `seq` namespaces are therefore also swept table-wide, independently
// of Redis.
type boteventEvidence struct {
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

const queueScanPageSize int64 = 500

type queueScoreStats struct {
	members    int
	duplicates int
	maxScore   int64
}

func (e boteventEvidence) observedMax() int64 {
	m := e.maxQueueScore
	if e.maxLegacyMinSeq > m {
		m = e.maxLegacyMinSeq
	}
	if e.maxHighWater > m {
		m = e.maxHighWater
	}
	return m
}

// out receives the per-queue collision diagnostics so one activation's evidence
// lands in one place; read failures still go to stderr alongside the other
// warnings.
func gatherBoteventEvidence(ctx *config.Context, client *rd.Client, out io.Writer, sample int) (boteventEvidence, error) {
	var ev boteventEvidence

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
		return boteventEvidence{}, fmt.Errorf("scan queues: %w", err)
	}
	sort.Strings(keys)
	ev.queues = len(keys)

	inspect := keys
	if sample > 0 && len(inspect) > sample {
		inspect, ev.sampled = inspect[:sample], true
	}
	ev.inspected = len(inspect)

	for _, k := range inspect {
		stats, err := inspectQueueScores(
			func() ([]rd.Z, error) {
				return client.ZRevRangeWithScores(k, 0, 0).Result()
			},
			func(start, stop int64) ([]rd.Z, error) {
				return client.ZRangeWithScores(k, start, stop).Result()
			},
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: read failed: %v\n", k, err)
			ev.failures++
			continue
		}
		if stats.maxScore > ev.maxQueueScore {
			ev.maxQueueScore = stats.maxScore
		}
		if stats.duplicates > 0 {
			ev.dupQueues++
			ev.dupMembers += stats.duplicates
			fmt.Fprintf(out, "  COLLISIONS %s: members=%d distinct=%d dup=%d\n",
				k, stats.members, stats.members-stats.duplicates, stats.duplicates)
		}
	}
	return ev, nil
}

// inspectQueueScores captures the load-bearing maximum with one atomic Redis command,
// then gathers diagnostic duplicate counts in bounded rank pages.
//
// The separation is deliberate. Pausing producers at cutover does not pause consumer ACKs:
// ZREM from the low end shifts subsequent ranks, so a paged walk can stop before observing
// the surviving high end. A ZREVRANGE 0 0 result taken before that walk is an upper bound
// under ACK-only mutation and therefore safe to validate an irreversible floor against.
func inspectQueueScores(fetchMax func() ([]rd.Z, error), fetchPage func(start, stop int64) ([]rd.Z, error)) (queueScoreStats, error) {
	top, err := fetchMax()
	if err != nil {
		return queueScoreStats{}, err
	}
	stats, err := scanQueueScores(fetchPage)
	if err != nil {
		return queueScoreStats{}, err
	}
	if len(top) > 0 {
		atomicMax := int64(top[0].Score)
		if atomicMax > stats.maxScore {
			stats.maxScore = atomicMax
		}
	}
	return stats, nil
}

// scanQueueScores reads one sorted set in bounded rank pages and counts adjacent equal
// scores as duplicates. ZRANGE orders by score and then member, so equal scores remain
// adjacent even across page boundaries; retaining only the previous score keeps memory O(1)
// regardless of queue size and caps every Redis response at queueScanPageSize members.
// Under concurrent ACKs these diagnostics may be a lower bound; inspectQueueScores obtains
// the activation-critical maximum independently, so this best-effort walk cannot lower the
// cutover floor.
func scanQueueScores(fetch func(start, stop int64) ([]rd.Z, error)) (queueScoreStats, error) {
	var stats queueScoreStats
	var previous float64
	havePrevious := false
	for start := int64(0); ; {
		page, err := fetch(start, start+queueScanPageSize-1)
		if err != nil {
			return queueScoreStats{}, err
		}
		for _, member := range page {
			stats.members++
			if havePrevious && member.Score == previous {
				stats.duplicates++
			}
			previous = member.Score
			havePrevious = true
			if score := int64(member.Score); score > stats.maxScore {
				stats.maxScore = score
			}
		}
		if int64(len(page)) < queueScanPageSize {
			return stats, nil
		}
		start += int64(len(page))
	}
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

func fprintBoteventState(ctx *config.Context, out io.Writer) error {
	st, err := botevent.ReadState(ctx)
	switch {
	case errors.Is(err, botevent.ErrStateMissing):
		fmt.Fprintf(out, "state: MISSING — the migration has not run. Readers treat this as legacy.\n")
	case err != nil:
		return fmt.Errorf("read state: %w", err)
	default:
		mode := botevent.ModeLegacy
		if st.Activated() {
			mode = botevent.ModeIncr + " (ACTIVATED)"
		}
		fmt.Fprintf(out, "state: mode=%s epoch=%d cutover_floor=%d\n", mode, st.Epoch, st.CutoverFloor)
	}
	return nil
}

func reportBoteventEvidence(ctx *config.Context, out io.Writer, ev boteventEvidence) (int64, error) {
	if err := fprintBoteventState(ctx, out); err != nil {
		return 0, err
	}
	mirror, mErr := readBoteventMirror(ctx)
	fmt.Fprintf(out, "mirror (%s): %s\n\n", botevent.ModeKey, mirror)
	if mErr != nil {
		fmt.Fprintf(os.Stderr, "note: %v\n", mErr)
	}

	fmt.Fprintf(out, "queues: %d   inspected: %d%s\n", ev.queues, ev.inspected,
		map[bool]string{true: "  ** SAMPLED — not activation evidence, rerun with -sample 0 **"}[ev.sampled])
	fmt.Fprintf(out, "max queue score:      %d\n", ev.maxQueueScore)
	fmt.Fprintf(out, "max legacy min_seq:   %d  (whole `seq` namespace, not just bots with a live queue)\n", ev.maxLegacyMinSeq)
	fmt.Fprintf(out, "max durable high-water: %d  (whole `seq` namespace)\n", ev.maxHighWater)
	fmt.Fprintf(out, "observed max (all three): %d\n", ev.observedMax())
	fmt.Fprintf(out, "queues with duplicate scores: %d   duplicate members: %d\n", ev.dupQueues, ev.dupMembers)
	if ev.failures > 0 {
		fmt.Fprintf(out, "\n** %d evidence source(s) could not be read — every total above is a LOWER BOUND **\n", ev.failures)
	}

	recommended := ev.observedMax() + 2000
	fmt.Fprintf(out, "\nrecommended cutover floor: %d  (observed max + one reserved block + margin)\n", recommended)
	fmt.Fprintf(out, "\nAfter activation the duplicate count must stop increasing. It will not drop:\n")
	fmt.Fprintf(out, "existing duplicates are deliberately left alone — an ack deletes every member\n")
	fmt.Fprintf(out, "sharing a score, and there is no record of which of a pair was ever delivered.\n")
	return recommended, nil
}

// maxSafeFloor is the highest cutover floor this command will accept. The bound
// itself lives in the domain (botevent.MaxCutoverFloor) and is enforced inside
// Activate as well, so a caller that is not this command is bounded too; the
// check here exists to refuse before gathering evidence and to say why.
const maxSafeFloor = botevent.MaxCutoverFloor

func boteventCutoverActivate(rt *cutoverRuntime, out io.Writer, sample int) error {
	// -yes confirms these conditions; it must not hide them from the audit log.
	fmt.Fprint(os.Stderr, activationPreconditions)
	if !rt.yes {
		return errors.New("activate requires -yes after every precondition above has been verified")
	}
	client := boteventRedisClient(rt.cfg)
	defer client.Close()

	ev, err := gatherBoteventEvidence(rt.ctx, client, out, sample)
	if err != nil {
		return err
	}
	if ev.sampled {
		return errors.New("refusing to activate from sampled evidence; rerun without -sample")
	}
	if ev.failures > 0 {
		return fmt.Errorf("refusing to activate from incomplete evidence: %d source(s) could not be read, so "+
			"the observed maximum is a lower bound. A floor validated against a lower bound can "+
			"still land beneath a live consumer cursor, which is the loss this flip exists to stop. "+
			"Fix the read errors above and rerun.", ev.failures)
	}
	recommended, err := reportBoteventEvidence(rt.ctx, out, ev)
	if err != nil {
		return err
	}
	floor := rt.floor
	if floor == 0 {
		floor = recommended
	}
	if floor <= 0 || floor > maxSafeFloor {
		return fmt.Errorf("refusing floor=%d: it must be positive and at most %d, above which int64 ids no "+
			"longer have distinct float64 sorted-set scores", floor, int64(maxSafeFloor))
	}
	if err := refuseUnauthorizedBoteventMirror(rt.ctx, client); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nactivating with floor=%d (observed max=%d)\n", floor, ev.observedMax())

	flipped, epoch, err := botevent.Activate(rt.deadline, rt.ctx, floor, ev.observedMax())
	// flipped before err: Flip joins a post-commit connection-cleanup failure
	// into err, and treating that as "activate failed" would skip the mirror
	// publication below for an authority that is already committed — the exact
	// half-done state this command exists to prevent.
	if !flipped {
		if err != nil {
			return fmt.Errorf("activate: %w", err)
		}
		fmt.Fprintf(out, "already activated at epoch %d; nothing to do\n", epoch)
		// Still reconcile the mirror. Failure is operationally incomplete even though
		// the authority was already committed, so preserve the write pause and exit non-zero.
		return writeMirror(client, epoch)
	}
	if err != nil {
		// The row is committed; the mirror still must be published, and the
		// operator must know the connection released badly.
		fmt.Fprintf(os.Stderr, "note: the flip COMMITTED, but releasing the database connection failed: %v\n", err)
	}
	// Mirror second: the authority is committed. A failure here leaves cached negative
	// beliefs able to use GenSeq temporarily, so it is not a successful activation.
	if mirrorErr := writeMirror(client, epoch); mirrorErr != nil {
		return mirrorErr
	}
	fmt.Fprintf(out, "activated at epoch %d. Replicas switch on their next allocation.\n\n", epoch)
	fmt.Fprintf(out, "Verify now:\n")
	fmt.Fprintf(out, "  - no `botevent: seed event id counter` errors in logs (a failed seed refuses\n")
	fmt.Fprintf(out, "    the enqueue rather than issuing an unsafe id)\n")
	fmt.Fprintf(out, "  - rerun preflight: the duplicate count must stop growing\n")
	fmt.Fprintf(out, "  - bot events still flowing (POST /v1/bot/events returning results)\n")
	fmt.Fprintf(out, "  - dmwork_bot_event_seq_mirror_unauthorized_total stays 0 (non-zero means some\n")
	fmt.Fprintf(out, "    replica saw a mode mirror the authority did not confirm)\n")
	fmt.Fprintf(out, "  - then roll out OCTO_BOTEVENT_EXPECTED_MODE=incr so a lost mirror AND a lost\n")
	fmt.Fprintf(out, "    authority row fail closed instead of degrading\n\n")
	fmt.Fprintf(out, "There is no online deactivate. Going back means every counter-issued id is above\n")
	fmt.Fprintf(out, "what GenSeq would hand out next, so legacy ids would land below consumer cursors —\n")
	fmt.Fprintf(out, "the same loss, in reverse. Roll forward.\n")
	return nil
}

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
// command carries the whole activation procedure and its standalone predecessor had no
// tests at all, which is how the bug below shipped (review round 7 P2-1, found
// independently by two reviewers).
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

// refuseUnauthorizedBoteventMirror stops an activation while a mirror claiming activation
// sits against an authority that says otherwise.
//
// That state means this Redis is not what the operator believes: someone wrote the key, it is
// shared with another environment, or a snapshot from an activated era was restored. Any of
// those wants a human look before an irreversible flip — and it is also the precondition for
// the one residual window in the allocator's denial cooldown (see pkg/botevent/mode.go), so
// checking it here is where that window gets closed in practice.
//
// Runs after the floor checks so the operator sees every other problem in one pass.
func refuseUnauthorizedBoteventMirror(ctx *config.Context, client *rd.Client) error {
	v, err := client.Get(botevent.ModeKey).Result()
	absent := err == rd.Nil
	if err != nil && !absent {
		return fmt.Errorf("cannot read the mode mirror (%w); refusing to activate without knowing whether a "+
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
		return fmt.Errorf("cannot read the allocator state (%w); refusing to judge the mirror without it — "+
			"that is the mistake this check was added to fix", sErr)
	}

	verdict, msg := judgeMirror(activated, v, absent)
	switch verdict {
	case mirrorRefuse:
		return errors.New(msg)
	case mirrorNote:
		fmt.Fprint(os.Stderr, msg)
	}
	return nil
}

// writeMirror publishes the generation-stamped mirror value.
//
// It must be the exact spelling the allocator validates (botevent.MirrorValue): a bare
// `incr` is only a claim. Failure is returned because the MySQL authority is already
// activated at this point while replicas may retain a negative belief for a bounded
// interval; callers must exit non-zero and keep bot-event writes paused.
func writeMirror(client *rd.Client, epoch uint64) error {
	want := botevent.MirrorValue(epoch)
	if err := client.Set(botevent.ModeKey, want, 0).Err(); err != nil {
		return fmt.Errorf("authority is already activated at epoch %d, but writing Redis mirror "+
			"%s=%q failed: %w; replicas may retain a legacy belief for up to %s, so keep "+
			"bot-event writes paused until %s exists with expected value %q",
			epoch, botevent.ModeKey, want, err, botevent.NegativeBeliefTTL(), botevent.ModeKey, want)
	}
	return nil
}

func readBoteventMirror(ctx *config.Context) (string, error) {
	v, err := ctx.GetRedisConn().GetString(botevent.ModeKey)
	if err != nil {
		return "(unreadable)", fmt.Errorf("read mirror: %w", err)
	}
	if v == "" {
		return "(absent — allocators will rebuild it from the authority)", nil
	}
	return v, nil
}
