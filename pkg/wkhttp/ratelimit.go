package wkhttp

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const unknownIPKey = "__unknown_ip__"

// tokenBucketScript 实现分布式原子令牌桶。
//
// KEYS[1]: bucket 键
// ARGV[1]: rps（每秒填充速率，float）
// ARGV[2]: burst（桶容量，int）
// ARGV[3]: now（当前时间，秒，float）
//
// 返回 {allowed (0/1), remaining (int), retry_after_seconds (int, ceil)}。
// 单次请求消耗 1 个 token；过期时间设为填满一次的 2 倍，旧 key 自然回收，
// 无需后台清理 goroutine。
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

// keyedLimiter 把一对 (rps, burst) 约束映射到 Redis 上的分布式令牌桶。
// 多实例部署下配额是全局共享的——这是本组件相对前一代纯内存实现的根本区别。
type keyedLimiter struct {
	client    *rd.Client
	script    *rd.Script
	keyPrefix string
	rps       float64
	burst     int

	// 最近一次降级告警的 Unix 秒；每 degradeWarnInterval 秒至多打一条，
	// 既避免日志风暴，也不会因 sync.Once 导致 Redis 长时间不可用后告警永久沉默。
	lastDegradeWarn atomic.Int64
}

const degradeWarnInterval = 30 // seconds

// logDegrade 按 degradeWarnInterval 节流降级告警；首次故障立即打印，
// 之后每 30 秒至多一条，故障持续时也不会永久沉默。
func (k *keyedLimiter) logDegrade(msg string, fields ...zap.Field) {
	now := time.Now().Unix()
	prev := k.lastDegradeWarn.Load()
	if now-prev < degradeWarnInterval {
		return
	}
	if !k.lastDegradeWarn.CompareAndSwap(prev, now) {
		return // 并发竞争下由另一 goroutine 打印
	}
	log.Warn("rate limit degraded: "+msg,
		append([]zap.Field{zap.String("prefix", k.keyPrefix)}, fields...)...)
}

func newKeyedLimiter(client *rd.Client, keyPrefix string, rps float64, burst int) *keyedLimiter {
	return &keyedLimiter{
		client:    client,
		script:    rd.NewScript(tokenBucketScript),
		keyPrefix: keyPrefix,
		rps:       rps,
		burst:     burst,
	}
}

// allow 执行一次限流判定。
//
// 返回值：
//   - allowed: 是否放行
//   - remaining: 当前剩余 token 数（clamp 到 [0, burst]）
//   - retryAfter: 建议的重试等待秒数（不放行时 >=1，放行时为 0）
//   - degraded: Redis 调用失败走 fail-open，此时 allowed 恒为 true
//
// fail-open 设计：Redis 是 IM 核心依赖，挂掉时整个系统已失能；
// 若限流层反而返回 503，会把基础设施抖动放大成业务不可用，监控也无从判定
// 是 Redis 问题还是业务问题。因此这里放行 + 记日志 + 节流告警。
//
// ctx 未传入 Redis 调用（go-redis v6 的 Script.Run 不支持 context）。
// 即使未来升级到 v8+，限流判定也应忽略请求取消——token 一旦消耗就不应因
// 客户端断连而退还，否则攻击者可 connect/disconnect 绕过限流。
func (k *keyedLimiter) allow(ctx context.Context, key string) (allowed bool, remaining int, retryAfter int, degraded bool) {
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	res, err := k.script.Run(k.client, []string{k.keyPrefix + key},
		k.rps, k.burst, now).Result()
	if err != nil {
		k.logDegrade("redis eval failed, falling open", zap.Error(err))
		return true, k.burst, 0, true
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) != 3 {
		k.logDegrade("unexpected redis script return shape")
		return true, k.burst, 0, true
	}

	allowedN, _ := arr[0].(int64)
	remainingN, _ := arr[1].(int64)
	retryAfterN, _ := arr[2].(int64)

	rem := int(remainingN)
	if rem < 0 {
		rem = 0
	}
	if rem > k.burst {
		rem = k.burst
	}
	return allowedN == 1, rem, int(retryAfterN), false
}

// setRateLimitHeaders 写入标准限流响应头，allowed=false 时同时写 Retry-After。
// Retry-After 的下限由 Lua 脚本保证（math.max(1, ...)）；fail-open 分支
// allowed=true 不会进入此处的 !allowed 写入路径，因此无需在 Go 侧兜底 clamp。
//
// scope 标识触发层（"ip" / "uid" / "strict:{tag}"），通过 X-RateLimit-Scope
// 暴露给客户端，便于区分 429 的归因。多层链路上后挂的层会覆盖前层的头，
// 看到的 scope 就是"最后一层写入者"——通常是通过的最紧桶或命中拒绝的层。
func setRateLimitHeaders(h http.Header, scope string, burst, remaining int, allowed bool, retryAfter int) {
	h.Set("X-RateLimit-Limit", strconv.Itoa(burst))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Scope", scope)
	if !allowed {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
}

// getClientIP 从请求头按优先级取客户端 IP。
// 生产架构为腾讯云 CLB 直连 Pod（pass-to-target），单层代理，XFF 只含客户端真实 IP。
// 若未来新增 CDN 或多层反代，需重新评估 rightmost XFF 的取值是否正确。
func getClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if ip, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return ip
	}
	return ""
}

// RateLimitMiddleware 全局 per-IP 限流，作为 DDoS 底线（挂载点：UseGin）。
//
// 状态存储于 Redis，多副本间共享配额。Redis 不可达时 fail-open（放行 + 告警）。
// ctx 目前仅用于未来的取消语义，当前实现不起作用。
func RateLimitMiddleware(ctx context.Context, client *rd.Client, rps float64, burst int, excludePaths ...string) gin.HandlerFunc {
	kl := newKeyedLimiter(client, "ratelimit:ip:", rps, burst)

	excludeSet := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		excludeSet[p] = struct{}{}
	}

	var unknownIPWarnOnce sync.Once

	return func(c *gin.Context) {
		if _, ok := excludeSet[c.Request.URL.Path]; ok {
			c.Next()
			return
		}

		// fail-closed: 拿不到 IP 时走全局桶，不放行
		ip := getClientIP(c.Request)
		if ip == "" {
			unknownIPWarnOnce.Do(func() {
				log.Warn("rate limit: client IP unavailable, falling back to shared bucket; check reverse proxy / XFF configuration")
			})
			ip = unknownIPKey
		}

		allowed, remaining, retryAfter, _ := kl.allow(c.Request.Context(), ip)
		setRateLimitHeaders(c.Writer.Header(), "ip", burst, remaining, allowed, retryAfter)

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"msg":    "请求过于频繁，请稍后再试",
				"status": http.StatusTooManyRequests,
			})
			return
		}

		c.Next()
	}
}

// StrictIPRateLimitMiddleware 端点级 per-IP 严格限流，挂在敏感端点（登录/注册/SMS/搜索）作为额外防护。
//
// 与全局 RateLimitMiddleware 区别：
//   - 全局：DDoS 底线，宽松阈值（数百 req/s），挂全局 UseGin
//   - 严格：暴力破解/枚举防御，紧阈值（数 req/min），挂在端点级 RouterGroup
//
// 同类端点（如所有登录端点）应共享同一个中间件实例，使同一 IP 的总配额受控，防攻击者跨端点分散：
//
//	loginLimit := wkhttp.StrictIPRateLimitMiddleware(ctx, rds, "login", 10.0/60, 5)
//	v.POST("/user/login", loginLimit, u.login)
//	v.POST("/user/usernamelogin", loginLimit, u.usernameLogin)
//
// tag 区分不同端点组的 Redis keyspace。同一 tag 的多处调用共享配额（等同"同组"），
// 不同 tag 互相隔离。必须是稳定字符串（如 "login"、"register"），不要用随机值
// 或与请求相关的数据，否则滚动部署后会重置配额。
//
// fail-closed（IP 缺失）：归入同一全局桶，与全局 RateLimitMiddleware 行为一致。
// fail-open（Redis 故障）：Redis 调用失败时放行 + 告警，与其余中间件保持一致。
func StrictIPRateLimitMiddleware(ctx context.Context, client *rd.Client, tag string, rps float64, burst int) libwkhttp.HandlerFunc {
	kl := newKeyedLimiter(client, "ratelimit:strict:"+tag+":", rps, burst)
	scope := "strict:" + tag
	var unknownIPWarnOnce sync.Once

	return func(c *libwkhttp.Context) {
		ip := getClientIP(c.Request)
		if ip == "" {
			unknownIPWarnOnce.Do(func() {
				log.Warn("strict rate limit: client IP unavailable, falling back to shared bucket; check reverse proxy / XFF configuration")
			})
			ip = unknownIPKey
		}

		allowed, remaining, retryAfter, _ := kl.allow(c.Request.Context(), ip)
		setRateLimitHeaders(c.Writer.Header(), scope, burst, remaining, allowed, retryAfter)

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"msg":    "请求过于频繁，请稍后再试",
				"status": http.StatusTooManyRequests,
			})
			return
		}

		c.Next()
	}
}

// UIDRateLimitMiddleware 按登录用户 uid 限流。
//
// ⚠️ 挂载要求：必须挂在 AuthMiddleware 之后，且只用于认证路由组。
//
//	r.Group("/v1/foo", ctx.AuthMiddleware(r), wkhttp.UIDRateLimitMiddleware(ctx, rds, 1, 2))
//
// Fail-open 语义（uid 缺失）：读不到 uid（未经 AuthMiddleware 或 token 无效）时直接放行，
// 不按任何维度限流。这意味着本中间件**不具备**未认证场景的防护能力，需配合全局
// per-IP RateLimitMiddleware 作为底线。错误的挂载顺序会导致限流静默失效，请务必用
// AuthMiddleware 前置并在测试中验证。
//
// Fail-open 语义（Redis 故障）：Redis 调用失败时放行 + 告警，不降级为内存桶，
// 避免"挂了就回到有 bug 的状态"把攻击面悄悄放大。
func UIDRateLimitMiddleware(ctx context.Context, client *rd.Client, rps float64, burst int) libwkhttp.HandlerFunc {
	kl := newKeyedLimiter(client, "ratelimit:uid:", rps, burst)
	return func(c *libwkhttp.Context) {
		uidAny, exists := c.Get("uid")
		if !exists {
			c.Next()
			return
		}
		uid, ok := uidAny.(string)
		if !ok || uid == "" {
			c.Next()
			return
		}

		allowed, remaining, retryAfter, _ := kl.allow(c.Request.Context(), uid)
		setRateLimitHeaders(c.Writer.Header(), "uid", burst, remaining, allowed, retryAfter)

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"msg":    "请求过于频繁，请稍后再试",
				"status": http.StatusTooManyRequests,
			})
			return
		}

		c.Next()
	}
}
