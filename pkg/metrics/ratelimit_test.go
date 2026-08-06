package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// TestBotRateLimitMetricsLabelCardinalityIsBounded 是 brief Acceptance 里
// 「Prometheus label 基数有界」那一条的硬钉。
//
// 这不是洁癖:生产实测活跃 bot **2903 个**且随业务增长无上界(Redis
// `bot:heartbeat:*`,TTL 60s)。把 robot_id 放进 label 会产生数万条随时间 churn 的
// series,足以拖垮抓取端。身份维度由 pkg/ratelimit.OffenderRecorder 的有界 ZSet 承担。
//
// 同理禁止 path:bot 端点 40+ 个,放进 label 就白省了 robot_id 的基数。
func TestBotRateLimitMetricsLabelCardinalityIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewBotRateLimitMetrics(reg)

	// 灌入两条不同 class/outcome 的样本,确保 Gather 拿得到 metric family。
	m.Decisions.WithLabelValues("business", "denied").Inc()
	m.Decisions.WithLabelValues("heartbeat", "would_deny").Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if !strings.Contains(f.GetName(), "bot_ratelimit") {
			continue
		}
		found = true
		for _, metric := range f.GetMetric() {
			names := labelNames(metric)
			require.ElementsMatch(t, []string{"class", "outcome"}, names,
				"%s 的 label 集合必须恰好是 {class, outcome};多一个高基数维度就会让 series 爆炸",
				f.GetName())
			for _, n := range names {
				require.NotContains(t, n, "robot",
					"robot_id 绝不可作为 label —— 生产活跃 bot 2903 个且无上界")
				require.NotContains(t, n, "uid")
				require.NotContains(t, n, "path")
				require.NotContains(t, n, "token")
			}
		}
	}
	require.True(t, found, "未注册 bot_ratelimit 指标,守卫失效——先修守卫本身")
}

// TestObserveBotRateLimitDecisionIsNoopWithoutRegistration 钉住包级 Observe 的
// 安全 no-op 语义(同 dependency.go / avatar.go 的约定)。
//
// 载荷性:限流判定跑在每个 bot 请求上。若未注册时 Observe 会 panic,
// 那么任何忘记调用 NewBotRateLimitMetrics 的部署都会在第一个 bot 请求上崩——
// 而观测本身绝不该成为业务可用性的前置条件。
func TestObserveBotRateLimitDecisionIsNoopWithoutRegistration(t *testing.T) {
	previous := defaultBotRateLimitMetrics.Load()
	defaultBotRateLimitMetrics.Store(nil)
	t.Cleanup(func() { defaultBotRateLimitMetrics.Store(previous) })

	require.NotPanics(t, func() {
		ObserveBotRateLimitDecision("business", "denied")
	}, "未注册时 Observe 必须是安全 no-op,不得让业务路径 panic")
}

func labelNames(m *dto.Metric) []string {
	out := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out = append(out, l.GetName())
	}
	return out
}
