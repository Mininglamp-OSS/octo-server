package wkhttp

import (
	"context"
	"os"
	"strconv"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const (
	// 默认 2 rps（120 req/min）+ burst 60：覆盖 Web 端冷启动多接口同步、
	// 快速切换会话、多端同登共享桶等场景。真实 IM 稳态常见 1-2 rps，
	// 偏宽松取 2 留余量；burst 60 吸收瞬时冷启动和连续切会话。
	// 异常放大由 #1083 全局 per-IP 桶兜底。
	defaultUIDRateLimitRPS   = 2.0
	defaultUIDRateLimitBurst = 60

	// UID 限流专用连接池。令牌桶 Lua 脚本是短事务、Redis 端执行时间 <1ms，
	// 不需要大并发池；显式设 10 避免 go-redis 默认 10*NumCPU 在大核机上失控
	// （8 核 × 2 client = 160 连接/副本，×5 副本 = 800 连接，Redis 压力大）。
	uidRateLimitPoolSize = 10
)

var (
	uidRateLimitMu    sync.Mutex
	uidRateLimitMW    libwkhttp.HandlerFunc
	uidRateLimitReady bool
)

// SharedUIDRateLimiter 返回进程级共享的 UID 限流中间件。
//
// 所有调用点共用同一个中间件实例和同一把 Redis keyspace（"ratelimit:uid:{uid}"），
// 即同一 UID 跨所有挂载端点的总配额受控——这是 octo-server#1086 P2 的设计目标，
// 把公平性锚定到"用户"维度，避免共享出网 IP 的办公室场景互相挤占。
//
// 挂载要求：必须放在 AuthMiddleware 之后，仅用于认证路由组；未认证场景下本中间件
// 会 fail-open 放行（详见 UIDRateLimitMiddleware 注释）。
//
//	auth := r.Group("/v1/foo",
//	    ctx.AuthMiddleware(r),
//	    appwkhttp.SharedUIDRateLimiter(ctx),
//	)
//
// 环境变量（进程启动时读取一次，不支持热更新）：
//   - DM_API_UID_RATELIMIT_RPS   每秒填充速率（float，缺省 2.0 = 120 req/min）
//   - DM_API_UID_RATELIMIT_BURST 桶容量（int，缺省 60）
//
// 进程级单例：首次调用时按传入的 ctx 初始化；后续调用忽略 ctx，返回同一个中间件。
// 在集成测试中若需用不同 Redis 实例重建，调用 resetUIDRateLimiterForTest。
func SharedUIDRateLimiter(ctx *config.Context) libwkhttp.HandlerFunc {
	uidRateLimitMu.Lock()
	defer uidRateLimitMu.Unlock()
	if uidRateLimitReady {
		return uidRateLimitMW
	}

	rps := ParseRPSFromEnv("DM_API_UID_RATELIMIT_RPS", defaultUIDRateLimitRPS)
	burst := ParseBurstFromEnv("DM_API_UID_RATELIMIT_BURST", defaultUIDRateLimitBurst)

	// 独立构造 go-redis client 的原因同 main.go：lib 的 redis.Conn 未暴露
	// Eval/Script 接口，令牌桶 Lua 脚本必须走原生 go-redis。生命周期跟随进程。
	// ctx 传 context.Background()：go-redis v6 的 Script.Run 不接受 context；
	// 即便未来升级到 v8+ 也不应传请求 ctx（token 消耗不可因客户端断连回退）。
	client := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = uidRateLimitPoolSize
	}))
	uidRateLimitMW = UIDRateLimitMiddleware(context.Background(), client, rps, burst)
	uidRateLimitReady = true
	return uidRateLimitMW
}

// ParseRPSFromEnv 解析 float 环境变量；缺省或解析失败回退到 def，
// 无效值（负数 / 非法格式）打 Warn 日志，避免操作配置错误静默失败。
func ParseRPSFromEnv(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		log.Warn("invalid rate limit env var, using default",
			zap.String("key", key), zap.String("value", v), zap.Float64("default", def))
		return def
	}
	return n
}

// ParseBurstFromEnv 解析 int 环境变量；语义同 ParseRPSFromEnv。
func ParseBurstFromEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Warn("invalid rate limit env var, using default",
			zap.String("key", key), zap.String("value", v), zap.Int("default", def))
		return def
	}
	return n
}

