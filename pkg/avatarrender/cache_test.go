package avatarrender

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCacheHitMiss:首次未命中触发渲染,二次命中直接复用,不再渲染。
func TestCacheHitMiss(t *testing.T) {
	var hits, misses, renders int64
	c, err := NewCache(Config{Hooks: Hooks{
		OnHit:    func() { atomic.AddInt64(&hits, 1) },
		OnMiss:   func() { atomic.AddInt64(&misses, 1) },
		OnRender: func(time.Duration, error) { atomic.AddInt64(&renders, 1) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	render := func() ([]byte, error) { return []byte("png-A"), nil }

	b, err := c.GetOrRender("k1", render)
	if err != nil || string(b) != "png-A" {
		t.Fatalf("first get: %q %v", b, err)
	}
	b, err = c.GetOrRender("k1", render)
	if err != nil || string(b) != "png-A" {
		t.Fatalf("second get: %q %v", b, err)
	}
	if got := atomic.LoadInt64(&renders); got != 1 {
		t.Fatalf("expected exactly 1 render, got %d", got)
	}
	if hits != 1 || misses != 1 {
		t.Fatalf("expected 1 hit / 1 miss, got %d / %d", hits, misses)
	}
	if c.Len() != 1 {
		t.Fatalf("expected 1 cached entry, got %d", c.Len())
	}
}

// TestCacheErrorNotCached:渲染失败不写缓存,下次重试。
func TestCacheErrorNotCached(t *testing.T) {
	c, _ := NewCache(Config{})
	var calls int64
	render := func() ([]byte, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			return nil, errors.New("boom")
		}
		return []byte("ok"), nil
	}
	if _, err := c.GetOrRender("k", render); err == nil {
		t.Fatal("expected error on first render")
	}
	b, err := c.GetOrRender("k", render)
	if err != nil || string(b) != "ok" {
		t.Fatalf("expected retry to succeed, got %q %v", b, err)
	}
	if c.Len() != 1 {
		t.Fatalf("expected entry cached after success, got %d", c.Len())
	}
}

// TestCacheSingleflight:同 key 的大量并发请求只触发一次渲染,其余复用。
func TestCacheSingleflight(t *testing.T) {
	c, _ := NewCache(Config{})
	var renders int64
	var shared int64
	c.hooks.OnShared = func() { atomic.AddInt64(&shared, 1) }
	render := func() ([]byte, error) {
		atomic.AddInt64(&renders, 1)
		time.Sleep(30 * time.Millisecond) // 拉长渲染窗口,让并发请求都挤进同一次 flight
		return []byte("png"), nil
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := c.GetOrRender("same", render); err != nil || string(b) != "png" {
				t.Errorf("get: %q %v", b, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&renders); got != 1 {
		t.Fatalf("expected exactly 1 render for %d concurrent same-key requests, got %d", n, got)
	}
	if atomic.LoadInt64(&shared) == 0 {
		t.Fatal("expected some requests to be coalesced (shared), got 0")
	}
}

// TestCacheSemaphoreBoundsConcurrency:MaxConcurrentRenders 限制同时进行的真实
// 渲染数 —— 即使大量不同 key 同时到达,inflight 也不超过上限。
func TestCacheSemaphoreBoundsConcurrency(t *testing.T) {
	const limit = 2
	var inflight, maxInflight int64
	c, _ := NewCache(Config{
		MaxConcurrentRenders: limit,
		Hooks: Hooks{
			OnInflight: func(delta float64) {
				cur := atomic.AddInt64(&inflight, int64(delta))
				for {
					m := atomic.LoadInt64(&maxInflight)
					if cur <= m || atomic.CompareAndSwapInt64(&maxInflight, m, cur) {
						break
					}
				}
			},
		},
	})
	render := func() ([]byte, error) {
		time.Sleep(20 * time.Millisecond)
		return []byte("x"), nil
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i)) // 全部不同 key,绕过 singleflight 合并
			_, _ = c.GetOrRender(key, render)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt64(&maxInflight); got > limit {
		t.Fatalf("max concurrent renders %d exceeded semaphore limit %d", got, limit)
	}
	if atomic.LoadInt64(&maxInflight) == 0 {
		t.Fatal("expected renders to run")
	}
}

// TestCacheNilReceiver:nil 接收者直接渲染,不 panic(缓存未配置时的退化路径)。
func TestCacheNilReceiver(t *testing.T) {
	var c *Cache
	b, err := c.GetOrRender("k", func() ([]byte, error) { return []byte("raw"), nil })
	if err != nil || string(b) != "raw" {
		t.Fatalf("nil receiver get: %q %v", b, err)
	}
	if c.Len() != 0 {
		t.Fatal("nil cache Len should be 0")
	}
}
