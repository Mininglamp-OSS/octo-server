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
//   - webhook 变更（disable / delete / regenerate）会在【本实例】即时 invalidate 对应
//     webhook 条目；群【解散】(event.GroupDisband) 会在【本实例】即时 invalidate 群条目。
//     「即时」是严格的：invalidate 自增代际，任何在变更前就开始、读到旧行的在途 miss 读
//     都会在 setIfGen 时因代际变化被丢弃，不会把旧快照回填回来（读-后-失效竞态已关闭）。
//   - 跨实例没有主动失效，最多 stale 一个 TTL：上述刚变更的 webhook 或刚解散的群，在 TTL
//     窗口内对等实例上可能仍按旧状态放行。
//   - **群【管理员禁用】(GroupStatusNormal→Disabled, 走 event.GroupUpdate) 不在失效矩阵
//     内**：本模块只订阅 GroupDisband，不订阅 GroupUpdate，所以 admin 禁用群后，push 鉴权
//     的群闸在【所有实例（含执行禁用的那台）】上最多 stale 一个 TTL，而非「本实例即时」。
//     这是有意为之、经维护者确认接受的取舍（不是「本实例即时、对等 TTL」那一类）：
//       * 破坏性路径（解散）仍是即时的；admin 禁用是可逆的运营动作，秒级延迟可接受。
//       * 被禁用的群其 IM 频道同时被 Ban，下游消息投递本就被拦，鉴权闸短暂放行不等于真投递。
//       * 需要即时生效就把 TTL 调小或设 0。
//     若日后要求 admin 禁用也即时，最小改动是订阅 event.GroupUpdate 并 invalidate 群条目
//     （与 handleGroupDisband 对称）。TestPush_GroupAdminDisable_TTLBounded 钉住当前语义。
//   - **创建者退群 / 管理员降级（memberCache）同属 TTL 兜底**：创建者在群闸与 push
//     覆盖判权（cachedCreatorMembership）只缓存正向结果（false 绝不缓存——安全不变量，
//     由 TestCreatorMembershipCache_NeverCachesNegative 钉住），退群/降级后旧的
//     member/admin 快照最多再活一个 TTL，随后懒级联禁用 / 覆盖权限收回即生效。
//     退群没有可订阅的跨模块事件，TTL 是唯一收敛机制。
//   - TTL 默认很短（3s）以把这些窗口压到秒级。把 TTL 设为 0
//     （DM_INCOMINGWEBHOOK_CACHE_TTL_MS=0）可彻底关闭缓存，退化为每次直查 DB 的旧行为。
//
// ⚠️ 因此 DM_INCOMINGWEBHOOK_CACHE_TTL_MS 是【安全参数】而不只是性能旋钮：调大它会
// 同步拉长上述全部鉴权类 staleness 窗口（刚退群成员的 webhook 继续可推、旧 token
// 残存、对等实例放行已解散群）。运维上调前须明确接受这一取舍。
const (
	envCacheTTLMs = "DM_INCOMINGWEBHOOK_CACHE_TTL_MS"
	envCacheMax   = "DM_INCOMINGWEBHOOK_CACHE_MAX"

	// 默认 3s：push 鉴权闸可容忍的 staleness 窗口（秒级，见上）。
	defaultCacheTTL = 3 * time.Second
	// 条目数上限：超过则整桶清空（粗粒度淘汰）。活跃推送的 webhook/group 工作集很小，
	// 正常远不触顶；上限只防异常场景的无界增长。
	//
	// **不做负缓存**：指不缓存「未命中」(webhookID 在库里不存在 → queryByWebhookID 返回
	// nil)，所以不存在的 ID 扫描不会在缓存里占位（这类扫描本就由 per-IP 失败预算在打 DB 前
	// 拦截，#285）。注意：这【不】等于「只缓存 enabled 行」——查到的存在行无论 status 为
	// enabled / disabled / deleted 都会被缓存（它们是合法的 DB 行，不是负结果）。安全性不受
	// 影响：push 路径在 cache 读出后【每次】都重新判 m.Status，disabled/deleted 行照样被拒，
	// 缓存只省了那次 DB 读、并不绕过状态闸。
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
	// gen 是单调代际计数，每次 invalidate 自增。配合 loadGen/setIfGen 关闭「读-后-失效」
	// 回填竞态：cache miss 时先 loadGen 捕获代际，DB 读完用 setIfGen 落库——若期间有任何
	// invalidate（代际变了），本次落库被丢弃，从而保证「本实例 invalidate 后旧快照不会被
	// 一个在途的 miss 读重新填回」。粗粒度（任一 key 的 invalidate 都会让在途 set 作废），
	// 但变更稀少，误丢顶多多读一次 DB，不影响正确性。
	gen uint64
	m   map[string]cacheEntry[T]
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

// loadGen 捕获当前代际，应在 cache miss 后、发起 DB 读【之前】调用，把它传给随后的
// setIfGen，使并发的 invalidate 能在落库时被检测到。
func (c *ttlCache[T]) loadGen() uint64 {
	if !c.enabled() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// setIfGen 仅在代际未变（自 loadGen 起没有 invalidate 发生）时落库；否则丢弃。这正是
// 关闭回填竞态的关键：一个在 mutation 之前就开始的 miss 读，不得用「变更前的旧行」把缓存
// 重新填上、令刚被禁用/删除/改 token 的条目在本实例上复活一个 TTL。丢弃最多多读一次 DB。
func (c *ttlCache[T]) setIfGen(key string, val T, gen uint64) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	if len(c.m) >= c.maxSize {
		if _, exists := c.m[key]; !exists {
			c.m = make(map[string]cacheEntry[T], c.maxSize)
		}
	}
	c.m[key] = cacheEntry[T]{val: val, exp: time.Now().Add(c.ttl)}
}

// set 写入并打 TTL 戳（无代际守卫，仅供测试预热缓存用；生产读路径走 loadGen+setIfGen）。
// 超过容量上限且是新键时整桶清空（粗粒度淘汰：工作集小、正常不触发；触发时最坏退化为
// 下一轮重填，不影响正确性）。
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

// invalidate 删除单个键并自增代际（变更路径的即时失效入口）。自增代际让任何在途的
// miss 读在随后 setIfGen 时作废，保证失效后旧快照不会被回填——即「本实例即时失效」。
func (c *ttlCache[T]) invalidate(key string) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	c.gen++
	delete(c.m, key)
	c.mu.Unlock()
}
