package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/module"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/server"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	octodb "github.com/Mininglamp-OSS/octo-server/pkg/db"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
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

func loadConfigFromFile(cfgFile string) *viper.Viper {
	vp := viper.New()
	vp.SetConfigFile(cfgFile)
	if err := vp.ReadInConfig(); err != nil {
		panic(fmt.Sprintf("Failed to load config file %s: %v", cfgFile, err))
	}
	fmt.Println("Using config file:", vp.ConfigFileUsed())
	return vp
}

func main() {
	var CfgFile string //config file
	flag.StringVar(&CfgFile, "config", "configs/tsdd.yaml", "config file")
	flag.Parse()
	vp := loadConfigFromFile(CfgFile)
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()

	gin.SetMode(gin.ReleaseMode)

	cfg := config.New()
	cfg.Version = Version
	cfg.ConfigureWithViper(vp)

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
	// 创建server
	s := server.New(ctx)
	ctx.SetHttpRoute(s.GetRoute())
	// 替换web下的配置文件
	replaceWebConfig(ctx.GetConfig())
	// 初始化api
	s.GetRoute().UseGin(ctx.Tracer().GinMiddle()) // 需要放在 api.Route(s.GetRoute())的前面
	// HTTP 入口指标(per-route latency / status / in-flight)。
	// 装在 tracer 之后, 以便未来 histogram exemplar 能拿到 trace context;
	// 装在 RateLimit 之前, 以记录 429 响应(被限流的请求也是真实流量)。
	// 指标走全局 DefaultRegisterer, 与 modules/oidc/metrics.go 共享 /metrics 端点。
	httpMetrics := metrics.NewHTTPMetrics(prometheus.DefaultRegisterer)
	s.GetRoute().UseGin(httpMetrics.GinMiddleware())
	metricsSrv := startMetricsScrapeServer()
	s.GetRoute().UseGin(func(c *gin.Context) {
		ingorePaths := ingorePaths()
		for _, ingorePath := range ingorePaths {
			if ingorePath == c.FullPath() {
				return
			}
		}
		gin.Logger()(c)
	})
	// 全局 per-IP 作为 DDoS 底线：办公室共享出网 IP 下 IM 基础量就能到 100+ rps
	// （每人 1-2 rps × 数十人），200 余量过小；真实 DDoS 常数千 rps+，底线设 500
	// 更合理。精细限流交给 UID 层和端点级严格桶（#1090）。
	// 无效环境变量值回退到默认值 + Warn 日志，行为与 UID 层 helper 一致。
	rps := wkhttp.ParseRPSFromEnv("DM_API_RATELIMIT_RPS", 500.0)
	burst := wkhttp.ParseBurstFromEnv("DM_API_RATELIMIT_BURST", 1000)
	// 限流状态存 Redis，多副本共享配额；与 dmwork-lib 的 GetRedisConn 指向同一实例。
	// 独立构造 client 的原因：lib 的 redis.Conn 未暴露 Eval/Script 接口，
	// 而令牌桶需要 Lua 脚本保证原子性。
	// 生命周期：跟随进程存续，不显式 Close——与 lib 自身的 redis.Conn 处理方式一致。
	// PoolSize 显式设 10：令牌桶 Lua 脚本是短事务，Redis 端 <1ms，不需要大池；
	// go-redis v6 默认 10*NumCPU 在大核机上会失控（多副本 × 多 client 连接数叠加）。
	rlRedis := rd.NewClient(octoredis.MustBuildOptions(ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	}))
	s.GetRoute().UseGin(wkhttp.RateLimitMiddleware(context.Background(), rlRedis, rps, burst, "/v1/ping"))
	// CORS 白名单覆盖：dmwork-lib 的 server.New 默认注入 "*" + Credentials:true，
	// 本中间件在其后执行，按 DM_CORS_ALLOWED_ORIGINS 重写/剥离 Allow-Origin/Credentials。
	// 未配置时等价于禁用跨域（剥离所有 CORS 响应头），仅允许同源调用。
	s.GetRoute().UseGin(wkhttp.SecureCORSOverrideMiddleware(
		wkhttp.ParseAllowedOrigins(os.Getenv("DM_CORS_ALLOWED_ORIGINS")),
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
	err := module.Setup(ctx)
	if err != nil {
		panic(err)
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
