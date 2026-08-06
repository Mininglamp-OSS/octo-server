package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Bot API 限流指标（issue #696）。
//
// 背景：在此之前，限流的**拒绝路径完全不写日志**（octo-lib 的中间件只设响应头后
// abort），access log 也不含 robot_id，集群又没有任何日志采集组件。结果是
// 2026-08-05 那次「一个 bot 打满配额、同出网 IP 的其它 bot 全部连坐 429」只能靠
// 人工 kubectl logs 现捞复原。这组指标就是补这个盲区。
//
// label 基数是**硬约束**：生产实测活跃 bot 2903 个（Redis bot:heartbeat:*，TTL 60s）
// 且随业务增长无上界，因此 robot_id **绝不进 label**。身份维度由
// pkg/ratelimit.OffenderRecorder 的有界 ZSet 承担。
// 这里两个 label 都是固定枚举：class(3) × outcome(6) = 18 条 series 封顶。

// BotRateLimitMetrics 持有 bot 限流判定指标。每进程一个实例，注册到一个 Registerer。
type BotRateLimitMetrics struct {
	// Decisions 按 class(business/heartbeat/register) 与
	// outcome(bypassed/no_key/allowed/would_deny/denied/degraded) 计数每次判定。
	//
	// would_deny 与 denied 刻意分开：影子模式（dry-run）下判定为拒绝但不拦截，
	// 「关掉 dry-run 会多拒多少」正是靠这两个值的对比来回答的——合并就等于
	// 放弃了影子模式的全部意义。
	//
	// degraded 单列：它表示 Redis 故障导致限流层 fail-open，
	// 即「限流实际上没在工作」，这必须能在监控上一眼看出来，不能混进 allowed。
	//
	// no_key 与 bypassed 分开：bypassed 是「这条通道被显式关闭」（预期状态），
	// no_key 是「通道开着但取不到限流维度」（配置或中间件顺序出了问题，
	// 桶实际没生效）。两者都不拦截，混在一起就看不出后者。
	Decisions *prometheus.CounterVec
}

// defaultBotRateLimitMetrics 供包级 Observe 使用。未设置时 Observe 为安全 no-op
// （同 dependency.go / avatar.go 的 atomic.Pointer 约定）。
var defaultBotRateLimitMetrics atomic.Pointer[BotRateLimitMetrics]

// NewBotRateLimitMetrics 在 reg 上注册指标并登记为包级默认。
// 调用契约同 NewDependencyMetrics：同一 Registerer 只应调用一次。
func NewBotRateLimitMetrics(reg prometheus.Registerer) *BotRateLimitMetrics {
	m := &BotRateLimitMetrics{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "bot_ratelimit",
			Name:      "decisions_total",
			Help: "Bot API rate-limit decisions, labeled by class (business/heartbeat/register) " +
				"and outcome (bypassed/no_key/allowed/would_deny/denied/degraded). " +
				"Deliberately carries no bot identity: see pkg/ratelimit.OffenderRecorder for the bounded per-bot view.",
		}, []string{"class", "outcome"}),
	}
	reg.MustRegister(m.Decisions)
	defaultBotRateLimitMetrics.Store(m)
	return m
}

// ObserveBotRateLimitDecision 记录一次限流判定。
// class 与 outcome 必须来自固定枚举——这是 label 基数有界的前提，
// 调用方不得把任意字符串（尤其是 path 或 robot_id）透传进来。
func ObserveBotRateLimitDecision(class, outcome string) {
	if m := defaultBotRateLimitMetrics.Load(); m != nil {
		m.Decisions.WithLabelValues(class, outcome).Inc()
	}
}
