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
// (Warm) is skipped so a sustained Redis outage can't launch one blocking SETNX
// (dial/write timeout × retries + pool wait) per auth request. The very request
// that observes the failure trips the circuit before its own warm-up runs, so no
// write storm accumulates; warm-ups resume once Redis has been healthy this long.
// Authoritative writes (Add publish / Remove revoke) are NOT gated by it.
const appBotDegradedCooldown = 5 * time.Second

// appBotTombstone is the sentinel value a revocation writes to the shared key
// instead of deleting it. A tombstone (a) positively denies the token on every
// replica immediately, and (b) survives a racing best-effort Warm — which uses
// SETNX and so cannot overwrite it — closing the repopulate-resurrection race
// (a delayed auth-path warm-up landing just after a delete/unpublish DEL used to
// re-create the just-revoked key). It is not valid spec JSON, so FindByToken
// distinguishes it with an exact-value check before unmarshalling.
const appBotTombstone = "\x00appbot:revoked"

// appBotRevokeMaxAttempts bounds the retry on a revocation (tombstone) write so a
// transient Redis blip doesn't silently leave a revoked token authenticating
// until its key TTL. Kept small (the go-redis client already retries internally)
// so an admin revoke isn't blocked too long on a degraded backend.
const appBotRevokeMaxAttempts = 2

// RedisAppBotRegistry is a SHARED, write-through Redis cache for App Bot auth
// (issue #309). Replacing the per-process in-memory map with one shared store
// makes token revocation (rotate / unpublish / delete) take effect on every
// replica the instant the admin request commits, instead of lingering on peer
// replicas until they restart.
//
// Authority model: the app_bot table (queryAppBotByToken + status==1 gate) is
// the source of truth. This cache is a fast path in front of it:
//   - FindByToken miss, tombstone, OR any Redis error -> nil -> authAppBot's DB
//     fallback runs (fail safe; a Redis outage degrades to a correct, slower DB
//     lookup, never to serving a stale/revoked spec).
//
// Write model (the asymmetry is load-bearing):
//   - Add (authoritative publish / rotate-new): SET, overwrites any tombstone.
//   - Warm (best-effort warm-up: auth-path repopulate + startup load): SETNX,
//     never overwrites a tombstone or a fresher spec; circuit-gated so a Redis
//     outage can't pile up blocking writes on the auth hot path.
//   - Remove (revoke: unpublish / delete / rotate-old): writes a short-lived
//     TOMBSTONE (not a DEL), with bounded retry + loud-on-failure.
//
// Revocation-resurrection is closed by the tombstone + SETNX pairing: a delayed
// auth-path Warm can no longer re-create a just-revoked key, because Remove left
// a tombstone there and SETNX won't overwrite it (and FindByToken denies on a
// tombstone regardless). The only residual is a failed revocation WRITE on a
// transient Redis error after retries — bounded by the key TTL, and moot when
// Redis is fully down (FindByToken then errors -> DB fallback -> rejected). The
// safety-net TTL (ttl(), clamped) also self-heals any orphaned key.
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
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 2
		o.DialTimeout = 3 * time.Second
		o.ReadTimeout = 2 * time.Second
		o.WriteTimeout = 2 * time.Second
	})
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
	if val == appBotTombstone {
		// Revoked: deny the cache hit and fall through to the DB, which is
		// authoritative and will reject the deleted/unpublished bot.
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

// Add authoritatively write-throughs a spec (publish / rotate-new): an
// unconditional SET that overwrites any tombstone so a re-publish re-enables the
// token cluster-wide at once. Not circuit-gated — it is a low-frequency admin
// write that must establish authoritative state even on a slow backend.
func (r *RedisAppBotRegistry) Add(token string, spec *AppBotRegistrySpec) {
	r.set(token, spec)
}

// Warm is a best-effort cache warm-up (DB-fallback auth-path repopulate + startup
// load). It uses SETNX so it only ever fills an ABSENT key — it never overwrites
// a concurrent revocation's tombstone or a fresher authoritative spec, so a
// delayed warm-up cannot resurrect a just-revoked token. Circuit-gated so a Redis
// outage can't pile up blocking writes on the auth hot path.
func (r *RedisAppBotRegistry) Warm(token string, spec *AppBotRegistrySpec) {
	if token == "" || spec == nil {
		return
	}
	if r.writesDegraded() {
		return
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		r.warnDegraded("app bot auth spec marshal failed", err)
		return
	}
	if err := r.client.SetNX(appBotAuthKey(token), payload, r.safeTTL()).Err(); err != nil {
		r.noteRedisFailure("app bot auth cache warm (SETNX) failed", err)
	}
}

// Remove revokes a token by writing a short-lived TOMBSTONE (not a DEL), so every
// replica denies it immediately AND a racing Warm (SETNX) can't re-create the key.
// Bounded retry + loud-on-failure: a transient blip must not silently leave the
// token authenticating until TTL. Not circuit-gated — a revocation must always be
// attempted, even when the backend is degraded.
func (r *RedisAppBotRegistry) Remove(token string) {
	if token == "" {
		return
	}
	ttl := r.safeTTL()
	var err error
	for attempt := 1; attempt <= appBotRevokeMaxAttempts; attempt++ {
		if err = r.client.Set(appBotAuthKey(token), appBotTombstone, ttl).Err(); err == nil {
			return
		}
	}
	// All attempts failed. When Redis is fully unreachable, FindByToken also errors
	// → DB fallback → the revoked bot is rejected anyway; the only exposed case is a
	// transient failure leaving an earlier spec key readable, bounded by its TTL.
	r.noteRedisFailure("app bot auth cache revoke (tombstone SET) failed after retries; token may auth until key TTL", err)
}

// Update revokes the old token (tombstone) and authoritatively write-throughs the
// new one. The two writes are not atomic, but each is on the shared store so peers
// converge immediately; the new token is brand-new (no tombstone) so the SET wins.
func (r *RedisAppBotRegistry) Update(oldToken, newToken string, spec *AppBotRegistrySpec) {
	r.Remove(oldToken)
	r.set(newToken, spec)
}

// set is the authoritative SET shared by Add and Update's new-token write. It is
// NOT circuit-gated (see Add); a marshal failure is non-availability so it only
// warns. A Redis-command failure trips the circuit (so best-effort Warms back off).
func (r *RedisAppBotRegistry) set(token string, spec *AppBotRegistrySpec) {
	if token == "" || spec == nil {
		return
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		r.warnDegraded("app bot auth spec marshal failed", err)
		return
	}
	if err := r.client.Set(appBotAuthKey(token), payload, r.safeTTL()).Err(); err != nil {
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
