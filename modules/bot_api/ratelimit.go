package bot_api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
//	heartbeat —— /v1/bot/heartbeat，key = robotID
//	register  —— /v1/bot/register，key = bot token 指纹（**不是** robotID）
//
// heartbeat 与 register **两条都移出了全局 per-IP 桶**（见 main.go
// globalRateLimitExcludePaths），否则邻居 bot 打满共享配额时它们仍会被挤死——
// 只保住 heartbeat 不够：bot 能"发现自己掉线"却仍换不到新 token，永远起不来。
//
// register 用 token 指纹而非 robotID，是因为它挂在 botAPI 组**之外**、跑在
// authBot 之前，此时还没有 bot 身份。而 token 就在请求里，取其哈希即可得到一个
// 稳定且与 bot 一一对应的维度——这正好命中事故里的形态：token 失效的 bot 以约
// 4 rps 持续重试 register（3 分钟 709 次 400），按 token 分桶能把这类风暴关进
// 它自己的桶，不影响同 IP 的健康 bot。
//
// 安全说明：token 是凭据，**只落哈希**，绝不写进 Redis key、日志或指标。
// 换 token 可绕过该维度，兜底由 `bot_register` 的鉴权前 IP 底线承担
// （**不是**全局 per-IP 桶——register 已连同 heartbeat 一起被 exclude，见下）。

// 限流通道分类。取值是**固定枚举**——它会成为 Prometheus label，
// 基数有界依赖于此，不得透传 path 或任何请求内容。
//
// **导出**是因为组合根（main.go）的 provider 按 class 分支取配置——见
// RateLimitParamsProvider。包内继续用小写别名，避免大范围改动。
const (
	RateLimitClassBusiness  = "business"
	RateLimitClassHeartbeat = "heartbeat"
	RateLimitClassRegister  = "register"

	rateLimitClassBusiness  = RateLimitClassBusiness
	rateLimitClassHeartbeat = RateLimitClassHeartbeat
	rateLimitClassRegister  = RateLimitClassRegister
)

// Redis keyspace。三条通道彼此隔离：共用前缀会让不同语义的流量互相消耗令牌。
const (
	rateLimitKeyPrefixBusiness  = "ratelimit:bot:business:"
	rateLimitKeyPrefixHeartbeat = "ratelimit:bot:heartbeat:"
	rateLimitKeyPrefixRegister  = "ratelimit:bot:register:"
	rateLimitKeyPrefixOffenders = "ratelimit:bot:offenders:"
)

// register / heartbeat 的鉴权前 IP 底线。
//
// **这两道桶不是本任务的目标**——目标是 per-bot 限流（按 bot 身份分配额）。
// 它们是两处不得不付的对价：
//
//   - `register` 跑在 `authBot` **之前**，此时没有 bot 身份。token 指纹能挡"失效
//     token 重试风暴"，但它是客户端提供的值，换 token 即换桶；而令牌桶首次判定就
//     建 Redis key，于是轮换 token 会按请求速率造 key。需要一层不依赖客户端提供值
//     的兜底。
//   - `heartbeat` 已被移出全局 per-IP 桶（否则同 IP 邻居会饿死它），而 per-bot 桶挂在
//     `authBot` 之后——无效 token 在鉴权里就 abort，永远走不到那道桶。移出全局桶就
//     必须在鉴权前补一层，否则任意无效 token 可无限触发 `authBot` 的 Redis/DB 查询。
//
// 鉴权前唯一可用的维度是 IP，所以这不是"回到 IP 维度",而是"那个阶段只有 IP"。
//
// **用 octo-lib 的 StrictIPRateLimitMiddleware,不自建**:它正是为未鉴权敏感端点设计
// 的（login/register/sms/search 等 6 处在用），自带 client-IP 提取。自建虽能换来
// 参数热调与配置统一,但要复制 lib 未导出的 `getClientIP`、并偏离 rate-limit 规则
// 明文的第二条——**为一个非目标的对价引入新基础设施不划算**。
//
// 参数走 env(`OCTO_BOT_RATELIMIT_{REGISTER,HEARTBEAT}_IP_{RPS,BURST}`)。
// 与 per-bot 配额不同,IP 底线是防滥用阈值,不需要按影子数据反复收敛;
// env(改 configmap + 滚动重启)对这类参数够用。
//
// **但必须自己消毒**:`ParseRPSFromEnv` 底层是 `strconv.ParseFloat`,会原样接受
// "NaN"/"+Inf";而 lib 的 `newKeyedLimiter` 只检查 `rps <= 0`,**`NaN <= 0` 为 false**,
// 所以 NaN 会穿过它的启动期 panic 校验直达 Lua、让所有算术与比较静默失效。
// 故统一过 `ratelimit.SanitizeRPS/SanitizeBurst`——与 per-bot 层同一份逻辑。
//
// **默认值按「防滥用底线」定，不按实测流量的小倍数定。** 第一版犯的正是这个错：
// 按单 IP 实测峰值（register ~4.4 rps、heartbeat ~45 rps）各留两倍余量，得出
// 10/50 与 100/300。那是**配额**的定值方式，而这两道桶的职责只是"别让未鉴权流量
// 无限打进来"，两者对余量的要求差一个量级：
//
//   - heartbeat：2903 个活跃 bot、心跳 key TTL 60s ⇒ 客户端至少每 60s 打一次，
//     实际约 30s ⇒ **全车队心跳约 97 rps**。实测单 IP 45 rps 说明约半个车队挤在
//     一个出网 IP 后面，那么 100 rps 的余量只有 2.2 倍——车队翻倍、或 NAT 收敛成
//     单出口就顶满。**在一个"被 429 就是事故本身"的端点上留 2.2 倍余量方向是反的。**
//   - register：定这个值的时候它还在全局桶里，原上限是全局的 1500 rps；压到 10 rps
//     是 150 倍收紧，而且落在**自愈路径**上。车队集体重连时（唯一真正要紧的时刻），
//     1450 个 bot 里 burst 只放过前 50 个，其余按 10/s 排队要 140 秒——而客户端已知
//     把 429 与 400 混在一套重试策略里。这条改动的核心论点是"凡是用于恢复的端点都
//     必须在保命集合里"，按平峰量给它裁上限与该论点直接矛盾。
//
// 现值仍然显著收敛，且足以关掉它们各自要关的洞：
//
//   - register 100 rps：keyspace 放大的约束是**key 生成速率有界**，不是"贴着实测"。
//     100 rps × TTL(约 40s) ≈ 4000 个 live key，相对全局 1500 rps 下最坏约 6 万个
//     已收敛一个量级。（且 per-token 桶默认关闭时 Check 在碰 Redis 前就旁路，
//     放大只在该通道开启后才存在。）burst 500 让集体重连在约 10 秒内排空。
//
//     **后续变更**：register 已随 heartbeat 一并移出全局桶（见 main.go
//     globalRateLimitExcludePaths）。这不改变上面的定值——移出前 register 同时受
//     全局 1500 与本底线 100 约束，紧约束本来就是 100，移出后有效上限不变。但它
//     意味着 100 现在是这条路径**唯一**的上限，调它的人没有外层兜底可依赖。
//
//   - heartbeat 500 rps：给全车队 97 rps 留 5 倍余量，同时仍比它原先所在的全局桶
//     1500 rps 紧 3 倍。它要挡的是"无效 token 无限触发 authBot 的 DB 查询"，
//     500 rps 完全够——那条路径的成本在 DB 往返，不在计数。
//
// 环境变量可就地收紧或放宽，无需改代码。
const (
	envRegisterIPRPS    = "OCTO_BOT_RATELIMIT_REGISTER_IP_RPS"
	envRegisterIPBurst  = "OCTO_BOT_RATELIMIT_REGISTER_IP_BURST"
	envHeartbeatIPRPS   = "OCTO_BOT_RATELIMIT_HEARTBEAT_IP_RPS"
	envHeartbeatIPBurst = "OCTO_BOT_RATELIMIT_HEARTBEAT_IP_BURST"

	defaultRegisterIPRPS   = 100.0
	defaultRegisterIPBurst = 500

	defaultHeartbeatIPRPS   = 500.0
	defaultHeartbeatIPBurst = 1500
)

// ipLimitParams 读 env 并消毒。**刻意没有 enabled 开关**:per-bot 层默认关闭
// （灰度需要）,若这两道外层也可关,被 exclude 的 heartbeat 与 register 就会出现
// 完全无防护的未鉴权端点窗口。
func ipLimitParams(envRPS string, defRPS float64, envBurst string, defBurst int) (float64, int) {
	rps := ratelimit.SanitizeRPS(wkhttp.ParseRPSFromEnv(envRPS, defRPS), defRPS)
	burst := ratelimit.SanitizeBurst(wkhttp.ParseBurstFromEnv(envBurst, defBurst), defBurst)
	return rps, burst
}

// requireBotIdentity 断言 `authBot` 已经写入了 bot 身份，否则**fail-closed** 中止。
//
// 为什么不直接用 `botActorUID()`（第二轮 code review P2-2）：那个中间件除了做同样的
// 断言，还会 `c.Set("uid", robotID)`——把 robotID 别名成 `uid`。而 `uid` 正是
// `Context.GetLoginUID()` 读的键，也是 lib `UIDRateLimitMiddleware` 的维度。
// 于是主组内任何 handler 调 `GetLoginUID()` 都会**静默拿到一个 robotID 而不是登录用户**。
// 今天恰好没有 handler 这么做（7 处调用全在 /v1/obo/* user-token 组），但那是
// "此刻为真"而非结构性成立的性质。
//
// 而 per-bot 限流**压根不需要那个别名**:它的 keyFn 是 `getRobotIDFromContext`,
// 读的是 `authBot` 写的 `CtxKeyRobotID`("robot_id")。初版注释声称
// "botActorUID 供 per-bot 限流取维度"——那是错的,别名在这两处挂载上对限流毫无作用,
// 白担了一个授权语义被混淆的风险。
//
// 但也不能只是删掉:`botActorUID` 的断言是 fail-**closed**（身份缺失即中止），
// 而限流器对空 key 是 fail-**open**（旁路放行，见 Limiter.Check 的 OutcomeNoKey）。
// 直接删会把"authBot 通过但没写身份"这种本该报错的状态降级成静默放行。
// 所以保留断言、去掉别名——两个性质各自独立，本来就不该捆在一个中间件里。
func (ba *BotAPI) requireBotIdentity() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if getRobotIDFromContext(c) == "" {
			ba.respondBotAPIIdentityMissing(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// onlyPOST 让非 POST 请求在**已经通过 IP 底线之后**以 404 结束。
//
// 存在理由（第二轮 code review P1-2）：octo-lib 的全局限流按
// `excludeSet[c.Request.URL.Path]` 匹配——**只看路径，不看方法**。而 IP 底线原先只挂在
// `r.POST` 上。于是 `GET /v1/bot/heartbeat` 会：跳过全局桶（路径被豁免）→ 没有对应
// 方法的路由 → 落 gin 的 no-route。全局中间件仍会跑（含访问日志），最后渲染 404。
// 净效果：**任意未鉴权来源可以无界地打 404**，而这两条路径正是全站唯一不受
// 1500 rps/IP 约束的地方（其它路径连不存在的 URL 都走全局桶）。
//
// 单请求成本不高（无 DB、无 Redis），但它违反了 main.go globalRateLimitExcludePaths
// 注释里那条不变量：**豁免不等于无限制**。所以用 `r.Any` 注册、把底线放在最前，
// 再由本中间件收口。
//
// **`r.Any` 不等于"所有方法"——残留两个洞，都不是本改动引入的：**
//
//   - **扩展方法**：gin 1.9.1 的 `anyMethods` 恰好九个（GET/POST/PUT/PATCH/HEAD/
//     OPTIONS/DELETE/CONNECT/TRACE）。`PROPFIND`/`LOCK`/`curl -X FOO` 这类
//     `net/http` 接受的 token 仍然匹配不到路由、仍然跳过只看路径的全局豁免、
//     仍然落无底线的 gin no-route 404。
//   - **OPTIONS**：`wkhttp.CORSMiddleware()` 注册在 `server.New` 里（octo-lib
//     `server/server.go`），排在 `route.Use(RateLimitMiddleware)` **之前**，直接答
//     204，既到不了底线也到不了本中间件。这条是**全站既有**行为，与豁免无关。
//
// 增量攻击能力为零：`/v1/ping`、`/v1/health` 同在豁免名单且**故意完全没有底线**，
// 且 `GET /v1/ping` 比一次路由 miss 更便宜——攻击者拿 `PROPFIND /v1/bot/heartbeat`
// 做不到今天做不到的事。要结构性关掉，得让豁免认方法（根因在 lib，已上游记账
// octo-lib#115），或把两道底线改成按路径守卫的 `route.Use` 中间件——后者能一次覆盖
// no-route 与所有方法，但那是比本改动更大的形状变更。
//
// 为什么返回 404 而不是 405：保持与改动前**状态码**一致（那时也是 404），不引入新的
// 状态码语义。**响应体确实变了**：改动前是 gin 的纯文本 `404 page not found`，现在是
// 本地化 JSON 信封——这些 (路径, 方法) 组合从来不是有效调用，故认为无影响。用 i18n
// 信封而非 `c.AbortWithStatus`，因为后者被 `make i18n-lint` 的 D23 守卫禁止；
// `WithStatus` 门面在这里不涉及 D14 兼容风险——这个组合此前**从未**返回过信封响应，
// 不存在依赖固定 400 的历史客户端。
func onlyPOST(c *wkhttp.Context) {
	if c.Request.Method != http.MethodPost {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		c.Abort()
		return
	}
	c.Next()
}

// RateLimitParams 是三条通道的运行时配置快照。主要给测试与组合根做便利形状用；
// 判定路径本身**按通道单独取值**，不构造整个快照（见 RateLimitParamsProvider）。
type RateLimitParams struct {
	Business  ratelimit.Params
	Heartbeat ratelimit.Params
	Register  ratelimit.Params
}

// forClass 把快照按 class 拆出单条通道，供 provider 形状适配用。
func (p RateLimitParams) forClass(class string) ratelimit.Params {
	switch class {
	case rateLimitClassBusiness:
		return p.Business
	case rateLimitClassHeartbeat:
		return p.Heartbeat
	case rateLimitClassRegister:
		return p.Register
	default:
		return ratelimit.Params{}
	}
}

// RateLimitParamsProvider 每次判定时被调用，返回**该 class** 当前的配置。
//
// 用注入闭包而不是直接读 modules/common，理由与 RedisAppBotRegistry 的 ttl 注入
// 一致：让 bot_api 不依赖 system-settings 包。组合根（main.go）负责把它接到热更新
// 快照上。
//
// **按 class 取值而非返回三条通道的快照**（第二轮 code review P2-3）：初版签名是
// `func() RateLimitParams`，于是每个请求都要构造全部三条通道——12 个 getter，
// 每个 getter 的默认值参数还会被**急切求值**（`os.Getenv` + 解析），
// 而一次判定只用得上其中一条。这条路径实测被单个 bot 打到过约 590 rps，
// 且在出厂的全关状态下答案恒为 bypassed。按 class 取值把成本降到 1/3，
// 再配合组合根里的 `enabled` 短路，全关状态下每请求只读 1 个配置项。
type RateLimitParamsProvider func(class string) ratelimit.Params

// disabledRateLimitParams 是**未注入时的安全默认**：任何 class 都返回零值
// （Enabled=false），于是每次 Check 都走 OutcomeBypassed，行为与本次改动前逐字节一致。
//
// 这条默认值是刻意的：限流层的失败模式应当是「没限流」而不是「误拒」——
// 一个忘了 wiring 的部署不该表现为全部 bot 被拒。
func disabledRateLimitParams(string) ratelimit.Params { return ratelimit.Params{} }

var rateLimitParamsProvider atomic.Pointer[RateLimitParamsProvider]

// SetRateLimitParamsProvider 由组合根调用，把限流配置接到热更新的 system_setting
// 快照上。未调用时使用 disabledRateLimitParams（完全旁路）。
func SetRateLimitParamsProvider(fn RateLimitParamsProvider) {
	if fn == nil {
		return
	}
	rateLimitParamsProvider.Store(&fn)
}

func currentRateLimitParams(class string) ratelimit.Params {
	if p := rateLimitParamsProvider.Load(); p != nil {
		return (*p)(class)
	}
	return disabledRateLimitParams(class)
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
			func() ratelimit.Params { return currentRateLimitParams(rateLimitClassBusiness) }, obs,
			ratelimit.Params{RPS: 20, Burst: 200}),
		heartbeat: ratelimit.New(client, rateLimitKeyPrefixHeartbeat, rateLimitClassHeartbeat,
			func() ratelimit.Params { return currentRateLimitParams(rateLimitClassHeartbeat) }, obs,
			ratelimit.Params{RPS: 1, Burst: 10}),
		register: ratelimit.New(client, rateLimitKeyPrefixRegister, rateLimitClassRegister,
			func() ratelimit.Params { return currentRateLimitParams(rateLimitClassRegister) }, obs,
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
func (ba *BotAPI) rateLimitMiddleware(pick func(*botRateLimiters) *ratelimit.Limiter, keyFn func(*wkhttp.Context) string, scope string) wkhttp.HandlerFunc {
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
			setBotRateLimitHeaders(c, res, scope)
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
func setBotRateLimitHeaders(c *wkhttp.Context, res ratelimit.Result, scope string) {
	h := c.Writer.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(res.Burst))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
	h.Set("X-RateLimit-Scope", scope)
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
