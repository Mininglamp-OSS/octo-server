package group

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// 本文件登记 group 模块的 Prometheus 指标（#394 reconcile 可观测性）。
//
// 设计取舍（与 modules/oidc/metrics.go 一致）：
//   - 注册到全局默认 Registry（promauto.New*），由基础设施统一暴露 /metrics。
//   - Counter 带 result 维度，便于 Grafana 切分 orphan 检出 / 修复 / 失败比例。
//   - 不加 group_no / space_id 这类高基维 label，避免 Prometheus 内存爆炸。
const groupMetricNamespace = "group"

var (
	// metricReconcileTickTotal reconcile worker 每次 tick 的出口分布：
	// ran（正常跑完）| error（查询孤儿失败提前返回）。
	metricReconcileTickTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: groupMetricNamespace,
		Name:      "channel_reconcile_tick_total",
		Help:      "Channel reconcile worker tick outcomes (ran|error).",
	}, []string{"result"})

	// metricReconcileOrphanTotal 单个孤儿群的处理出口分布：
	//   detected  — 本轮扫描发现的 channel_synced=0 孤儿候选（每个候选 +1）；
	//   resolved  — 成功补建 IM 频道并翻 channel_synced=1；
	//   im_failed — IMCreateOrUpdateChannel 失败，留待下个 tick 重试；
	//   mark_failed — 频道补建成功但翻 channel_synced=1 失败，下个 tick 会再确认。
	metricReconcileOrphanTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: groupMetricNamespace,
		Name:      "channel_reconcile_orphan_total",
		Help:      "Channel reconcile per-orphan outcomes (detected|resolved|im_failed|mark_failed).",
	}, []string{"result"})
)
