package incomingwebhook

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"golang.org/x/time/rate"
)

// Redis-independent, process-local rate ceiling for the public push endpoint.
//
// `POST /v1/incoming-webhooks/:webhook_id/:token` is the only unauthenticated,
// publicly reachable route in this module. Its per-IP and per-webhook limiters
// are both Redis-backed and fail-open: when Redis is unavailable (or its
// connection pool is saturated) they silently stop limiting, leaving the
// endpoint — which still performs 2 DB reads + 1 WuKongIM send per call — open
// to flood amplification. webhook_id (128-bit) and token (256-bit) make
// enumeration infeasible regardless, so the real exposure is a flood, not token
// theft. This in-memory token bucket sits in FRONT of the Redis limiters as an
// always-on floor, so a single instance keeps a bounded push rate even with
// Redis down, and a flood is shed before it ever touches Redis.
//
// Defaults are deliberately generous (200 rps / 400 burst per instance): under
// healthy Redis the per-IP request limiter (100 rps) and per-webhook limiter
// (5 rps) are lower and bite first, so the floor only engages as a backstop.
// Tunable via
// DM_INCOMINGWEBHOOK_LOCAL_RPS / _BURST, read once at construction (no hot
// reload, matching octo-lib's limiter middlewares). Fail-safe: the shared env
// parsers coerce 0 / negative / unparseable values to the generous default, so
// the floor CANNOT be silently switched off via env — loosen it with a high
// rps/burst instead.
const (
	envLocalFloorRPS   = "DM_INCOMINGWEBHOOK_LOCAL_RPS"
	envLocalFloorBurst = "DM_INCOMINGWEBHOOK_LOCAL_BURST"

	defaultLocalFloorRPS   = 200.0
	defaultLocalFloorBurst = 400
)

// localFloor is a per-instance in-memory token bucket, built once from env at
// construction. New() builds a fresh floor per IncomingWebhook (one per process
// in production, one per test server), so the env is read at the same point as
// the module's other startup config.
type localFloor struct {
	lim *rate.Limiter // nil = disabled
}

func newLocalFloor() *localFloor {
	rps := wkhttp.ParseRPSFromEnv(envLocalFloorRPS, defaultLocalFloorRPS)
	burst := wkhttp.ParseBurstFromEnv(envLocalFloorBurst, defaultLocalFloorBurst)
	// Defensive: env "0"/invalid already coerces to a positive default, so this
	// only fires if a default *constant* is ever set <=0. Without it,
	// rate.NewLimiter(0, ...) would build a deny-all bucket and the floor would
	// reject every push — far worse than being off. Treat <=0 as disabled.
	if rps <= 0 || burst <= 0 {
		return &localFloor{}
	}
	return &localFloor{lim: rate.NewLimiter(rate.Limit(rps), burst)}
}

// allow reports whether a push may proceed. f.lim is immutable after
// construction and rate.Limiter.Allow() is internally synchronized, so allow()
// needs no extra locking; a nil limiter (disabled) always allows.
func (f *localFloor) allow() bool {
	if f.lim == nil {
		return true
	}
	return f.lim.Allow()
}

// localFloorMiddleware enforces the Redis-independent per-instance push ceiling
// ahead of the Redis-backed IP limiter, so a flood is shed in-memory without
// even reaching Redis. On rejection it returns the same i18n 429 as the
// per-webhook limiter (pushRateLimited aborts the chain); on pass it calls
// c.Next() to continue to the next handler, mirroring StrictIPRateLimitMiddleware.
func (w *IncomingWebhook) localFloorMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if !w.floor.allow() {
			pushRateLimited(c)
			return
		}
		c.Next()
	}
}
