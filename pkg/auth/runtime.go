package auth

import (
	"context"
	"errors"
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
	store   *RedisSessionStore
	client  *rd.Client
	control *RolloutControlStore
	boot    RolloutBoot
	policy  sessionPolicy
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
	// The module migration that creates the MySQL authority has not run yet.
	// Keep issuance fenced until InitializeSessionRollout is called after
	// module.Setup and before the server starts accepting traffic.
	store.issuanceFenced.Store(true)
	runtime := &sessionRuntime{store: store, client: client, policy: policy}
	ctx.SetValue(runtime, sessionRuntimeContextKey)
	return store, client
}

// InitializeSessionRollout runs after module migrations and before HTTP serve.
// The #725 Redis floor and deprecated MODE are consulted only if the MySQL
// singleton does not exist yet; every later boot reads MySQL exclusively.
func InitializeSessionRollout(ctx *config.Context) (RolloutBoot, *RolloutControlStore, error) {
	if ctx == nil {
		return RolloutBoot{}, nil, errors.New("auth: initialize session rollout requires non-nil context")
	}
	runtime, ok := ctx.Value(sessionRuntimeContextKey).(*sessionRuntime)
	if !ok || runtime == nil {
		return RolloutBoot{}, nil, errors.New("auth: session store must be constructed before rollout initialization")
	}
	if runtime.control != nil {
		return runtime.boot, runtime.control, nil
	}
	if ctx.DB() == nil {
		return RolloutBoot{}, nil, errors.New("auth: session rollout requires MySQL")
	}
	control := NewRolloutControlStore(ctx.DB())
	state, err := control.Load(context.Background())
	outcome := RolloutBootNormal
	if errors.Is(err, ErrRolloutStateUninitialized) {
		bootCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		legacy, legacyErr := runtime.store.RolloutControl(bootCtx)
		if legacyErr != nil {
			return RolloutBoot{}, nil, fmt.Errorf("auth: read #725 rollout floor for MySQL takeover: %w", legacyErr)
		}
		legacyFloor := SessionMode("")
		legacyMaxPerUID := 0
		if legacy != nil {
			legacyFloor = legacy.ModeFloor
			legacyMaxPerUID = legacy.MaxPerUID
		}
		seed, seedErr := ResolveRolloutSeed(
			legacyFloor,
			runtime.policy.legacyMode,
			legacyMaxPerUID,
			runtime.policy.legacyMaxPerUID,
		)
		if seedErr != nil {
			return RolloutBoot{}, nil, seedErr
		}
		seed.Actor = "startup"
		switch {
		case legacyFloor.valid():
			seed.Source = "legacy-redis"
			outcome = RolloutBootAdopted
		case runtime.policy.legacyMode.valid() && runtime.policy.legacyMode != SessionModeExpand:
			seed.Source = "legacy-env"
			outcome = RolloutBootAdopted
		default:
			seed.Source = "bootstrap"
			outcome = RolloutBootFresh
		}
		if redisID, idErr := runtime.store.currentRedisInstanceID(); idErr == nil {
			seed.RedisID = redisID
		}
		state, err = control.Initialize(bootCtx, seed)
	}
	if err != nil {
		return RolloutBoot{}, nil, err
	}
	mode := state.Floor
	if runtime.policy.canaryAhead && mode.rank() < SessionModeEnforce.rank() {
		mode = mode.next()
	}
	if err := runtime.store.ApplyRolloutState(mode, state.MaxPerUID); err != nil {
		return RolloutBoot{}, nil, err
	}
	boot := RolloutBoot{
		Outcome:       outcome,
		Floor:         state.Floor,
		Mode:          runtime.store.Mode(),
		MaxPerUID:     state.MaxPerUID,
		AutoAdvance:   runtime.policy.autoAdvance,
		CanaryAhead:   runtime.policy.canaryAhead,
		ExpectWriters: runtime.policy.expectWriters,
	}
	runtime.control = control
	runtime.boot = boot
	return boot, control, nil
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
