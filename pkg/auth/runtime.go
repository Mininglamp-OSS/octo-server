package auth

import (
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const sessionRuntimeContextKey = "octo-server/auth/session-runtime/v1"

type sessionRuntime struct {
	store  *RedisSessionStore
	client *rd.Client
}

var sessionRuntimeMu sync.Mutex

// SessionStoreForContext returns one bounded Redis pool per server context.
// All modules in a replica share it; security hardening must not multiply
// connection pools with the number of token-consuming modules.
func SessionStoreForContext(ctx *config.Context) *RedisSessionStore {
	store, _ := SessionStoreAndClientForContext(ctx)
	return store
}

// SessionStoreAndClientForContext also exposes the shared client to adjacent
// auth stores that require Lua. Callers must not close it independently; its
// lifetime is the same as the config.Context, matching existing Redis pools.
func SessionStoreAndClientForContext(ctx *config.Context) (*RedisSessionStore, *rd.Client) {
	if ctx == nil {
		panic("auth: session store requires non-nil context")
	}
	if existing, ok := ctx.Value(sessionRuntimeContextKey).(*sessionRuntime); ok {
		return existing.store, existing.client
	}

	sessionRuntimeMu.Lock()
	defer sessionRuntimeMu.Unlock()
	if existing, ok := ctx.Value(sessionRuntimeContextKey).(*sessionRuntime); ok {
		return existing.store, existing.client
	}
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
		o.DialTimeout = 2 * time.Second
		o.ReadTimeout = 2 * time.Second
		o.WriteTimeout = 2 * time.Second
		o.PoolTimeout = time.Second
	})
	store := NewRedisSessionStore(
		client,
		ctx.GetConfig().Cache.TokenCachePrefix,
		ctx.GetConfig().Cache.UIDTokenCachePrefix,
		ctx.GetConfig().Cache.TokenExpire,
	)
	ctx.SetValue(&sessionRuntime{store: store, client: client}, sessionRuntimeContextKey)
	return store, client
}
