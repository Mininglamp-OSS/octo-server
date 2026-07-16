package messages_search

import (
	"sort"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"golang.org/x/time/rate"
)

// uidLimiterMaxBuckets caps how many distinct uids the in-memory limiter will
// hold. Past this size we run an aggressive sweep and, if still over, evict
// the oldest entries by `seen` time. Without the cap a tenant cycling many
// UIDs (e.g. a credential-stuffing attempt) could grow the map without bound
// because the time-based GC requires entries to age 30+ minutes.
const uidLimiterMaxBuckets = 10_000

// uidLimiter is a per-loginUID 5 QPS / 20 burst token bucket layered on top of
// the broader per-IP / per-UID limits enforced upstream by SharedUIDRateLimiter.
//
// We keep this local because the search workload has a different cost profile:
// each request triggers an OS query plus a DB JOIN, so a tight ceiling here
// protects the search backend even when the global UID bucket has plenty of
// quota left.
//
// Implementation: golang.org/x/time/rate.Limiter, indexed by uid. Stale
// entries are cleared by a lazy sweep on Allow rather than a background
// goroutine so the limiter has no lifecycle to manage.
type uidLimiter struct {
	rps     rate.Limit
	burst   int
	mu      sync.Mutex
	buckets map[string]*uidBucket
	last    time.Time
}

type uidBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func newUIDLimiter(rps float64, burst int) *uidLimiter {
	if rps <= 0 {
		rps = 5
	}
	if burst <= 0 {
		burst = 20
	}
	return &uidLimiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		buckets: make(map[string]*uidBucket),
		last:    time.Now(),
	}
}

// Allow returns true if the bucket key is within budget; false otherwise.
// The key is normally the loginUID; the middleware substitutes an ip:-prefixed
// key when the uid is missing. An empty key fails closed — AuthMiddleware
// mounts before us so it should never happen, and silently exempting an
// unidentifiable caller from the search budget is the wrong default.
func (l *uidLimiter) Allow(key string) bool {
	if key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &uidBucket{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	if now.Sub(l.last) > 5*time.Minute || len(l.buckets) > uidLimiterMaxBuckets {
		l.sweepLocked(now)
		l.last = now
	}
	return b.limiter.Allow()
}

// sweepLocked GCs stale buckets and, if the map still exceeds the cap,
// evicts the oldest entries by `seen` time. Caller must hold l.mu.
func (l *uidLimiter) sweepLocked(now time.Time) {
	for k, v := range l.buckets {
		if now.Sub(v.seen) > 30*time.Minute {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) <= uidLimiterMaxBuckets {
		return
	}
	// Hard-cap path: build a slice of (uid, seen) pairs, sort by seen, and
	// drop the oldest entries until we're below the cap. O(n log n) is fine
	// here — this branch only runs when we've already exceeded 10K uids and
	// the per-call cost amortises to a single sweep until the next blow-up.
	type entry struct {
		uid  string
		seen time.Time
	}
	entries := make([]entry, 0, len(l.buckets))
	for k, v := range l.buckets {
		entries = append(entries, entry{uid: k, seen: v.seen})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].seen.Before(entries[j].seen)
	})
	excess := len(l.buckets) - uidLimiterMaxBuckets
	for i := 0; i < excess && i < len(entries); i++ {
		delete(l.buckets, entries[i].uid)
	}
}

// searchRateLimiter is the gin middleware factory that enforces the per-uid
// search budget. Errors emit RATE_LIMITED with a soft retry-after hint of one
// second (the bucket refills at >= 5 RPS).
//
// AuthMiddleware mounts before this, so uid is normally always present; if it
// ever is not, the bucket falls back to the client IP rather than waving the
// request through unmetered.
func (h *Handler) searchRateLimiter() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		// 限流键走 principal（决策十）：真人=自身 uid（现状不变）；as-bot / obo 都按
		// botUID 计（防单 bot 打爆）；uk 按 key UID。
		key := h.principal(c).RateLimitKey()
		if key == "" {
			key = "ip:" + c.ClientIP()
		}
		if !h.limiter.Allow(key) {
			respondRateLimited(c, 1)
			c.Abort()
			return
		}
		c.Next()
	}
}
