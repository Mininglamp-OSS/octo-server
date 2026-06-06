package incomingwebhook

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/go-redis/redis"
	"go.uber.org/zap"
)

// tokenBucketScript 与 octo-lib pkg/wkhttp/ratelimit.go 中的脚本同形，单独维护一份是为了
// 让 incoming webhook 的限流键空间独立、并允许后续按需调优配额而不牵连其他端点。
const tokenBucketScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

if rate <= 0 then return {0, 0, 1} end

local fill_time = burst / rate
local ttl = math.max(1, math.ceil(fill_time * 2))

local state = redis.call("HMGET", key, "tokens", "ts")
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then tokens = burst end
if ts == nil then ts = now end

local delta = math.max(0, now - ts)
local filled = math.min(burst, tokens + delta * rate)

local allowed = 0
local retry_after = 0
local need_write = false
if filled >= 1 then
    allowed = 1
    filled = filled - 1
    need_write = true
else
    retry_after = math.max(1, math.ceil((1 - filled) / rate))
    if state[2] == false then
        need_write = true
    end
end

if need_write then
    redis.call("HMSET", key, "tokens", filled, "ts", now)
    redis.call("EXPIRE", key, ttl)
end

return {allowed, math.floor(filled), retry_after}
`

// ipFailureBucketScript is a per-IP "auth-failure budget" token bucket whose
// tokens are spent by the CALLER, not on every request. The push gate PEEKS
// (spend=0) to reject an IP that has already burned its budget; each genuine
// auth failure SPENDS (spend=1) one token. Valid pushes never spend, so a
// fixed/shared server IP is not throttled by request volume — only sustained
// scanning (repeated auth failures) from an IP exhausts the budget and gets
// gated. It deliberately shares the same refill math as tokenBucketScript but
// stays a separate script: tokenBucketScript always consumes, this one's spend
// is caller-controlled, and folding both into one parametrized script would add
// peek/spend branching to the per-webhook limiter for no benefit.
const ipFailureBucketScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local spend = tonumber(ARGV[4])

if rate <= 0 or burst <= 0 then return {1} end

local state = redis.call("HMGET", key, "tokens", "ts")
local existed = state[1] ~= false
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then tokens = burst end
if ts == nil then ts = now end

local delta = math.max(0, now - ts)
tokens = math.min(burst, tokens + delta * rate)

local allowed = 0
if tokens >= 1 then allowed = 1 end

local ttl = math.max(1, math.ceil((burst / rate) * 2))
if spend == 1 then
    -- an auth failure: deduct a token and (re)arm the key TTL.
    if tokens >= 1 then tokens = tokens - 1 end
    redis.call("HMSET", key, "tokens", tokens, "ts", now)
    redis.call("EXPIRE", key, ttl)
elseif existed then
    -- read-only peek on an existing (draining/throttled) key: refresh its TTL so
    -- an actively-probing scanner cannot wait the key out and reset to a full
    -- budget. The token/ts state is left untouched, so refill stays correct
    -- (computed from the last spend's ts); legit IPs never created a key, so
    -- this stays write-free for them.
    redis.call("EXPIRE", key, ttl)
end

return {allowed}
`

// Compile the Lua scripts once (caches the SHA1) instead of per request.
var (
	perWebhookScript = redis.NewScript(tokenBucketScript)
	ipFailureScript  = redis.NewScript(ipFailureBucketScript)
)

// runBucketScript runs a token-bucket Lua script and returns whether the call is
// allowed (return value [0] == 1). A Redis failure yields (true, err) so every
// caller fails open.
func (w *IncomingWebhook) runBucketScript(script *redis.Script, key string, args ...interface{}) (bool, error) {
	res, err := script.Run(w.rateRedis, []string{key}, args...).Result()
	if err != nil {
		return true, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 1 {
		// Unexpected return shape from the Lua script — fail open, but log it
		// (throttled): on security-sensitive limiter code a silently-swallowed
		// malformed reply would hide a real bug behind "always allowed".
		w.warnDegraded("rate limit script returned unexpected shape, fail-open",
			fmt.Errorf("result type %T", res))
		return true, nil
	}
	allowed, _ := arr[0].(int64)
	return allowed == 1, nil
}

// allowPerWebhook 按 webhook_id 维度做令牌桶判定，独立于 IP 限流。
// Redis 故障时返回 (true, err)，由调用方决定是否记日志（fail-open）。
func (w *IncomingWebhook) allowPerWebhook(_ context.Context, webhookID string) (bool, error) {
	rps := w.settings.IncomingWebhookPerWebhookRPS()
	burst := w.settings.IncomingWebhookPerWebhookBurst()
	if rps <= 0 || burst <= 0 {
		return true, nil
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	return w.runBucketScript(perWebhookScript, "ratelimit:incoming_webhook:"+webhookID, rps, burst, now)
}

// unknownIPKey buckets requests whose client IP could not be resolved, so they
// share one auth-failure budget (fail-closed) rather than being silently exempt.
const unknownIPKey = "__unknown_ip__"

func ipFailureKey(ip string) string {
	if ip == "" {
		ip = unknownIPKey
	}
	return "ratelimit:incoming_webhook_ipfail:" + ip
}

// clientIP resolves the caller IP the same way octo-lib's limiters do: X-Real-Ip,
// then the RIGHTMOST X-Forwarded-For entry (the value a trusted reverse proxy /
// CLB appends — clients can prepend fake hops but not control the rightmost),
// then RemoteAddr. Using gin's c.ClientIP() here would be WRONG: wkhttp builds
// gin with the default trust-all-proxies config, so c.ClientIP() returns the
// leftmost (client-controlled, spoofable) XFF entry, letting a scanner forge a
// new IP per request and evade the failure budget entirely.
//
// ⚠️ Trust assumption (same as octo-lib): the edge proxy must overwrite
// X-Real-Ip / append the real client to X-Forwarded-For.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if ip, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return ip
	}
	return ""
}

// ipFailureBudgetOK peeks (no spend) whether the IP still has auth-failure
// budget. A disabled budget always passes; an empty IP shares the unknown bucket.
func (w *IncomingWebhook) ipFailureBudgetOK(ip string) (bool, error) {
	rps, burst := ipFailRPS(), ipFailBurst()
	if rps <= 0 || burst <= 0 {
		return true, nil
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	return w.runBucketScript(ipFailureScript, ipFailureKey(ip), rps, burst, now, 0)
}

// penalizeIPFailure spends one token from the IP's auth-failure budget. Called
// only on genuine auth-failure signals (unknown webhook / bad token / malformed
// request), never on valid pushes or server-side (DB) errors. Best-effort: a
// Redis failure is logged (throttled) and ignored (fail-open).
func (w *IncomingWebhook) penalizeIPFailure(ip string) {
	rps, burst := ipFailRPS(), ipFailBurst()
	if rps <= 0 || burst <= 0 {
		return
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	if _, err := w.runBucketScript(ipFailureScript, ipFailureKey(ip), rps, burst, now, 1); err != nil {
		w.warnDegraded("penalize ip failure budget redis failed, ignoring", err)
	}
}

// ipFailureGateMiddleware rejects (429) requests from an IP that has burned its
// auth-failure budget, before the handler runs — so a token-scanning IP stops
// reaching the DB and the per-webhook limiter once cut off. Sits after the floor
// and per-IP request limiter (which it cannot un-consume), but with a lower,
// failure-specific budget it cuts a scanner faster and without charging valid
// traffic. Read-only peek; fail-open on Redis error. On pass it calls c.Next().
func (w *IncomingWebhook) ipFailureGateMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		ok, err := w.ipFailureBudgetOK(clientIP(c.Request))
		if err != nil {
			w.warnDegraded("ip failure budget peek redis failed, fail-open", err)
			ok = true
		}
		if !ok {
			pushRateLimited(c)
			return
		}
		c.Next()
	}
}

// degradeWarnInterval throttles fail-open warnings from the Redis-backed
// limiters so a sustained Redis outage on the push hot path logs at most once
// per interval instead of once per request (mirrors octo-lib keyedLimiter's
// logDegrade).
const degradeWarnInterval = 30 * time.Second

var (
	degradeWarnMu   sync.Mutex
	degradeWarnLast time.Time
)

func (w *IncomingWebhook) warnDegraded(msg string, err error) {
	degradeWarnMu.Lock()
	if !degradeWarnLast.IsZero() && time.Since(degradeWarnLast) < degradeWarnInterval {
		degradeWarnMu.Unlock()
		return
	}
	degradeWarnLast = time.Now()
	degradeWarnMu.Unlock()
	w.Warn(msg, zap.Error(err))
}
