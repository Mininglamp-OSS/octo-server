package avatarrender

import (
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
)

// 进程级共享头像缓存。所有头像端点(当前是 user 的 UserAvatar;未来 group 等任何
// 加入按需渲染的端点)都经包级 GetOrRender 共用这一个 Cache 实例,从而:
//
//   - 共享同一个 LRU(一份内存预算,不按端点翻倍);
//   - 共享同一个渲染信号量 —— 这点是关键:信号量只有进程级唯一,才是真正的"全机
//     渲染并发上限"。若每个端点各建一个 Cache 各限 N,合起来就是 2N 并发渲染,等于
//     没限住,issue#480 的 CPU 饿死照样发生。
//
// 用 atomic.Pointer 持有,未设置时 GetOrRender 退化为直接渲染(Cache 方法对 nil
// 接收者安全),与 pkg/metrics 的包级默认实例约定一致。

var defaultCache atomic.Pointer[Cache]

// SetDefaultCache 设置进程级共享头像缓存(组合根在启动时调用一次)。
func SetDefaultCache(c *Cache) { defaultCache.Store(c) }

// DefaultCache 返回当前进程级共享缓存(未设置时为 nil)。
func DefaultCache() *Cache { return defaultCache.Load() }

// GetOrRender 用进程级共享缓存渲染 key 对应的头像;未设置默认缓存时直接渲染。
// 各端点 handler 应调用本函数,而不是各自持有 Cache。
func GetOrRender(key string, render func() ([]byte, error)) ([]byte, error) {
	return defaultCache.Load().GetOrRender(key, render)
}

// ConfigFromEnv 从环境变量读取共享缓存配置(不含 Hooks —— 观测点由组合根注入,
// 以免本包依赖 pkg/metrics):
//
//   - DM_AVATAR_CACHE_SIZE: LRU 条目数,缺省/非法用 DefaultCacheSize;
//   - DM_AVATAR_RENDER_MAX_CONCURRENCY: 最大并发真实渲染数,缺省 max(1, GOMAXPROCS-1)。
func ConfigFromEnv() Config {
	return Config{
		Size:                 envPositiveInt("DM_AVATAR_CACHE_SIZE", 0),
		MaxConcurrentRenders: envPositiveInt("DM_AVATAR_RENDER_MAX_CONCURRENCY", defaultRenderConcurrency()),
	}
}

// defaultRenderConcurrency 默认渲染并发上限:给非头像流量至少留一个 P,防止冷头像
// 洪峰占满所有核饿死其它请求。
func defaultRenderConcurrency() int {
	if n := runtime.GOMAXPROCS(0) - 1; n > 0 {
		return n
	}
	return 1
}

// envPositiveInt 读取一个正整数环境变量,缺省/非法时返回 def。
func envPositiveInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
