package auth

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

func TestSessionStoreForContextSharesOneBoundedPool(t *testing.T) {
	ctx := config.NewContext(config.New())
	store1, client1 := SessionStoreAndClientForContext(ctx)
	store2, client2 := SessionStoreAndClientForContext(ctx)
	t.Cleanup(func() { _ = client1.Close() })

	require.Same(t, store1, store2)
	require.Same(t, client1, client2)
	require.Equal(t, 10, client1.Options().PoolSize)
	require.Equal(t, time.Second, client1.Options().PoolTimeout)
	require.Equal(t, 1, client1.Options().MaxRetries)
}
