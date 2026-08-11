package auth

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const sessionRuntimeContextKey = "octo-server/auth/session-runtime/v1"

const (
	sessionRedisPoolSizeEnv        = "OCTO_AUTH_SESSION_REDIS_POOL_SIZE"
	sessionRedisPoolTimeoutEnv     = "OCTO_AUTH_SESSION_REDIS_POOL_TIMEOUT"
	sessionRedisPoolSizeMax        = 4096
	sessionRedisPoolTimeoutMax     = 30 * time.Second
	sessionRedisPoolTimeoutDefault = 3 * time.Second
)

type sessionRedisOptions struct {
	poolSize    int
	poolTimeout time.Duration
}

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
	options, err := sessionRedisOptionsFromEnv()
	if err != nil {
		panic(err)
	}
	policy, err := sessionPolicyFromEnv()
	if err != nil {
		panic(err)
	}
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = options.poolSize
		o.DialTimeout = 2 * time.Second
		o.ReadTimeout = 2 * time.Second
		o.WriteTimeout = 2 * time.Second
		o.PoolTimeout = options.poolTimeout
	})
	store := NewRedisSessionStore(
		client,
		ctx.GetConfig().Cache.TokenCachePrefix,
		ctx.GetConfig().Cache.UIDTokenCachePrefix,
		ctx.GetConfig().Cache.TokenExpire,
		WithSessionMode(policy.mode),
		WithSessionMaxPerUID(policy.maxPerUID),
	)
	validationCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.ValidateRolloutControl(validationCtx, policy.requiredFloor); err != nil {
		_ = client.Close()
		panic(err)
	}
	ctx.SetValue(&sessionRuntime{store: store, client: client}, sessionRuntimeContextKey)
	return store, client
}

func sessionRedisOptionsFromEnv() (sessionRedisOptions, error) {
	options := sessionRedisOptions{
		poolSize:    10 * runtime.GOMAXPROCS(0),
		poolTimeout: sessionRedisPoolTimeoutDefault,
	}
	if raw, ok := os.LookupEnv(sessionRedisPoolSizeEnv); ok {
		value := strings.TrimSpace(raw)
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 {
			return sessionRedisOptions{}, fmt.Errorf("%s must be a positive integer", sessionRedisPoolSizeEnv)
		}
		maxPoolSize := sessionRedisPoolSizeMax
		if options.poolSize > maxPoolSize {
			maxPoolSize = options.poolSize
		}
		if parsed > int64(maxPoolSize) {
			return sessionRedisOptions{}, fmt.Errorf("%s must not exceed %d", sessionRedisPoolSizeEnv, maxPoolSize)
		}
		options.poolSize = int(parsed)
	}
	if raw, ok := os.LookupEnv(sessionRedisPoolTimeoutEnv); ok {
		value := strings.TrimSpace(raw)
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return sessionRedisOptions{}, fmt.Errorf("invalid %s %q: %w", sessionRedisPoolTimeoutEnv, value, err)
		}
		if parsed <= 0 {
			return sessionRedisOptions{}, fmt.Errorf("%s must be greater than zero", sessionRedisPoolTimeoutEnv)
		}
		if parsed > sessionRedisPoolTimeoutMax {
			return sessionRedisOptions{}, fmt.Errorf("%s must not exceed %s", sessionRedisPoolTimeoutEnv, sessionRedisPoolTimeoutMax)
		}
		options.poolTimeout = parsed
	}
	return options, nil
}
