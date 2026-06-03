package incomingwebhook

import (
	"context"
	"time"

	"github.com/go-redis/redis"
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

// allowPerWebhook 按 webhook_id 维度做令牌桶判定，独立于 IP 限流。
// Redis 故障时返回 (true, err)，由调用方决定是否记日志（fail-open）。
func (w *IncomingWebhook) allowPerWebhook(_ context.Context, webhookID string) (bool, error) {
	rps := perWebhookRPS()
	burst := perWebhookBurst()
	if rps <= 0 || burst <= 0 {
		return true, nil
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	script := redis.NewScript(tokenBucketScript)
	res, err := script.Run(w.rateRedis, []string{"ratelimit:incoming_webhook:" + webhookID},
		rps, burst, now).Result()
	if err != nil {
		return true, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		return true, nil
	}
	allowed, _ := arr[0].(int64)
	return allowed == 1, nil
}
