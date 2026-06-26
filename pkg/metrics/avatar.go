package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 头像渲染缓存指标(issue#480)。头像「按需渲染」在成员列表扇出下会打满 CPU、
// 饿死其它请求;pkg/avatarrender.Cache 用 LRU + singleflight + 渲染信号量防御之。
// 这些指标用于验证防御生效(命中率、渲染并发、信号量等待)并对回归告警。
//
// label 维度收窄到低基数枚举(result / status),不放 uid 等高基数内容。

// AvatarMetrics 持有头像渲染缓存指标。每进程一个实例,注册到一个 Registerer。
type AvatarMetrics struct {
	// CacheEvents 按 result(hit/miss)计数 LRU 命中情况;命中率 = hit/(hit+miss)。
	CacheEvents *prometheus.CounterVec
	// SingleflightShared 计数被 singleflight 合并(复用在途渲染)的请求数。
	SingleflightShared prometheus.Counter
	// NotModified 计数 If-None-Match 命中返回 304 的请求(客户端缓存生效的体现)。
	NotModified prometheus.Counter
	// RenderDuration 按 status(ok/error)切分的单次真实渲染耗时直方图。
	RenderDuration *prometheus.HistogramVec
	// RenderInflight 当前正在进行的真实渲染数(gauge)。
	RenderInflight prometheus.Gauge
	// SemaphoreWait 获取渲染信号量的等待耗时直方图(>0 表示渲染并发饱和、排队)。
	SemaphoreWait prometheus.Histogram
}

// defaultAvatarMetrics 是供包级 Observe 函数使用的进程默认实例。未设置时 Observe
// 为安全 no-op(同 dependency.go 的 atomic.Pointer 约定)。
var defaultAvatarMetrics atomic.Pointer[AvatarMetrics]

// NewAvatarMetrics 在 reg 上注册头像渲染缓存指标,并登记为包级默认。
// 调用契约同 NewDependencyMetrics:同一 Registerer 只应调用一次。
func NewAvatarMetrics(reg prometheus.Registerer) *AvatarMetrics {
	m := &AvatarMetrics{
		CacheEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "cache_events_total",
			Help:      "Avatar render-cache lookups, labeled by result (hit/miss).",
		}, []string{"result"}),
		SingleflightShared: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "render_singleflight_shared_total",
			Help:      "Avatar requests whose render was coalesced onto an in-flight render (singleflight).",
		}),
		NotModified: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "not_modified_total",
			Help:      "Avatar requests answered 304 via If-None-Match (client cache hit).",
		}),
		RenderDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "render_duration_seconds",
			Help:      "Latency of a single actual avatar render (supersample + downscale + PNG encode), labeled by ok/error.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"status"}),
		RenderInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "render_inflight",
			Help:      "Number of avatar renders currently in progress.",
		}),
		SemaphoreWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: "avatar",
			Name:      "render_semaphore_wait_seconds",
			Help:      "Time spent waiting on the avatar render-concurrency semaphore before a render starts.",
			Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
	}
	reg.MustRegister(m.CacheEvents, m.SingleflightShared, m.NotModified, m.RenderDuration, m.RenderInflight, m.SemaphoreWait)
	defaultAvatarMetrics.Store(m)
	return m
}

// 下列包级函数是头像缓存的便捷入口,签名与 avatarrender.Hooks 的字段一致,便于在
// modules/user 构造 Cache 时直接接入。未初始化默认实例时全部为 no-op。

// ObserveAvatarCacheHit 记录一次 LRU 命中。
func ObserveAvatarCacheHit() {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.CacheEvents.WithLabelValues("hit").Inc()
	}
}

// ObserveAvatarCacheMiss 记录一次 LRU 未命中。
func ObserveAvatarCacheMiss() {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.CacheEvents.WithLabelValues("miss").Inc()
	}
}

// ObserveAvatarSingleflightShared 记录一次被 singleflight 合并的请求。
func ObserveAvatarSingleflightShared() {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.SingleflightShared.Inc()
	}
}

// ObserveAvatarNotModified 记录一次 304 响应。
func ObserveAvatarNotModified() {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.NotModified.Inc()
	}
}

// ObserveAvatarRender 记录一次真实渲染的耗时与结果。
func ObserveAvatarRender(dur time.Duration, err error) {
	if m := defaultAvatarMetrics.Load(); m != nil {
		status := dependencyStatusOK
		if err != nil {
			status = dependencyStatusError
		}
		m.RenderDuration.WithLabelValues(status).Observe(dur.Seconds())
	}
}

// AddAvatarRenderInflight 调整正在进行的渲染数(+1 开始 / -1 结束)。
func AddAvatarRenderInflight(delta float64) {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.RenderInflight.Add(delta)
	}
}

// ObserveAvatarSemaphoreWait 记录一次渲染信号量等待耗时。
func ObserveAvatarSemaphoreWait(dur time.Duration) {
	if m := defaultAvatarMetrics.Load(); m != nil {
		m.SemaphoreWait.Observe(dur.Seconds())
	}
}
