// Package metrics 收集 dmwork 进程级 Prometheus 指标。
//
// HTTP 中间件提供 per-route 的延迟直方图、并发请求计数,
// 以及"业务错误"计数 —— 后者通过解析响应 body 的 envelope `status` 字段
// 来识别,而不是依赖 HTTP status code(详见 BusinessError 字段的注释)。
//
// path label 用 gin.Context.FullPath() 取路由模板而非真实 URI,
// 防止 uid/orderID 这类高基数值打爆 Prometheus 内存。
//
// 与 modules/oidc/metrics.go 不同: 那里用全局默认 Registry (promauto),
// 本文件让调用方注入 Registerer, 便于测试隔离和未来按需迁移到独立 registry。
package metrics

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "dmwork"
	metricSubsystem = "http"

	// metricsEndpointPath 抓取端点本身在中间件中跳过埋点,
	// 防止 Prometheus 抓取自身产生指标 -> 循环放大。
	metricsEndpointPath = "/metrics"

	// pathLabelUnmatched 未匹配到任何路由模板时的 path 兜底值,
	// 防止把真实 URI(可能含 uid/token)写进 label 导致基数爆炸。
	pathLabelUnmatched = "unmatched"

	// methodLabelOther 非标准 HTTP method 的兜底值,
	// 防止 fuzz 攻击造的奇怪 method 打爆 method label 维度。
	methodLabelOther = "other"

	// bodyCaptureLimit 业务错误识别只需读响应 body 头部的 envelope 字段,
	// 1 KiB 足以覆盖 wkhttp 错误信封(典型 < 200 字节);超出后的字节直接透传,
	// 避免大响应(列表查询、文件流)占用额外内存。
	bodyCaptureLimit = 1024
)

// allowedMethods 是 method label 的白名单。任何不在此集合的方法
// 都归一化为 methodLabelOther,见 normalizeMethod。
var allowedMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodHead:    {},
	http.MethodOptions: {},
}

// HTTPMetrics 持有所有 HTTP 入口指标。每进程一个实例。
type HTTPMetrics struct {
	// Duration 按 method/path/status 切分的请求延迟直方图。
	// Buckets 覆盖 5ms ~ 10s, 匹配 IM 业务的真实 P99 区间。
	Duration *prometheus.HistogramVec

	// InFlight 当前并发处理中的请求数, 用于发现 hang 或慢请求堆积。
	InFlight prometheus.Gauge

	// BusinessError 业务层显式返回错误的次数, 按 path/code 切分。
	//
	// 背景: 业务约定(octo-lib/pkg/wkhttp.ResponseError 与本仓库同名实现)
	// 把绝大多数服务端错误统一返回 HTTP 400, 导致 Duration{status=~"5.."}
	// 看到的 5xx 率几乎恒为 0, 无法反映真实故障率。
	//
	// 采集方式: 中间件在响应体内解析 envelope JSON 的 `status` 字段
	// (即 wkhttp 写入的语义错误码) — 与 HTTP status code 解耦,无需要求
	// handler 显式调用埋点 API,任何 wkhttp 变体(local / octo-lib)只要遵循
	// `{"status": N, "msg": ...}` 信封就都能采到。
	// 当未来把服务端错误改回 HTTP 5xx 时(参见 upstream issue #140 阶段 2),
	// code label 自然反映新分布,不需要再改 dashboard 名字。
	BusinessError *prometheus.CounterVec
}

// NewHTTPMetrics 在传入的 Registerer 上注册所有 HTTP 指标。
//
// 调用契约: 同一个 Registerer 只能调用一次。重复注册触发 MustRegister 的 panic
// (这是 prometheus 库的契约,不是本函数的 bug)。
//   - 生产代码: 在 main.go 启动时一次性传入 prometheus.DefaultRegisterer。
//   - 测试代码: 每个用例传入 prometheus.NewRegistry() 以隔离全局状态。
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency in seconds, labeled by method, route template, and status.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "path", "status"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "requests_in_flight",
			Help:      "Number of HTTP requests currently being processed.",
		}),
		BusinessError: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "business_error_total",
			Help:      "Count of business-level error responses (envelope status != 2xx) per route template, sniffed from JSON response body.",
		}, []string{"path", "code"}),
	}
	reg.MustRegister(m.Duration, m.InFlight, m.BusinessError)
	return m
}

// GinMiddleware 返回一个 gin 中间件,
// 在 c.Next() 前后记录延迟、状态码、并发计数和业务错误。
//
// panic 处理: 上层 gin.Recovery() 是最外层中间件, 它的 defer recover
// 在本中间件 defer 之后才会执行 — 直接读 c.Writer.Status() 会拿到 200。
// 因此本中间件自己 recover 一次记录 status=500, 再 re-panic 让外层 Recovery
// 完成最终响应写出。
//
// 业务错误识别: 包一层 ResponseWriter 抓前 1 KiB 响应 body,defer 中解析 JSON
// envelope 的 `status` 字段;若 >= 400 累加 BusinessError。解析失败时退回
// HTTP status code(覆盖 AbortWithStatusJSON / 静态文件等非 envelope 的错误)。
func (m *HTTPMetrics) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 抓取端点本身不埋点。用 URL.Path 而非 FullPath, 因为 /metrics
		// 在中间件链入口路由还未匹配时也要跳过(虽然此场景目前不存在,
		// 但更安全的不变量是 URL.Path == "/metrics" 即跳过)。
		if c.Request.URL.Path == metricsEndpointPath {
			c.Next()
			return
		}

		capture := &bodyCapture{ResponseWriter: c.Writer, limit: bodyCaptureLimit}
		c.Writer = capture

		m.InFlight.Inc()
		start := time.Now()

		defer func() {
			r := recover()
			httpStatus := c.Writer.Status()
			if r != nil {
				httpStatus = http.StatusInternalServerError
			}
			path := pathLabel(c)
			m.Duration.WithLabelValues(
				normalizeMethod(c.Request.Method),
				path,
				strconv.Itoa(httpStatus),
			).Observe(time.Since(start).Seconds())
			m.InFlight.Dec()

			if code := businessErrorCode(capture.captured(), httpStatus, r != nil); code > 0 {
				m.BusinessError.WithLabelValues(path, strconv.Itoa(code)).Inc()
			}

			if r != nil {
				panic(r) // 让外层 gin.Recovery 完成 500 响应写出
			}
		}()

		c.Next()
	}
}

// bodyCapture 在不影响下游 ResponseWriter 的前提下抓取前 limit 字节,
// 供 defer 中解析业务 envelope 用。超出 limit 的字节直接透传,不再缓冲,
// 避免大响应占用额外内存。
type bodyCapture struct {
	gin.ResponseWriter
	buf   bytes.Buffer
	limit int
}

func (b *bodyCapture) Write(p []byte) (int, error) {
	if remain := b.limit - b.buf.Len(); remain > 0 {
		take := len(p)
		if take > remain {
			take = remain
		}
		b.buf.Write(p[:take])
	}
	return b.ResponseWriter.Write(p)
}

func (b *bodyCapture) WriteString(s string) (int, error) {
	return b.Write([]byte(s))
}

func (b *bodyCapture) captured() []byte {
	return b.buf.Bytes()
}

// businessErrorCode 决定本次响应是否算业务错误,以及对应的 code label。
//
// 优先级:
//  1. panic -> 500(panic 时 body 还没写出,看 HTTP 没意义)
//  2. JSON body 顶层 `status` 字段(wkhttp envelope 写入的语义错误码)
//  3. 退回 HTTP status code(覆盖 AbortWithStatusJSON / 静态文件等场景)
//
// 返回 0 表示不应累加(2xx 成功或无法判定)。
func businessErrorCode(body []byte, httpStatus int, panicked bool) int {
	if panicked {
		return http.StatusInternalServerError
	}
	if code, ok := envelopeStatus(body); ok {
		if code >= 400 {
			return code
		}
		return 0
	}
	if httpStatus >= 400 {
		return httpStatus
	}
	return 0
}

// envelopeStatus 在 body 中尝试找顶层 `status` 整数字段。
// 解析失败、不是 JSON 对象、或 status 不是整数时返回 ok=false。
//
// 用 json.Unmarshal 到只关心 status 的小结构体 —— 比手撕字符串安全。
// 截断的 body(超过 bodyCaptureLimit)大概率反序列化失败 ->
// businessErrorCode 自动退回 HTTP status。
func envelopeStatus(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var env struct {
		Status *int `json:"status"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, false
	}
	if env.Status == nil {
		return 0, false
	}
	return *env.Status, true
}

// pathLabel 返回 path label 的值: 命中路由用模板, 否则用兜底常量。
func pathLabel(c *gin.Context) string {
	if p := c.FullPath(); p != "" {
		return p
	}
	return pathLabelUnmatched
}

// normalizeMethod 把请求方法收敛到白名单, 防止 method label 维度爆炸。
func normalizeMethod(method string) string {
	if _, ok := allowedMethods[method]; ok {
		return method
	}
	return methodLabelOther
}
