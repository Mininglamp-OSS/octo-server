// Package ratelimit 提供可热调、可影子运行（dry-run）的令牌桶限流原语。
//
// 与 octo-lib `pkg/wkhttp` 的三个限流中间件的分工：
//
//   - lib 的中间件在**构造时**固定 rps/burst（middleware 只创建一次），
//     因此改配额必须重启进程。全局 per-IP 桶就是这个形状。
//   - 本包把 rps/burst/开关做成**每次判定时读取**的 ParamsFunc，
//     于是配额可经 system_setting 运行时热调，无需重启。
//
// 为什么需要热调（这不是锦上添花）：2026-08-05 的生产事故里，止血手段是把全局
// per-IP 配额从 500 调到 1500，而该值只能靠滚动重启生效；滚动的 93 秒窗口内新旧
// 副本共用同一个 Redis 桶却各传各的参数，限流行为抖动，一个 bot 恰在此窗口被踢
// 下线且无法自愈。任何新增的桶若仍然只能靠重启调整，等于把这个故障模式复制一份。
//
// 与 repo rule `rate-limit` 的关系：该 rule 要求 authenticated route 挂
// `SharedUIDRateLimiter`，但那是**进程级单例**（octo-server pkg/wkhttp
// ratelimit_helper.go 的 uidRateLimitReady 守卫），bot 与登录用户共用同一套
// rps/burst。本包服务的场景需要「独立配额值 + 运行时热调」，共享单例在结构上
// 表达不了——这与 modules/incomingwebhook 的 per-webhook 桶同构，落在该 rule
// 已写明的 Exception（按业务身份的独立配额）之内，不是绕过。
package ratelimit

import (
	"math"
	"time"

	rd "github.com/go-redis/redis"
)

// tokenBucketScript 与 octo-lib `pkg/wkhttp/ratelimit.go`、
// `modules/incomingwebhook/ratelimit.go` 中的脚本**逐字节同源**，刻意不做改动：
// 三处共用同一套填充/拒绝语义，任何一处调整都应当同步，否则同一个系统里会出现
// 两种「1 秒是什么意思」。
//
// KEYS[1]: bucket 键
// ARGV[1]: rps（每秒填充速率，float）
// ARGV[2]: burst（桶容量，int）
// ARGV[3]: now（当前时间，秒，float）
//
// 返回 {allowed (0/1), remaining (int), retry_after_seconds (int, ceil)}。
const tokenBucketScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

-- 纯防御：调用方都传正数，但脚本独立可审计，rate<=0 会让 burst/rate 变 inf 导致 EXPIRE 报错。
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
    -- 拒绝路径跳过写回，避免 DDoS 下的 HMSET+EXPIRE 写放大。
    -- 正确性：filled 是 (tokens, ts, now) 的纯函数，不写回不会丢信息。
    -- 例外：首访即被拒（burst<1）时 key 尚不存在，仍需初始化以设置 TTL。
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

// bucket 是一条 Redis 令牌桶。状态存 Redis，因此配额是**集群级共享**的：
// 配置的 rps 是全站合计，不是每副本——与 pkg/botevent 的进程级并发预算语义相反，
// 别搞混。
type bucket struct {
	client *rd.Client
	script *rd.Script
	prefix string
}

func newBucket(client *rd.Client, prefix string) *bucket {
	return &bucket{
		client: client,
		script: rd.NewScript(tokenBucketScript),
		prefix: prefix,
	}
}

// bucketResult 是一次桶判定的原始结果。
type bucketResult struct {
	allowed    bool
	remaining  int
	retryAfter int
	// degraded 为 true 表示 Redis 调用失败、走了 fail-open（allowed 恒为 true）。
	degraded bool
}

// allow 执行一次令牌桶判定。
//
// fail-open 设计（与 octo-lib 三个中间件保持一致，刻意不发明新语义）：Redis 是 IM
// 核心依赖，挂掉时整个系统已失能；若限流层反而开始拒绝，会把基础设施抖动放大成
// 业务不可用，且监控无从判断是 Redis 问题还是业务问题。
//
// 不接受 context：token 一旦消耗就不应因客户端断连而退还，否则攻击者可用
// connect/disconnect 绕过限流。（go-redis v6 的 Script.Run 本也不接受 context。）
func (b *bucket) allow(key string, rps float64, burst int) bucketResult {
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	res, err := b.script.Run(b.client, []string{b.prefix + key}, rps, burst, now).Result()
	if err != nil {
		return bucketResult{allowed: true, remaining: burst, degraded: true}
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		// 返回形状异常：fail-open，但必须让调用方知道（degraded），否则一个真实
		// bug 会永远藏在「总是放行」后面。
		return bucketResult{allowed: true, remaining: burst, degraded: true}
	}

	allowedN, _ := arr[0].(int64)
	remainingN, _ := arr[1].(int64)
	retryAfterN, _ := arr[2].(int64)

	remaining := int(remainingN)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > burst {
		remaining = burst
	}

	return bucketResult{
		allowed:    allowedN == 1,
		remaining:  remaining,
		retryAfter: int(retryAfterN),
	}
}

// SanitizeRPS 是**读侧防御**：配置值再怎么校验，也可能被直接改库、或被 env 绕过
// 写侧校验（`ParseRPSFromEnv` 底层是 strconv.ParseFloat，会原样接受 "NaN"/"+Inf"）。
//
// 非法值必须回退到一个**合法且仍然限流**的值，绝不能退化成「放行」：
//   - rps <= 0 会让上面的 Lua 走 `rate <= 0` 分支 → 100% 拒绝（不是放行，但等于
//     把整条路由打死）；
//   - **NaN 尤其危险**：octo-lib 的 `newKeyedLimiter` 只检查 `rps <= 0`，而
//     `NaN <= 0` 为 false，所以 NaN 会穿过它的启动期 panic 校验直达 Lua。
//     导出本函数正是为了让走 lib 中间件的调用方也能补上这道消毒；
//   - NaN 会让 Lua 的算术全部变 NaN，比较失败，行为不可预测。
//
// 两者都不可接受，故一律回退到 fallback（由调用方给出代码默认值）。
func SanitizeRPS(v, fallback float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fallback
	}
	return v
}

// SanitizeBurst 同 SanitizeRPS。burst <= 0 会让桶永远填不进 1 个 token → 100% 拒绝。
func SanitizeBurst(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}
