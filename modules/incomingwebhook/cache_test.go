package incomingwebhook

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- ttlCache mechanics (no infra) -----

func TestTTLCache_GetSetHit(t *testing.T) {
	c := newTTLCache[int](time.Minute, 10)
	if _, ok := c.get("k"); ok {
		t.Fatal("empty cache must miss")
	}
	c.set("k", 42)
	v, ok := c.get("k")
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestTTLCache_Expiry(t *testing.T) {
	c := newTTLCache[int](20*time.Millisecond, 10)
	c.set("k", 1)
	if _, ok := c.get("k"); !ok {
		t.Fatal("fresh entry must hit")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry must miss")
	}
}

func TestTTLCache_Invalidate(t *testing.T) {
	c := newTTLCache[int](time.Minute, 10)
	c.set("k", 1)
	c.invalidate("k")
	if _, ok := c.get("k"); ok {
		t.Fatal("invalidated entry must miss")
	}
}

func TestTTLCache_DisabledWhenTTLZero(t *testing.T) {
	c := newTTLCache[int](0, 10) // ttl<=0 → disabled
	assert.False(t, c.enabled())
	c.set("k", 1)
	if _, ok := c.get("k"); ok {
		t.Fatal("disabled cache must never hit")
	}
}

func TestTTLCache_NilReceiverSafe(t *testing.T) {
	var c *ttlCache[int] // nil
	assert.False(t, c.enabled())
	c.set("k", 1) // no-op, must not panic
	if _, ok := c.get("k"); ok {
		t.Fatal("nil cache must miss")
	}
	c.invalidate("k") // no-op, must not panic
}

func TestTTLCache_MaxSizeClears(t *testing.T) {
	c := newTTLCache[int](time.Minute, 2)
	c.set("a", 1)
	c.set("b", 2)
	c.set("c", 3) // len>=max and new key → bucket cleared, then c inserted
	if _, ok := c.get("a"); ok {
		t.Fatal("a should have been evicted by the size-cap clear")
	}
	v, ok := c.get("c")
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

// ----- env parsers (no infra) -----

func TestCacheTTLEnv(t *testing.T) {
	t.Setenv(envCacheTTLMs, "")
	assert.Equal(t, defaultCacheTTL, cacheTTL(), "缺省回退默认")
	t.Setenv(envCacheTTLMs, "0")
	assert.Equal(t, time.Duration(0), cacheTTL(), "0 表示禁用")
	t.Setenv(envCacheTTLMs, "1500")
	assert.Equal(t, 1500*time.Millisecond, cacheTTL())
	t.Setenv(envCacheTTLMs, "-5")
	assert.Equal(t, defaultCacheTTL, cacheTTL(), "负值非法回退默认")
	t.Setenv(envCacheTTLMs, "abc")
	assert.Equal(t, defaultCacheTTL, cacheTTL(), "非数字回退默认")
}

func TestCacheMaxEnv(t *testing.T) {
	t.Setenv(envCacheMax, "")
	assert.Equal(t, defaultCacheMax, cacheMax())
	t.Setenv(envCacheMax, "500")
	assert.Equal(t, 500, cacheMax())
	t.Setenv(envCacheMax, "0")
	assert.Equal(t, defaultCacheMax, cacheMax(), "非正整数回退默认")
}

// ----- cached read helpers: hit path skips DB (no infra) -----
//
// w.db is left nil on purpose: a cache HIT must return without touching the DB,
// so these prove the hot path issues 0 DB reads on a warm cache (#284 acceptance).
// The miss → DB and invalidate → refetch paths need a real DB and are covered by
// the integration test in api_test.go.

func TestCachedQueryByWebhookID_HitSkipsDB(t *testing.T) {
	w := &IncomingWebhook{webhookCache: newTTLCache[*incomingWebhookModel](time.Minute, 10)}
	want := &incomingWebhookModel{WebhookID: "iwh_x", Status: statusEnabled}
	w.webhookCache.set("iwh_x", want)

	got, err := w.cachedQueryByWebhookID("iwh_x")
	require.NoError(t, err)
	require.Same(t, want, got, "must be served from cache, not the (nil) DB")
}

func TestCachedRequireActiveGroup_HitSkipsDB(t *testing.T) {
	w := &IncomingWebhook{groupCache: newTTLCache[*group.Model](time.Minute, 10)}
	want := &group.Model{Status: group.GroupStatusNormal}
	w.groupCache.set("g_x", want)

	got, err := w.cachedRequireActiveGroup("g_x")
	require.NoError(t, err)
	require.Same(t, want, got, "must be served from cache, not the (nil) DB")
}
