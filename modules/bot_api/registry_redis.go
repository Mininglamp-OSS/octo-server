package bot_api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// appBotAuthKeyPrefix namespaces the App Bot auth cache in Redis. One key per
// token: appbot:auth:{sha256hex(token)} -> JSON(AppBotRegistrySpec). The token
// is HASHED rather than embedded verbatim so a live bearer credential never
// lands in a Redis key (visible to KEYS/MONITOR/RDB dumps/ops tooling). SHA-256
// over the high-entropy token is sufficient — no salt needed, and the hash is
// stable so every replica derives the same key.
const appBotAuthKeyPrefix = "appbot:auth:"

// degradeWarnInterval throttles fail-open warnings so a sustained Redis outage
// on the bot-auth hot path logs at most once per interval instead of once per
// request (mirrors modules/incomingwebhook warnDegraded).
const appBotDegradeWarnInterval = 30 * time.Second

// appBotDegradedCooldown is how long a Redis-command failure keeps the
// best-effort write circuit open: while open, the DB-fallback cache warm-up
// (Add) is skipped so a sustained Redis outage can't launch one blocking SET
// (dial/write timeout × retries + pool wait) per auth request. The very request
// that observes the failure trips the circuit before its own warm-up runs, so no
// SET storm accumulates; writes resume once Redis has been healthy this long.
const appBotDegradedCooldown = 5 * time.Second

// RedisAppBotRegistry is a SHARED, write-through Redis cache for App Bot auth
// (issue #309). Replacing the per-process in-memory map with one shared store
// makes token revocation (rotate / unpublish / delete) take effect on every
// replica the instant the admin request commits, instead of lingering on peer
// replicas until they restart.
//
// Authority model: the app_bot table (queryAppBotByToken + status==1 gate) is
// the source of truth. This cache is a fast path in front of it:
//   - FindByToken miss OR any Redis error -> nil -> authAppBot's DB fallback
//     runs (fail safe; a Redis outage degrades to a correct, slower DB lookup,
//     never to serving a stale/revoked spec).
//   - mutators are best-effort write-through; the DB write in the admin handler
//     already happened and is authoritative, so a Redis write error only costs
//     a bounded window of staleness (keys carry the safety-net TTL below).
//
// Bounded-staleness contract (mirrors modules/incomingwebhook/cache.go):
//   - Revocation is instant cross-instance via the shared DEL.
//   - The only residual is a narrow cache-invalidation race: an auth miss that
//     read the DB as still-valid immediately before a concurrent revocation can
//     re-populate the just-deleted key. That re-add is bounded by the safety-net
//     TTL (ttl()), after which the key expires and the next auth re-validates
//     against the DB. The TTL also self-heals any drift from a failed DEL.
type RedisAppBotRegistry struct {
	log.Log
	client *rd.Client
	// ttl returns the safety-net expiry written with every key. Injected (rather
	// than reading modules/common directly) so bot_api stays decoupled from the
	// system-settings package; app_bot wires it over the hot-reloaded snapshot.
	ttl func() time.Duration

	degradeMu     sync.Mutex
	degradeLast   time.Time // last WARN emit (log throttle)
	degradedUntil time.Time // best-effort writes are skipped until this instant after a Redis-command failure
}

// NewRedisAppBotRegistry builds the shared registry. The Redis client is built
// the same way modules/opanalytics does (octoredis.MustBuildOptions over the
// process config). ttl supplies the safety-net key expiry; a non-positive value
// is coerced to a sane floor in set().
func NewRedisAppBotRegistry(ctx *config.Context, ttl func() time.Duration) *RedisAppBotRegistry {
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 2
		o.DialTimeout = 3 * time.Second
		o.ReadTimeout = 2 * time.Second
		o.WriteTimeout = 2 * time.Second
	}))
	return &RedisAppBotRegistry{
		Log:    log.NewTLog("RedisAppBotRegistry"),
		client: client,
		ttl:    ttl,
	}
}

// appBotAuthKey derives the Redis key for a token. The token is SHA-256 hashed
// so the raw bearer credential never appears in a Redis key (see prefix doc).
func appBotAuthKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return appBotAuthKeyPrefix + hex.EncodeToString(sum[:])
}

// FindByToken reads the shared cache. Miss (redis.Nil) and any other Redis error
// both return nil so the caller falls through to the authoritative DB lookup —
// auth must never fail open on a degraded backend.
func (r *RedisAppBotRegistry) FindByToken(token string) *AppBotRegistrySpec {
	if token == "" {
		return nil
	}
	val, err := r.client.Get(appBotAuthKey(token)).Result()
	if err == rd.Nil {
		return nil // genuine miss -> DB fallback populates it
	}
	if err != nil {
		r.noteRedisFailure("app bot auth cache GET failed, fail-safe to DB", err)
		return nil
	}
	var spec AppBotRegistrySpec
	if uerr := json.Unmarshal([]byte(val), &spec); uerr != nil {
		// Corrupt entry: drop it and miss to DB rather than trusting garbage.
		r.warnDegraded("app bot auth cache entry corrupt, dropping", uerr)
		_ = r.client.Del(appBotAuthKey(token)).Err()
		return nil
	}
	return &spec
}

// Add write-throughs a spec with the safety-net TTL. Best-effort: a failure is
// logged (throttled) and ignored — the DB remains authoritative.
//
// While the write circuit is open (a recent Redis-command failure), the warm-up
// is skipped entirely. The DB-fallback auth path calls Add via `go reg.Add(...)`;
// without this guard a sustained Redis outage would spawn one goroutine blocking
// on a doomed SET (dial/write timeout × retries + pool wait) per auth request,
// piling up goroutines and pool waiters for the exact failure this cache rides
// out. Dropping the warm-up is harmless — the DB already produced the
// authoritative answer, and the next miss re-attempts once Redis heals.
func (r *RedisAppBotRegistry) Add(token string, spec *AppBotRegistrySpec) {
	if r.writesDegraded() {
		return
	}
	r.set(token, spec)
}

// Remove deletes the shared key, instantly revoking the token on every replica.
func (r *RedisAppBotRegistry) Remove(token string) {
	if token == "" {
		return
	}
	if err := r.client.Del(appBotAuthKey(token)).Err(); err != nil {
		// A failed DEL leaves a stale key until its TTL expires; log so ops can
		// see it, but the bounded TTL is the backstop. Also trips the write circuit
		// so concurrent warm-ups stop hammering a backend that's clearly degraded.
		r.noteRedisFailure("app bot auth cache DEL failed (stale until TTL)", err)
	}
}

// Update revokes the old token and write-throughs the new one. The DEL+SET are
// not atomic, but each is on the shared store, so peers converge immediately;
// the brief window only affects the rotating bot's own old/new tokens.
func (r *RedisAppBotRegistry) Update(oldToken, newToken string, spec *AppBotRegistrySpec) {
	r.Remove(oldToken)
	r.set(newToken, spec)
}

func (r *RedisAppBotRegistry) set(token string, spec *AppBotRegistrySpec) {
	if token == "" || spec == nil {
		return
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		r.warnDegraded("app bot auth spec marshal failed", err)
		return
	}
	ttl := r.safeTTL()
	if err := r.client.Set(appBotAuthKey(token), payload, ttl).Err(); err != nil {
		r.noteRedisFailure("app bot auth cache SET failed", err)
	}
}

// safeTTL coerces a missing/invalid TTL provider result to a sane floor so a
// misconfiguration can never write a never-expiring (0) or negative key.
func (r *RedisAppBotRegistry) safeTTL() time.Duration {
	if r.ttl == nil {
		return defaultAppBotAuthCacheTTL
	}
	d := r.ttl()
	if d <= 0 {
		return defaultAppBotAuthCacheTTL
	}
	return d
}

// defaultAppBotAuthCacheTTL is the fallback safety-net expiry when the injected
// provider yields a non-positive value. Kept in sync with the system-settings
// default (defaultAppBotAuthCacheTTLSeconds) in modules/common.
const defaultAppBotAuthCacheTTL = 60 * time.Second

// warnDegraded emits a throttled WARN without tripping the write circuit. Used
// for non-availability problems (corrupt cache entry, spec marshal failure) where
// the backend is reachable and pausing warm-up writes would be pointless.
func (r *RedisAppBotRegistry) warnDegraded(msg string, err error) {
	r.degradeMu.Lock()
	if !r.degradeLast.IsZero() && time.Since(r.degradeLast) < appBotDegradeWarnInterval {
		r.degradeMu.Unlock()
		return
	}
	r.degradeLast = time.Now()
	r.degradeMu.Unlock()
	r.Warn(msg, zap.Error(err))
}

// noteRedisFailure records an actual Redis-command failure (GET/SET/DEL): it
// opens the best-effort write circuit for appBotDegradedCooldown (so warm-up
// Adds are skipped, preventing a per-request SET storm under a sustained outage)
// AND emits the same throttled WARN as warnDegraded.
func (r *RedisAppBotRegistry) noteRedisFailure(msg string, err error) {
	now := time.Now()
	r.degradeMu.Lock()
	r.degradedUntil = now.Add(appBotDegradedCooldown)
	shouldWarn := r.degradeLast.IsZero() || now.Sub(r.degradeLast) >= appBotDegradeWarnInterval
	if shouldWarn {
		r.degradeLast = now
	}
	r.degradeMu.Unlock()
	if shouldWarn {
		r.Warn(msg, zap.Error(err))
	}
}

// writesDegraded reports whether a recent Redis-command failure still has the
// best-effort write circuit open (skip warm-up writes until the cooldown lapses).
func (r *RedisAppBotRegistry) writesDegraded() bool {
	r.degradeMu.Lock()
	defer r.degradeMu.Unlock()
	return !r.degradedUntil.IsZero() && time.Now().Before(r.degradedUntil)
}
