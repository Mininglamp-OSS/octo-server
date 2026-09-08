package message

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestConversationExtraLeaseUsesUUIDTTLAndOwnerCheckedScripts(t *testing.T) {
	cfg := config.New()
	ctx := config.NewContext(cfg)
	lock := newConversationExtraLock(ctx)
	t.Cleanup(func() { _ = lock.client.Close() })

	uid := "lease-owner-" + uuid.NewString()
	key := conversationExtraLockKey(uid)
	require.NoError(t, lock.client.Del(key).Err())
	t.Cleanup(func() { _ = lock.client.Del(key).Err() })

	lease, err := lock.Acquire(context.Background(), uid)
	require.NoError(t, err)
	_, err = uuid.Parse(lease.owner)
	require.NoError(t, err, "lock owner must be a UUID")

	ttl, err := lock.client.PTTL(key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, conversationExtraLockTTL)

	require.NoError(t, lease.Renew(context.Background()))
	renewedTTL, err := lock.client.PTTL(key).Result()
	require.NoError(t, err)
	require.Greater(t, renewedTTL, 9*time.Second)

	// Simulate A expiring and B acquiring the same key. A's compare-delete must
	// not remove B's lease, and A's compare-renew must report lost ownership.
	newOwner := uuid.NewString()
	require.NoError(t, lock.client.Set(key, newOwner, conversationExtraLockTTL).Err())
	require.ErrorIs(t, lease.Renew(context.Background()), errConversationExtraLockLost)
	require.NoError(t, lease.Release(context.Background()))
	storedOwner, err := lock.client.Get(key).Result()
	require.NoError(t, err)
	require.Equal(t, newOwner, storedOwner)
}

func TestConversationExtraLockSerializesSameUID(t *testing.T) {
	cfg := config.New()
	ctx := config.NewContext(cfg)
	lock := newConversationExtraLock(ctx)
	t.Cleanup(func() { _ = lock.client.Close() })

	uid := "lease-serial-" + uuid.NewString()
	key := conversationExtraLockKey(uid)
	require.NoError(t, lock.client.Del(key).Err())
	t.Cleanup(func() { _ = lock.client.Del(key).Err() })

	first, err := lock.Acquire(context.Background(), uid)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err = lock.Acquire(waitCtx, uid)
	require.Error(t, err)

	require.NoError(t, first.Release(context.Background()))
	second, err := lock.Acquire(context.Background(), uid)
	require.NoError(t, err)
	require.NotEqual(t, first.owner, second.owner)
	require.NoError(t, second.Release(context.Background()))
}
