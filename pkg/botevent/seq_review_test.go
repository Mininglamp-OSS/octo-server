package botevent

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

// TestNeverDowngradesToLegacyAfterIssuingCounterIDs is review P1-4.
//
// Once a process has handed out a counter id for a bot, a GenSeq id for that same bot
// is arbitrarily far below the bot's live consumer cursor, so the event is born
// invisible: #697, self-inflicted by the fix. The old code would do exactly that
// whenever the mirror went missing and the authority came back saying legacy — which
// is what a migration rollback produces, and `Down` used to drop the table
// unconditionally.
//
// Both shapes are covered: the authority answering "legacy", and the authority
// vanishing entirely.
func TestNeverDowngradesToLegacyAfterIssuingCounterIDs(t *testing.T) {
	cases := []struct {
		name           string
		breakAuthority func(t *testing.T, ctx *config.Context)
	}{
		{
			name: "authority rolled back to legacy",
			breakAuthority: func(t *testing.T, ctx *config.Context) {
				setStateMode(t, ctx, StateModeLegacy, 0)
			},
		},
		{
			name: "authority row deleted (migration rollback)",
			breakAuthority: func(t *testing.T, ctx *config.Context) {
				if _, err := ctx.DB().DeleteFrom(stateTable).
					Where("`singleton_id`=?", stateSingletonID).Exec(); err != nil {
					t.Fatalf("delete state row: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client := seqTestCtx(t)
			robotID := "seqtest_nodowngrade_bot"
			fixture(t, ctx, client, robotID)

			issued, err := nextEventID(ctx, client, robotID)
			if err != nil {
				t.Fatalf("allocate while activated: %v", err)
			}

			// Now the authority disagrees, and the mirror is gone so the fast path cannot
			// carry the allocation on its own.
			tc.breakAuthority(t, ctx)
			client.Del(ModeKey)

			got, err := nextEventID(ctx, client, robotID)
			if err == nil {
				t.Fatalf("allocated %d from the legacy allocator after already issuing counter id "+
					"%d for this bot; that id lands below the bot's consumer cursor and is never "+
					"delivered", got, issued)
			}
			// Restore the row so fixture cleanup and the next subtest start from a known
			// state (the deleted-row case removed it).
			setStateMode(t, ctx, StateModeIncr, 0)
		})
	}
}

// TestLostMirrorInvalidatesEverySeededBot is review P1-3's second half.
//
// The mirror is global; a seed is per-bot. So when the mirror has to be rebuilt — the
// mode key was lost, which is evidence Redis lost data — every counter this process
// believes it seeded is suspect, not just the one being allocated for. Re-opening the
// gate while other bots still hold a stale `seeded` marker lets their next allocation
// INCR a counter that was never re-seeded.
func TestLostMirrorInvalidatesEverySeededBot(t *testing.T) {
	ctx, client := seqTestCtx(t)
	first, second := "seqtest_mirrorwide_a", "seqtest_mirrorwide_b"
	fixture(t, ctx, client, first)
	fixture(t, ctx, client, second)

	for _, id := range []string{first, second} {
		if _, err := nextEventID(ctx, client, id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, ok := seeded.Load(id); !ok {
			t.Fatalf("%s should be marked seeded after an allocation", id)
		}
	}

	// Redis loses the mode key. The authority still says activated, so the next
	// allocation rebuilds the mirror.
	client.Del(ModeKey)
	if _, err := nextEventID(ctx, client, first); err != nil {
		t.Fatalf("allocate after mirror loss: %v", err)
	}

	if _, ok := seeded.Load(second); ok {
		t.Error("a lost mode mirror left another bot's seeded marker in place; that bot's next " +
			"allocation would INCR a counter this process never re-seeded, even though the " +
			"evidence for the rebuild was Redis losing data")
	}
	if MirrorRebuilds() == 0 {
		t.Error("rebuilding the mirror must be counted; it is a self-healing path, so nothing " +
			"else makes Redis data loss visible")
	}
}

// TestColdProcessRefusesWhenACounterOutlivesTheAuthority is review P1-A.
//
// The file header used to claim a denied mirror "does not split the fleet: every replica
// reads the same authority row and reaches the same conclusion". It does not: a replica
// with a positive belief never re-reads, and with an intact mirror the gate never closes.
// So an authority that regresses after activation leaves running replicas on the counter
// while a freshly started one reads `legacy` and would delegate to GenSeq — two live id
// sources on one queue, which is #697.
//
// The evidence a cold process has instead of memory is the counter key itself: it exists
// only because some replica allocated from it, which can only happen after the authority
// said `incr`. So `legacy` plus a live counter must refuse, not degrade.
func TestColdProcessRefusesWhenACounterOutlivesTheAuthority(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_counteroutlives_bot"
	fixture(t, ctx, client, robotID)

	issued, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate while activated: %v", err)
	}

	// The authority regresses, and this process restarts — no belief, no lastIssued, so
	// nothing in memory remembers the counter era. The mirror is gone too; only the
	// counter key survives, which is the realistic shape (Redis kept its data, MySQL was
	// restored).
	setStateMode(t, ctx, StateModeLegacy, 0)
	client.Del(ModeKey)
	ResetSeededForTest()

	before := CounterFoundWithoutAuthority()
	got, err := nextEventID(ctx, client, robotID)
	if err == nil {
		t.Fatalf("a cold process allocated %d from GenSeq while %q still had a counter that had "+
			"already issued %d; that id lands below the bot's live cursor, and the replicas still "+
			"running are meanwhile allocating from the counter — two id sources on one queue",
			got, robotID, issued)
	}
	if CounterFoundWithoutAuthority() == before {
		t.Error("the refusal must be counted; it names the cause, which a failed enqueue alone " +
			"does not")
	}

	// And the residual is the *other* shape: with the counter gone as well there is no
	// evidence at all, and legacy is what a pre-migration deploy looks like. Pinned so the
	// accepted residual is a test fact rather than a sentence in a comment.
	client.Del(SeqKey(robotID))
	ResetSeededForTest()
	if _, err := nextEventID(ctx, client, robotID); err != nil {
		t.Fatalf("with neither a mirror nor a counter, legacy is the only available answer "+
			"(OCTO_BOTEVENT_EXPECTED_MODE=incr is what closes this, by design after the flip): %v", err)
	}

	setStateMode(t, ctx, StateModeIncr, 0)
}

// TestUnauthorizedMirrorIsLeftAloneAndDoesNotSwallowActivation covers two findings that
// pull in opposite directions, which is why it asserts both halves.
//
// P1-C (round 5): a mirror claiming activation skips the negative TTL by design — that is
// what makes a flip propagate without waiting — but nothing clears a forged mirror, so every
// allocation used to perform its own authority read, serialized behind beliefMu inside a held
// msgSem slot, with no exit. So a denial has to be remembered for *something*.
//
// Jerry-Xin's 🔴 (round 6): remembering it for negativeBeliefTTL swallowed the genuine
// activation, because `Activate` bumps epoch 0 → 1 and the tool then writes exactly the
// `incr:1` that may already have been sitting there forged. The denied value and the real one
// are the same bytes, and nothing visible from Redis tells them apart.
//
// Round 6 answered that by deleting the key instead of remembering it, and round 7 found that
// deleting is a fleet-wide outage when the *authority* is the regressed party (see
// TestRegressedAuthorityDoesNotDestroyTheLegitimateMirror). So the answer is: leave the key
// alone, remember the denial only briefly, and only when there is no counter to contradict
// the authority.
func TestUnauthorizedMirrorIsLeftAloneAndDoesNotSwallowActivation(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_unauthmirror_bot"
	fixture(t, ctx, client, robotID)

	// Authority says legacy; somebody planted a mirror claiming otherwise — and planted
	// exactly the value the first real activation will produce, which is the case that broke
	// round 6's fix. No counter for this bot, so the mirror is the artifact in doubt.
	setStateMode(t, ctx, StateModeLegacy, 0)
	ResetSeededForTest()
	client.Del(SeqKey(robotID))
	if err := client.Set(ModeKey, MirrorValue(1), 0).Err(); err != nil {
		t.Fatalf("plant mirror: %v", err)
	}

	before := AuthorityReads()
	const allocations = 20
	for i := 0; i < allocations; i++ {
		if _, err := nextEventID(ctx, client, robotID); err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
	}
	if reads := AuthorityReads() - before; reads > 1 {
		t.Errorf("%d allocations against an unauthorized mirror issued %d authority reads; a key "+
			"nobody clears must not cost a serialized DB read per allocation on the fan-out path",
			allocations, reads)
	}

	// The allocator must not have touched the key. It is not the allocator's to write: only
	// the operator tool writes it, and round 7 showed what deleting it costs when the
	// authority is the party that regressed.
	if v, err := client.Get(ModeKey).Result(); err != nil || v != MirrorValue(1) {
		t.Errorf("the mirror was modified by the allocator (got %q, err=%v); it must be left "+
			"exactly as found", v, err)
	}

	// Now the operator activates for real, to the same epoch the forged key claimed, so the
	// mirror value does not change at all. Once the denial cooldown lapses the flip must take
	// effect — the cache may delay it by repairCooldown, never swallow it. Ageing the belief
	// stands in for that second passing.
	setStateMode(t, ctx, StateModeIncr, 0)
	if _, err := ctx.DB().UpdateBySql("update `"+stateTable+"` set `epoch`=1 where `singleton_id`=?",
		stateSingletonID).Exec(); err != nil {
		t.Fatalf("advance epoch: %v", err)
	}
	AgeModeBeliefForTest()

	id, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate after activation: %v", err)
	}
	if n, _ := client.Exists(SeqKey(robotID)).Result(); n == 0 {
		t.Fatalf("the allocation after activation returned %d without creating the counter, so it "+
			"came from GenSeq: the genuine activation was swallowed because its mirror value "+
			"happened to equal one that had been denied earlier", id)
	}
}

// TestFailedDurableWriteIsThrottledAndDoesNotFailTheEnqueue pins what the durable mark
// mechanism actually guarantees, after two attempts to bound it were both wrong.
//
// What is guaranteed:
//
//   - A failing durable write does not fail the enqueue. The id is already allocated and
//     unique; refusing here would trade a live event for bookkeeping.
//   - The attempt is throttled to roughly one per highWaterInterval rather than one per
//     event. That is the half of review P1-7 that stays fixed here: leaving the throttle
//     marker unset on failure put one un-deadlined INSERT on the fan-out path per event.
//
// What is NOT guaranteed, and is pinned as a test fact below rather than asserted away:
// while the writes keep failing the mark trails **without bound**. See persistHighWater for
// the arithmetic showing why bounding it means changing what the recovery floor is, and
// Mininglamp-OSS/octo-server#704, which owns that and gates activation.
func TestFailedDurableWriteIsThrottledAndDoesNotFailTheEnqueue(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_highwaterthrottle_bot"
	fixture(t, ctx, client, robotID)

	// One healthy allocation, so the mark exists and the throttle marker is armed.
	if _, err := nextEventID(ctx, client, robotID); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	mark, err := highWaterCeiling(ctx, robotID)
	if err != nil || mark == 0 {
		t.Fatalf("the first allocation must land the durable mark (mark=%d err=%v)", mark, err)
	}

	// Renaming the table out from under the allocator is the closest thing to a
	// write-side DB outage that leaves Redis perfectly usable, which is the state that
	// matters: the seed's reads already succeeded, so this is reachable without a full
	// MySQL outage (a read-only failover, disk full, lock contention, or simply the
	// 300ms deadline under load).
	if _, err := ctx.DB().Exec("RENAME TABLE `seq` TO `seq_hidden_for_test`"); err != nil {
		t.Skipf("cannot rename seq table to simulate a durable-write outage: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_, _ = ctx.DB().Exec("RENAME TABLE `seq_hidden_for_test` TO `seq`")
		}
	})

	before := HighWaterWriteFailures()
	const allocations = 3 * highWaterInterval
	for i := 0; i < allocations; i++ {
		if _, err := nextEventID(ctx, client, robotID); err != nil {
			t.Fatalf("allocation %d failed while durable writes were broken: %v — the mark is "+
				"bookkeeping, and a failed write must not cost a live event", i, err)
		}
	}
	attempts := HighWaterWriteFailures() - before
	if attempts == 0 {
		t.Fatal("no durable write was even attempted, so this test proved nothing about the throttle")
	}
	// Roughly one attempt per interval. The bound is generous — what it rules out is the
	// per-event storm, which would be ~3000 here.
	if maxAttempts := int64(allocations/highWaterInterval) + 2; attempts > maxAttempts {
		t.Errorf("%d allocations with the durable write failing produced %d attempts (max %d); "+
			"the throttle must not be re-armed only on success, or a DB outage puts one "+
			"un-deadlined INSERT on the fan-out path per event", allocations, attempts, maxAttempts)
	}

	// The known gap, asserted as still present so it becomes the regression test the day
	// #704 closes it: the mark has not moved while thousands of ids were issued above it.
	if _, err := ctx.DB().Exec("RENAME TABLE `seq_hidden_for_test` TO `seq`"); err != nil {
		t.Fatalf("restore seq table: %v", err)
	}
	restored = true
	after, err := highWaterCeiling(ctx, robotID)
	if err != nil {
		t.Fatalf("read mark: %v", err)
	}
	if after != mark {
		t.Fatalf("the durable mark moved from %d to %d while writes were failing, which this "+
			"test does not expect — if the mechanism now advances it some other way, this "+
			"assertion is the one to update", mark, after)
	}
	issued, _ := client.Get(SeqKey(robotID)).Int64()
	if issued-after < seedSafetyMargin {
		t.Fatalf("only %d ids were issued past the frozen mark, less than seedSafetyMargin (%d); "+
			"this test is supposed to demonstrate the *unbounded* trail, so it needs to run "+
			"longer to be meaningful", issued-after, int64(seedSafetyMargin))
	}
	t.Logf("known gap (#704): %d ids issued past a frozen durable mark at %d; a seed recovering "+
		"from that mark alone would land at %d, below ids already issued",
		issued-after, after, after+seedSafetyMargin)

	// And it recovers on its own once writes work again — after one more interval of ids,
	// because the throttle marker was re-armed at the last failed attempt. That latency is
	// the price of not putting an INSERT on every allocation, and it is stated here rather
	// than assumed: an earlier revision added a second, time-based probe to shorten it,
	// which is what turned the failure path into a storm inside msgSem slots.
	for i := 0; i <= highWaterInterval; i++ {
		if _, err := nextEventID(ctx, client, robotID); err != nil {
			t.Fatalf("allocate %d after restoring the table: %v", i, err)
		}
	}
	if final, err := highWaterCeiling(ctx, robotID); err != nil || final <= after {
		t.Errorf("the mark must resume advancing once durable writes work (was %d, now %d, err=%v)",
			after, final, err)
	}
}

// TestAckDeletesExactlyTheTargetMember is the acceptance item the brief asks for and
// the previous revisions did not have: the ZAdd score equals the payload `event_id`,
// and an ack addresses the one member it names.
//
// The invariant is what couples this allocator to the consumer: the cursor is a payload
// `event_id` while reads and acks are bounded by *score*
// (modules/bot_api/events.go). This is the change that makes the id source pluggable,
// so this is the round where the pin has value — and the ack shape is reproduced here
// rather than imported to keep pkg/botevent free of a dependency on modules/.
func TestAckDeletesExactlyTheTargetMember(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_ackprecision_bot"
	fixture(t, ctx, client, robotID)

	const events = 5
	ids := make([]int64, 0, events)
	for i := 0; i < events; i++ {
		id, err := nextEventID(ctx, client, robotID)
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		// Score == payload event_id. A writer that scored by, say, timestamp while
		// keeping a separate event_id would break the consumer silently.
		if err := client.ZAdd(QueueKey(robotID), rd.Z{
			Score:  float64(id),
			Member: fmt.Sprintf(`{"event_id":%d}`, id),
		}).Err(); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		ids = append(ids, id)
	}

	// Ack the middle event exactly as modules/bot_api does: by score range [id, id].
	target := ids[events/2]
	if err := client.ZRemRangeByScore(QueueKey(robotID),
		fmt.Sprintf("%d", target), fmt.Sprintf("%d", target)).Err(); err != nil {
		t.Fatalf("ack: %v", err)
	}

	remaining, err := client.ZRangeWithScores(QueueKey(robotID), 0, -1).Result()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(remaining) != events-1 {
		t.Fatalf("ack removed %d members, expected exactly 1 — with unique scores an ack must "+
			"never take a member it was not addressed to (with GenSeq's duplicate scores it did, "+
			"destroying events that had never been delivered to anybody)",
			events-len(remaining))
	}
	for _, m := range remaining {
		if int64(m.Score) == target {
			t.Fatalf("the acked event %d is still in the queue", target)
		}
		// The equality invariant, read back from the wire: score and payload agree.
		var payload struct{ EventID int64 }
		member, ok := m.Member.(string)
		if !ok {
			t.Fatalf("unexpected member type %T", m.Member)
		}
		if _, err := fmt.Sscanf(member, `{"event_id":%d}`, &payload.EventID); err != nil {
			t.Fatalf("decode member %q: %v", member, err)
		}
		if payload.EventID != int64(m.Score) {
			t.Fatalf("score %d and payload event_id %d disagree; the consumer's cursor is the "+
				"payload value while its reads and acks are bounded by score, so the two must "+
				"stay equal", int64(m.Score), payload.EventID)
		}
	}
}

// TestRegressedAuthorityDoesNotDestroyTheLegitimateMirror is review round 7's P1-1, and it
// is written before the fix so it can be seen failing.
//
// The mirror repair that replaced the denied-mirror cache deletes a mirror the authority
// contradicts. That is right when the *mirror* is the wrong artifact — a forged key against
// a genuinely pre-activation authority. It is exactly backwards when the **authority** is
// the wrong artifact, and the allocator already knows which case it is four lines earlier:
// `counterExists` means some replica allocated from this counter, which can only have
// happened after the authority said `incr`.
//
// The consequence of getting the order wrong is not a metadata loss. One freshly started
// replica — a rolling restart, an HPA scale-up, a crash-loop — deletes the mirror that every
// *healthy activated* replica's gate compares against. Their gate then returns
// gateNotActivated, they force an authority read, the authority says legacy while their
// belief is activated, and they hit the deliberate refusal. Every enqueue for every bot
// fails until the state row is restored. Before this mechanism existed the mirror stayed,
// the cold replica refused only itself, and the rest of the fleet kept allocating correctly.
//
// Same family as the P1-3 accepted in round 4: the evidence that invalidates the operation's
// premise is computed before it and consulted after it.
func TestRegressedAuthorityDoesNotDestroyTheLegitimateMirror(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_authorityregressed_bot"
	fixture(t, ctx, client, robotID)

	// An activated fleet that has issued at least one id, so the counter exists.
	issued, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate while activated: %v", err)
	}
	if n, _ := client.Exists(SeqKey(robotID)).Result(); n == 0 {
		t.Fatal("fixture did not create the counter, so this test cannot exercise the case")
	}

	// Now the authority regresses — a restore of a pre-activation dump, or a manual UPDATE.
	// Redis keeps everything: the legitimate mirror AND the counter are both intact.
	setStateMode(t, ctx, StateModeLegacy, 0)
	mirrorBefore, err := client.Get(ModeKey).Result()
	if err != nil {
		t.Fatalf("the mirror must still be present at this point: %v", err)
	}

	// One freshly started replica allocates once. Cold: no belief, no seeded marker, no
	// lastIssued — it has only what Redis and the authority tell it.
	ResetSeededForTest()

	if got, err := nextEventID(ctx, client, robotID); err == nil {
		t.Fatalf("the cold replica allocated %d instead of refusing; a counter that exists while "+
			"the authority says legacy means the authority regressed (already issued %d)",
			got, issued)
	}

	// That refusal is correct. Deleting the mirror on the way to it is not: it is the only
	// artifact the healthy activated replicas' gate matches against.
	if v, err := client.Get(ModeKey).Result(); err != nil {
		t.Fatalf("the legitimate mirror %q was destroyed (%v). One cold replica has now taken "+
			"every activated replica's gate down: their runGate compares against %q, gets "+
			"gateNotActivated, forces an authority read, finds legacy against an activated "+
			"belief, and refuses every enqueue for every bot until the state row is restored. "+
			"The mirror must survive when counterExists says the authority is the regressed party",
			mirrorBefore, err, mirrorBefore)
	} else if v != mirrorBefore {
		t.Fatalf("the mirror changed from %q to %q; it must be left exactly as it was", mirrorBefore, v)
	}

	// And the fleet is still able to allocate once the authority is put back, without any
	// replica having had to rebuild the mirror.
	setStateMode(t, ctx, StateModeIncr, 0)
	ResetSeededForTest()
	next, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("allocate after restoring the authority: %v", err)
	}
	if next <= issued {
		t.Fatalf("id %d after recovery is not above the %d already issued", next, issued)
	}
}
