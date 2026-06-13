package user

import (
	"context"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/cache"
)

// RoleCacheKeyPrefix is the Redis key prefix for the `user_role:{uid}` hot
// cache. Exported so ops scripts / role-mutation sites can invalidate the key
// without duplicating the literal.
const RoleCacheKeyPrefix = "user_role:"

// RoleCacheTTL bounds how long a stale system role can survive after a
// privilege change. The token used to bake the role in for its full lifetime
// (days); resolving per request against this cache caps that staleness to the
// TTL while keeping the auth hot path off MySQL for the common case. Kept
// deliberately short because this gates admin / superAdmin access.
const RoleCacheTTL = 60 * time.Second

// roleNegativeMarker is the sentinel stored when the DB role is empty (a
// normal user — the overwhelming majority). It lets Resolve distinguish a
// cache miss ("" → query DB) from a confirmed "no role" (negative hit → skip
// DB), so the hot path does not hammer MySQL for every normal-user request.
// A bare '-' is unambiguous because it is not a valid role string.
//
// IMPORTANT for future authors: any code path that MUTATES user.role on an
// *existing* uid (today only addAdminUser, which writes role on a brand-new
// uid, and deleteAdminUsers, which calls Invalidate) MUST call Invalidate(uid)
// afterwards. Otherwise a cached negative marker (or a stale positive role)
// suppresses the change for up to RoleCacheTTL.
const roleNegativeMarker = "-"

// roleReader is the read surface RoleService needs from the user DB layer,
// defined consumer-side so tests can stub it without dbr. *DB satisfies it.
type roleReader interface {
	QueryRoleByUID(uid string) (string, error)
}

// RoleService resolves the authoritative system role for a user. It satisfies
// pkg/auth.RoleResolver, consumed by CacheTokenParser so that a role baked
// into a token at login no longer outlives a demotion until token expiry.
//
// Lookup order: Redis `user_role:{uid}` → DB `user.role` → "". DB results are
// written back to Redis (empty stored as a negative marker) so subsequent
// requests for normal users don't touch MySQL. The TTL alone bounds staleness;
// callers that demote/remove an admin should additionally invalidate the hot
// key (Invalidate) for immediate effect.
type RoleService struct {
	db    roleReader
	cache cache.Cache
	ttl   time.Duration
}

// NewRoleService constructs a RoleService with the canonical TTL. cache and db
// must be non-nil; nil is a programmer error and panics on construction rather
// than silently degrading at request time.
func NewRoleService(db roleReader, c cache.Cache) *RoleService {
	if db == nil {
		panic("user: NewRoleService requires non-nil db reader")
	}
	if c == nil {
		panic("user: NewRoleService requires non-nil cache")
	}
	return &RoleService{db: db, cache: c, ttl: RoleCacheTTL}
}

// ResolveRole returns the user's current system role, or "" if none.
// Errors are surfaced to the caller; pkg/auth.CacheTokenParser keeps the token
// snapshot on resolver error so a cache/DB outage does not fail authentication.
func (s *RoleService) ResolveRole(ctx context.Context, uid string) (string, error) {
	if uid == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := RoleCacheKeyPrefix + uid
	cached, cacheErr := s.cache.Get(key)
	if cacheErr == nil {
		switch cached {
		case "":
			// cache miss → fall through to DB
		case roleNegativeMarker:
			return "", nil
		default:
			return cached, nil
		}
	}

	role, dbErr := s.db.QueryRoleByUID(uid)
	if dbErr != nil {
		return "", fmt.Errorf("user: read role from db: %w", dbErr)
	}
	s.writeCache(key, role)
	return role, nil
}

// Invalidate drops the hot-cache entry for a user so the next request re-reads
// the role from DB. Call after any mutation of user.role (e.g. removing an
// admin) to make the change take effect within one round-trip instead of
// waiting out RoleCacheTTL. Best-effort: a Redis error degrades to TTL-bounded
// staleness, not a failure.
func (s *RoleService) Invalidate(uid string) {
	if uid == "" {
		return
	}
	_ = s.cache.Delete(RoleCacheKeyPrefix + uid)
}

func (s *RoleService) writeCache(key, role string) {
	value := role
	if value == "" {
		value = roleNegativeMarker
	}
	// Best-effort write — a Redis outage degrades to "no cache", not a
	// request failure.
	_ = s.cache.SetAndExpire(key, value, s.ttl)
}
