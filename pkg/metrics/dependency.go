package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 依赖调用延迟指标。覆盖 handler 之下的上游依赖(对象存储等)的耗时,
// 用一个共享 HistogramVec 承载所有依赖,新依赖加 label 值即可,不再新增指标。
//
// label 维度刻意收窄到 dependency/op/backend/status —— 全部是低基数枚举值,
// 不放 path/uid/object-key 这类高基数内容,防止 Prometheus 序列爆炸。

const (
	// dependencyStatusOK / dependencyStatusError 是 status label 仅有的两个取值,
	// 由调用是否返回 error 决定,而非依赖侧的具体错误码(避免基数膨胀)。
	dependencyStatusOK    = "ok"
	dependencyStatusError = "error"

	// DependencyObjectStore 是对象存储类依赖的 dependency label 值。
	DependencyObjectStore = "objectstore"
	// OpUploadFile / OpGetFile 是对象存储上「真正发生网络 I/O」的两个操作的 op
	// label 值:上传(PutObject 等)与读取对象(GetObject+Stat 等)。
	// 注意:不为 DownloadURL 打点 —— 各后端的 DownloadURL 只是从 config 本地拼出
	// 公开/CDN URL,不触达对象存储,给它打 latency 会产生误导性指标(见 #442 P1-1)。
	OpUploadFile = "upload_file"
	OpGetFile    = "get_file"
)

// DependencyMetrics 持有依赖调用指标。每进程一个实例,注册到一个 Registerer。
type DependencyMetrics struct {
	// Duration 按 dependency/op/backend/status 切分的依赖调用延迟直方图。
	// Buckets 比 HTTP 入口更细、低段下探到 1ms —— 依赖调用(签名 URL、池内
	// 查询)正常应在毫秒级,粗桶会把"正常"和"偶发抖动"糊在一起。
	Duration *prometheus.HistogramVec
}

// defaultDependencyMetrics 是供包级 Observe 函数使用的进程默认实例。
// 用 atomic.Pointer 持有:启动时 NewDependencyMetrics 设置一次,业务路径只读;
// 未设置(指标关闭 / 单测未初始化)时 Observe 为安全 no-op,绝不 panic。
var defaultDependencyMetrics atomic.Pointer[DependencyMetrics]

// NewDependencyMetrics 在传入的 Registerer 上注册依赖调用指标,并把实例登记为
// 包级默认(供 ObserveObjectStore 等自由函数使用)。
//
// 调用契约同 NewHTTPMetrics:同一 Registerer 只应调用一次,重复注册会触发
// MustRegister 的 panic(prometheus 库契约)。生产传 DefaultRegisterer;
// 测试传 prometheus.NewRegistry() 隔离,并可用返回值直接断言。
func NewDependencyMetrics(reg prometheus.Registerer) *DependencyMetrics {
	m := &DependencyMetrics{
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "dependency",
			Name:      "duration_seconds",
			Help:      "Latency of calls to upstream dependencies below the HTTP handler, labeled by dependency, operation, backend, and ok/error status.",
			Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"dependency", "op", "backend", "status"}),
	}
	reg.MustRegister(m.Duration)
	defaultDependencyMetrics.Store(m)
	return m
}

// Observe 记录一次依赖调用。status 由 err 是否为 nil 决定。
func (m *DependencyMetrics) Observe(dependency, op, backend string, start time.Time, err error) {
	status := dependencyStatusOK
	if err != nil {
		status = dependencyStatusError
	}
	m.Duration.WithLabelValues(dependency, op, backend, status).Observe(time.Since(start).Seconds())
}

// ObserveObjectStore 是对象存储调用的包级便捷入口,供 modules/file 等调用方使用,
// 无需把 *DependencyMetrics 实例穿透到业务层。未初始化默认实例时为 no-op。
//
// 用法:
//
//	start := time.Now()
//	res, err := backend.UploadFile(...)
//	metrics.ObserveObjectStore(metrics.OpUploadFile, backendLabel, start, err)
//	return res, err
func ObserveObjectStore(op, backend string, start time.Time, err error) {
	if m := defaultDependencyMetrics.Load(); m != nil {
		m.Observe(DependencyObjectStore, op, backend, start, err)
	}
}
