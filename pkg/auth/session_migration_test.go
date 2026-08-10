package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

const rolloutObservationGapForTest = time.Hour

func TestLegacyMigrationDryRunIsZeroWriteAndClassifiesRecords(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(30 * time.Minute)

	keys := map[string]string{
		"persistent-v1": `{"uid":"legacy"}`,
		"long-v2":       "v2:long",
		"short-v2":      "v2:short",
		"persistent-v3": "v3:invalid-but-must-not-be-repaired",
	}
	require.NoError(t, client.Set(store.tokenKey("persistent-v1"), keys["persistent-v1"], 0).Err())
	require.NoError(t, client.Set(store.tokenKey("long-v2"), keys["long-v2"], 2*time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("short-v2"), keys["short-v2"], 5*time.Minute).Err())
	require.NoError(t, client.Set(store.tokenKey("persistent-v3"), keys["persistent-v3"], 0).Err())

	before := legacyMigrationSnapshot(t, client, store, keys)
	result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
		CampaignID:   "dry-run",
		CutoffAt:     cutoff,
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        false,
	})
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, int64(4), result.Scanned)
	require.Equal(t, int64(1), result.V1)
	require.Equal(t, int64(2), result.V2)
	require.Equal(t, int64(1), result.V3)
	require.Equal(t, int64(2), result.Shortened)
	require.Equal(t, int64(1), result.Unchanged)
	require.Equal(t, int64(1), result.Invalid)
	require.Equal(t, int64(1), result.V3NonFinite)
	after := legacyMigrationSnapshot(t, client, store, keys)
	for token, expected := range before {
		require.Equal(t, expected.Value, after[token].Value)
		if expected.TTL == -time.Millisecond {
			require.Equal(t, expected.TTL, after[token].TTL)
			continue
		}
		require.Positive(t, after[token].TTL)
		require.LessOrEqual(t, after[token].TTL, expected.TTL)
	}
	require.Equal(t, int64(0), mustExists(t, client, store.migrationCampaignKey("dry-run"), store.migrationCheckpointKey("dry-run"), store.migrationLockKey()))
}

func TestLegacyMigrationRequiresExplicitFinitePolicy(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)

	_, err := store.MigrateLegacySessions(context.Background(), LegacyMigrationOptions{
		CampaignID: "missing-policy",
		CutoffAt:   time.Now().UTC().Add(time.Hour),
		BatchSize:  100,
	})
	require.ErrorContains(t, err, "finite policy")
}

func TestLegacyMigrationFinitePolicyControlsOnlyFiniteLegacyDeadlines(t *testing.T) {
	tests := []struct {
		name             string
		policy           LegacyFinitePolicy
		wantFiniteCapped bool
	}{
		{name: "natural keeps an already bounded finite deadline", policy: LegacyFinitePolicyNatural},
		{name: "cap compresses a finite deadline to the cutoff", policy: LegacyFinitePolicyCap, wantFiniteCapped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
			ctx := context.Background()
			require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
			require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
			cutoff := time.Now().UTC().Add(15 * time.Minute)
			require.NoError(t, client.Set(store.tokenKey("finite"), "v2:finite", 30*time.Minute).Err())
			require.NoError(t, client.Set(store.tokenKey("over-max"), "v2:over-max", 2*time.Hour).Err())
			require.NoError(t, client.Set(store.tokenKey("persistent"), "v2:persistent", 0).Err())
			finiteBefore := mustPTTL(t, client, store.tokenKey("finite"))

			result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
				CampaignID:   "policy-" + string(tt.policy),
				CutoffAt:     cutoff,
				FinitePolicy: tt.policy,
				BatchSize:    100,
				Apply:        true,
				Lease:        5 * time.Second,
			})
			require.NoError(t, err)
			require.True(t, result.Complete)

			persistentAfter := mustPTTL(t, client, store.tokenKey("persistent"))
			require.Positive(t, persistentAfter)
			require.LessOrEqual(t, persistentAfter, time.Until(cutoff)+time.Second)

			finiteAfter := mustPTTL(t, client, store.tokenKey("finite"))
			overMaxAfter := mustPTTL(t, client, store.tokenKey("over-max"))
			require.Positive(t, finiteAfter)
			require.Positive(t, overMaxAfter)
			require.LessOrEqual(t, overMaxAfter, store.maxTTL+time.Second)
			if tt.wantFiniteCapped {
				require.LessOrEqual(t, finiteAfter, time.Until(cutoff)+time.Second)
				require.LessOrEqual(t, overMaxAfter, time.Until(cutoff)+time.Second)
				return
			}
			require.Greater(t, finiteAfter, 25*time.Minute)
			require.LessOrEqual(t, finiteAfter, finiteBefore)
			require.Greater(t, overMaxAfter, 50*time.Minute)
		})
	}
}

func TestLegacyMigrationApplyRequiresFloorAndOnlyShortensLegacy(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(30 * time.Minute)
	options := LegacyMigrationOptions{
		CampaignID:   "apply",
		CutoffAt:     cutoff,
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        5 * time.Second,
	}
	require.NoError(t, client.Set(store.tokenKey("persistent-v1"), `{"uid":"legacy"}`, 0).Err())

	_, err := store.MigrateLegacySessions(ctx, options)
	require.ErrorContains(t, err, "persisted revoke rollout floor")
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.tokenKey("persistent-v1")))
	require.Equal(t, int64(0), mustExists(t, client, store.migrationCampaignKey("apply"), store.migrationCheckpointKey("apply"), store.migrationLockKey()))

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
	require.NoError(t, client.Set(store.tokenKey("long-v2"), "v2:long", 2*time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("short-v2"), "v2:short", 5*time.Minute).Err())
	require.NoError(t, client.Set(store.tokenKey("persistent-v3"), "v3:invalid", 0).Err())
	shortBefore := mustPTTL(t, client, store.tokenKey("short-v2"))

	result, err := store.MigrateLegacySessions(ctx, options)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, int64(1), result.V1)
	require.Equal(t, int64(2), result.V2)
	require.Equal(t, int64(1), result.V3)
	require.Equal(t, int64(2), result.Shortened)
	require.Equal(t, int64(1), result.V3NonFinite)
	for _, token := range []string{"persistent-v1", "long-v2"} {
		ttl := mustPTTL(t, client, store.tokenKey(token))
		require.Positive(t, ttl)
		require.LessOrEqual(t, ttl, time.Until(cutoff)+time.Second)
	}
	shortAfter := mustPTTL(t, client, store.tokenKey("short-v2"))
	require.Positive(t, shortAfter)
	require.LessOrEqual(t, shortAfter, shortBefore)
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.tokenKey("persistent-v3")))
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.migrationCampaignKey("apply")))
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.migrationCheckpointKey("apply")))

	beforeRepeat := mustPTTL(t, client, store.tokenKey("long-v2"))
	_, err = store.MigrateLegacySessions(ctx, options)
	require.NoError(t, err)
	require.LessOrEqual(t, mustPTTL(t, client, store.tokenKey("long-v2")), beforeRepeat)

	drifted := options
	drifted.CutoffAt = cutoff.Add(time.Minute)
	_, err = store.MigrateLegacySessions(ctx, drifted)
	require.ErrorContains(t, err, "parameters do not match")
}

func TestLegacyMigrationApplyIsSingleOwnerAndDetectsLockLossWithinBatch(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
	for i := 0; i < 8; i++ {
		require.NoError(t, client.Set(store.tokenKey(fmt.Sprintf("legacy-%02d", i)), "v2:legacy", 0).Err())
	}
	options := LegacyMigrationOptions{
		CampaignID:   "lock-loss",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    1000,
		Interval:     500 * time.Millisecond,
		Apply:        true,
		Lease:        5 * time.Second,
	}

	type outcome struct {
		result LegacyMigrationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := store.MigrateLegacySessions(ctx, options)
		done <- outcome{result: result, err: err}
	}()

	require.Eventually(t, func() bool {
		return mustExists(t, client, store.migrationLockKey()) == 1
	}, 2*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		for i := 0; i < 8; i++ {
			if mustPTTL(t, client, store.tokenKey(fmt.Sprintf("legacy-%02d", i))) > 0 {
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond)
	require.NoError(t, client.Del(store.migrationLockKey()).Err())

	got := <-done
	require.ErrorIs(t, got.err, ErrMigrationLockLost)
	require.False(t, got.result.Complete)
	require.True(t, got.result.LockLost)

	require.NoError(t, client.Set(store.migrationLockKey(), "another-owner", 10*time.Second).Err())
	_, err := store.MigrateLegacySessions(ctx, options)
	require.ErrorIs(t, err, ErrMigrationLockHeld)
}

func TestLegacyMigrationCancellationReportsIncompleteAndCanResume(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
	for i := 0; i < 40; i++ {
		require.NoError(t, client.Set(store.tokenKey(fmt.Sprintf("resume-%02d", i)), "v2:legacy", 0).Err())
	}
	options := LegacyMigrationOptions{
		CampaignID:   "resume",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    1,
		Interval:     20 * time.Millisecond,
		Apply:        true,
		Lease:        5 * time.Second,
	}
	cancelled, cancel := context.WithCancel(ctx)
	done := make(chan struct {
		result LegacyMigrationResult
		err    error
	}, 1)
	go func() {
		result, err := store.MigrateLegacySessions(cancelled, options)
		done <- struct {
			result LegacyMigrationResult
			err    error
		}{result: result, err: err}
	}()
	require.Eventually(t, func() bool {
		raw, err := client.Get(store.migrationCheckpointKey("resume")).Result()
		return err == nil && raw != ""
	}, 2*time.Second, 20*time.Millisecond)
	cancel()
	first := <-done
	require.ErrorIs(t, first.err, context.Canceled)
	require.False(t, first.result.Complete)

	result, err := store.MigrateLegacySessions(ctx, options)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Zero(t, result.LastCursor)
	for i := 0; i < 40; i++ {
		require.Positive(t, mustPTTL(t, client, store.tokenKey(fmt.Sprintf("resume-%02d", i))))
	}
}

func TestLegacyMigrationCampaignCanRetuneRuntimeRateAndStartAnotherCampaign(t *testing.T) {
	t.Run("resume with retuned batch and interval", func(t *testing.T) {
		store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
		ctx := context.Background()
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
		options := LegacyMigrationOptions{
			CampaignID:   "retunable",
			CutoffAt:     time.Now().UTC().Add(time.Hour),
			FinitePolicy: LegacyFinitePolicyCap,
			BatchSize:    100,
			Apply:        true,
			Lease:        5 * time.Second,
		}
		result, err := store.MigrateLegacySessions(ctx, options)
		require.NoError(t, err)
		require.True(t, result.Complete)

		options.BatchSize = 1
		options.Interval = time.Millisecond
		result, err = store.MigrateLegacySessions(ctx, options)
		require.NoError(t, err, "batch size and QPS are resumable runtime controls, not campaign safety identity")
		require.True(t, result.Complete)
	})

	t.Run("a completed campaign does not permanently block the next campaign", func(t *testing.T) {
		store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
		ctx := context.Background()
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
		first := LegacyMigrationOptions{
			CampaignID:   "first",
			CutoffAt:     time.Now().UTC().Add(time.Hour),
			FinitePolicy: LegacyFinitePolicyCap,
			BatchSize:    100,
			Apply:        true,
			Lease:        5 * time.Second,
		}
		result, err := store.MigrateLegacySessions(ctx, first)
		require.NoError(t, err)
		require.True(t, result.Complete)

		second := first
		second.CampaignID = "second"
		second.CutoffAt = first.CutoffAt.Add(time.Hour)
		result, err = store.MigrateLegacySessions(ctx, second)
		require.NoError(t, err, "campaign/checkpoint records must be isolated by campaign ID")
		require.True(t, result.Complete)
	})
}

func TestLegacyMigrationElapsedCutoffClassifiesDeletionWithoutExtendingDeadline(t *testing.T) {
	tests := []struct {
		name            string
		policy          LegacyFinitePolicy
		apply           bool
		confirmElapsed  bool
		wantErr         string
		wantDeleted     int64
		wantWouldDelete int64
		wantExists      int64
		wantUnchanged   int64
	}{
		{name: "cap dry-run reports both records", policy: LegacyFinitePolicyCap, wantWouldDelete: 2, wantExists: 2},
		{name: "cap apply requires explicit confirmation", policy: LegacyFinitePolicyCap, apply: true, wantErr: "confirm elapsed cutoff", wantExists: 2},
		{name: "cap confirmed apply deletes both records", policy: LegacyFinitePolicyCap, apply: true, confirmElapsed: true, wantDeleted: 2},
		{name: "natural dry-run reports only persistent record", policy: LegacyFinitePolicyNatural, wantWouldDelete: 1, wantExists: 2, wantUnchanged: 1},
		{name: "natural confirmed apply deletes only persistent record", policy: LegacyFinitePolicyNatural, apply: true, confirmElapsed: true, wantDeleted: 1, wantExists: 1, wantUnchanged: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now().UTC()
			store, client := newLegacyMigrationTestStore(t, SessionModeRevoke, WithSessionClock(func() time.Time { return now }))
			ctx := context.Background()
			require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
			require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
			options := LegacyMigrationOptions{
				CampaignID:           "elapsed-cutoff-" + string(tt.policy),
				CutoffAt:             now.Add(-time.Hour),
				FinitePolicy:         tt.policy,
				BatchSize:            100,
				Apply:                tt.apply,
				ConfirmElapsedCutoff: tt.confirmElapsed,
				Lease:                5 * time.Second,
			}
			if tt.apply {
				persisted := legacyMigrationCampaign{
					CampaignID:   options.CampaignID,
					CutoffAtMS:   options.CutoffAt.UnixMilli(),
					MaxTTLMS:     store.maxTTL.Milliseconds(),
					FinitePolicy: string(tt.policy),
				}
				raw, err := json.Marshal(persisted)
				require.NoError(t, err)
				require.NoError(t, client.Set(store.migrationCampaignKey(options.CampaignID), string(raw), 0).Err(), "simulate a campaign created before its cutoff elapsed")
			}
			require.NoError(t, client.Set(store.tokenKey("finite"), "v2:finite", 30*time.Minute).Err())
			require.NoError(t, client.Set(store.tokenKey("persistent"), "v2:persistent", 0).Err())

			result, err := store.MigrateLegacySessions(ctx, options)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Equal(t, tt.wantExists, mustExists(t, client, store.tokenKey("finite"), store.tokenKey("persistent")))
				return
			}
			require.NoError(t, err, "an interrupted campaign must remain resumable after its immutable cutoff passes")
			require.True(t, result.Complete)
			require.Equal(t, int64(2), result.Scanned)
			require.Equal(t, tt.wantDeleted, result.Deleted)
			require.Equal(t, tt.wantWouldDelete, result.WouldDelete)
			require.Zero(t, result.Shortened, "elapsed cutoffs must not report immediate deletion as TTL shortening")
			require.Equal(t, tt.wantUnchanged, result.Unchanged)
			require.Equal(t, tt.wantExists, mustExists(t, client, store.tokenKey("finite"), store.tokenKey("persistent")))
		})
	}
}

func TestLegacyMigrationStopsBeforeUnconfirmedCutoffDeletionDuringApply(t *testing.T) {
	base := time.Now().UTC()
	cutoff := base.Add(time.Minute)
	var clockCalls atomic.Int64
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke, WithSessionClock(func() time.Time {
		// validateMigrationOptions, campaign creation, and the first record run
		// before cutoff. The second record observes the same run after cutoff.
		if clockCalls.Add(1) <= 3 {
			return base
		}
		return cutoff.Add(time.Minute)
	}))
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))
	keys := []string{store.tokenKey("first"), store.tokenKey("second")}
	for _, key := range keys {
		require.NoError(t, client.Set(key, "v2:persistent", 0).Err())
	}
	options := LegacyMigrationOptions{
		CampaignID:   "cutoff-elapses-during-apply",
		CutoffAt:     cutoff,
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        5 * time.Second,
	}

	result, err := store.MigrateLegacySessions(ctx, options)
	require.ErrorIs(t, err, ErrMigrationElapsedCutoffConfirmationRequired)
	require.False(t, result.Complete)
	require.Zero(t, result.Deleted)
	require.Equal(t, int64(2), mustExists(t, client, keys...))
	require.Equal(t, int64(0), mustExists(t, client, store.latestMigrationCompletionKey()))

	// SCAN may return the two records in one page or separate pages. When the
	// first page completes, its checkpoint records the key already shortened to
	// the campaign cutoff; a confirmed resume correctly starts at the saved
	// cursor instead of rescanning that key. Make the shortened key reach its
	// Redis deadline so both pagination shapes converge on the production state:
	// one naturally expired key and one remaining key that requires confirmed
	// deletion.
	var shortenedKey string
	for _, key := range keys {
		if ttl := mustPTTL(t, client, key); ttl > 0 {
			shortenedKey = key
		}
	}
	require.NotEmpty(t, shortenedKey)
	require.NoError(t, client.PExpire(shortenedKey, time.Millisecond).Err())
	require.Eventually(t, func() bool { return mustExists(t, client, shortenedKey) == 0 }, time.Second, 10*time.Millisecond)

	options.ConfirmElapsedCutoff = true
	result, err = store.MigrateLegacySessions(ctx, options)
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, int64(1), result.Deleted)
	require.Equal(t, int64(0), mustExists(t, client, keys...))
}

func TestAdvanceBoundedFloorRequiresMigrationAndObservationEvidence(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))

	err := store.AdvanceRolloutControl(ctx, SessionModeBounded, rolloutObservationGapForTest)
	require.ErrorContains(t, err, "evidence")
}

func TestRolloutFloorAdvancesOnlyWithRecordedMigrationAndSpacedObservationEvidence(t *testing.T) {
	now := time.Now().UTC()
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke, WithSessionClock(func() time.Time { return now }))
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	increasedGap := 2 * rolloutObservationGapForTest
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, increasedGap))

	result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
		CampaignID:   "floor-evidence",
		CutoffAt:     now.Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        5 * time.Second,
	})
	require.NoError(t, err)
	require.True(t, result.Complete)

	boundedObservation := SessionObservation{Complete: true, Finite: 2, V1: 1, V2: 1}
	require.NoError(t, store.RecordRolloutObservation(ctx, boundedObservation))
	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeBounded, increasedGap), "two complete")
	now = now.Add(increasedGap - time.Millisecond)
	require.NoError(t, store.RecordRolloutObservation(ctx, boundedObservation))
	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeBounded, increasedGap), "minimum gap")
	now = now.Add(increasedGap)
	require.NoError(t, store.RecordRolloutObservation(ctx, boundedObservation))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeBounded, increasedGap))

	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeEnforce, increasedGap), "two complete")
	enforceObservation := SessionObservation{Complete: true, Finite: 2, V3: 2}
	now = now.Add(time.Millisecond)
	require.NoError(t, store.RecordRolloutObservation(ctx, enforceObservation))
	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeEnforce, increasedGap), "two complete")
	now = now.Add(increasedGap - time.Millisecond)
	require.NoError(t, store.RecordRolloutObservation(ctx, enforceObservation))
	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeEnforce, increasedGap), "minimum gap")
	now = now.Add(increasedGap)
	require.NoError(t, store.RecordRolloutObservation(ctx, enforceObservation))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeEnforce, increasedGap))
}

func TestRecordRolloutObservationRejectsIncompleteOrAmbiguousEvidence(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, rolloutObservationGapForTest))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, rolloutObservationGapForTest))

	for _, observation := range []SessionObservation{
		{},
		{Complete: true, ReadErrors: 1},
		{Complete: true, InvalidTTL: 1},
		{Complete: true, DecodeInvalid: 1},
	} {
		require.Error(t, store.RecordRolloutObservation(ctx, observation))
	}
}

type legacyTokenSnapshot struct {
	Value string
	TTL   time.Duration
}

func newLegacyMigrationTestStore(t *testing.T, mode SessionMode, extraOptions ...SessionStoreOption) (*RedisSessionStore, *rd.Client) {
	t.Helper()
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-migration:" + util.GenerUUID() + ":"
	uidPrefix := "session-migration-uid:" + util.GenerUUID() + ":"
	options := []SessionStoreOption{WithSessionMode(mode)}
	if mode.writesV3() {
		options = append(options, WithSessionMaxPerUID(10))
	}
	options = append(options, extraOptions...)
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, options...)
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		metadata, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})
	return store, client
}

func legacyMigrationSnapshot(t *testing.T, client *rd.Client, store *RedisSessionStore, tokens map[string]string) map[string]legacyTokenSnapshot {
	t.Helper()
	result := make(map[string]legacyTokenSnapshot, len(tokens))
	for token := range tokens {
		key := store.tokenKey(token)
		value, err := client.Get(key).Result()
		require.NoError(t, err)
		result[token] = legacyTokenSnapshot{Value: value, TTL: mustPTTL(t, client, key)}
	}
	return result
}

func mustPTTL(t *testing.T, client *rd.Client, key string) time.Duration {
	t.Helper()
	ttl, err := client.PTTL(key).Result()
	require.NoError(t, err)
	return ttl
}

func mustExists(t *testing.T, client *rd.Client, keys ...string) int64 {
	t.Helper()
	value, err := client.Exists(keys...).Result()
	require.NoError(t, err)
	return value
}
