package auth

// Characterization of the rollout control-plane rules that MUST SURVIVE the
// token-session-rollout-simplify redesign (.octospec/tasks/…/brief.md).
//
// Everything here must stay green before and after the redesign. If a refactor
// makes one of these fail, the refactor is wrong, not the test. The behaviour
// the redesign REMOVES is asserted in its inverted form by
// session_rollout_boot_test.go and session_rollout_wiring_test.go.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The floor is monotonic and advances exactly one phase at a time. Both a
// downgrade and a phase skip must be refused, so no operator or reconciler can
// jump a stage or walk one back.
func TestRolloutFloorRejectsDowngradeAndPhaseSkip(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutMaxPerUIDForTest))

	require.Error(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest),
		"floor must not walk backwards")
	require.Error(t, store.AdvanceRolloutControl(ctx, SessionModeEnforce, rolloutMaxPerUIDForTest),
		"floor must not skip bounded")

	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, control.ModeFloor, "refused advances must not mutate the record")
}

// expand is not a floor, and the ladder must start at v3-write. A first advance
// straight to a later phase would skip the writer-convergence stage entirely.
func TestRolloutFloorFirstAdvanceMustBeV3Write(t *testing.T) {
	ctx := context.Background()
	for _, target := range []SessionMode{SessionModeExpand, SessionModeRevoke, SessionModeBounded, SessionModeEnforce} {
		store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
		require.Error(t, store.AdvanceRolloutControl(ctx, target, rolloutMaxPerUIDForTest),
			"first floor must be v3-write, not %s", target)
	}
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest))
}

// Two operators (or two reconciler replicas) racing the same transition must
// produce exactly one advance, and every loser must fail closed.
//
// Losers surface in one of two shapes depending on when they read, and both are
// correct: AdvanceRolloutControl is an optimistic pre-check in front of a Lua
// CAS. A racer that read before the winner's write reaches the CAS and gets
// ErrRolloutControlChanged; one that read after is rejected by the pre-check
// with "must advance exactly one phase". Anything that races the reconciler
// must treat BOTH as a benign no-op, not as an error worth alerting on.
func TestRolloutControlAdvanceIsSingleWinnerUnderConcurrency(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	ctx := context.Background()

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest)
		}(i)
	}
	close(start)
	wg.Wait()

	winners, casConflicts, precheckRejects := 0, 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrRolloutControlChanged):
			casConflicts++
		default:
			// Sentinels, not message substrings: renaming an error must not turn
			// a benign race into an alert or mask a real one-phase violation.
			require.ErrorIs(t, err, ErrRolloutFloorNotNext,
				"a loser must fail closed, with either the CAS conflict or the pre-check")
			precheckRejects++
		}
	}
	require.Equal(t, 1, winners, "exactly one racer may advance the floor")
	require.Equal(t, racers-1, casConflicts+precheckRejects, "every other racer must fail")
	t.Logf("losers: %d CAS conflicts, %d pre-check rejects (both benign)", casConflicts, precheckRejects)

	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeV3Write, control.ModeFloor)
	require.Equal(t, 3, control.WriterVersion)
}

// The floor is safety state with no TTL. A record that can expire must be
// refused outright rather than read and trusted, because an expiring floor
// silently becomes a missing floor.
func TestRolloutControlMustNotExpire(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest))

	raw, err := client.Get(store.rolloutControlKey()).Result()
	require.NoError(t, err)
	require.NoError(t, client.Set(store.rolloutControlKey(), raw, time.Hour).Err())

	_, err = store.RolloutControl(ctx)
	require.ErrorContains(t, err, "must not expire")
}

// A corrupt or semantically invalid floor record must fail closed. It must
// never be read as "no floor yet", which would re-enable a v2 writer.
func TestRolloutControlCorruptRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, payload := range map[string]string{
		"not-json":       "{not json",
		"unknown-floor":  `{"mode_floor":"banana","writer_version":3,"observation_min_gap_ms":3600000}`,
		"expand-floor":   `{"mode_floor":"expand","writer_version":3,"observation_min_gap_ms":3600000}`,
		"writer-version": `{"mode_floor":"revoke","writer_version":2,"observation_min_gap_ms":3600000}`,
		"empty-floor":    `{"mode_floor":"","writer_version":3,"observation_min_gap_ms":3600000}`,
	} {
		t.Run(name, func(t *testing.T) {
			store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
			require.NoError(t, client.Set(store.rolloutControlKey(), payload, 0).Err())

			_, err := store.RolloutControl(ctx)
			require.Error(t, err, "corrupt floor must not be readable")

			// A corrupt record must never be read as "the floor said expand".
			// Boot resolves it via the marker exactly like a missing record; the
			// dedicated coverage lives in the boot tests. Here we only pin that
			// the record itself stays unreadable rather than decoding to a
			// permissive value.
			_, _, loadErr := store.loadRolloutControl(ctx)
			require.Error(t, loadErr, "corrupt floor must not decode")
			require.Error(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest),
				"corrupt floor must not be advanced over")
		})
	}
}

// Migration apply is gated on a persisted revoke floor. This is the barrier
// that guarantees no legacy writer can add records behind a running scan, and
// it is what makes a completed full SCAN cycle sufficient.
func TestMigrationApplyStaysGatedOnRevokeFloor(t *testing.T) {
	ctx := context.Background()
	options := func() LegacyMigrationOptions {
		return LegacyMigrationOptions{
			CampaignID:   "invariant-gate",
			CutoffAt:     time.Now().UTC().Add(time.Hour),
			FinitePolicy: LegacyFinitePolicyNatural,
			BatchSize:    100,
			Apply:        true,
			Lease:        30 * time.Second,
		}
	}

	noFloor, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	_, err := noFloor.MigrateLegacySessions(ctx, options())
	require.ErrorContains(t, err, "revoke rollout floor")

	tooLow, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	require.NoError(t, tooLow.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest))
	_, err = tooLow.MigrateLegacySessions(ctx, options())
	require.ErrorContains(t, err, "revoke rollout floor")

	ok, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	require.NoError(t, ok.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutMaxPerUIDForTest))
	require.NoError(t, ok.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutMaxPerUIDForTest))
	result, err := ok.MigrateLegacySessions(ctx, options())
	require.NoError(t, err)
	require.True(t, result.Complete)
}
