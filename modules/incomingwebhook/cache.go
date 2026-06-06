package incomingwebhook

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// push 热路径缓存（#284 item 2）。push 在限流与发送前有两次未缓存的 DB 点读：
// webhook 行（queryByWebhookID）+ 群状态（requireActiveGroup）。二者近乎不可变 /
// 极少变更，却是任意 webhook 高 QPS 推送时的第一道 DB 读墙。本缓存是进程内、短 TTL、
// 不依赖 Redis 的（与 localFloor 一脉相承——Redis 故障也能命中），命中即 0 DB 读。
//
// ⚠️ 鉴权 staleness 契约（#284 验收明确接受秒级 staleness）：
//   - disable / delete / regenerate 会在【本实例】即时 invalidate 对应 webhook 条目；
//     群解散在【本实例】即时 invalidate 群条目。
//   - 跨实例没有主动失效，最多 stale 一个 TTL：刚被禁用/删除/改 token 的 webhook、或
//     刚解散的群，在 TTL 窗口内对等实例上可能仍按旧状态放行。TTL 默认很短（3s）以把这
//     个窗口压到秒级。把 TTL 设为 0（DM_INCOMINGWEBHOOK_CACHE_TTL_MS=0）可彻底关闭缓存，
//     退化为每次直查 DB 的旧行为。
const (
	envCacheTTLMs = "DM_INCOMINGWEBHOOK_CACHE_TTL_MS"
	envCacheMax   = "DM_INCOMINGWEBHOOK_CACHE_MAX"

	// 默认 3s：push 鉴权闸可容忍的 staleness 窗口（秒级，见上）。
	defaultCacheTTL = 3 * time.Second
	// 条目数上限：超过则整桶清空（粗粒度淘汰）。活跃推送的 webhook/group 工作集很小，
	// 正常远不触顶；上限只防异常场景的无界增长。**不做负缓存**——不存在/已删的 webhookID
	// 扫描由 per-IP 失败预算在打 DB 前拦截（#285），缓存它们只会被扫描流量污染。
	defaultCacheMax = 10000
)

// cacheTTL 读 DM_INCOMINGWEBHOOK_CACHE_TTL_MS（毫秒）；0 表示禁用缓存，缺省/非法回退默认。
func cacheTTL() time.Duration {
	if v := os.Getenv(envCacheTTLMs); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return defaultCacheTTL
}

// cacheMax 读 DM_INCOMINGWEBHOOK_CACHE_MAX（条目数上限）；仅接受正整数，否则回退默认。
func cacheMax() int {
	if v := os.Getenv(envCacheMax); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultCacheMax
}

type cacheEntry[T any] struct {
	val T
	exp time.Time
}

// ttlCache 是 push 热路径用的进程内短 TTL 缓存：mutex map + 惰性过期 + 容量上限。
// 泛型以同一实现服务 webhook 行与群状态两种值。ttl<=0 视为禁用（所有方法 no-op，
// get 永远 miss），从而让 DM_INCOMINGWEBHOOK_CACHE_TTL_MS=0 等价于"无缓存"。
type ttlCache[T any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	m       map[string]cacheEntry[T]
}

func newTTLCache[T any](ttl time.Duration, maxSize int) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, maxSize: maxSize, m: make(map[string]cacheEntry[T])}
}

// enabled 在 nil 接收者或 ttl<=0 时返回 false（nil 检查短路，方法整体 nil-safe）。
func (c *ttlCache[T]) enabled() bool { return c != nil && c.ttl > 0 }

// get 返回未过期条目；过期则惰性删除并 miss。
func (c *ttlCache[T]) get(key string) (T, bool) {
	var zero T
	if !c.enabled() {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return zero, false
	}
	if time.Now().After(e.exp) {
		delete(c.m, key)
		return zero, false
	}
	return e.val, true
}

// set 写入并打 TTL 戳。超过容量上限且是新键时整桶清空（粗粒度淘汰：工作集小、正常不
// 触发；触发时最坏退化为下一轮重填，不影响正确性）。
func (c *ttlCache[T]) set(key string, val T) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.maxSize {
		if _, exists := c.m[key]; !exists {
			c.m = make(map[string]cacheEntry[T], c.maxSize)
		}
	}
	c.m[key] = cacheEntry[T]{val: val, exp: time.Now().Add(c.ttl)}
}

// invalidate 删除单个键（变更路径的即时失效入口）。
func (c *ttlCache[T]) invalidate(key string) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
