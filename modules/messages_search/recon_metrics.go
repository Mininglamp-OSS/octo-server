package messages_search

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ES↔MySQL 对账 drift 只读指标（YUJ-4667 步骤 5 / YUJ-4662 §4 步骤 5）。
//
// 权威对账作业（doc-count vs MySQL message 行数 + 抽样比对）跑在 indexer 仓
// （近 OS 写侧，复用阶段 6 recon 范式，产出结构化报告）。octo-server 侧**只读**
// 暴露最近一次对账结果，供「搜不到」定位：运维在 Grafana 上看到
// search_recon_doc_drift != 0 即知 ES 与 MySQL 失配。
//
// 失败阈值（机检口径，钉死在此）：
//   - search_recon_doc_drift == 0           → 健康
//   - search_recon_doc_drift > 0  (ES 多 doc：该删没删 / 越权可搜)  → 告警
//   - search_recon_doc_drift < 0  (ES 少 doc：漏索引 / 搜不到)      → 告警
//   - now - search_recon_last_run_timestamp_seconds > 2 * 对账周期 → 作业停摆告警
//
// 指标命名：search_recon_* 命名空间，无高基维 label（单集群够用）。
var (
	// reconDocDrift = ES doc-count - MySQL message 行数（带符号）。
	reconDocDrift = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "search_recon",
		Name:      "doc_drift",
		Help: "Signed ES↔MySQL message-count drift from the latest reconciliation " +
			"run (ES doc-count minus MySQL row-count). 0=healthy; >0 ES has extra " +
			"docs (delete-miss / over-recall); <0 ES is missing docs (index-miss).",
	})

	// reconSampleMismatch = 抽样比对中字段级失配的 doc 数。
	reconSampleMismatch = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "search_recon",
		Name:      "sample_mismatch",
		Help: "Number of sampled documents whose ES projection diverged from the " +
			"MySQL source of truth in the latest reconciliation run. 0=healthy.",
	})

	// reconLastRunTimestamp = 最近一次对账完成的 unix 秒。
	reconLastRunTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "search_recon",
		Name:      "last_run_timestamp_seconds",
		Help: "Unix timestamp (seconds) when the latest reconciliation run " +
			"completed. Staleness vs the expected cadence signals a stalled job.",
	})
)

// ReconReport is the structured drift summary the indexer-side reconciliation
// job pushes to octo-server's read-only ingestion point. octo-server NEVER
// computes drift itself (that needs the OS write side); it only stores the last
// report so the gauges above can be scraped.
type ReconReport struct {
	ESDocCount       int64 `json:"es_doc_count"`
	MySQLRowCount    int64 `json:"mysql_row_count"`
	SampleMismatch   int64 `json:"sample_mismatch"`
	RanAtUnixSeconds int64 `json:"ran_at_unix_seconds"`
}

// DocDrift is the signed ES-minus-MySQL count.
func (r ReconReport) DocDrift() int64 { return r.ESDocCount - r.MySQLRowCount }

// PublishReconReport updates the read-only drift gauges from a reconciliation
// report. Idempotent; safe to call on every report push. Returns the computed
// signed drift so callers (and tests) can assert on it.
func PublishReconReport(r ReconReport) int64 {
	drift := r.DocDrift()
	reconDocDrift.Set(float64(drift))
	reconSampleMismatch.Set(float64(r.SampleMismatch))
	reconLastRunTimestamp.Set(float64(r.RanAtUnixSeconds))
	return drift
}
