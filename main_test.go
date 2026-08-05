package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestGlobalRateLimitExcludePathsIncludesProbeEndpoints(t *testing.T) {
	paths := globalRateLimitExcludePaths()

	require.Contains(t, paths, "/v1/ping")
	require.Contains(t, paths, "/v1/health")
	require.NotContains(t, paths, "/v1/ready")
}

// TestGlobalRateLimitExcludesBotHeartbeat 钉住 issue #696 的一半修复。
//
// 全局 per-IP 桶按 client IP 分片，因此同一出网 IP 上的所有 bot 共享一份配额；
// 一个 bot 打满，同 IP 其它 bot 的心跳被连坐 429、心跳 key(TTL 60s)过期、断联。
// 把心跳移出全局桶是让它不被邻居饿死的**唯一**手段——组级中间件绕不过 route.Use。
//
// 另一半在 modules/bot_api：心跳的上限改由 per-bot heartbeat 桶承担。
// **两者缺一不可**：只 exclude 会让心跳变成无限额度的未鉴权可达端点，
// 只加 per-bot 桶则挡不住来自邻居的 IP 层拒绝。
func TestGlobalRateLimitExcludesBotHeartbeat(t *testing.T) {
	paths := globalRateLimitExcludePaths()

	require.Contains(t, paths, "/v1/bot/heartbeat")

	// 其余 bot 端点必须仍在全局桶内：exclude 是在 DDoS 底线上开洞，
	// 只对论证过的单个路径开，不得扩大到整个 /v1/bot。
	require.NotContains(t, paths, "/v1/bot/register")
	require.NotContains(t, paths, "/v1/bot/sendMessage")
	require.NotContains(t, paths, "/v1/bot")
}

// TestBotRateLimitWiringMapsMatchingGetters 是 main.go 里那段 provider 闭包的守卫。
//
// 那段闭包把 SystemSettings 的 12 个 getter 映射到三条限流通道的 Params。
// 所有限流测试（单测、守卫、集成）都通过 SetRateLimitParamsProvider 直接注入参数，
// **完全绕过这段闭包**——所以若有人把 `Heartbeat.RPS` 接成
// `s.BotRateLimitRegisterRPS()`，没有任何现存测试会失败，而后果是运维在管理台调
// 心跳配额时实际改的是 register 通道，两条保命通道的配额互相错位。
//
// 这条守卫用文本级检查覆盖这个具体失败模式：每条通道的字段只允许引用
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
			segStart := strings.Index(block, channel+": ratelimitpkg.Params{")
			require.NotEqual(t, -1, segStart, "找不到 %s 通道的 Params 字面量，守卫失效", channel)
			seg := block[segStart:]
			if end := strings.Index(seg, "\n\t\t\t},"); end != -1 {
				seg = seg[:end]
			}

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

func TestInstallCardTmplRegistryRegistersReasoningHistoryAndV3Default(t *testing.T) {
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
	}, versions)

	current, err := registry.Lookup(aireasoningprocess.TemplateID, "")
	require.NoError(t, err)
	require.Equal(t, aireasoningprocess.TemplateVersionV3, current.Meta().Version)
	for _, version := range versions {
		_, err := registry.Lookup(aireasoningprocess.TemplateID, version)
		require.NoError(t, err)
	}
}
