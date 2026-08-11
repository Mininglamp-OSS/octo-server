package oidc

import (
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	"github.com/stretchr/testify/assert"
)

func newCallbackGuardRedis(t *testing.T) *redis.Conn {
	t.Helper()
	return redis.New("127.0.0.1:6379", "")
}

func clearGuardIPs(t *testing.T, g *CallbackGuard, ips ...string) {
	t.Helper()
	for _, ip := range ips {
		_ = g.Reset(ip)
	}
}

func TestCallbackGuard_Check_NoRecord(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 10, 5*time.Minute)
	clearGuardIPs(t, g, "1.2.3.4")
	assert.NoError(t, g.Check("1.2.3.4"))
}

func TestCallbackGuard_LocksAtThreshold(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 3, 5*time.Minute)
	ip := "10.0.0.1"
	clearGuardIPs(t, g, ip)
	for i := 0; i < 3; i++ {
		assert.NoError(t, g.RecordFailure(ip))
	}
	err := g.Check(ip)
	assert.True(t, errors.Is(err, ErrCallbackBlocked), "expected ErrCallbackBlocked, got %v", err)
}

func TestCallbackGuard_UnderThreshold_NotLocked(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 3, 5*time.Minute)
	ip := "10.0.0.2"
	clearGuardIPs(t, g, ip)
	for i := 0; i < 2; i++ {
		assert.NoError(t, g.RecordFailure(ip))
	}
	assert.NoError(t, g.Check(ip))
}

func TestCallbackGuard_Reset_ClearsCounter(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 3, 5*time.Minute)
	ip := "10.0.0.3"
	clearGuardIPs(t, g, ip)
	for i := 0; i < 3; i++ {
		assert.NoError(t, g.RecordFailure(ip))
	}
	assert.Error(t, g.Check(ip))
	assert.NoError(t, g.Reset(ip))
	assert.NoError(t, g.Check(ip))
}

func TestCallbackGuard_EmptyIPUsesUnknownBucket(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 3, 5*time.Minute)
	const unknownIP = "__unknown_ip__"
	assert.NoError(t, g.redis.Del(g.key(unknownIP)))
	t.Cleanup(func() { _ = g.redis.Del(g.key(unknownIP)) })

	for i := 0; i < 3; i++ {
		assert.NoError(t, g.RecordFailure(""))
	}
	assert.ErrorIs(t, g.Check("   "), ErrCallbackBlocked)
	assert.NoError(t, g.Reset(""))
	assert.NoError(t, g.Check(""))
}

func TestCallbackGuard_DefaultThresholdAndWindow(t *testing.T) {
	g := NewCallbackGuard(newCallbackGuardRedis(t), 0, 0)
	assert.Equal(t, int64(defaultCallbackFailThreshold), g.threshold)
	assert.Equal(t, defaultCallbackFailWindow, g.window)
}

func TestCallbackGuard_EnvHelpers(t *testing.T) {
	t.Setenv("DM_OIDC_CALLBACK_FAIL_THRESHOLD", "12")
	t.Setenv("DM_OIDC_CALLBACK_FAIL_WINDOW_SEC", "120")
	assert.Equal(t, int64(12), callbackGuardThresholdFromEnv())
	assert.Equal(t, 120*time.Second, callbackGuardWindowFromEnv())

	t.Setenv("DM_OIDC_CALLBACK_FAIL_THRESHOLD", "")
	t.Setenv("DM_OIDC_CALLBACK_FAIL_WINDOW_SEC", "")
	assert.Equal(t, int64(0), callbackGuardThresholdFromEnv())
	assert.Equal(t, time.Duration(0), callbackGuardWindowFromEnv())

	t.Setenv("DM_OIDC_CALLBACK_FAIL_THRESHOLD", "abc")
	t.Setenv("DM_OIDC_CALLBACK_FAIL_WINDOW_SEC", "-1")
	assert.Equal(t, int64(0), callbackGuardThresholdFromEnv())
	assert.Equal(t, time.Duration(0), callbackGuardWindowFromEnv())
}

func TestCallbackGuard_NilReceiver_NoPanic(t *testing.T) {
	var g *CallbackGuard
	assert.NoError(t, g.Check("1.2.3.4"))
	assert.NoError(t, g.RecordFailure("1.2.3.4"))
	assert.NoError(t, g.Reset("1.2.3.4"))
	g.RecordFailureLogged("1.2.3.4")
	g.ResetLogged("1.2.3.4")
}

func TestCallbackGuard_TTLAutoUnlock(t *testing.T) {
	window := 2 * time.Second
	g := NewCallbackGuard(newCallbackGuardRedis(t), 3, window)
	ip := "10.0.0.4"
	clearGuardIPs(t, g, ip)

	for i := 0; i < 3; i++ {
		assert.NoError(t, g.RecordFailure(ip))
	}
	assert.Error(t, g.Check(ip))
	time.Sleep(window + 500*time.Millisecond)
	assert.NoError(t, g.Check(ip))
}
