package botevent

import (
	"errors"
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

// TestHighWaterFailuresAreBoundedAndThrottled is review P1-7.
//
// Counting a failed durable write is not enough. Leaving `lastPersisted` unset means
// the once-per-interval guard never engages, so the allocator does one un-deadlined
// INSERT per event while the DB is unhappy. And the safety argument at the top of
// seq.go — the mark trails by at most one interval, which seedSafetyMargin covers —
// only holds while the writes land, so the number of intervals allowed to pass without
// one has to be bounded rather than asserted away.
func TestHighWaterFailuresAreBoundedAndThrottled(t *testing.T) {
	robotID := "seqtest_highwaterbound_bot"
	t.Cleanup(func() {
		highWaterFailures.Delete(robotID)
		lastPersisted.Delete(robotID)
	})
	highWaterFailures.Delete(robotID)
	lastPersisted.Delete(robotID)

	cause := errors.New("simulated DB outage")
	for i := 1; i < highWaterFailureLimit; i++ {
		if err := noteHighWaterFailure(robotID, int64(i)*highWaterInterval, cause); err != nil {
			t.Fatalf("failure %d must not fail the allocation yet: %v", i, err)
		}
		// Re-arming the interval guard is what stops the retry storm: the next write is
		// one interval away, not one id away.
		if _, armed := lastPersisted.Load(robotID); !armed {
			t.Fatalf("failure %d left the interval guard unarmed, so the next allocation would "+
				"attempt another INSERT immediately", i)
		}
	}
	if err := noteHighWaterFailure(robotID, highWaterFailureLimit*highWaterInterval, cause); err == nil {
		t.Fatalf("after %d consecutive failures the allocation must fail; issuing ids whose "+
			"recovery floor is frozen spends the RDB safety margin silently",
			highWaterFailureLimit)
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
