package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestValidateTokenExpireConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		env     string
		envSet  bool
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: 720 * time.Hour},
		{name: "yaml override", yaml: "cache:\n  tokenExpire: 48h\n", want: 48 * time.Hour},
		{name: "env overrides yaml", yaml: "cache:\n  tokenExpire: 48h\n", env: "24h", envSet: true, want: 24 * time.Hour},
		{name: "empty env does not fall back to yaml", yaml: "cache:\n  tokenExpire: 48h\n", envSet: true, wantErr: true},
		{name: "invalid env does not fall back to yaml", yaml: "cache:\n  tokenExpire: 48h\n", env: "30d", envSet: true, wantErr: true},
		{name: "day suffix unsupported", yaml: "cache:\n  tokenExpire: 30d\n", wantErr: true},
		{name: "bare number unsupported", yaml: "cache:\n  tokenExpire: 24\n", wantErr: true},
		{name: "malformed", yaml: "cache:\n  tokenExpire: tomorrow\n", wantErr: true},
		{name: "zero", yaml: "cache:\n  tokenExpire: 0s\n", wantErr: true},
		{name: "negative", yaml: "cache:\n  tokenExpire: -1h\n", wantErr: true},
		{name: "sub-millisecond", yaml: "cache:\n  tokenExpire: 1us\n", wantErr: true},
		{name: "one millisecond", yaml: "cache:\n  tokenExpire: 1ms\n", want: time.Millisecond},
		{name: "over hard limit", yaml: "cache:\n  tokenExpire: 721h\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, existed := os.LookupEnv("TS_CACHE_TOKENEXPIRE")
			require.NoError(t, os.Unsetenv("TS_CACHE_TOKENEXPIRE"))
			t.Cleanup(func() {
				if existed {
					_ = os.Setenv("TS_CACHE_TOKENEXPIRE", old)
					return
				}
				_ = os.Unsetenv("TS_CACHE_TOKENEXPIRE")
			})
			vp := viper.New()
			if tt.yaml != "" {
				vp.SetConfigType("yaml")
				require.NoError(t, vp.ReadConfig(bytes.NewBufferString(tt.yaml)))
			}
			vp.SetEnvPrefix("TS")
			vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
			vp.AutomaticEnv()
			if tt.envSet {
				t.Setenv("TS_CACHE_TOKENEXPIRE", tt.env)
			}

			got, err := validateTokenExpireConfig(vp)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestValidateTokenExpireConfigDoesNotChangeOtherEmptyEnvSemantics(t *testing.T) {
	const yamlConfig = `
db:
  mysqlAddr: produser:prodpass@tcp(prod-mysql:3306)/prod
  redisTLS: true
`

	vp := viper.New()
	vp.SetConfigType("yaml")
	require.NoError(t, vp.ReadConfig(bytes.NewBufferString(yamlConfig)))
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	t.Setenv("TS_DB_MYSQLADDR", "")
	t.Setenv("TS_DB_REDISTLS", "")

	_, err := validateTokenExpireConfig(vp)
	require.NoError(t, err)

	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	require.Equal(t, "produser:prodpass@tcp(prod-mysql:3306)/prod", cfg.DB.MySQLAddr)
	require.True(t, cfg.DB.RedisTLS)
}

func TestRunAPIRegistersSessionRedisPool(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	require.NoError(t, err)

	foundRegistration := false
	foundSession := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RegisterPoolCollectors" || len(call.Args) < 3 {
			return true
		}
		foundRegistration = true
		clients, ok := call.Args[2].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, element := range clients.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := pair.Key.(*ast.BasicLit)
			if ok && key.Value == `"session"` {
				foundSession = true
			}
		}
		return true
	})

	require.True(t, foundRegistration, "main must register Redis pool collectors")
	require.True(t, foundSession, "the shared authentication session pool must be observable")
}

func TestRunAPIProbesSessionLuaBeforeInstallingTokenParser(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	require.NoError(t, err)

	var probePosition, parserPosition token.Pos
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "runAPI" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "Probe":
				probePosition = call.Pos()
			case "SetTokenParser":
				parserPosition = call.Pos()
			}
			return true
		})
	}

	require.NotZero(t, probePosition, "API startup must exercise the real session read Lua")
	require.NotZero(t, parserPosition, "API startup must install the canonical token parser")
	require.Less(t, probePosition, parserPosition, "Lua compatibility must be proven before authentication is installed")
}

func TestGlobalRateLimitExcludePathsIncludesProbeEndpoints(t *testing.T) {
	paths := globalRateLimitExcludePaths()

	require.Contains(t, paths, "/v1/ping")
	require.Contains(t, paths, "/v1/health")
	require.NotContains(t, paths, "/v1/ready")
}

// TestGlobalRateLimitExcludesBothSelfHealChannels 钉住 issue #696 的一半修复。
//
// 全局 per-IP 桶按 client IP 分片，因此同一出网 IP 上的所有 bot 共享一份配额；
// 一个 bot 打满，同 IP 其它 bot 无差别被拒。把路径移出全局桶是让它不被邻居饿死的
// **唯一**手段——组级中间件绕不过 route.Use。
//
// 另一半在 modules/bot_api：这两条路径的上限改由 per-bot 桶 + 鉴权前的 strict IP
// 底线承担。**两者缺一不可**：只 exclude 会让端点变成无限额度的未鉴权可达端点，
// 只加 per-bot 桶则挡不住来自邻居的 IP 层拒绝。
//
// **两条都要断言,不能只断言 heartbeat。** 第一版只豁免了 heartbeat,于是死锁链条
// 仍然通着:邻居打满全局桶 → heartbeat 活着(能发现掉线)→ register 仍被全局桶拒 →
// 拿不到新 token → 永远回不来。保住"发现掉线"却没保住"爬起来",结果一样是断联。
func TestGlobalRateLimitExcludesBothSelfHealChannels(t *testing.T) {
	paths := globalRateLimitExcludePaths()

	// heartbeat = 发现自己掉线的能力；register = 爬起来的能力。缺一等于没修。
	require.Contains(t, paths, "/v1/bot/heartbeat")
	require.Contains(t, paths, "/v1/bot/register")

	// 其余 bot 端点必须仍在全局桶内：exclude 是在 DDoS 底线上开洞，
	// 只对论证过的单个路径开，不得扩大到整个 /v1/bot。
	require.NotContains(t, paths, "/v1/bot/sendMessage")
	require.NotContains(t, paths, "/v1/bot/typing")
	require.NotContains(t, paths, "/v1/bot")
}

// TestExcludedBotPathsEachHaveTheirOwnFloor 把「豁免不等于无限制」从注释变成断言。
//
// 这是 main.go globalRateLimitExcludePaths 那段注释里写明的不变量，但注释拦不住任何人。
// 真实教训有两次:
//
//  1. heartbeat 被豁免时,配对的 per-bot 桶挂在 `authBot` **之后**,而无效 token 在
//     鉴权里就 abort——豁免打开的未鉴权 DDoS 面没有任何东西挡（第一轮 code review P1）。
//  2. register 压根没被豁免,于是自愈链条仍然断着（第二轮 code review P1-1）。
//
// 两次都是"豁免"与"配额"这对动作被拆开做。所以这里按结构检查:**每一条被豁免的
// /v1/bot 路径,都必须在 modules/bot_api/bot_api.go 里有一道 StrictIPRateLimitMiddleware
// 用它自己的 tag 挂着**。新增豁免却忘了配底线时,这条会红。
//
// 只覆盖 /v1/bot/*:探针端点(/v1/ping、/v1/health)是刻意完全无限制的,它们被限流
// 会让 k8s 误判实例不健康,不适用本不变量。
func TestExcludedBotPathsEachHaveTheirOwnFloor(t *testing.T) {
	src, err := os.ReadFile("modules/bot_api/bot_api.go")
	require.NoError(t, err)
	routeSrc := string(src)

	// 自证:守卫必须真的看到了 strict 中间件的挂载,否则下面的断言在空集上恒真。
	require.True(t, strings.Contains(routeSrc, "StrictIPRateLimitMiddleware"),
		"没在 bot_api.go 里找到任何 strict IP 中间件——守卫失去意义,先确认文件路径/实现是否搬走了")

	var botPaths []string
	for _, p := range globalRateLimitExcludePaths() {
		if strings.HasPrefix(p, "/v1/bot/") {
			botPaths = append(botPaths, p)
		}
	}
	require.NotEmpty(t, botPaths, "豁免列表里没有 /v1/bot 路径——本守卫恒真,应先检查列表")

	for _, p := range botPaths {
		// 约定:tag 是路径末段加 bot_ 前缀,如 /v1/bot/heartbeat → "bot_heartbeat"。
		tag := "bot_" + p[strings.LastIndex(p, "/")+1:]
		// 用 strings.Contains + True 而非 require.Contains:后者失败时会把整个
		// bot_api.go(数百行)打进输出,真正的信息被埋掉。
		require.True(t, strings.Contains(routeSrc, `"`+tag+`"`),
			"路径 %s 已被移出全局 per-IP 桶,但 bot_api.go 里找不到 tag %q 的 "+
				"StrictIPRateLimitMiddleware。豁免不等于无限制:没有配对的底线,"+
				"任意未鉴权请求都能无限打到这个端点(heartbeat 还会打到 authBot 的 Redis/DB 查询)。", p, tag)
	}
}

// TestBotRateLimitWiringMapsMatchingGetters 是 main.go 里那段 provider 闭包的守卫。
//
// 那段闭包把 SystemSettings 的 12 个 getter 映射到三条限流通道的 Params。
// 所有限流测试（单测、守卫、集成）都通过 SetRateLimitParamsProvider 直接注入参数，
// **完全绕过这段闭包**——所以若有人把 heartbeat 分支的 RPS 接成
// `s.BotRateLimitRegisterRPS()`，没有任何现存测试会失败，而后果是运维在管理台调
// 心跳配额时实际改的是 register 通道，两条保命通道的配额互相错位。
//
// 这条守卫用文本级检查覆盖这个具体失败模式：每条通道的分支只允许引用
// 带该通道名的 getter。
func TestBotRateLimitWiringMapsMatchingGetters(t *testing.T) {
	src, err := os.ReadFile("main.go")
	require.NoError(t, err)

	block := string(src)
	start := strings.Index(block, "bot_api.SetRateLimitParamsProvider(")
	require.NotEqual(t, -1, start, "找不到 provider 注入块，守卫失效——先修守卫本身")
	block = block[start:]
	if end := strings.Index(block, "\n\t})"); end != -1 {
		block = block[:end]
	}

	getterRe := regexp.MustCompile(`s\.(BotRateLimit\w+)\(\)`)

	for _, channel := range []string{"Business", "Heartbeat", "Register"} {
		t.Run(channel, func(t *testing.T) {
			// provider 按 class 分支取值（P2-3 后的形状），故按 case 标签切段。
			segStart := strings.Index(block, "case bot_api.RateLimitClass"+channel+":")
			require.NotEqual(t, -1, segStart,
				"找不到 %s 通道的 case 分支，守卫失效——provider 形状变了就要同步改守卫", channel)
			seg := block[segStart:]
			// 段的终点是下一个 case / default，取最近的那个。
			end := len(seg)
			for _, marker := range []string{"\n\t\tcase ", "\n\t\tdefault:"} {
				if i := strings.Index(seg[1:], marker); i != -1 && i+1 < end {
					end = i + 1
				}
			}
			seg = seg[:end]

			getters := getterRe.FindAllStringSubmatch(seg, -1)
			// 自证：必须真的抓到 4 个 getter（Enabled/DryRun/RPS/Burst）。
			// 少于 4 个说明正则失配或字段漏接，那时下面的断言会变成恒真。
			require.Len(t, getters, 4,
				"%s 通道应引用 4 个 getter（Enabled/DryRun/RPS/Burst），实际 %d 个",
				channel, len(getters))

			for _, g := range getters {
				require.Contains(t, g[1], channel,
					"%s 通道引用了 %s —— 通道与 getter 错位，"+
						"运维调该通道配额时实际会改到另一条通道", channel, g[1])
			}
		})
	}
}

func TestAccessLogIgnorePathsIncludesProbeEndpoints(t *testing.T) {
	paths := ingorePaths()

	require.Contains(t, paths, "/v1/ping")
	require.Contains(t, paths, "/v1/health")
	require.Contains(t, paths, "/v1/ready")
}

func TestInstallCardTmplRegistryRegistersReasoningHistoryAndV5Default(t *testing.T) {
	previousRegistry := cardtmpl.DefaultRegistry()
	previousRegisterer := prometheus.DefaultRegisterer
	previousGatherer := prometheus.DefaultGatherer
	metricsRegistry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = metricsRegistry
	prometheus.DefaultGatherer = metricsRegistry
	t.Cleanup(func() {
		cardtmpl.SetDefaultRegistry(previousRegistry)
		cardtmpl.SetGlobalMetrics(nil)
		prometheus.DefaultRegisterer = previousRegisterer
		prometheus.DefaultGatherer = previousGatherer
	})

	registry := installCardTmplRegistry()
	var versions []string
	for _, meta := range registry.List() {
		if meta.ID == aireasoningprocess.TemplateID {
			versions = append(versions, meta.Version)
		}
	}
	sort.Strings(versions)
	require.Equal(t, []string{
		aireasoningprocess.TemplateVersionV1,
		aireasoningprocess.TemplateVersionV2,
		aireasoningprocess.TemplateVersionV3,
		aireasoningprocess.TemplateVersionV4,
		aireasoningprocess.TemplateVersionV5,
	}, versions)

	current, err := registry.Lookup(aireasoningprocess.TemplateID, "")
	require.NoError(t, err)
	require.Equal(t, aireasoningprocess.TemplateVersionV5, current.Meta().Version)
	for _, version := range versions {
		_, err := registry.Lookup(aireasoningprocess.TemplateID, version)
		require.NoError(t, err)
	}
}
