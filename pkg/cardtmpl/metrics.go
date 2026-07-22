package cardtmpl

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics 是 pkg/cardtmpl 自打的指标集合,与 internal/carddispatch.Metrics 分开;
// 后者只覆盖 Sender.Send 之后的派发结果,看不到 Registry.Lookup / Build 阶段。
//
// label 基数 = 注册表大小 (template_id 值只来自 Template.ID() 硬编码),天花板
// 可预期 <20,无基数爆炸风险 (§13)。
type Metrics struct {
	build *prometheus.CounterVec
}

// NewMetrics 构造并注册指标。传 nil registerer 得到 no-op 实例。
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		build: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_cardtmpl_build_total",
			Help: "Total cardtmpl Registry.Render outcomes by template_id/version/view/result.",
		}, []string{"template_id", "version", "view", "result"}),
	}
	reg.MustRegister(m.build)
	return m
}

// buildResult 记录一次 Render 的结果。view 可能为空 (若在决定 view 前失败)。
func (m *Metrics) buildResult(templateID, version, view, result string) {
	if m == nil {
		return
	}
	m.build.WithLabelValues(templateID, version, view, result).Inc()
}

// globalMetrics 是包级默认实例,供 render.go 无侵入打点。
// 生产 composition root 通过 SetGlobalMetrics 注入注册好的实例;
// 测试环境默认不注入,打点变 no-op。
var (
	globalMetricsMu sync.RWMutex
	globalMetrics   *Metrics
)

// SetGlobalMetrics 由 composition root 在 NewMetrics 之后一次性调用。
// 重复调用允许 (后者覆盖),但生产不应发生。
func SetGlobalMetrics(m *Metrics) {
	globalMetricsMu.Lock()
	defer globalMetricsMu.Unlock()
	globalMetrics = m
}

// metricBuildResult 是 render.go 内部用的便捷入口。
func metricBuildResult(id ID, version, view, result string) {
	globalMetricsMu.RLock()
	m := globalMetrics
	globalMetricsMu.RUnlock()
	m.buildResult(string(id), version, view, result)
}

// RecordFieldsInvalid 是 preflight-only path 的导出入口 (R2-8):
// caller 在 Registry.Render 之前独立跑 schema 校验(如 modules/notify
// preflightDocsAccessRequestSchema),失败时 Render 不会被触发,`build_total`
// 就漏一条 400 路径。此入口只允许 result="fields_invalid",view 传空。
// 用法与直接调 metricBuildResult 等价,只是限定为 preflight 场景避免误用。
func RecordFieldsInvalid(id ID, version string) {
	metricBuildResult(id, version, "", "fields_invalid")
}
