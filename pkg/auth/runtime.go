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
	boot   RolloutBoot
	policy sessionPolicy
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
	policy := sessionPolicyFromEnv()
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
	)

	// Boot resolution replaces the old startup validation, which panicked when
	// the Redis floor key was missing at mode >= revoke and so turned one lost
	// key into a fleet that could not start. Nothing below is fatal.
	bootCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var markers *RolloutMarkerStore
	if db := ctx.DB(); db != nil {
		markers = NewRolloutMarkerStore(db)
	}
	boot, err := ResolveRolloutBoot(bootCtx, store, markers, policy.legacyMode, policy.legacyMaxPerUID)
	if err != nil {
		// Only a MySQL marker read failure reaches here; an unreadable Redis
		// floor is resolved inside ResolveRolloutBoot. Without the marker we
		// cannot tell "never initialised" from "lost it", and guessing the
		// wrong way re-admits revoked legacy bearers, so take the strict side.
		boot = RolloutBoot{Outcome: RolloutBootRecovered, Mode: SessionModeEnforce, MaxPerUID: policy.legacyMaxPerUID}
		boot.Warning = fmt.Sprintf("session rollout marker unreadable at boot (%v); resolving upward to %s", err, boot.Mode)
	}
	boot.AutoAdvance = policy.autoAdvance
	boot.CanaryAhead = policy.canaryAhead
	boot.ExpectWriters = policy.expectWriters
	if policy.canaryAhead && boot.Mode.rank() < SessionModeEnforce.rank() {
		boot.Mode = boot.Mode.next()
	}
	if applyErr := store.ApplyRolloutState(boot.Mode, boot.MaxPerUID); applyErr != nil {
		boot.Warning = strings.TrimSpace(boot.Warning + " " + applyErr.Error())
	}
	if boot.Outcome == RolloutBootAdopted && markers != nil {
		if stampErr := markers.StampOnce(bootCtx, boot.Floor); stampErr != nil {
			boot.Warning = strings.TrimSpace(boot.Warning + " " + stampErr.Error())
		}
	}
	if boot.Outcome == RolloutBootRecovered {
		if recoverErr := store.RecoverRolloutControlAtEnforce(bootCtx, boot.MaxPerUID); recoverErr != nil {
			boot.Warning = strings.TrimSpace(boot.Warning + " " + recoverErr.Error())
		}
	}

	runtime := &sessionRuntime{store: store, client: client, boot: boot, policy: policy}
	ctx.SetValue(runtime, sessionRuntimeContextKey)
	return store, client
}

// SessionBootForContext exposes what boot resolved, for the startup log line
// and the rollout status subcommand.
func SessionBootForContext(ctx *config.Context) (RolloutBoot, []string) {
	if existing, ok := ctx.Value(sessionRuntimeContextKey).(*sessionRuntime); ok {
		return existing.boot, existing.policy.warnings
	}
	return RolloutBoot{}, nil
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
