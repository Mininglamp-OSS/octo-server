package incomingwebhook

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"golang.org/x/time/rate"
)

// Redis-independent, process-local rate ceiling for the public push endpoint.
//
// `POST /v1/incoming-webhooks/:webhook_id/:token` (and its shorter alias
// `/v1/webhooks/:webhook_id/:token`, #455) are the only unauthenticated,
// publicly reachable routes in this module. Their per-IP and per-webhook limiters
// are both Redis-backed and fail-open: when Redis is unavailable (or its
// connection pool is saturated) they silently stop limiting, leaving the
// endpoint — which still performs 2 DB reads + 1 WuKongIM send per call — open
// to flood amplification. webhook_id (128-bit) and token (256-bit) make
// enumeration infeasible regardless, so the real exposure is a flood, not token
// theft. This in-memory token bucket sits in FRONT of the Redis limiters as an
// always-on floor, so a single instance keeps a bounded push rate even with
// Redis down, and a flood is shed before it ever touches Redis.
//
// The floor has TWO Redis-independent layers, checked per request in order:
//
//  1. a per-IP in-memory token bucket (perIPFloor, 100 rps / 200 burst), and
//  2. a global per-instance token bucket (200 rps / 400 burst).
//
// The per-IP layer is checked FIRST so a single abusive IP can consume at most
// its per-IP share of the global floor: without it, the global bucket is a
// single shared resource that one IP (bounded only by the ~500 rps/IP DDoS
// limiter in main.go) can drain, causing collateral 429s for valid pushes from
// other IPs (issue #287). The global layer still bounds a DISTRIBUTED flood
// (many IPs) under a Redis outage, which is the instance cap's whole point —
// a pure per-IP floor would lose that.
//
// Defaults are deliberately generous so the floor stays a Redis-outage backstop
// rather than the binding cap under healthy Redis: the global layer (200 rps) is
// above the Redis per-IP limiter (100 rps), and the per-IP layer is set EQUAL to
// that Redis per-IP limiter (100 rps / 200 burst) — not below it — so it does not
// narrow a legitimate high-volume IP's allowance while Redis is healthy (the
// Redis limiter and this floor bite at the same per-IP threshold), yet it is half
// the global floor so a single IP can still take at most ~half and never starve
// others. The per-webhook limiter (5 rps) is lower still and shapes legit traffic
// first. Tunable via
// DM_INCOMINGWEBHOOK_LOCAL_RPS / _BURST (global) and
// DM_INCOMINGWEBHOOK_LOCAL_PERIP_RPS / _BURST / _MAX_IPS (per-IP), read once at
// construction (no hot reload, matching octo-lib's limiter middlewares).
// Fail-safe: the shared env parsers coerce 0 / negative / unparseable values to
// the generous default, so neither layer can be silently switched off via env —
// loosen it with a high rps/burst instead.
const (
	envLocalFloorRPS   = "DM_INCOMINGWEBHOOK_LOCAL_RPS"
	envLocalFloorBurst = "DM_INCOMINGWEBHOOK_LOCAL_BURST"

	envLocalFloorPerIPRPS   = "DM_INCOMINGWEBHOOK_LOCAL_PERIP_RPS"
	envLocalFloorPerIPBurst = "DM_INCOMINGWEBHOOK_LOCAL_PERIP_BURST"
	envLocalFloorMaxIPs     = "DM_INCOMINGWEBHOOK_LOCAL_MAX_IPS"

	defaultLocalFloorRPS   = 200.0
	defaultLocalFloorBurst = 400

	// Per-IP sub-limit: a single IP gets at most 100 rps / 200 burst of the
	// global floor, so one IP cannot starve others. Set EQUAL to the Redis per-IP
	// StrictIPRateLimitMiddleware (defaultIngressIPRPS/Burst, 100/200) so under
	// healthy Redis this always-on floor does not throttle a legit high-volume IP
	// any tighter than the Redis limiter already would; it is still half the
	// global cap, so several distinct IPs coexist under the global ceiling and no
	// single IP drains it. Keep these two in lockstep if either is retuned.
	defaultLocalFloorPerIPRPS   = 100.0
	defaultLocalFloorPerIPBurst = 200
	// Hard cap on the number of distinct per-IP buckets kept in memory, so a
	// Redis-down DISTRIBUTED flood (many source IPs) cannot grow the map without
	// bound. At the cap the oldest generation of buckets is dropped wholesale
	// (see perIPFloor); evicted IPs simply start from a full bucket again, which
	// is harmless because the global floor still bounds total throughput.
	defaultLocalFloorMaxIPs = 100000
)

// localFloor is the per-instance Redis-independent push ceiling, built once from
// env at construction. New() builds a fresh floor per IncomingWebhook (one per
// process in production, one per test server), so the env is read at the same
// point as the module's other startup config.
type localFloor struct {
	lim   *rate.Limiter // global instance cap; nil = disabled
	perIP *perIPFloor   // per-IP sub-limit; nil = disabled
}

func newLocalFloor() *localFloor {
	f := &localFloor{}

	rps := wkhttp.ParseRPSFromEnv(envLocalFloorRPS, defaultLocalFloorRPS)
	burst := wkhttp.ParseBurstFromEnv(envLocalFloorBurst, defaultLocalFloorBurst)
	// Defensive: env "0"/invalid already coerces to a positive default, so this
	// only fires if a default *constant* is ever set <=0. Without it,
	// rate.NewLimiter(0, ...) would build a deny-all bucket and the floor would
	// reject every push — far worse than being off. Treat <=0 as disabled.
	if rps > 0 && burst > 0 {
		f.lim = rate.NewLimiter(rate.Limit(rps), burst)
	}

	perIPRPS := wkhttp.ParseRPSFromEnv(envLocalFloorPerIPRPS, defaultLocalFloorPerIPRPS)
	perIPBurst := wkhttp.ParseBurstFromEnv(envLocalFloorPerIPBurst, defaultLocalFloorPerIPBurst)
	maxIPs := wkhttp.ParseBurstFromEnv(envLocalFloorMaxIPs, defaultLocalFloorMaxIPs)
	if perIPRPS > 0 && perIPBurst > 0 && maxIPs > 0 {
		f.perIP = newPerIPFloor(rate.Limit(perIPRPS), perIPBurst, maxIPs)
	}

	return f
}

// allow reports whether a push from ip may proceed. The per-IP sub-limit is
// checked FIRST (so a gated IP never even spends a global token, preventing
// single-IP starvation of the shared global bucket), then the global instance
// cap. Both limiters are internally synchronized / lock their own state, so
// allow() needs no extra locking; a nil layer is disabled and always allows.
func (f *localFloor) allow(ip string) bool {
	if f.perIP != nil && !f.perIP.allow(ip) {
		return false
	}
	if f.lim == nil {
		return true
	}
	return f.lim.Allow()
}

// perIPUnknownKey buckets requests whose client IP could not be resolved into
// one shared per-IP bucket, so an unresolved-IP flood is throttled together
// rather than each getting its own fresh bucket (which would defeat the cap).
const perIPUnknownKey = "__unknown_ip__"

// perIPFloor is a Redis-independent, memory-bounded set of per-IP token
// buckets. Memory is capped WITHOUT a background goroutine using a
// two-generation map: lookups hit `cur` first, then promote from `prev`; when
// `cur` fills to maxEntries — on BOTH the new-IP and the promotion paths — the
// generations rotate (prev := cur, cur := empty), dropping the coldest buckets
// wholesale. Enforcing the cap on promotion too is what keeps the bound real: a
// cycling attacker who re-touches every `prev` entry cannot otherwise inflate
// `cur` past the cap each round. This bounds live buckets to ~2*maxEntries; an
// actively-pushing IP survives at least one rotation (promoted out of `prev`)
// but is not a permanent survivor — acceptable because the per-IP layer only
// needs to stop single-IP starvation, while the global floor remains the
// throughput bound under a distributed flood.
type perIPFloor struct {
	mu         sync.Mutex
	rps        rate.Limit
	burst      int
	maxEntries int
	cur        map[string]*rate.Limiter
	prev       map[string]*rate.Limiter
}

func newPerIPFloor(rps rate.Limit, burst, maxEntries int) *perIPFloor {
	return &perIPFloor{
		rps:        rps,
		burst:      burst,
		maxEntries: maxEntries,
		cur:        make(map[string]*rate.Limiter),
	}
}

// limiterFor returns the (lazily created) bucket for ip, promoting it into the
// current generation so active IPs survive rotation.
func (p *perIPFloor) limiterFor(ip string) *rate.Limiter {
	if ip == "" {
		ip = perIPUnknownKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if lim, ok := p.cur[ip]; ok {
		return lim
	}
	if lim, ok := p.prev[ip]; ok {
		// Promote the survivor into the current generation so a steadily-pushing
		// IP survives AT LEAST one rotation. The cap MUST be enforced here too:
		// without it, an attacker who keeps prev warm could promote every prev
		// entry back into cur each cycle (cur→2N, then rotate prev:=cur→2N, …),
		// growing the live set without bound and defeating the memory cap. So if
		// cur is already full, rotate first — dropping the rest of the cold prev
		// generation wholesale — before promoting into a fresh cur. A promoted
		// survivor can therefore itself be dropped on the NEXT rotation; that is
		// acceptable: the global floor still bounds throughput, the per-IP layer
		// only needs to stop single-IP starvation, not be an exact LRU.
		p.rotateIfFullLocked()
		p.cur[ip] = lim
		delete(p.prev, ip)
		return lim
	}
	// New IP: rotate generations first if the current one is full, dropping the
	// previous (coldest) generation wholesale to keep memory bounded.
	p.rotateIfFullLocked()
	lim := rate.NewLimiter(p.rps, p.burst)
	p.cur[ip] = lim
	return lim
}

// rotateIfFullLocked rotates the generations (prev := cur, cur := empty) when cur
// has reached the cap, dropping the coldest generation wholesale. Caller must
// hold p.mu. The fresh cur is allocated WITHOUT a size hint so it grows lazily,
// matching newPerIPFloor's initial map: a `make(map, maxEntries)` here would do a
// multi-MB pre-allocation inside the hot-path mutex on every rotation — and
// rotations are triggered precisely by the distinct-IP flood the floor exists to
// shed, so an attacker could force back-to-back large allocations under the lock.
func (p *perIPFloor) rotateIfFullLocked() {
	if len(p.cur) >= p.maxEntries {
		p.prev = p.cur
		p.cur = make(map[string]*rate.Limiter)
	}
}

func (p *perIPFloor) allow(ip string) bool {
	return p.limiterFor(ip).Allow()
}

// localFloorMiddleware enforces the Redis-independent per-instance push ceiling
// (per-IP sub-limit + global cap) ahead of the Redis-backed IP limiter, so a
// flood is shed in-memory without even reaching Redis. It keys the per-IP layer
// on the same trusted clientIP() the Redis failure budget uses (X-Real-Ip /
// rightmost XFF), NOT gin's spoofable c.ClientIP(), so a scanner cannot forge a
// new IP per request to dodge its per-IP share. On rejection it returns the same
// i18n 429 as the per-webhook limiter (pushRateLimited aborts the chain); on
// pass it calls c.Next() to continue, mirroring StrictIPRateLimitMiddleware.
func (w *IncomingWebhook) localFloorMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if !w.floor.allow(clientIP(c.Request)) {
			pushRateLimited(c)
			return
		}
		c.Next()
	}
}
