package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

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
		CampaignID: "dry-run",
		CutoffAt:   cutoff,
		BatchSize:  100,
		Apply:      false,
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
	require.Equal(t, int64(0), mustExists(t, client, store.migrationCampaignKey(), store.migrationCheckpointKey(), store.migrationLockKey()))
}

func TestLegacyMigrationApplyRequiresFloorAndOnlyShortensLegacy(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(30 * time.Minute)
	options := LegacyMigrationOptions{
		CampaignID: "apply",
		CutoffAt:   cutoff,
		BatchSize:  100,
		Apply:      true,
		Lease:      5 * time.Second,
	}
	require.NoError(t, client.Set(store.tokenKey("persistent-v1"), `{"uid":"legacy"}`, 0).Err())

	_, err := store.MigrateLegacySessions(ctx, options)
	require.ErrorContains(t, err, "persisted revoke rollout floor")
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.tokenKey("persistent-v1")))
	require.Equal(t, int64(0), mustExists(t, client, store.migrationCampaignKey(), store.migrationCheckpointKey(), store.migrationLockKey()))

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke))
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
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.migrationCampaignKey()))
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.migrationCheckpointKey()))

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
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke))
	for i := 0; i < 8; i++ {
		require.NoError(t, client.Set(store.tokenKey(fmt.Sprintf("legacy-%02d", i)), "v2:legacy", 0).Err())
	}
	options := LegacyMigrationOptions{
		CampaignID: "lock-loss",
		CutoffAt:   time.Now().UTC().Add(time.Hour),
		BatchSize:  1000,
		Interval:   500 * time.Millisecond,
		Apply:      true,
		Lease:      5 * time.Second,
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
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke))
	for i := 0; i < 40; i++ {
		require.NoError(t, client.Set(store.tokenKey(fmt.Sprintf("resume-%02d", i)), "v2:legacy", 0).Err())
	}
	options := LegacyMigrationOptions{
		CampaignID: "resume",
		CutoffAt:   time.Now().UTC().Add(time.Hour),
		BatchSize:  1,
		Interval:   20 * time.Millisecond,
		Apply:      true,
		Lease:      5 * time.Second,
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
		raw, err := client.Get(store.migrationCheckpointKey()).Result()
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

type legacyTokenSnapshot struct {
	Value string
	TTL   time.Duration
}

func newLegacyMigrationTestStore(t *testing.T, mode SessionMode) (*RedisSessionStore, *rd.Client) {
	t.Helper()
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-migration:" + util.GenerUUID() + ":"
	uidPrefix := "session-migration-uid:" + util.GenerUUID() + ":"
	options := []SessionStoreOption{WithSessionMode(mode)}
	if mode.writesV3() {
		options = append(options, WithSessionMaxPerUID(10))
	}
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
