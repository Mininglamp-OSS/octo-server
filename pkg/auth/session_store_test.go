package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

func TestRedisSessionStoreKeepsTokenDeadline(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	store := NewRedisSessionStore(
		client,
		ctx.GetConfig().Cache.TokenCachePrefix,
		ctx.GetConfig().Cache.UIDTokenCachePrefix,
		2*time.Minute,
	)
	token := "session-store-" + util.GenerUUID()
	t.Cleanup(func() {
		_ = client.Del(ctx.GetConfig().Cache.TokenCachePrefix + token).Err()
		_ = client.Close()
	})

	require.NoError(t, store.IssueNew(context.Background(), token, "old", "u1", 1))
	before, err := client.PTTL(ctx.GetConfig().Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Positive(t, before)

	time.Sleep(20 * time.Millisecond)
	ok, err := store.UpdatePayloadKeepDeadline(context.Background(), token, "new")
	require.NoError(t, err)
	require.True(t, ok)
	after, err := client.PTTL(ctx.GetConfig().Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Positive(t, after)
	require.LessOrEqual(t, after, before, "payload update must never extend the bearer deadline")
	got, err := client.Get(ctx.GetConfig().Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Equal(t, "new", got)
}

func TestRedisSessionStoreMissingTokenIsNotRecreated(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	store := NewRedisSessionStore(client, ctx.GetConfig().Cache.TokenCachePrefix, ctx.GetConfig().Cache.UIDTokenCachePrefix, time.Minute)
	t.Cleanup(func() { _ = client.Close() })
	token := "session-store-missing-" + util.GenerUUID()

	ok, err := store.ReuseExisting(context.Background(), token, "payload", "u1", 1)
	require.NoError(t, err)
	require.False(t, ok)
	exists, err := client.Exists(ctx.GetConfig().Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestRedisSessionStoreBoundsTouchedPersistentToken(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	store := NewRedisSessionStore(client, ctx.GetConfig().Cache.TokenCachePrefix, ctx.GetConfig().Cache.UIDTokenCachePrefix, time.Minute)
	token := "session-store-persistent-" + util.GenerUUID()
	key := ctx.GetConfig().Cache.TokenCachePrefix + token
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
