package resourceshare

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func limiterConfig(prefix string, now time.Time) TargetLimiterConfig {
	return TargetLimiterConfig{
		KeyPrefix:         prefix,
		GlobalBudget:      RateBudget{RatePerSecond: 100, Burst: 100},
		DMBudget:          RateBudget{RatePerSecond: 1, Burst: 1},
		ChannelBudget:     RateBudget{RatePerSecond: 1, Burst: 1},
		FailureRetryAfter: 10 * time.Second,
		Now:               func() time.Time { return now },
	}
}

func limiterRegistry(t *testing.T, budgets map[ProviderID]RateBudget) *Registry {
	t.Helper()
	key := newIntentTestKey(t)
	specs := make([]ProviderSpec, 0, len(budgets))
	for id, budget := range budgets {
		spec := validProviderSpec(t, key)
		spec.ID = id
		spec.ResourceType = string(id)
		spec.Issuer = "https://" + string(id) + ".internal"
		spec.Limits.TargetBudget = budget
		specs = append(specs, spec)
	}
	registry, err := NewRegistry(specs)
	require.NoError(t, err)
	return registry
}

func testLimiterRedis(t *testing.T) *rd.Client {
	t.Helper()
	client := rd.NewClient(&rd.Options{
		Addr:         "127.0.0.1:6399",
		MaxRetries:   1,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("testenv redis unavailable at 127.0.0.1:6399: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func uniqueLimiterPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test:resource-share:%d:", time.Now().UnixNano())
}

func cleanupLimiterKeys(t *testing.T, client *rd.Client, prefix string) {
	t.Helper()
	keys, err := client.Keys(prefix + "*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, client.Del(keys...).Err())
	}
}

func TestNewRedisTargetLimiter_RejectsInvalidOrUnboundedConfiguration(t *testing.T) {
	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6399"})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Unix(1_800_000_000, 0).UTC()
	registry := limiterRegistry(t, map[ProviderID]RateBudget{
		"smart-summary": {RatePerSecond: 10, Burst: 20},
	})

	tests := []struct {
		name     string
		client   *rd.Client
		registry *Registry
		mutate   func(*TargetLimiterConfig)
	}{
		{name: "missing redis", registry: registry},
		{name: "missing registry", client: client},
		{name: "missing key prefix", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.KeyPrefix = "" }},
		{name: "unsafe key prefix", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.KeyPrefix = "resource share raw:" }},
		{name: "zero global rate", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.GlobalBudget.RatePerSecond = 0 }},
		{name: "zero dm burst", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.DMBudget.Burst = 0 }},
		{name: "zero channel rate", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.ChannelBudget.RatePerSecond = 0 }},
		{name: "unbounded failure retry", client: client, registry: registry, mutate: func(c *TargetLimiterConfig) { c.FailureRetryAfter = time.Hour }},
		{name: "provider exceeds global rate", client: client, registry: limiterRegistry(t, map[ProviderID]RateBudget{
			"smart-summary": {RatePerSecond: 101, Burst: 20},
		})},
		{name: "provider exceeds global burst", client: client, registry: limiterRegistry(t, map[ProviderID]RateBudget{
			"smart-summary": {RatePerSecond: 10, Burst: 101},
		})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := limiterConfig("resource-share:v1:", now)
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			_, err := NewRedisTargetLimiter(tt.client, tt.registry, cfg)
			assert.ErrorIs(t, err, ErrLimitConfig)
		})
	}
}

func TestRedisTargetLimiter_IsSharedAcrossReplicasAndHashesTargetIdentity(t *testing.T) {
	client := testLimiterRedis(t)
	prefix := uniqueLimiterPrefix(t)
	t.Cleanup(func() { cleanupLimiterKeys(t, client, prefix) })
	now := time.Unix(1_800_000_000, 0).UTC()
	registry := limiterRegistry(t, map[ProviderID]RateBudget{
		"smart-summary": {RatePerSecond: 100, Burst: 100},
	})
	cfg := limiterConfig(prefix, now)
	one, err := NewRedisTargetLimiter(client, registry, cfg)
	require.NoError(t, err)
	two, err := NewRedisTargetLimiter(client, registry, cfg)
	require.NoError(t, err)
	request := LimitRequest{
		ActorUID: "user-secret-a", SpaceID: "space-secret-a", ProviderID: "smart-summary",
		Target: Target{Kind: TargetGroup, GroupNo: "group-secret-a"},
	}

	first, err := one.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, first.Allowed)
	second, err := two.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, second.Allowed)
	assert.Equal(t, time.Second, second.RetryAfter)

	keys, err := client.Keys(prefix + "*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	joined := strings.Join(keys, "\n")
	assert.NotContains(t, joined, request.ActorUID)
	assert.NotContains(t, joined, request.SpaceID)
	assert.NotContains(t, joined, request.Target.GroupNo)
}

func TestRedisTargetLimiter_DenialDoesNotConsumeOtherBuckets(t *testing.T) {
	client := testLimiterRedis(t)
	prefix := uniqueLimiterPrefix(t)
	t.Cleanup(func() { cleanupLimiterKeys(t, client, prefix) })
	now := time.Unix(1_800_000_000, 0).UTC()
	registry := limiterRegistry(t, map[ProviderID]RateBudget{
		"smart-summary": {RatePerSecond: 0.01, Burst: 1},
		"docs":          {RatePerSecond: 0.01, Burst: 1},
	})
	cfg := limiterConfig(prefix, now)
	cfg.GlobalBudget = RateBudget{RatePerSecond: 0.01, Burst: 2}
	cfg.ChannelBudget = RateBudget{RatePerSecond: 100, Burst: 100}
	limiter, err := NewRedisTargetLimiter(client, registry, cfg)
	require.NoError(t, err)

	request := LimitRequest{ActorUID: "user-a", SpaceID: "space-a", ProviderID: "smart-summary"}
	request.Target = Target{Kind: TargetGroup, GroupNo: "group-a"}
	first, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, first.Allowed)

	request.Target.GroupNo = "group-b"
	providerDenied, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, providerDenied.Allowed)

	request.ProviderID = "docs"
	request.Target.GroupNo = "group-c"
	otherProvider, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, otherProvider.Allowed, "the denied attempt must not consume the global bucket")
}

func TestRedisTargetLimiter_ScopesDMCooldownByActorPeerAndSpace(t *testing.T) {
	client := testLimiterRedis(t)
	prefix := uniqueLimiterPrefix(t)
	t.Cleanup(func() { cleanupLimiterKeys(t, client, prefix) })
	now := time.Unix(1_800_000_000, 0).UTC()
	registry := limiterRegistry(t, map[ProviderID]RateBudget{
		"smart-summary": {RatePerSecond: 100, Burst: 100},
	})
	cfg := limiterConfig(prefix, now)
	cfg.DMBudget = RateBudget{RatePerSecond: 0.01, Burst: 1}
	limiter, err := NewRedisTargetLimiter(client, registry, cfg)
	require.NoError(t, err)

	request := LimitRequest{
		ActorUID: "user-a", SpaceID: "space-a", ProviderID: "smart-summary",
		Target: Target{Kind: TargetDM, PeerUID: "user-b"},
	}
	first, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, first.Allowed)
	repeat, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, repeat.Allowed)

	request.Target.PeerUID = "user-c"
	differentPeer, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, differentPeer.Allowed)
	request.SpaceID = "space-b"
	differentSpace, err := limiter.Allow(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, differentSpace.Allowed)
}

func TestRedisTargetLimiter_FailsClosedOnCancellationAndRedisFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	registry := limiterRegistry(t, map[ProviderID]RateBudget{
		"smart-summary": {RatePerSecond: 10, Burst: 20},
	})
	client := rd.NewClient(&rd.Options{
		Addr: "127.0.0.1:0", MaxRetries: -1,
		DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := NewRedisTargetLimiter(client, registry, limiterConfig("resource-share:v1:", now))
	require.NoError(t, err)
	request := LimitRequest{
		ActorUID: "user-a", SpaceID: "space-a", ProviderID: "smart-summary",
		Target: Target{Kind: TargetGroup, GroupNo: "group-a"},
	}

	decision, err := limiter.Allow(context.Background(), request)
	assert.ErrorIs(t, err, ErrLimitStore)
	assert.False(t, decision.Allowed)
	assert.Equal(t, 10*time.Second, decision.RetryAfter)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	decision, err = limiter.Allow(canceled, request)
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, decision.Allowed)
	assert.Equal(t, 10*time.Second, decision.RetryAfter)
}
