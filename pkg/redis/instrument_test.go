package redis

import (
	"sync"
	"testing"
	"time"

	liboredis "github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	rd "github.com/go-redis/redis"
)

// NewInstrumentedClient 构造的裸 client 必须已挂上 octo-lib 的命令计时 hook。
// 命令打向无人监听的地址会立即失败,但 WrapProcess 仍会触发上报 —— 借此在无真实
// Redis 的情况下证明插桩已生效。共享进程级 observer 单例,勿加 t.Parallel()。
func TestNewInstrumentedClientInstruments(t *testing.T) {
	var (
		mu     sync.Mutex
		sawGet bool
	)
	liboredis.SetRedisObserver(func(cmd string, _ time.Duration, _ error) {
		mu.Lock()
		if cmd == "get" {
			sawGet = true
		}
		mu.Unlock()
	})
	t.Cleanup(func() { liboredis.SetRedisObserver(nil) })

	cfg := newConfig()
	cfg.DB.RedisAddr = "127.0.0.1:1" // 无人监听:命令立即失败,但 hook 仍触发
	c := NewInstrumentedClient(cfg, func(o *rd.Options) {
		o.MaxRetries = 0
		o.DialTimeout = 200 * time.Millisecond
	})
	if c == nil {
		t.Fatal("NewInstrumentedClient returned nil")
	}
	defer func() { _ = c.Close() }()

	_ = c.Get("k").Err()

	mu.Lock()
	defer mu.Unlock()
	if !sawGet {
		t.Fatal("expected the instrumented client's GET to reach the observer")
	}
}

// InstrumentedClientFromOptions 同样应挂上 hook。
func TestInstrumentedClientFromOptionsInstruments(t *testing.T) {
	var (
		mu     sync.Mutex
		sawGet bool
	)
	liboredis.SetRedisObserver(func(cmd string, _ time.Duration, _ error) {
		mu.Lock()
		if cmd == "get" {
			sawGet = true
		}
		mu.Unlock()
	})
	t.Cleanup(func() { liboredis.SetRedisObserver(nil) })

	c := InstrumentedClientFromOptions(&rd.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  0,
		DialTimeout: 200 * time.Millisecond,
	})
	if c == nil {
		t.Fatal("InstrumentedClientFromOptions returned nil")
	}
	defer func() { _ = c.Close() }()

	_ = c.Get("k").Err()

	mu.Lock()
	defer mu.Unlock()
	if !sawGet {
		t.Fatal("expected the instrumented client's GET to reach the observer")
	}
}
