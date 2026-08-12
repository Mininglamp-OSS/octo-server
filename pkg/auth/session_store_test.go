package auth

import (
	"context"
	"fmt"
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

	oldPayload, err := Encode(TokenInfo{UID: "u1", Name: "old"})
	require.NoError(t, err)
	require.NoError(t, store.IssueNew(context.Background(), token, oldPayload, "u1", 1))
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

func TestRedisSessionStoreRejectsV3PayloadDowngrade(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-v3-downgrade-" + util.GenerUUID()
	key := cfg.Cache.TokenCachePrefix + token
	t.Cleanup(func() {
		_ = client.Del(key).Err()
		_ = client.Close()
	})
	now := time.Now().UTC()
	v3, err := EncodeV3(TokenInfo{
		UID:               "u-v3",
		IssuedAt:          now.Add(-time.Minute).Unix(),
		ExpiresAt:         now.Add(time.Minute).Unix(),
		SessionGeneration: "generation-v3",
		SessionRevision:   1,
	})
	require.NoError(t, err)
	v2, err := Encode(TokenInfo{UID: "u-v3", Name: "renamed"})
	require.NoError(t, err)
	require.NoError(t, client.Set(key, v3, time.Minute).Err())
	before, err := client.PTTL(key).Result()
	require.NoError(t, err)

	ok, err := store.UpdatePayloadKeepDeadline(context.Background(), token, v2)
	require.False(t, ok)
	require.ErrorContains(t, err, "version downgrade")
	after, ttlErr := client.PTTL(key).Result()
	require.NoError(t, ttlErr)
	require.Positive(t, after)
	require.LessOrEqual(t, after, before)
	got, getErr := client.Get(key).Result()
	require.NoError(t, getErr)
	require.Equal(t, v3, got, "a rejected update must leave the v3 payload unchanged")
}

type sessionStoreProber interface {
	Probe(context.Context) error
}

func TestRedisSessionStoreProbeExecutesReadOnlyLua(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-probe:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, "session-probe-uid:", time.Minute)
	t.Cleanup(func() { _ = client.Close() })

	prober, ok := interface{}(store).(sessionStoreProber)
	require.True(t, ok, "RedisSessionStore must expose a startup Lua compatibility probe")
	require.NoError(t, prober.Probe(context.Background()))
	keys, err := client.Keys(prefix + "*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "the startup probe must not write Redis")
}

func TestRedisSessionStoreProbeFailsClosed(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, "session-probe-fail:", "session-probe-fail-uid:", time.Minute)
	t.Cleanup(func() { _ = client.Close() })
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			if cmd.Name() == "evalsha" || cmd.Name() == "eval" {
				cmd.Args()[0] = "eval-disabled-by-proxy"
			}
			return old(cmd)
		}
	})

	prober, ok := interface{}(store).(sessionStoreProber)
	require.True(t, ok, "RedisSessionStore must expose a startup Lua compatibility probe")
	require.ErrorContains(t, prober.Probe(context.Background()), "unknown command")
}

func TestRedisSessionStoreConcurrentLogoutCannotBeResurrected(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	storeA := NewRedisSessionStore(clientA, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	storeB := NewRedisSessionStore(clientB, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-race-" + util.GenerUUID()
	tokenKey := cfg.Cache.TokenCachePrefix + token
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1u-race"
	t.Cleanup(func() {
		_ = clientA.Del(tokenKey, uidKey).Err()
		_ = clientA.Close()
		_ = clientB.Close()
	})
	oldPayload, err := Encode(TokenInfo{UID: "u-race", Name: "old"})
	require.NoError(t, err)
	require.NoError(t, storeA.IssueNew(context.Background(), token, oldPayload, "u-race", 1))

	start := make(chan struct{})
	errCh := make(chan error, 33)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		store := storeA
		if i%2 == 1 {
			store = storeB
		}
		wg.Add(1)
		go func(store *RedisSessionStore) {
			defer wg.Done()
			<-start
			_, err := store.ReuseExisting(context.Background(), token, "new", "u-race", 1)
			errCh <- err
		}(store)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		errCh <- storeB.DeleteToken(context.Background(), token)
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	exists, err := clientA.Exists(tokenKey).Result()
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
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			args := cmd.Args()
			if cmd.Name() == "set" && !redisArgsContain(args, "nx") {
				args[0] = "injected-uid-index-failure"
			}
			return old(cmd)
		}
	})

	payload, err := Encode(TokenInfo{UID: "u-compensate", Name: "issued"})
	require.NoError(t, err)
	issueErr := store.IssueNew(context.Background(), token, payload, "u-compensate", 1)
	require.Error(t, issueErr)
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
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			args := cmd.Args()
			if cmd.Name() == "set" && !redisArgsContain(args, "nx") {
				args[0] = "injected-uid-index-failure"
			}
			return old(cmd)
		}
	})

	ok, err := store.ReuseExisting(context.Background(), token, "new", "u-reuse", 1)
	require.True(t, ok)
	require.Error(t, err)
	got, getErr := client.Get(tokenKey).Result()
	require.NoError(t, getErr)
	require.Equal(t, "new", got, "reuse failure must not delete a credential that predated the attempt")
}

func redisArgsContain(args []interface{}, want string) bool {
	for _, arg := range args {
		if fmt.Sprint(arg) == want {
			return true
		}
	}
	return false
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
		SessionRevision:   1,
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
	// Three, not four: the shape counters describe legacy CREDENTIALS, not keys,
	// so the undecodable "not-a-token" is reported once as DecodeInvalid and
	// counted in no TTL bucket. Total still counts every key scanned.
	//
	// The distinction is load-bearing rather than cosmetic. These counters feed
	// the bounded gate, and while they described the keyspace a single permanent
	// undecodable record held Persistent at 1 forever — bounded refused, bounded
	// is the only route to enforce, migration skips undecodable records by
	// design, and nothing expires a key with no TTL.
	require.Equal(t, int64(3), stats.Finite)
	require.Equal(t, int64(1), stats.OverMax)
	require.Equal(t, int64(1), stats.DecodeInvalid)
	require.Equal(t, int64(1), stats.V1)
	require.Equal(t, int64(2), stats.V2)
	require.Equal(t, int64(1), stats.V3)
}

func TestRedisSessionStoreObserveCountsZeroTTLAsInvalid(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "observe-zero-ttl:" + util.GenerUUID() + ":"
	uidPrefix := "observe-zero-ttl-uid:" + util.GenerUUID() + ":"
	key := prefix + "token"
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Minute)
	require.NoError(t, client.Set(key, "u1@legacy", time.Minute).Err())
	require.NoError(t, readTokenScript.Load(client).Err())
	t.Cleanup(func() {
		_ = client.Del(key, store.rolloutScanLeaseKey()).Err()
		_ = client.Close()
	})
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			args := cmd.Args()
			if cmd.Name() == "evalsha" && len(args) > 1 && fmt.Sprint(args[1]) == readTokenScript.Hash() {
				args[0] = "eval"
				args[1] = `return {"u1@legacy", 0}`
				args[2] = 0
			}
			return old(cmd)
		}
	})

	stats, err := store.Observe(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.Total)
	require.Equal(t, int64(1), stats.InvalidTTL)
	require.Zero(t, stats.Finite)
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

func TestRedisSessionStoreBoundsTouchedLegacyToken(t *testing.T) {
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

	overMaxToken := "session-store-over-max-" + util.GenerUUID()
	overMaxKey := cfg.Cache.TokenCachePrefix + overMaxToken
	t.Cleanup(func() { _ = client.Del(overMaxKey).Err() })
	require.NoError(t, client.Set(overMaxKey, "old", 2*time.Minute).Err())
	ok, err = store.UpdatePayloadKeepDeadline(context.Background(), overMaxToken, "new")
	require.NoError(t, err)
	require.True(t, ok)
	ttl, err = client.PTTL(overMaxKey).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Minute, "touched over-max token must be clamped, never preserved beyond policy")
}
