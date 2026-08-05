package bot_api

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

// Bot API 限流（issue #696）。
//
// 问题：`/v1/bot` 主组此前**没有任何 per-endpoint / per-bot 限流**，唯一的约束是
// main.go 用 route.Use 挂的全局 per-IP 令牌桶。因为桶按 client IP 分片，
// **同一出网 IP 上的所有 bot 共享一份配额**——2026-08-05 生产事故里，一个 bot 以
// 约 590 rps 冲刷 /v1/bot/typing 就把配额打满，同 IP 其它 bot 的 heartbeat /
// sendMessage / card 请求全部连坐 429，其中一个 bot 因此断联且无法自愈。
//
// 本文件把限流维度从「出网 IP」换成「bot 身份」，并给两条**自愈通道**单独的配额：
//
//	business  —— /v1/bot 主组业务端点，key = robotID
//	heartbeat —— /v1/bot/heartbeat，key = robotID；该端点同时移出全局 per-IP 桶
//	             （见 main.go globalRateLimitExcludePaths），否则邻居仍能把它挤死
//	register  —— /v1/bot/register，key = bot token 指纹（**不是** robotID）
//
// register 用 token 指纹而非 robotID，是因为它挂在 botAPI 组**之外**、跑在
// authBot 之前，此时还没有 bot 身份。而 token 就在请求里，取其哈希即可得到一个
// 稳定且与 bot 一一对应的维度——这正好命中事故里的形态：token 失效的 bot 以约
// 4 rps 持续重试 register（3 分钟 709 次 400），按 token 分桶能把这类风暴关进
// 它自己的桶，不影响同 IP 的健康 bot。
//
// 安全说明：token 是凭据，**只落哈希**，绝不写进 Redis key、日志或指标。
// 换 token 可绕过该维度，兜底仍由全局 per-IP 桶承担（register 未被 exclude）。

// 限流通道分类。取值是**固定枚举**——它会成为 Prometheus label，
// 基数有界依赖于此，不得透传 path 或任何请求内容。
const (
	rateLimitClassBusiness  = "business"
	rateLimitClassHeartbeat = "heartbeat"
	rateLimitClassRegister  = "register"
)

// Redis keyspace。三条通道彼此隔离：共用前缀会让不同语义的流量互相消耗令牌。
const (
	rateLimitKeyPrefixBusiness  = "ratelimit:bot:business:"
	rateLimitKeyPrefixHeartbeat = "ratelimit:bot:heartbeat:"
	rateLimitKeyPrefixRegister  = "ratelimit:bot:register:"
	rateLimitKeyPrefixOffenders = "ratelimit:bot:offenders:"
)

// register 端点的 per-IP strict 桶参数（code review 补入）。
//
// 这是 token 指纹桶**之外**的一层，专门堵 keyspace 放大：token 由客户端提供、
// 在限流前不校验有效性，每换一个随机 token 就换一个新桶，而令牌桶首次判定即
// HMSET+EXPIRE 建 key，于是 live key 数 = 请求速率 × TTL。详见 Route 中的注释。
//
// 固定值而非热调：IP 层是防滥用底线，不随业务负载变化。10 rps 远高于实测正常量
// （单 IP register 约 1~2 rps；事故中那个异常 bot 约 4 rps），但把 key 生成速率
// 从全局桶允许的 1500/s 压到 10/s。
const (
	defaultRegisterIPRPS   = 10.0
	defaultRegisterIPBurst = 50
)

// RateLimitParams 是三条通道的运行时配置快照。
type RateLimitParams struct {
	Business  ratelimit.Params
	Heartbeat ratelimit.Params
	Register  ratelimit.Params
}

// RateLimitParamsProvider 每次判定时被调用，返回当前配置。
//
// 用注入闭包而不是直接读 modules/common，理由与 RedisAppBotRegistry 的 ttl 注入
// 一致：让 bot_api 不依赖 system-settings 包。组合根（main.go）负责把它接到热更新
// 快照上。
type RateLimitParamsProvider func() RateLimitParams

// disabledRateLimitParams 是**未注入时的安全默认**：三条通道全部关闭，
// 于是每次 Check 都走 OutcomeBypassed，行为与本次改动前逐字节一致。
//
// 这条默认值是刻意的：限流层的失败模式应当是「没限流」而不是「误拒」——
// 一个忘了 wiring 的部署不该表现为全部 bot 被拒。
func disabledRateLimitParams() RateLimitParams { return RateLimitParams{} }

var rateLimitParamsProvider atomic.Pointer[RateLimitParamsProvider]

// SetRateLimitParamsProvider 由组合根调用，把限流配置接到热更新的 system_setting
// 快照上。未调用时使用 disabledRateLimitParams（完全旁路）。
func SetRateLimitParamsProvider(fn RateLimitParamsProvider) {
	if fn == nil {
		return
	}
	rateLimitParamsProvider.Store(&fn)
}

func currentRateLimitParams() RateLimitParams {
	if p := rateLimitParamsProvider.Load(); p != nil {
		return (*p)()
	}
	return disabledRateLimitParams()
}

// rateLimitRedisOnce 让限流 client 在进程内单例化，避免每次 New() 都开新连接池
// （同 modules/incomingwebhook 的 sharedRateRedis 与 pkg/wkhttp 的 SharedUIDRateLimiter）。
var (
	rateLimitRedisOnce sync.Once
	rateLimitRedis     *rd.Client
)

func sharedRateLimitRedis(cfg *config.Config) *rd.Client {
	rateLimitRedisOnce.Do(func() {
		// 经 octoredis 构造以确保 cfg.DB.RedisTLS 场景下 TLSConfig 不被遗漏——
		// 否则限流 client 连不上 TLS-only Redis，三条通道会全部 fail-open，
		// 即「限流看起来装上了但从未生效」。
		// PoolSize 显式设 10：令牌桶 Lua 是短事务，与 main.go / incomingwebhook /
		// user / group 等其它限流 client 的全局约定一致。
		rateLimitRedis = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = 10
		})
	})
	return rateLimitRedis
}

// botRateLimiters 持有三条通道。
type botRateLimiters struct {
	business  *ratelimit.Limiter
	heartbeat *ratelimit.Limiter
	register  *ratelimit.Limiter
}

// botRateLimitObserver 把判定结果分发到两层观测：
//
//	Prometheus  —— 无身份、低基数（class × outcome = 20 条 series 封顶）
//	OffenderZSet —— 有身份、有界（top N），只在拒绝/影子拒绝时写
//
// 身份不进 Prometheus 是硬约束：生产实测活跃 bot 2903 个且无上界。
type botRateLimitObserver struct {
	offenders *ratelimit.OffenderRecorder
}

func (o *botRateLimitObserver) Observe(class, key string, outcome ratelimit.Outcome) {
	metrics.ObserveBotRateLimitDecision(class, outcome.String())
	// 只在（影子）拒绝时记名单。每请求都写会让观测本身成为一条与业务同阶的
	// Redis 写路径——那正是限流要防的那种放大。
	if outcome == ratelimit.OutcomeDenied || outcome == ratelimit.OutcomeWouldDeny {
		o.offenders.Record(class, key)
	}
}

func newBotRateLimiters(cfg *config.Config) *botRateLimiters {
	client := sharedRateLimitRedis(cfg)
	obs := &botRateLimitObserver{offenders: ratelimit.NewOffenderRecorder(client, rateLimitKeyPrefixOffenders)}

	// fallback 是**代码内的安全默认**，仅在配置给出非法值（NaN/±Inf/≤0）时顶上。
	// 与 modules/common 的 default* 常量保持同量级；两处漂移只会让读侧防御顶出
	// 一个与运维预期不同的值，故此处刻意写成同样的数。
	return &botRateLimiters{
		business: ratelimit.New(client, rateLimitKeyPrefixBusiness, rateLimitClassBusiness,
			func() ratelimit.Params { return currentRateLimitParams().Business }, obs,
			ratelimit.Params{RPS: 20, Burst: 200}),
		heartbeat: ratelimit.New(client, rateLimitKeyPrefixHeartbeat, rateLimitClassHeartbeat,
			func() ratelimit.Params { return currentRateLimitParams().Heartbeat }, obs,
			ratelimit.Params{RPS: 1, Burst: 10}),
		register: ratelimit.New(client, rateLimitKeyPrefixRegister, rateLimitClassRegister,
			func() ratelimit.Params { return currentRateLimitParams().Register }, obs,
			ratelimit.Params{RPS: 0.5, Burst: 10}),
	}
}

// botTokenFingerprint 把 bot token 折成一个稳定的限流维度。
//
// **只落哈希**：token 是凭据，明文不得进入 Redis key、日志或指标。取 SHA-256 的前
// 16 字节（32 hex）——碰撞概率可忽略，且比全长省 keyspace。
func botTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// rateLimitMiddleware 构造一条限流中间件。
//
// keyFn 返回空串时 Limiter 会旁路（fail-open）：拿不到身份就限不了流。
// 对已鉴权路径而言身份缺失属于挂载顺序错误，应当由测试守卫拦住，
// 而不是在生产上退化成「所有无身份请求共享一个桶」这种更难排查的形态。
func (ba *BotAPI) rateLimitMiddleware(pick func(*botRateLimiters) *ratelimit.Limiter, keyFn func(*wkhttp.Context) string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if ba.rateLimiters == nil {
			c.Next()
			return
		}
		res := pick(ba.rateLimiters).Check(keyFn(c))

		// 旁路与影子两种情况都**不下发** X-RateLimit-* 头。
		// 影子期若下发，客户端会据此自行降频，观测到的就不再是真实流量——
		// 而观测的全部意义就是回答「设成这个配额会拒谁」。
		if res.ShouldSetHeaders() {
			setBotRateLimitHeaders(c, res)
		}
		if res.ShouldReject() {
			respondBotRateLimited(c, res.RetryAfter)
			c.Abort()
			return
		}
		c.Next()
	}
}

// setBotRateLimitHeaders 下发与 octo-lib 三个限流中间件同形的四个头，
// 使客户端的归因逻辑（尤其 X-RateLimit-Scope）无需为 bot 通道特判。
func setBotRateLimitHeaders(c *wkhttp.Context, res ratelimit.Result) {
	h := c.Writer.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(res.Burst))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
	h.Set("X-RateLimit-Scope", "bot")
	if res.Outcome == ratelimit.OutcomeDenied {
		h.Set("Retry-After", strconv.Itoa(res.RetryAfter))
	}
}

// respondBotRateLimited 复用 shared 限流码，使 bot 侧 429 的线上形状与全局/UID 层
// **完全一致**（同一个 error.code、同一个 retry_after detail）。客户端已按该 code
// 分支处理，新增一个 bot 专用码只会让它们多一条分支而没有信息增益。
//
// 用 WithStatus 而非默认门面：`ResponseErrorL` 把 wire status 钉死在 400（D14 兼容），
// 而限流**必须**返回真正的 429——octo-lib 的三个限流中间件走 `c.RenderError` +
// `TransportStatus: 429`，客户端（含 issue #696 报告里的插件）正是按状态码识别限流并
// 决定退避的。若 bot 通道返回 400，同一个系统里就出现了两种"被限流"的表示法，
// 客户端要么为 bot 通道加分支，要么把限流误判成参数错误而停止重试。
//
// 所以这里选 WithStatus 不是"新端点偏离 D14"，恰恰是**与既有限流层保持一致**；
// 集成测试 TestRateLimitEnforcedShape 钉住状态码与四个响应头。
func respondBotRateLimited(c *wkhttp.Context, retryAfter int) {
	var details map[string]any
	if retryAfter > 0 {
		details = map[string]any{"retry_after": retryAfter}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedRateLimited, nil, details)
}
