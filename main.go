package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/module"
	libdb "github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	libredis "github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/tokenlifecycle"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	cardtemplatecatalog "github.com/Mininglamp-OSS/octo-server/modules/card_template_catalog"
	commonmodule "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/accesslog"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
	docsshared "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_shared"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
	summaryfailed "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_failed"
	octodb "github.com/Mininglamp-OSS/octo-server/pkg/db"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	ratelimitpkg "github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
	"github.com/judwhite/go-svc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robfig/cron"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// go ldflags
var Version string    // version
var Commit string     // git commit id
var CommitDate string // git commit date
var TreeState string  // git tree state

// globalRateLimitExcludePaths 列出**不**参与全局 per-IP 令牌桶的路径。
//
// 每加一条都是在 DDoS 底线上开洞，因此必须逐条论证，并且被豁免的端点**必须**另有
// 自己的配额——豁免不等于无限制。这条不变量由 main_test.go 的
// TestExcludedBotPathsEachHaveTheirOwnFloor 结构性钉住（不是靠这段注释）。
//
//   - /v1/ping、/v1/health：存活探针，被限流会让 k8s 误判实例不健康。
//
// 以下两条是 issue #696 的**自愈通道**，必须成对理解。全局桶按 client IP 分片，
// 于是同一出网 IP 上的所有 bot 共享一份配额；2026-08-05 生产事故中，一个 bot 以约
// 590 rps 冲刷业务端点就把配额打满，同 IP 其它 bot 无差别被拒。**组级中间件绕不过
// route.Use**，所以把路径移出全局桶是让它不被邻居饿死的唯一手段。
//
//   - /v1/bot/heartbeat：bot 保活通道，是发现自己掉线的唯一手段。心跳被业务流量
//     挤掉是纯粹的连带损害——它本身流量极小。上限改由 per-bot heartbeat 桶与鉴权前的
//     strict:bot_heartbeat IP 底线共同承担。
//   - /v1/bot/register：**重连时刷 IM token 的必经之路**，即"爬起来"的能力。
//     第一版漏了它，于是死锁链条仍然通着：邻居打满全局桶 → heartbeat 因已豁免而
//     活着（能发现掉线）→ register 仍被全局桶拒 → 拿不到新 token → 永远回不来。
//     **保住"发现掉线"却没保住"爬起来"，最终结果一样是断联。**
//     它同样已有自己的 strict:bot_register IP 底线（100 rps / burst 500，
//     见 modules/bot_api/ratelimit.go），与 heartbeat 那道结构完全一致——
//     所以"豁免会让它失去 IP 层防护"这个原始顾虑不再成立。
//
// > 一般性判据：**凡是"失败后用于恢复"的端点都属于保命集合**，不能只保住其中一环。
func globalRateLimitExcludePaths() []string {
	return []string{"/v1/ping", "/v1/health", "/v1/bot/heartbeat", "/v1/bot/register"}
}

func loadConfigFromFile(cfgFile string) *viper.Viper {
	vp := viper.New()
	vp.SetConfigFile(cfgFile)
	if err := vp.ReadInConfig(); err != nil {
		panic(fmt.Sprintf("Failed to load config file %s: %v", cfgFile, err))
	}
	fmt.Println("Using config file:", vp.ConfigFileUsed())
	return vp
}

func validateTokenExpireConfig(vp *viper.Viper) (time.Duration, error) {
	return tokenlifecycle.ValidateTokenExpire(vp)
}

func main() {
	var CfgFile string //config file
	flag.StringVar(&CfgFile, "config", "configs/tsdd.yaml", "config file")
	flag.Parse()
	vp := loadConfigFromFile(CfgFile)
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	tokenExpire, err := validateTokenExpireConfig(vp)
	if err != nil {
		panic(err)
	}

	gin.SetMode(gin.ReleaseMode)

	cfg := config.New()
	cfg.Version = Version
	cfg.ConfigureWithViper(vp)
	cfg.Cache.TokenExpire = tokenExpire
	fmt.Println("Effective access token TTL:", cfg.Cache.TokenExpire)

	// 安全校验：release 模式下禁止配置 smsCode（万能验证码后门）
	if err := commonapi.ValidateTestCodeConfig(cfg); err != nil {
		panic(err)
	}

	// 初始化context
	ctx := config.NewContext(cfg)
	ctx.Event = event.New(ctx)

	logOpts := log.NewOptions()
	logOpts.Level = cfg.Logger.Level
	logOpts.LineNum = cfg.Logger.LineNum
	logOpts.LogDir = cfg.Logger.Dir
	log.Configure(logOpts)

	var serverType string
	if len(os.Args) > 1 {
		serverType = strings.TrimSpace(os.Args[1])
		serverType = strings.Replace(serverType, "-", "", -1)
	}

	if serverType == "api" || serverType == "" || serverType == "config" { // api服务启动
		runAPI(ctx)
	}

}

func runAPI(ctx *config.Context) {
	// 在注册 recovery 之前给 gin 的 panic 输出包一层 token 脱敏 writer：octo-lib 的
	// server.New → wkhttp.New 内部装 gin.Recovery()。release 模式下 gin 在 broken-pipe
	// 分支会 dump 整条请求行（含 incoming-webhook 推送路由 path 里的明文 token），panic
	// value 本身也可能带上该 path——两者都会绕过 access logger 落进 DefaultErrorWriter。
	// gin.Recovery 在注册时即捕获 DefaultErrorWriter，故必须在 server.New 之前替换（#246）。
	gin.DefaultErrorWriter = accesslog.NewErrorWriter(gin.DefaultErrorWriter)

	// 创建server
	s := server.New(ctx)
	route := s.GetRoute()
	ctx.SetHttpRoute(route)
	defaultLanguage, err := octoi18n.DefaultLanguageFromEnv()
	if err != nil {
		panic(err)
	}
	if err := octoi18n.ValidateRuntimeLocales(defaultLanguage); err != nil {
		panic(fmt.Errorf("validate i18n runtime locales: %w", err))
	}
	route.SetErrorRenderer(octoi18n.NewErrorRenderer(octoi18n.NewLocalizer(defaultLanguage)))
	// 注入自定义 TokenParser，替代 octo-lib legacyTokenParser：以 pkg/auth.Decode
	// 解析 cache value，支持 v2 JSON envelope 与 "uid@name[@role]" 旧格式（i18n D19/D21）。
	// 同时注入 LanguageResolver，让 AuthMiddleware 把 user_language:{uid} → DB 的
	// 真相源结果写到 UserInfo.Language 上，供 i18n.LanguageFromContext 读侧合并。
	// 同时注入 RoleResolver：系统角色（admin/superAdmin）此前烧死在 token 里，
	// 降权 / 删除管理员要到 token 过期才生效。这里在 Parse 时按 uid 实时解析 DB
	// 角色（user_role:{uid} 热缓存 → DB），把降权生效窗口收敛到缓存 TTL。
	userLangSvc := user.NewLanguageService(user.NewDB(ctx), ctx.Cache())
	userRoleSvc := user.NewRoleService(user.NewDB(ctx), ctx.Cache())
	tokenStore, sessionRedis := auth.SessionStoreAndClientForContext(ctx)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := tokenStore.Probe(probeCtx); err != nil {
		probeCancel()
		panic(fmt.Errorf("verify authentication session Redis Lua support: %w", err))
	}
	probeCancel()
	route.SetTokenParser(auth.NewCacheTokenParser(
		ctx.Cache(),
		ctx.GetConfig().Cache.TokenCachePrefix,
		auth.WithTokenValidator(auth.NewTokenValidator(tokenStore, ctx.GetConfig().Cache.TokenCachePrefix)),
		auth.WithLanguageResolver(userLangSvc),
		auth.WithRoleResolver(userRoleSvc),
	))
	// 替换web下的配置文件
	replaceWebConfig(ctx.GetConfig())
	// 初始化api
	trustedLangCIDRs, err := octoi18n.TrustedLangHeaderCIDRsFromEnv()
	if err != nil {
		panic(err)
	}
	trustedProxyCIDRs, err := octoi18n.TrustedProxyCIDRsFromEnv()
	if err != nil {
		panic(err)
	}
	// trace_id 中间件（最前）：为每次请求生成/透传一个 trace_id，写进 gin.Context
	// （供 access-log formatter 与 handler 读）和 request context（供 pkg/auth 的
	// token parser 读）。让 GIN access log / token 解析 / handler / service 四条
	// 分段慢日志能按 trace_id 串到同一次请求。复用入站 X-Request-ID（若有）。
	route.UseGin(func(c *gin.Context) {
		id := reqid.Sanitize(c.GetHeader("X-Request-ID"))
		if id == "" {
			id = reqid.New()
		}
		c.Set(reqid.GinKey, id)
		c.Request = c.Request.WithContext(reqid.WithTraceID(c.Request.Context(), id))
		c.Writer.Header().Set("X-Request-ID", id)
		c.Next()
	})
	route.UseGin(octoi18n.EarlyMiddleware(octoi18n.MiddlewareOptions{
		DefaultLanguage:        defaultLanguage,
		TrustedLangHeaderCIDRs: trustedLangCIDRs,
		TrustedProxyCIDRs:      trustedProxyCIDRs,
	}))
	route.UseGin(ctx.Tracer().GinMiddle()) // 需要放在 api.Route(s.GetRoute())的前面
	// HTTP 入口指标(per-route latency / status / in-flight)。
	// 装在 tracer 之后, 以便未来 histogram exemplar 能拿到 trace context;
	// 装在 RateLimit 之前, 以记录 429 响应(被限流的请求也是真实流量)。
	// 指标走全局 DefaultRegisterer, 与 modules/oidc/metrics.go 共享 /metrics 端点。
	httpMetrics := metrics.NewHTTPMetrics(prometheus.DefaultRegisterer)
	route.UseGin(httpMetrics.GinMiddleware())
	// 用自定义 LogFormatter 替换 gin 默认 access logger：incoming-webhook 推送路由
	// /v1/incoming-webhooks/{webhook_id}/{token} 把明文 token 写在 URL path 里，
	// gin 默认 logger 会把整条 path 落进 access log，泄漏 token。accesslog.Formatter
	// 与默认行格式一致，但对该 path 的 token 段做脱敏（见 #246）。logger 只构造一次复用。
	accessLogger := gin.LoggerWithFormatter(accesslog.Formatter)
	route.UseGin(func(c *gin.Context) {
		ingorePaths := ingorePaths()
		for _, ingorePath := range ingorePaths {
			if ingorePath == c.FullPath() {
				return
			}
		}
		accessLogger(c)
	})
	// 全局 per-IP 作为 DDoS 底线：办公室共享出网 IP 下 IM 基础量就能到 100+ rps
	// （每人 1-2 rps × 数十人），200 余量过小；真实 DDoS 常数千 rps+，底线设 500
	// 更合理。精细限流交给 UID 层和端点级严格桶（#1090）。
	// 无效环境变量值回退到默认值 + Warn 日志，行为与 UID 层 helper 一致。
	rps := libwkhttp.ParseRPSFromEnv("DM_API_RATELIMIT_RPS", 500.0)
	burst := libwkhttp.ParseBurstFromEnv("DM_API_RATELIMIT_BURST", 1000)
	// 限流状态存 Redis，多副本共享配额；与 dmwork-lib 的 GetRedisConn 指向同一实例。
	// 独立构造 client 的原因：lib 的 redis.Conn 未暴露 Eval/Script 接口，
	// 而令牌桶需要 Lua 脚本保证原子性。
	// 生命周期：跟随进程存续，不显式 Close——与 lib 自身的 redis.Conn 处理方式一致。
	// PoolSize 显式设 10：令牌桶 Lua 脚本是短事务，Redis 端 <1ms，不需要大池；
	// go-redis v6 默认 10*NumCPU 在大核机上会失控（多副本 × 多 client 连接数叠加）。
	rlRedis := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	route.Use(route.RateLimitMiddleware(context.Background(), rlRedis, rps, burst, globalRateLimitExcludePaths()...))
	// 上游依赖可观测性(issue #440 P0-a):依赖调用延迟 + 连接池指标,均注册到
	// DefaultRegisterer,经 /metrics 暴露(与 httpMetrics 同一端点)。连接池用
	// scrape 时读取的 Collector,不起后台 goroutine。此处 rlRedis 与 ctx.DB().DB
	// 均已就绪,且在 api.Route 之前完成注册。
	metrics.NewDependencyMetrics(prometheus.DefaultRegisterer)
	// 头像渲染缓存指标(issue#480):命中率 / 渲染耗时 / inflight / 信号量等待 /
	// singleflight 合并 / 304。由 modules/user 的 avatarCache 经包级 Observe 函数灌入;
	// 此处注册即可,未注册前这些 Observe 为 no-op,不影响已构造的 cache。
	metrics.NewAvatarMetrics(prometheus.DefaultRegisterer)
	metrics.NewSessionMetrics(prometheus.DefaultRegisterer)
	metrics.SetSessionEffectiveTTL(ctx.GetConfig().Cache.TokenExpire)
	// 自定义贴纸 handle 上线观测指标(P0: Sticker Handle Enforcement Rollout):上传签发
	// handle 次数 / 注册结果分布 / 部署姿态 gauge。由 modules/file(签发) 与
	// modules/sticker(注册) 经包级 Observe 函数灌入;此处注册即可,未注册前为 no-op。
	// 部署姿态 gauge(能力=OCTO_MASTER_KEY / 策略=system_setting sticker.handle_required)
	// 由 sticker 模块在 New() 时落值——策略源在 system_setting(common 模块),故不在此组合根
	// 设置,避免反向依赖 modules/sticker。
	metrics.NewStickerMetrics(prometheus.DefaultRegisterer)
	// Bot API per-bot 限流的观测与配置接线(issue #696)。必须在 register.GetModules
	// 构造 bot_api 之前完成:限流参数虽是每次判定时才读(热调),但把 wiring 放在模块
	// 构造前可以避免"进程启动到注入之间的窗口里判定按默认值(全关)进行"这种时序依赖。
	//
	// provider 用闭包注入而非让 bot_api 直接读 modules/common,与
	// bot_api.NewRedisAppBotRegistry 的 ttl 注入同源——bot_api 刻意不依赖
	// system-settings 包。未注入时 bot_api 侧回退为"三条通道全关"(完全旁路),
	// 即行为与本改动前一致:限流层的失败模式应当是"没限流"而不是"误拒"。
	metrics.NewBotRateLimitMetrics(prometheus.DefaultRegisterer)
	// settings 指针在**接线时**捕获一次,不在每次判定时取。EnsureSystemSettings 会取
	// 进程级 sharedMu 才返回那个既有指针,而这段闭包跑在每一个 /v1/bot 请求上——
	// 实测被单个 bot 打到过约 590 rps。热更新不受影响:快照在 SystemSettings 内部
	// 由原子指针换出,getter 每次读到的都是最新值（Load 不持 sharedMu）。
	botRateLimitSettings := commonmodule.EnsureSystemSettings(ctx)
	// 按 class 取值:一次判定只读它自己那一条通道的配置项,而不是构造全部三条
	// （第二轮 code review P2-3）。**并且先看 enabled 再读其余**——出厂状态三条全关,
	// 此时每请求只读 1 个配置项;若不短路,getter 的默认值参数会被急切求值,
	// 每个都跑一次 os.Getenv + 解析,在恒为 bypassed 的路径上纯属浪费。
	bot_api.SetRateLimitParamsProvider(func(class string) ratelimitpkg.Params {
		s := botRateLimitSettings
		switch class {
		case bot_api.RateLimitClassBusiness:
			if !s.BotRateLimitBusinessEnabled() {
				return ratelimitpkg.Params{}
			}
			return ratelimitpkg.Params{
				Enabled: true,
				DryRun:  s.BotRateLimitBusinessDryRun(),
				RPS:     s.BotRateLimitBusinessRPS(),
				Burst:   s.BotRateLimitBusinessBurst(),
			}
		case bot_api.RateLimitClassHeartbeat:
			if !s.BotRateLimitHeartbeatEnabled() {
				return ratelimitpkg.Params{}
			}
			return ratelimitpkg.Params{
				Enabled: true,
				DryRun:  s.BotRateLimitHeartbeatDryRun(),
				RPS:     s.BotRateLimitHeartbeatRPS(),
				Burst:   s.BotRateLimitHeartbeatBurst(),
			}
		case bot_api.RateLimitClassRegister:
			if !s.BotRateLimitRegisterEnabled() {
				return ratelimitpkg.Params{}
			}
			return ratelimitpkg.Params{
				Enabled: true,
				DryRun:  s.BotRateLimitRegisterDryRun(),
				RPS:     s.BotRateLimitRegisterRPS(),
				Burst:   s.BotRateLimitRegisterBurst(),
			}
		default:
			// 未知 class ⇒ 全关。新增通道忘了接线时表现为「没限流」而不是「误拒」。
			return ratelimitpkg.Params{}
		}
	})
	// Install the one process registry before register.GetModules constructs
	// module instances. The foundation rollout deliberately has no production
	// producer registrations; later enablement injects only a bound Sender into
	// its owning module after the cross-repository route/contract gates pass.
	if err := installCardDispatch(ctx); err != nil {
		panic(fmt.Errorf("install internal card dispatch registry: %w", err))
	}
	cardTmplRegistry := installCardTmplRegistry() // built-ins: registration + Freeze
	runtimeCatalog, err := cardtemplatecatalog.InstallRuntimeCatalog(
		ctx.DB().DB,
		cardTmplRegistry,
		cardtmpl.RuntimeCatalogConfig{},
	)
	if err != nil {
		panic(fmt.Errorf("install card template runtime catalog: %w", err))
	}
	commonmodule.SetCardTemplateCatalogReadinessCheck(runtimeCatalog.CheckReady)
	cardActionRuntime, err := installCardActionDispatch(ctx)
	if err != nil {
		panic(fmt.Errorf("install card action callback dispatch: %w", err))
	}
	// 构造进程级共享头像渲染缓存,并把观测 hooks 接到上面注册的头像指标。所有头像端点
	// (user 的 UserAvatar;群组头像渲染合并后亦然——#478)经 avatarrender.GetOrRender
	// 共用这一个实例:共享 LRU + 同一个渲染信号量(后者唯一,才是真正的进程级渲染并发
	// 上限)。hooks 在组合根注入,使 pkg/avatarrender 不依赖 pkg/metrics。构造失败则不设
	// 默认,GetOrRender 退化为直接渲染,不阻断启动。
	avatarCfg := avatarrender.ConfigFromEnv()
	avatarCfg.Hooks = avatarrender.Hooks{
		OnHit:           metrics.ObserveAvatarCacheHit,
		OnMiss:          metrics.ObserveAvatarCacheMiss,
		OnShared:        metrics.ObserveAvatarSingleflightShared,
		OnRender:        metrics.ObserveAvatarRender,
		OnSemaphoreWait: metrics.ObserveAvatarSemaphoreWait,
		OnInflight:      metrics.AddAvatarRenderInflight,
	}
	if avatarCache, err := avatarrender.NewCache(avatarCfg); err != nil {
		log.Warn("构造头像渲染缓存失败,退化为无缓存", zap.Error(err))
	} else {
		avatarrender.SetDefaultCache(avatarCache)
	}
	// 把 octo-lib 客户端接缝的耗时回调接到 DependencyMetrics:
	//   - MySQL: db.NewMySQL 的 connect/query 计时 → dependency="mysql"
	//   - Redis: pkg/redis 客户端每条命令计时 → dependency="redis"
	// observer 在调用时按 atomic 解析,construct 时即已挂 hook,故放在此处(指标族
	// 注册之后、serve 之前)即可,不会漏掉后续流量。
	//
	// 覆盖范围:dependency="redis" 覆盖共享缓存 ctx.GetRedisConn()(数据面大头),
	// 以及所有需要 Eval/Script/SetNX 而走裸 *rd.Client 的控制面客户端(限流 rlRedis、
	// OIDC state/bind/logout/sync_lock、bot registry、user/group/space auth、health、
	// 各类 Lua 锁等)—— 后者统一经 octoredis.NewInstrumentedClient/InstrumentedClientFromOptions
	// 构造,在构造时即插桩(octo-lib#104)。仍在覆盖外的只有绕过 octoredis 直接
	// rd.NewClient 的极少数场景(若有),用连接池指标兜底。
	libdb.SetDBObserver(metrics.ObserveDB)
	libredis.SetRedisObserver(metrics.ObserveRedisCmd)
	metrics.RegisterPoolCollectors(prometheus.DefaultRegisterer, ctx.DB().DB, map[string]*rd.Client{
		"ratelimit":            rlRedis,
		"session":              sessionRedis,
		"card_action_dispatch": cardActionRuntime.redisClient,
	})
	// 在所有指标族注册完成后再起 scrape 端点,避免启动窗口内的 scrape 漏掉
	// 依赖/连接池指标族(Jerry-Xin #442 review)。
	metricsSrv := startMetricsScrapeServer()
	// CORS 白名单覆盖：dmwork-lib 的 server.New 默认注入 "*" + Credentials:true，
	// 本中间件在其后执行，按 DM_CORS_ALLOWED_ORIGINS 重写/剥离 Allow-Origin/Credentials。
	// 未配置时等价于禁用跨域（剥离所有 CORS 响应头），仅允许同源调用。
	route.UseGin(libwkhttp.SecureCORSOverrideMiddleware(
		libwkhttp.ParseAllowedOrigins(os.Getenv("DM_CORS_ALLOWED_ORIGINS")),
	))
	// Legacy-database upgrade shim: rewrite the historical filename IDs in
	// gorp_migrations to the new timestamp-prefixed format before
	// module.Setup (which internally calls migrate.Exec) runs. Without
	// this, sql-migrate's PlanMigration stage panics with "unknown
	// migration in database" the moment it sees an ID that's no longer
	// on disk. Idempotent: a fresh install (gorp_migrations table absent)
	// is a clean no-op; restarting an already-rewritten database is also
	// a no-op.
	rewriteCtx, rewriteCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := octodb.RewriteLegacyMigrationIDs(rewriteCtx, ctx.DB().DB); err != nil {
		rewriteCancel()
		panic(fmt.Errorf("rewrite legacy migration IDs: %w", err))
	}
	// Snapshot-built-thread compatibility: when an older init-db.sql
	// already created thread / thread_member / thread_setting but
	// gorp_migrations has no matching thread-* rows, pre-seed those six
	// IDs. The thread module's SQLDir is now registered unconditionally,
	// so without this reconciliation sql-migrate would see the embedded
	// thread migrations as un-applied, try to run `CREATE TABLE thread`
	// (no IF NOT EXISTS) against an existing table, and panic with
	// MySQL 1050.
	if err := octodb.ReconcileThreadSchemaRecords(rewriteCtx, ctx.DB().DB); err != nil {
		rewriteCancel()
		panic(fmt.Errorf("reconcile thread schema records: %w", err))
	}
	rewriteCancel()

	// 模块安装
	err = module.Setup(ctx)
	if err != nil {
		cardActionRuntime.Stop()
		panic(err)
	}
	if err := cardActionRuntime.Start(context.Background()); err != nil {
		cardActionRuntime.Stop()
		panic(fmt.Errorf("start card action callback dispatcher: %w", err))
	}
	//开始定时处理事件
	cn := cron.New()
	//定时发布事件 每59秒执行一次
	err = cn.AddFunc("0/59 * * * * ?", func() {
		if ev, ok := ctx.Event.(*event.Event); ok {
			ev.EventTimerPush()
		} else {
			log.Warn("ctx.Event is not *event.Event, skipping timer push")
		}
	})
	if err != nil {
		panic(err)
	}
	cn.Start()

	// 打印服务器信息
	printServerInfo(ctx)

	// 运行: 阻塞直到 go-svc 收到 SIGINT/SIGTERM 并完成业务 Stop。
	err = svc.Run(s)
	cardActionRuntime.Stop()

	// 业务停下后再 graceful shutdown metrics scrape 端点 — 时序上避开和 go-svc
	// 的信号处理竞态(go-svc 自己调 signal.Notify, 我们不再额外抢信号)。
	// 此时业务已停, /metrics 是最后一项可达服务, 让 Prometheus 拿到末次状态再断开。
	if metricsSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
			log.Warn("metrics scrape graceful shutdown error", zap.Error(shutdownErr))
		}
	}

	if err != nil {
		panic(err)
	}
}

func installCardDispatch(ctx *config.Context) error {
	actionRoutes, err := cardactiondispatch.LoadRouteSpecs(os.Getenv("OCTO_CARD_ACTION_ROUTES"))
	if err != nil {
		return err
	}
	actionProducers, err := cardActionApprovalProducerSpecs(actionRoutes)
	if err != nil {
		return err
	}
	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.DefaultRegisterer),
		Logger:           log.NewTLog("CardDispatch"),
	}
	producerSpecs := append(cardDispatchProducerSpecs(), actionProducers...)
	registry := carddispatch.NewRegistry(deps, producerSpecs)
	return carddispatch.Install(ctx, registry)
}

func cardActionApprovalProducerSpecs(routes []cardactiondispatch.RouteSpec) ([]carddispatch.ProducerSpec, error) {
	seen := make(map[cardactiondispatch.NotifyCapability]struct{})
	result := make([]carddispatch.ProducerSpec, 0)
	for _, route := range routes {
		if route.NotifyTokenEnv == "" {
			continue
		}
		capability := cardactiondispatch.NotifyCapability{SenderUID: route.SenderUID, Owner: route.Owner}
		if capability.SenderUID != notify.NotifyBotUIDValue {
			return nil, errors.New("action notify routes must use the shared notification sender")
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		result = append(result, carddispatch.ProducerSpec{
			ID: notify.ActionNotifyProducerID(capability.Owner), Enabled: true,
			SenderUID:           capability.SenderUID,
			AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
			AllowedProfiles:     []string{cardmsg.ProfileV2},
			ActionEventOwner:    capability.Owner,
			SpacePolicy:         carddispatch.SpacePolicySystemNotification,
			GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
			MaxInFlight:         20,
		})
	}
	return result, nil
}

func cardDispatchProducerSpecs() []carddispatch.ProducerSpec {
	// Every producer here shares the same shape (existing `notification` User
	// Bot, DM-only, display-only octo/v1, system-notification Space policy,
	// in-flight 20/process; brief › pilot table, all rows confirmed
	// 2026-07-13). modules/notify obtains each producer's bound Sender via
	// carddispatch.SenderFromContext.
	//
	// Registrations are inert until modules/notify receives a matching
	// structured card request (NotifyReq.Card or NotifyReq.DocsCard) AND
	// OCTO_CARD_MESSAGE_ENABLED is on. End-to-end enablement of a producer
	// additionally depends on octo-web shipping the matching deep-link route
	// (/s/:taskId for summary — not yet on main; /d/:docId for docs — already
	// live) and the caller-side switching from text payloads to structured
	// fields.
	base := carddispatch.ProducerSpec{
		Enabled:             true,
		SenderUID:           notify.NotifyBotUIDValue,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired, // unused at DM pilot; must be a valid value
		MaxInFlight:         20,
	}
	summarySpec := base
	summarySpec.ID = summaryNotifyProducerID
	docsSpec := base
	docsSpec.ID = docsNotifyProducerID
	actionOutcomeSpec := base
	actionOutcomeSpec.ID = actionOutcomeProducerID
	if notify.DocsApprovalCardsEnabled() {
		docsSpec.AllowedProfiles = []string{cardmsg.ProfileV1, cardmsg.ProfileV2}
		docsSpec.ActionEventOwner = "docs"
	}
	return []carddispatch.ProducerSpec{summarySpec, docsSpec, actionOutcomeSpec}
}

type cardActionDispatchRuntime struct {
	dispatcher  *cardactiondispatch.Dispatcher
	redisClient *rd.Client
}

func (r *cardActionDispatchRuntime) Start(ctx context.Context) error {
	if r == nil {
		return errors.New("card action dispatch runtime unavailable")
	}
	if r.dispatcher == nil {
		// Inert runtime: OCTO_CARD_MESSAGE_ENABLED is off, so no dispatch worker
		// was constructed. Nothing to start — boot must not panic on the gate.
		return nil
	}
	return r.dispatcher.Start(ctx)
}

func (r *cardActionDispatchRuntime) Stop() {
	if r == nil {
		return
	}
	if r.dispatcher != nil {
		r.dispatcher.Stop()
	}
	if r.redisClient != nil {
		_ = r.redisClient.Close()
	}
}

func installCardActionDispatch(ctx *config.Context) (*cardActionDispatchRuntime, error) {
	specs, err := cardactiondispatch.LoadRouteSpecs(os.Getenv("OCTO_CARD_ACTION_ROUTES"))
	if err != nil {
		return nil, err
	}
	if legacy := strings.TrimSpace(os.Getenv("OCTO_CARD_ACTION_ALLOWED_URLS")); legacy != "" {
		// OCTO_CARD_ACTION_ROUTES[].url is itself the exact allowlist since the
		// http-actions follow-up. The dual-write env is intentionally ignored to
		// avoid drift; keep the WARN so rolling upgrades surface the leftover.
		log.Warn("OCTO_CARD_ACTION_ALLOWED_URLS is deprecated and ignored; remove it from the deployment configuration",
			zap.String("deprecated_env", "OCTO_CARD_ACTION_ALLOWED_URLS"))
	}
	registry, err := cardactiondispatch.NewRegistry(specs, os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := registry.ValidateNotifyTokenExclusions(
		os.Getenv("NOTIFY_INTERNAL_TOKEN"),
		os.Getenv("OCTO_DOCS_NOTIFY_TOKEN"),
		os.Getenv("OCTO_DOCS_BOT_MENTION_TOKEN"),
		// Cross-capability exclusion for OCTO_DRIVE_INTERNAL_TOKEN vs the
		// dynamic route-scoped notify tokens / callback secrets loaded from
		// OCTO_CARD_ACTION_ROUTES MUST happen here — modules/internal_resolve
		// only sees the four fixed internal-token envs and cannot detect a
		// collision with route-level credentials. Without this argument, an
		// operator who accidentally sets the drive token equal to a route's
		// notify_token_env value would pass all three local checks (drive
		// module, registry construction, and this call), and a single leaked
		// value would then authorize BOTH resolve-bot-owner AND route notify
		// — breaking the "one credential / one capability" invariant.
		//
		// modules/internal_resolve/main_wiring_test.go asserts this argument
		// stays present so a future refactor cannot delete it silently.
		os.Getenv(internal_resolve.DriveInternalTokenEnv),
	); err != nil {
		return nil, err
	}
	// OCTO_CARD_MESSAGE_ENABLED is the deployment-level master gate (rollout /
	// emergency rollback). With it off, the card_action ingress rejects every
	// interaction (modules/message/api_card_action.go) and the notify/approval
	// send paths refuse to emit cards (modules/notify), so no callback can ever
	// enqueue and the dispatch worker would have no work. Skip the worker and its
	// Redis consumer entirely and return an inert runtime so the server still
	// boots with routes (and OCTO_DOCS_APPROVAL_CARD_ENABLED) left in the config —
	// flipping the gate off on an otherwise-valid config must not panic (runbook
	// rollback: "global OCTO_CARD_MESSAGE_ENABLED=false"). This is NOT a bypass for
	// config validation: LoadRouteSpecs / NewRegistry / ValidateNotifyTokenExclusions
	// run ABOVE this branch, so genuinely malformed route config (bad URL, short
	// secret, token reuse) still aborts boot loudly regardless of the gate.
	// Configured-but-inert routes/features only WARN so operators can see they will
	// resume once the gate is flipped back on.
	if !cardmsg.Enabled() {
		if producers := len(registry.NotifyProducers()); producers > 0 {
			log.Warn("OCTO_CARD_MESSAGE_ENABLED is off; card action notify routes are configured but inert until the gate is re-enabled",
				zap.Int("notify_producers", producers))
		}
		if notify.DocsApprovalCardsEnabled() {
			log.Warn("OCTO_CARD_MESSAGE_ENABLED is off; OCTO_DOCS_APPROVAL_CARD_ENABLED is set but inert until the gate is re-enabled")
		}
		return &cardActionDispatchRuntime{}, nil
	}
	if notify.DocsApprovalCardsEnabled() {
		resolution := registry.Resolve(notify.NotifyBotUIDValue, "docs", "access_request.decision")
		if resolution.Kind != cardactiondispatch.ResolutionCallback {
			return nil, errors.New("docs approval cards require a notification/docs/access_request.decision callback route")
		}
	}
	redisClient := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(options *rd.Options) {
		options.MaxRetries = 1
		options.PoolSize = 10
	})
	dlqRetention := cardactiondispatch.DLQRetentionFromEnv(os.Getenv)
	// Log both the raw override and the resolved retention so a typo'd
	// OCTO_CARD_ACTION_DLQ_RETENTION_DAYS that silently fell back to the default is visible
	// (raw="90x" but retention=720h reveals the fallback), and so operators can match the CLI.
	log.NewTLog("CardActionDispatch").Info("card action DLQ retention resolved",
		zap.Duration("retention", dlqRetention),
		zap.String("env", cardactiondispatch.DLQRetentionEnv),
		zap.String("raw", os.Getenv(cardactiondispatch.DLQRetentionEnv)))
	queue, err := cardactiondispatch.NewRedisQueue(redisClient, cardactiondispatch.QueueConfig{
		Prefix:       "card_action_dispatch",
		LiveTTL:      ctx.GetConfig().Robot.MessageExpire,
		DLQRetention: dlqRetention,
	})
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	service, err := cardactiondispatch.NewService(registry, queue, ctx)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	if err := cardactiondispatch.Install(ctx, service); err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	finalizer, err := notify.NewActionFinalizerFromContext(ctx)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	dispatcher, err := cardactiondispatch.NewDispatcher(
		queue,
		registry,
		cardactiondispatch.NewHTTPDeliverer(nil, time.Now),
		finalizer,
		cardactiondispatch.DispatcherConfig{
			Metrics: cardactiondispatch.NewMetrics(prometheus.DefaultRegisterer),
			Logger:  log.NewTLog("CardActionDispatch"),
		},
	)
	if err != nil {
		_ = redisClient.Close()
		return nil, err
	}
	return &cardActionDispatchRuntime{dispatcher: dispatcher, redisClient: redisClient}, nil
}

// {summaryNotify,docsNotify}ProducerID mirror modules/notify's producer IDs.
// Kept here so the composition root can register the spec without importing an
// unexported const.
const (
	summaryNotifyProducerID = carddispatch.ProducerID("summary-notify")
	docsNotifyProducerID    = carddispatch.ProducerID("docs-notify")
	actionOutcomeProducerID = carddispatch.ProducerID("action-outcome")
)

func printServerInfo(ctx *config.Context) {
	infoStr := `
[?25l[?7lLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL
LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL
LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL
LLLLLLLLLLLL0CLLLLLLLLLLLLLLLLLLLLLLLLLL
LLLLLLLLLL08@880CfLLLLLLLLLLLLLLLLLLLLLL
LLLLLLLLfL8@8@@8LfLLLLLLLLLLLLLLLLLLLLLL
ffffffffft0@@8@8ffffffffffffffffffffffff
fffffffffCCL8@GLLfLLLfffffffffffffffffff
ffffffffCLLC0@GCCLLLLCffffffffffffffffff
ffffffffG0@@@@@@@8Ltffffffffffffffffffff
ffffffftC888888888Gtffffffffffffffffffff
ffffffftttttttttttttffffffffffffffffffff
fffffffttttttttttttfffffffffffffffffffff
tttttttttttttfftffttttttttttttttttfttttt
tttttttttttttttttttttttttttttttttttttttt
tttttttttttttttttttttttttttttttttttttttt
tttttttttttttttttttttttttttttttttttttttt
tttttttttttttttttttttttttttttttttttttttt
tttttttttttttttttttttttttttttttttttttttt
111t111111111tt1111111tt1111111t11111111[0m
[20A[9999999D[43C[0m[0m 
[43C[0m[1m[32mTangSengDaoDao is running[0m 
[43C[0m-------------------------[0m 
[43C[0m[1m[33mMode[0m[0m:[0m #mode#[0m 
[43C[0m[1m[33mConfig[0m[0m:[0m #configPath#[0m 
[43C[0m[1m[33mApp name[0m[0m:[0m #appname#[0m 
[43C[0m[1m[33mVersion[0m[0m:[0m #version#[0m 
[43C[0m[1m[33mGit[0m[0m:[0m #git#[0m 
[43C[0m[1m[33mGo build[0m[0m:[0m #gobuild#[0m 
[43C[0m[1m[33mIM URL[0m[0m:[0m #imurl#[0m 
[43C[0m[1m[33mFile Service[0m[0m:[0m #fileService#[0m 
[43C[0m[1m[33mThe API is listening at[0m[0m:[0m #apiAddr#[0m 

[43C[30m[40m   [31m[41m   [32m[42m   [33m[43m   [34m[44m   [35m[45m   [36m[46m   [37m[47m   [m
[43C[38;5;8m[48;5;8m   [38;5;9m[48;5;9m   [38;5;10m[48;5;10m   [38;5;11m[48;5;11m   [38;5;12m[48;5;12m   [38;5;13m[48;5;13m   [38;5;14m[48;5;14m   [38;5;15m[48;5;15m   [m






[?25h[?7h
	`
	cfg := ctx.GetConfig()
	infoStr = strings.Replace(infoStr, "#mode#", string(cfg.Mode), -1)
	infoStr = strings.Replace(infoStr, "#appname#", cfg.AppName, -1)
	infoStr = strings.Replace(infoStr, "#version#", cfg.Version, -1)
	infoStr = strings.Replace(infoStr, "#git#", fmt.Sprintf("%s-%s", CommitDate, Commit), -1)
	infoStr = strings.Replace(infoStr, "#gobuild#", runtime.Version(), -1)
	infoStr = strings.Replace(infoStr, "#fileService#", cfg.FileService.String(), -1)
	infoStr = strings.Replace(infoStr, "#imurl#", cfg.WuKongIM.APIURL, -1)
	infoStr = strings.Replace(infoStr, "#apiAddr#", cfg.Addr, -1)
	infoStr = strings.Replace(infoStr, "#configPath#", cfg.ConfigFileUsed(), -1)
	fmt.Println(infoStr)
}

// startMetricsScrapeServer 在独立端口暴露 /metrics scrape 端点,
// 返回的 *http.Server 由调用方在业务退出后 Shutdown(graceful drain)。
// 当 DM_METRICS_ENABLED 未设为 "true" 时返回 nil。
//
// 配置(均通过环境变量,延续 DM_* 前缀约定):
//   - DM_METRICS_ENABLED: 默认 false(opt-in),设 "true" 才启用,
//     避免新版本默默开启端口与运维既有部署冲突。
//   - DM_METRICS_ADDR:   监听地址,默认 ":9090"。
//
// 端口失败不让进程挂掉,只记错 — 业务可用性优先于可观测性。
func startMetricsScrapeServer() *http.Server {
	if !strings.EqualFold(os.Getenv("DM_METRICS_ENABLED"), "true") {
		// 单行 audit log,让运维 grep 启动日志能确认"是关掉的,不是配错"。
		log.Info("metrics scrape endpoint disabled (set DM_METRICS_ENABLED=true to enable)")
		return nil
	}
	addr := os.Getenv("DM_METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	srv := metrics.NewScrapeServer(addr)
	// 文案用 "starting" 而非 "listening" — bind 失败时日志序列才不会
	// 误导成"先成功后崩"(starting → stopped 比 listening → stopped 自洽)。
	log.Info("starting metrics scrape endpoint", zap.String("addr", addr))
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics scrape endpoint stopped", zap.Error(err))
		}
	}()
	return srv
}

func ingorePaths() []string {

	return []string{
		"/v1/robots/:robot_id/:app_key/events",
		"/v1/ping",
		"/v1/health",
		"/v1/ready",
	}
}

func replaceWebConfig(cfg *config.Config) {
	path := "./assets/web/js/config.js"
	escapedURL, err := json.Marshal(cfg.External.APIBaseURL + "/")
	if err != nil {
		log.Error("failed to marshal APIBaseURL", zap.Error(err))
		return
	}
	newConfigContent := fmt.Sprintf(`const apiURL = %s`, string(escapedURL))
	if err := os.WriteFile(path, []byte(newConfigContent), 0644); err != nil {
		log.Error("failed to write web config", zap.String("path", path), zap.Error(err))
	}
}

// installCardTmplRegistry 装配 L0 cardtmpl.Registry 并注入到 pkg-scoped default。
//
// Fail-close 契约:任一 Register/SetDefault 失败 → panic(与 main.go:521 现有的
// docs approval callback route 校验同源)。init 期 schema/manifest 语法错无
// runtime env 可挽救,回滚 = 镜像 revert。
func installCardTmplRegistry() *cardtmpl.Registry {
	registry := cardtmpl.NewRegistry()
	// docs.access-request:同时保留已发布冻结的 0.2.0 与带 result 视图的
	// 0.3.0;新消息默认 0.3.0,旧 0.2.0 pending 消息仍可由 finalizer
	// 升级成 0.3.0/result。
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	// roadmap C:纯展示 v1 卡 —— docs.commented / docs.shared 迁入 Registry。
	// 每张卡自持 handoff embed;register-time sample self-check 与 fail-close 沿用。
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	registry.SetDefault(docsshared.TemplateID, docsshared.TemplateVersion)
	// roadmap C PR-2:summary.completed / summary.failed 迁入 Registry。
	// 达成 §2.2-5 L2b 硬门槛 ②(≥3 张 L2a 走 Registry 全链路;PR-2 后共 5 张)。
	registry.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
	registry.SetDefault(summarycompleted.TemplateID, summarycompleted.TemplateVersion)
	registry.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
	registry.SetDefault(summaryfailed.TemplateID, summaryfailed.TemplateVersion)
	// roadmap E1/E1c safety successor:保留冻结的 0.1.0–0.4.0 供历史消息按
	// 原契约编辑且不跨版本迁移，同时注册 full-bleed 版 0.4.1 并设为新默认。0.4.1 与
	// 0.4.0 的动作面与数据契约完全一致（只有客户端本地 toggle），差异只在根容器展示层。
	// Bot 新发/历史 edit 的授权集合由 bot_api catalog 独立收口；Registry 多版本
	// 只提供精确版本渲染能力，不隐式开放 Bot 权限。
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV1)
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV4)
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV5)
	registry.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV5)
	registry.Freeze()
	cardtmpl.SetGlobalMetrics(cardtmpl.NewMetrics(prometheus.DefaultRegisterer))
	cardtmpl.SetDefaultRegistry(registry)
	return registry
}
