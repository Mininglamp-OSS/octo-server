package auth

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

func TestSessionStoreForContextSharesOneBoundedPool(t *testing.T) {
	unsetSessionRuntimeEnv(t)
	ctx := config.NewContext(config.New())
	store1, client1 := SessionStoreAndClientForContext(ctx)
	store2, client2 := SessionStoreAndClientForContext(ctx)
	t.Cleanup(func() { _ = client1.Close() })

	require.Same(t, store1, store2)
	require.Same(t, client1, client2)
	require.Equal(t, 10*runtime.GOMAXPROCS(0), client1.Options().PoolSize)
	require.Equal(t, 3*time.Second, client1.Options().PoolTimeout)
	require.Equal(t, 1, client1.Options().MaxRetries)
}

func TestSessionStoreForContextHonorsPoolOverrides(t *testing.T) {
	unsetSessionRuntimeEnv(t)
	t.Setenv("OCTO_AUTH_SESSION_REDIS_POOL_SIZE", "37")
	t.Setenv("OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", "1500ms")

	ctx := config.NewContext(config.New())
	_, client := SessionStoreAndClientForContext(ctx)
	t.Cleanup(func() { _ = client.Close() })

	require.Equal(t, 37, client.Options().PoolSize)
	require.Equal(t, 1500*time.Millisecond, client.Options().PoolTimeout)
}

func TestSessionStoreForContextRejectsInvalidPoolOverrides(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty size", key: "OCTO_AUTH_SESSION_REDIS_POOL_SIZE", value: ""},
		{name: "zero size", key: "OCTO_AUTH_SESSION_REDIS_POOL_SIZE", value: "0"},
		{name: "negative size", key: "OCTO_AUTH_SESSION_REDIS_POOL_SIZE", value: "-1"},
		{name: "malformed size", key: "OCTO_AUTH_SESSION_REDIS_POOL_SIZE", value: "many"},
		{name: "oversized pool", key: "OCTO_AUTH_SESSION_REDIS_POOL_SIZE", value: "4097"},
		{name: "empty timeout", key: "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", value: ""},
		{name: "zero timeout", key: "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", value: "0s"},
		{name: "negative timeout", key: "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", value: "-1s"},
		{name: "malformed timeout", key: "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", value: "soon"},
		{name: "oversized timeout", key: "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT", value: "31s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetSessionRuntimeEnv(t)
			t.Setenv(tt.key, tt.value)
			ctx := config.NewContext(config.New())

			require.Panics(t, func() {
				_, client := SessionStoreAndClientForContext(ctx)
				t.Cleanup(func() { _ = client.Close() })
			})
		})
	}
}

func unsetSessionRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OCTO_AUTH_SESSION_REDIS_POOL_SIZE",
		"OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT",
		sessionModeEnv,
		sessionMaxPerUIDEnv,
		sessionRequiredFloorEnv,
	} {
		old, existed := os.LookupEnv(key)
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, old)
				return
			}
			_ = os.Unsetenv(key)
		})
	}
}
