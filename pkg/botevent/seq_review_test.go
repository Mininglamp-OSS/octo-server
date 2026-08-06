package botevent

import (
	"fmt"
	"testing"
	"time"

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

// TestDeniedMirrorIsNotRereadPerAllocation is review P1-C.
//
// A mirror claiming activation skips the negative TTL by design — that is what makes a
// flip propagate without waiting. But nothing rewrites or clears a *forged* mirror, so
// before this fix every allocation performed its own authority read, serialized behind
// beliefMu, inside a held msgSem slot, with no exit until a human deleted the key. That is
// the same hazard class as the per-allocation read review P1-1 removed.
func TestDeniedMirrorIsNotRereadPerAllocation(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_deniedmirror_bot"
	fixture(t, ctx, client, robotID)

	// Authority says legacy; somebody planted a mirror claiming otherwise.
	setStateMode(t, ctx, StateModeLegacy, 0)
	ResetSeededForTest()
	client.Del(SeqKey(robotID))
	if err := client.Set(ModeKey, MirrorValue(7), 0).Err(); err != nil {
		t.Fatalf("plant mirror: %v", err)
	}

	before := AuthorityReads()
	const allocations = 20
	for i := 0; i < allocations; i++ {
		if _, err := nextEventID(ctx, client, robotID); err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
	}
	reads := AuthorityReads() - before
	if reads > 1 {
		t.Errorf("%d allocations against a denied mirror issued %d authority reads; a forged key "+
			"nobody clears must not cost a serialized DB read per allocation on the fan-out path",
			allocations, reads)
	}

	// A *different* mirror value is still a conflict and must still be read — otherwise a
	// genuine activation would be swallowed by the cached denial.
	if err := client.Set(ModeKey, MirrorValue(8), 0).Err(); err != nil {
		t.Fatalf("change mirror: %v", err)
	}
	if _, err := nextEventID(ctx, client, robotID); err != nil {
		t.Fatalf("allocate after the mirror changed: %v", err)
	}
	if AuthorityReads()-before <= reads {
		t.Error("a mirror value that has not been denied yet must force an authority read; " +
			"caching the denial by value is what keeps a real flip from being swallowed")
	}
}

// round's P1-7 "fix", which both reviewers found did not fail closed.
//
// The bound was a count of failed intervals, and `persistHighWater` short-circuited on the
// throttle marker *before* consulting it — so once the bound was reached only 1 allocation
// in every `highWaterInterval` even reached the check, and the other 999 succeeded.
// Issuance continued indefinitely against a frozen mark at a 0.1% failure rate, which is
// the unbounded exposure the mechanism claimed to prevent. Worse, with a limit of 3 the ids
// ran to `M+2999` while `seedSafetyMargin` is 2000, so a *restarted* process (empty
// lastIssued, no in-process refusal) would resume at `M+2001` beneath cursors that had
// already reached `M+2999`.
//
// The previous test could not see any of it: it called `noteHighWaterFailure` directly with
// values spaced exactly one interval apart, so it never went through the short-circuit that
// swallowed the intervening 999, and it asserted a helper's return value rather than the
// allocator's behaviour. This one drives real allocations with the durable write broken,
// which is the only shape that could have caught it.
func TestHighWaterFailuresStopIssuanceThroughTheRealPath(t *testing.T) {
	ctx, client := seqTestCtx(t)
	robotID := "seqtest_highwaterbound_bot"
	fixture(t, ctx, client, robotID)

	// One healthy allocation establishes the durable base, as the first allocation for a
	// bot always does (it is never throttled).
	first, err := nextEventID(ctx, client, robotID)
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	base, err := highWaterCeiling(ctx, robotID)
	if err != nil || base == 0 {
		t.Fatalf("the first allocation must land the durable mark (base=%d err=%v)", base, err)
	}

	// Now the durable write breaks. Renaming the row's table out from under the allocator
	// is the closest thing to a DB outage that leaves Redis usable, which is the state the
	// bound exists for.
	if _, err := ctx.DB().Exec("RENAME TABLE `seq` TO `seq_hidden_for_test`"); err != nil {
		t.Skipf("cannot rename seq table to simulate a durable-write outage: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ctx.DB().Exec("RENAME TABLE `seq_hidden_for_test` TO `seq`")
	})

	// Walk the counter forward past the bound. Every allocation must either succeed with an
	// id inside the recoverable window, or fail — never succeed outside it.
	var lastOK int64 = first
	refusedAt := int64(0)
	for i := 0; i < 3*seedSafetyMargin; i++ {
		v, err := nextEventID(ctx, client, robotID)
		if err != nil {
			refusedAt = lastOK
			break
		}
		if v-base >= seedSafetyMargin {
			t.Fatalf("allocation %d succeeded at id %d, %d past the frozen durable mark %d; a "+
				"seed recovering from that mark lands at %d, so this id is unrecoverable and "+
				"must have been refused", i, v, v-base, base, base+seedSafetyMargin)
		}
		lastOK = v
	}
	if refusedAt == 0 {
		t.Fatalf("issuance never stopped: %d allocations succeeded with the durable write "+
			"failing throughout, which is the unbounded exposure the bound exists to close",
			3*seedSafetyMargin)
	}

	// And it stays refused — the bound is not a once-per-interval probe.
	for i := 0; i < 5; i++ {
		if _, err := nextEventID(ctx, client, robotID); err == nil {
			t.Fatalf("allocation %d past the bound succeeded; the refusal must apply to every "+
				"allocation in the frozen window, not one per interval", i)
		}
	}

	// Recovery must be reachable, and must not depend on the bot's traffic: the probe
	// while past the bound is on a time budget, so one budget after the DB comes back the
	// next allocation lands the mark and succeeds.
	if _, err := ctx.DB().Exec("RENAME TABLE `seq_hidden_for_test` TO `seq`"); err != nil {
		t.Fatalf("restore seq table: %v", err)
	}
	deadline := time.Now().Add(5 * highWaterProbeEvery)
	for {
		if _, err := nextEventID(ctx, client, robotID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("allocation did not resume within %v of durable writes working again; the "+
				"refusal has to be able to end on its own, not wait for the bot's next %d ids",
				5*highWaterProbeEvery, highWaterInterval)
		}
		time.Sleep(highWaterProbeEvery / 4)
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
