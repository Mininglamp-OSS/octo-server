package resourceshare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	rd "github.com/go-redis/redis"
)

const distributedTargetBucketScript = `
local now = tonumber(ARGV[1])
local states = {}
local retry_after = 0

for index, key in ipairs(KEYS) do
    local offset = 2 + ((index - 1) * 2)
    local rate = tonumber(ARGV[offset])
    local burst = tonumber(ARGV[offset + 1])
    if rate == nil or rate <= 0 or burst == nil or burst < 1 then
        return redis.error_reply("invalid resource share bucket configuration")
    end

    local state = redis.call("HMGET", key, "tokens", "ts")
    local tokens = tonumber(state[1])
    local timestamp = tonumber(state[2])
    if tokens == nil then tokens = burst end
    if timestamp == nil then timestamp = now end

    local elapsed = math.max(0, now - timestamp)
    local filled = math.min(burst, tokens + (elapsed * rate))
    states[index] = {key = key, rate = rate, burst = burst, filled = filled}
    if filled < 1 then
        retry_after = math.max(retry_after, math.max(1, math.ceil((1 - filled) / rate)))
    end
end

-- A denial is read-only. This prevents an exhausted provider/target bucket from
-- spending capacity in the feature-wide bucket and starving unrelated shares.
if retry_after > 0 then
    return {0, retry_after}
end

for _, state in ipairs(states) do
    local remaining = state.filled - 1
    local ttl = math.max(1, math.ceil((state.burst / state.rate) * 2))
    redis.call("HMSET", state.key, "tokens", remaining, "ts", now)
    redis.call("EXPIRE", state.key, ttl)
end

return {1, 0}
`

var limiterKeyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

type TargetLimiterConfig struct {
	KeyPrefix         string
	GlobalBudget      RateBudget
	DMBudget          RateBudget
	ChannelBudget     RateBudget
	FailureRetryAfter time.Duration
	Now               func() time.Time
}

type RedisTargetLimiter struct {
	client            *rd.Client
	script            *rd.Script
	keyPrefix         string
	globalBudget      RateBudget
	dmBudget          RateBudget
	channelBudget     RateBudget
	providerBudgets   map[ProviderID]RateBudget
	failureRetryAfter time.Duration
	now               func() time.Time
}

func NewRedisTargetLimiter(
	client *rd.Client,
	registry *Registry,
	config TargetLimiterConfig,
) (*RedisTargetLimiter, error) {
	if client == nil || registry == nil || !limiterKeyPrefixPattern.MatchString(config.KeyPrefix) ||
		!validRateBudget(config.GlobalBudget) || !validRateBudget(config.DMBudget) ||
		!validRateBudget(config.ChannelBudget) || config.FailureRetryAfter < time.Second ||
		config.FailureRetryAfter > time.Minute {
		return nil, ErrLimitConfig
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	providerBudgets := make(map[ProviderID]RateBudget, len(registry.providers))
	for id, provider := range registry.providers {
		if !provider.spec.Enabled {
			continue
		}
		budget := provider.spec.Limits.TargetBudget
		if !validRateBudget(budget) || budget.RatePerSecond > config.GlobalBudget.RatePerSecond ||
			budget.Burst > config.GlobalBudget.Burst {
			return nil, fmt.Errorf("%w: provider=%q traffic budget exceeds platform", ErrLimitConfig, id)
		}
		providerBudgets[id] = budget
	}
	return &RedisTargetLimiter{
		client: client, script: rd.NewScript(distributedTargetBucketScript), keyPrefix: config.KeyPrefix,
		globalBudget: config.GlobalBudget, dmBudget: config.DMBudget, channelBudget: config.ChannelBudget,
		providerBudgets: providerBudgets, failureRetryAfter: config.FailureRetryAfter, now: now,
	}, nil
}

func (l *RedisTargetLimiter) Allow(ctx context.Context, request LimitRequest) (LimitDecision, error) {
	failure := l.failureDecision()
	if l == nil || l.client == nil || l.script == nil || l.now == nil {
		return failure, ErrLimitConfig
	}
	if ctx == nil {
		return failure, fmt.Errorf("%w: context unavailable", ErrLimitStore)
	}
	if err := ctx.Err(); err != nil {
		return failure, err
	}
	providerBudget, ok := l.providerBudgets[request.ProviderID]
	if !ok || !validIdentifier(request.ActorUID, 1, maxActorUIDBytes) ||
		!validIdentifier(request.SpaceID, 1, maxSpaceIDBytes) {
		return failure, ErrLimitConfig
	}
	targetKey, targetBudget, err := l.targetBucket(request)
	if err != nil {
		return failure, err
	}
	now := l.now()
	if now.IsZero() {
		return failure, fmt.Errorf("%w: clock unavailable", ErrLimitStore)
	}
	keys := []string{
		l.keyPrefix + "global",
		l.keyPrefix + "provider:" + string(request.ProviderID),
		targetKey,
	}
	args := []interface{}{
		float64(now.UnixNano()) / float64(time.Second),
		l.globalBudget.RatePerSecond, l.globalBudget.Burst,
		providerBudget.RatePerSecond, providerBudget.Burst,
		targetBudget.RatePerSecond, targetBudget.Burst,
	}
	result, err := l.script.Run(l.client, keys, args...).Result()
	if err != nil {
		return failure, fmt.Errorf("%w: %v", ErrLimitStore, err)
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return failure, fmt.Errorf("%w: unexpected script result", ErrLimitStore)
	}
	allowed, allowedOK := values[0].(int64)
	retryAfter, retryOK := values[1].(int64)
	if !allowedOK || !retryOK || (allowed != 0 && allowed != 1) || retryAfter < 0 {
		return failure, fmt.Errorf("%w: malformed script result", ErrLimitStore)
	}
	if allowed == 1 {
		return LimitDecision{Allowed: true}, nil
	}
	return LimitDecision{RetryAfter: clampRetryAfter(time.Duration(retryAfter) * time.Second)}, nil
}

func (l *RedisTargetLimiter) failureDecision() LimitDecision {
	if l == nil || l.failureRetryAfter < time.Second || l.failureRetryAfter > time.Minute {
		return LimitDecision{RetryAfter: time.Second}
	}
	return LimitDecision{RetryAfter: l.failureRetryAfter}
}

func (l *RedisTargetLimiter) targetBucket(request LimitRequest) (string, RateBudget, error) {
	canonical, err := canonicalTargetKey(request.ActorUID, request.Target)
	if err != nil {
		return "", RateBudget{}, fmt.Errorf("%w: target invalid", ErrLimitConfig)
	}
	fields := []string{request.SpaceID, canonical}
	prefix := "channel:"
	budget := l.channelBudget
	if request.Target.Kind == TargetDM {
		fields = append([]string{request.SpaceID, request.ActorUID}, canonical)
		prefix = "dm:"
		budget = l.dmBudget
	}
	digest := sha256.New()
	for _, field := range fields {
		writeDigestField(digest, field)
	}
	return l.keyPrefix + prefix + hex.EncodeToString(digest.Sum(nil)), budget, nil
}

func clampRetryAfter(retryAfter time.Duration) time.Duration {
	if retryAfter < time.Second {
		return time.Second
	}
	if retryAfter > time.Minute {
		return time.Minute
	}
	return retryAfter
}
