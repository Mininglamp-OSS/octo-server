package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func TestRedisSessionStoreKeepsTokenDeadline(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(
		client,
		cfg.Cache.TokenCachePrefix,
		cfg.Cache.UIDTokenCachePrefix,
		2*time.Minute,
	)
	token := "session-store-" + util.GenerUUID()
	t.Cleanup(func() {
		_ = client.Del(cfg.Cache.TokenCachePrefix+token, cfg.Cache.UIDTokenCachePrefix+"1u1").Err()
		_ = client.Close()
	})

	require.NoError(t, store.IssueNew(context.Background(), token, "old", "u1", 1))
	before, err := client.PTTL(cfg.Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Positive(t, before)

	time.Sleep(20 * time.Millisecond)
	ok, err := store.UpdatePayloadKeepDeadline(context.Background(), token, "new")
	require.NoError(t, err)
	require.True(t, ok)
	after, err := client.PTTL(cfg.Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Positive(t, after)
	require.LessOrEqual(t, after, before, "payload update must never extend the bearer deadline")
	got, err := client.Get(cfg.Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Equal(t, "new", got)
}

func TestRedisSessionStoreConcurrentLogoutCannotBeResurrected(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-race-" + util.GenerUUID()
	tokenKey := cfg.Cache.TokenCachePrefix + token
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1u-race"
	t.Cleanup(func() {
		_ = client.Del(tokenKey, uidKey).Err()
		_ = client.Close()
	})
	require.NoError(t, store.IssueNew(context.Background(), token, "old", "u-race", 1))

	start := make(chan struct{})
	errCh := make(chan error, 33)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.ReuseExisting(context.Background(), token, "new", "u-race", 1)
			errCh <- err
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		errCh <- store.DeleteToken(context.Background(), token)
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	exists, err := client.Exists(tokenKey).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "SET XX Lua serialized with DEL must never recreate a logged-out bearer")
}

func TestRedisSessionStoreIssueFailureCompensatesNewCredential(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-compensate-" + util.GenerUUID()
	tokenKey := cfg.Cache.TokenCachePrefix + token
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1u-compensate"
	t.Cleanup(func() {
		_ = client.Del(tokenKey, uidKey).Err()
		_ = client.Close()
	})
	injected := errors.New("injected uid index failure")
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			args := cmd.Args()
			if cmd.Name() == "set" && len(args) > 1 && args[1] == uidKey {
				return injected
			}
			return old(cmd)
		}
	})

	err := store.IssueNew(context.Background(), token, "payload", "u-compensate", 1)
	require.ErrorIs(t, err, injected)
	exists, existsErr := client.Exists(tokenKey).Result()
	require.NoError(t, existsErr)
	require.Zero(t, exists, "partial issue must not leave an orphan bearer")
}

func TestRedisSessionStoreReuseFailureKeepsExistingCredential(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-reuse-failure-" + util.GenerUUID()
	tokenKey := cfg.Cache.TokenCachePrefix + token
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1u-reuse"
	t.Cleanup(func() {
		_ = client.Del(tokenKey, uidKey).Err()
		_ = client.Close()
	})
	require.NoError(t, client.Set(tokenKey, "old", time.Minute).Err())
	injected := errors.New("injected uid index failure")
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			args := cmd.Args()
			if cmd.Name() == "set" && len(args) > 1 && args[1] == uidKey {
				return injected
			}
			return old(cmd)
		}
	})

	ok, err := store.ReuseExisting(context.Background(), token, "new", "u-reuse", 1)
	require.True(t, ok)
	require.ErrorIs(t, err, injected)
	got, getErr := client.Get(tokenKey).Result()
	require.NoError(t, getErr)
	require.Equal(t, "new", got, "reuse failure must not delete a credential that predated the attempt")
}

func TestRedisSessionStoreObserveAggregatesOnly(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "observe:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, "observe-uid:", time.Minute)
	keys := []string{
		prefix + "legacy",
		prefix + "v2",
		prefix + "v3",
		prefix + "invalid",
		prefix + "over-max",
	}
	t.Cleanup(func() {
		_ = client.Del(keys...).Err()
		_ = client.Close()
	})
	v2, err := Encode(TokenInfo{UID: "u2", Name: "v2"})
	require.NoError(t, err)
	now := time.Now().UTC()
	v3, err := EncodeV3(TokenInfo{
		UID:               "u3",
		IssuedAt:          now.Add(-time.Minute).Unix(),
		ExpiresAt:         now.Add(time.Minute).Unix(),
		SessionGeneration: "g3",
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(keys[0], "u1@legacy", 0).Err())
	require.NoError(t, client.Set(keys[1], v2, 30*time.Second).Err())
	require.NoError(t, client.Set(keys[2], v3, 30*time.Second).Err())
	require.NoError(t, client.Set(keys[3], "not-a-token", 30*time.Second).Err())
	require.NoError(t, client.Set(keys[4], v2, 2*time.Minute).Err())

	stats, err := store.Observe(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, int64(5), stats.Total)
	require.Equal(t, int64(1), stats.Persistent)
	require.Equal(t, int64(4), stats.Finite)
	require.Equal(t, int64(1), stats.OverMax)
	require.Equal(t, int64(1), stats.DecodeInvalid)
	require.Equal(t, int64(1), stats.V1)
	require.Equal(t, int64(2), stats.V2)
	require.Equal(t, int64(1), stats.V3)
}

func TestRedisSessionStoreMissingTokenIsNotRecreated(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	t.Cleanup(func() { _ = client.Close() })
	token := "session-store-missing-" + util.GenerUUID()

	ok, err := store.ReuseExisting(context.Background(), token, "payload", "u1", 1)
	require.NoError(t, err)
	require.False(t, ok)
	exists, err := client.Exists(cfg.Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestRedisSessionStoreBoundsTouchedPersistentToken(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-persistent-" + util.GenerUUID()
	key := cfg.Cache.TokenCachePrefix + token
	t.Cleanup(func() {
		_ = client.Del(key).Err()
		_ = client.Close()
	})
	require.NoError(t, client.Set(key, "old", 0).Err())

	ok, err := store.UpdatePayloadKeepDeadline(context.Background(), token, "new")
	require.NoError(t, err)
	require.True(t, ok)
	ttl, err := client.PTTL(key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Minute)
}
