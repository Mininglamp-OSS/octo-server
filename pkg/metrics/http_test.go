package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// histogramSampleCount 通过 Gather 严格读取指定 label 组合的样本数。
//
// 不用 HistogramVec.WithLabelValues / GetMetricWithLabelValues, 因为它们在
// label 组合不存在时会"静默创建一个 0 样本子项"返回, 写错断言时容易得到
// 假的 0 而非显式失败 — Gather 路径只反映真实存在的 series。
func histogramSampleCount(
	t *testing.T,
	reg prometheus.Gatherer,
	name string,
	wantLabels map[string]string,
) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsEqual(m.GetLabel(), wantLabels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func labelsEqual(have []*dto.LabelPair, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, lp := range have {
		if v, ok := want[lp.GetName()]; !ok || v != lp.GetValue() {
			return false
		}
	}
	return true
}

func newRouterWithMetrics(t *testing.T) (*gin.Engine, *metrics.HTTPMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.NewHTTPMetrics(reg)
	r := gin.New()
	// 真实 main.go 顺序: wkhttp.New() 先装 gin.Recovery (最外层),
	// 再 UseGin(metricsMiddleware). 测试模拟这个顺序。
	r.Use(gin.Recovery())
	r.Use(m.GinMiddleware())
	return r, m, reg
}

func TestGinMiddleware_RecordsRequest(t *testing.T) {
	cases := []struct {
		name          string
		registerPath  string
		registerMeth  string
		requestPath   string
		requestMeth   string
		handlerStatus int
		handlerPanics bool
		wantPath      string
		wantMethod    string
		wantStatus    string
	}{
		{
			name:          "matched route uses FullPath template not raw uri",
			registerPath:  "/v1/users/:uid/im",
			registerMeth:  http.MethodGet,
			requestPath:   "/v1/users/abc-123/im",
			requestMeth:   http.MethodGet,
			handlerStatus: http.StatusOK,
			wantPath:      "/v1/users/:uid/im",
			wantMethod:    http.MethodGet,
			wantStatus:    "200",
		},
		{
			name:         "unmatched route collapses to 'unmatched'",
			registerPath: "/v1/known",
			registerMeth: http.MethodGet,
			requestPath:  "/v1/never-registered/xyz",
			requestMeth:  http.MethodGet,
			wantPath:     "unmatched",
			wantMethod:   http.MethodGet,
			wantStatus:   "404",
		},
		{
			name:          "5xx status recorded as label",
			registerPath:  "/v1/error",
			registerMeth:  http.MethodPost,
			requestPath:   "/v1/error",
			requestMeth:   http.MethodPost,
			handlerStatus: http.StatusInternalServerError,
			wantPath:      "/v1/error",
			wantMethod:    http.MethodPost,
			wantStatus:    "500",
		},
		{
			name:          "panic recorded as 500",
			registerPath:  "/v1/panic",
			registerMeth:  http.MethodGet,
			requestPath:   "/v1/panic",
			requestMeth:   http.MethodGet,
			handlerPanics: true,
			wantPath:      "/v1/panic",
			wantMethod:    http.MethodGet,
			wantStatus:    "500",
		},
		{
			name:          "non-standard method normalized to 'other'",
			registerPath:  "/v1/foo",
			registerMeth:  "WEIRD",
			requestPath:   "/v1/foo",
			requestMeth:   "WEIRD",
			handlerStatus: http.StatusOK,
			wantPath:      "/v1/foo",
			wantMethod:    "other",
			wantStatus:    "200",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r, _, reg := newRouterWithMetrics(t)
			r.Handle(tc.registerMeth, tc.registerPath, func(c *gin.Context) {
				if tc.handlerPanics {
					panic("boom")
				}
				if tc.handlerStatus != 0 {
					c.Status(tc.handlerStatus)
				}
			})

			req := httptest.NewRequest(tc.requestMeth, tc.requestPath, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			got := histogramSampleCount(t, reg, "dmwork_http_request_duration_seconds",
				map[string]string{
					"method": tc.wantMethod,
					"path":   tc.wantPath,
					"status": tc.wantStatus,
				})
			if got != 1 {
				t.Errorf("expected 1 sample for {method=%s, path=%s, status=%s}, got %d",
					tc.wantMethod, tc.wantPath, tc.wantStatus, got)
			}
		})
	}
}

func TestGinMiddleware_SkipsMetricsEndpoint(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)
	r.GET("/metrics", func(c *gin.Context) { c.String(http.StatusOK, "scrape body") })

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// /metrics 必须不产生任何样本 — 整个 family 在 Gather 中应不存在。
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "dmwork_http_request_duration_seconds" {
			t.Errorf("expected no histogram samples after /metrics scrape, got %d series",
				len(mf.GetMetric()))
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics handler should still execute, got status %d", rec.Code)
	}
}

func TestGinMiddleware_InFlightGaugeNoLeak(t *testing.T) {
	r, m, _ := newRouterWithMetrics(t)
	r.GET("/v1/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/v1/panic", func(c *gin.Context) { panic("boom") })

	hit := func(path string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}
	for i := 0; i < 5; i++ {
		hit("/v1/ok")
		hit("/v1/panic")
	}

	if v := testutil.ToFloat64(m.InFlight); v != 0 {
		t.Errorf("in-flight gauge leaked: expected 0 after all requests done, got %v", v)
	}
}

func TestGinMiddleware_InFlightGaugeIncrementsDuringRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewHTTPMetrics(reg)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(m.GinMiddleware())

	entered := make(chan struct{})
	release := make(chan struct{})
	r.GET("/slow", func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusOK)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}()

	<-entered
	if v := testutil.ToFloat64(m.InFlight); v != 1 {
		t.Errorf("expected in-flight=1 mid-request, got %v", v)
	}
	close(release)
	wg.Wait()

	if v := testutil.ToFloat64(m.InFlight); v != 0 {
		t.Errorf("expected in-flight=0 after request, got %v", v)
	}
}

func TestNewHTTPMetrics_RegistersOnProvidedRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewHTTPMetrics(reg)

	// HistogramVec / CounterVec 在未观测前不会被 Gather() 报告(prometheus 库行为),
	// 先打样本让 family 出现, 再断言三个指标都注册成功。
	m.Duration.WithLabelValues("GET", "/probe", "200").Observe(0.001)
	m.BusinessError.WithLabelValues("/probe", "400").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"dmwork_http_request_duration_seconds": false,
		"dmwork_http_requests_in_flight":       false,
		"dmwork_http_business_error_total":     false,
	}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected metric %q to be registered", name)
		}
	}
}

// counterValue 通过 Gather 严格读取 CounterVec 指定 label 组合的累加值。
// 不用 WithLabelValues / GetMetricWithLabelValues, 与 histogramSampleCount
// 同样的原因 —— 那两个 API 会"静默创建 0 子项", 让断言失败变成假成功。
func counterValue(
	t *testing.T,
	reg prometheus.Gatherer,
	name string,
	wantLabels map[string]string,
) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsEqual(m.GetLabel(), wantLabels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestBusinessError_FromEnvelopeStatus 覆盖核心采集路径:wkhttp.ResponseError
// 写出的 envelope `{"msg": "...", "status": 400}`,HTTP status 也是 400 —— 但
// code label 必须来自 envelope 字段(为阶段 2 解耦 HTTP/业务 status 做准备)。
func TestBusinessError_FromEnvelopeStatus(t *testing.T) {
	cases := []struct {
		name         string
		body         gin.H
		httpStatus   int
		wantPath     string
		wantCode     string
		wantIncrease bool
	}{
		{
			name:         "ResponseError envelope: status=400",
			body:         gin.H{"msg": "bad input", "status": 400},
			httpStatus:   http.StatusBadRequest,
			wantPath:     "/v1/users/:uid/im",
			wantCode:     "400",
			wantIncrease: true,
		},
		{
			name:         "ResponseErrorWithStatus(500): envelope status=500 even when HTTP also 500",
			body:         gin.H{"msg": "db down", "status": 500},
			httpStatus:   http.StatusInternalServerError,
			wantPath:     "/v1/groups/:gid",
			wantCode:     "500",
			wantIncrease: true,
		},
		{
			name:         "future: handler returns HTTP 400 but envelope says 500 (post-phase-2)",
			body:         gin.H{"msg": "fake", "status": 500},
			httpStatus:   http.StatusBadRequest,
			wantPath:     "/v1/x",
			wantCode:     "500", // envelope 优先于 HTTP
			wantIncrease: true,
		},
		{
			name:         "ResponseOK envelope status=200 -> no increment",
			body:         gin.H{"status": 200},
			httpStatus:   http.StatusOK,
			wantPath:     "/v1/ok",
			wantIncrease: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r, _, reg := newRouterWithMetrics(t)
			r.GET(tc.wantPath, func(c *gin.Context) {
				c.JSON(tc.httpStatus, tc.body)
			})

			req := httptest.NewRequest(http.MethodGet, tc.wantPath, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if tc.wantIncrease {
				got := counterValue(t, reg, "dmwork_http_business_error_total",
					map[string]string{"path": tc.wantPath, "code": tc.wantCode})
				if got != 1 {
					t.Errorf("expected counter=1 for {path=%s, code=%s}, got %v",
						tc.wantPath, tc.wantCode, got)
				}
			} else {
				families, _ := reg.Gather()
				for _, mf := range families {
					if mf.GetName() == "dmwork_http_business_error_total" && len(mf.GetMetric()) > 0 {
						t.Errorf("expected no business_error_total samples, got %d series",
							len(mf.GetMetric()))
					}
				}
			}
		})
	}
}

// TestBusinessError_FallbackToHTTPStatus 覆盖非 envelope 错误路径,例如
// gin.Context.AbortWithStatusJSON(401, {"msg": "..."}) 没有 status 字段,
// 或纯文本错误响应 —— 退回用 HTTP status 作为 code。
func TestBusinessError_FallbackToHTTPStatus(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)
	// 模拟 wkhttp.AuthMiddleware: AbortWithStatusJSON(401, {"msg": ...})
	// body 没有 "status" 字段,必须靠 HTTP status 兜底。
	r.GET("/v1/auth", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "please login"})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/auth", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := counterValue(t, reg, "dmwork_http_business_error_total",
		map[string]string{"path": "/v1/auth", "code": "401"})
	if got != 1 {
		t.Errorf("expected counter=1 for {path=/v1/auth, code=401}, got %v", got)
	}
}

// TestBusinessError_PanicCountedAs500 panic 时 body 通常未写出,
// 应当走 panic 分支按 500 计入,且 path label 仍是路由模板。
func TestBusinessError_PanicCountedAs500(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)
	r.GET("/v1/panic", func(c *gin.Context) { panic("boom") })

	req := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := counterValue(t, reg, "dmwork_http_business_error_total",
		map[string]string{"path": "/v1/panic", "code": "500"})
	if got != 1 {
		t.Errorf("expected counter=1 for {path=/v1/panic, code=500}, got %v", got)
	}
}

// TestBusinessError_SuccessPathNotCounted 普通 200 响应(无 envelope status 或
// envelope status=200)不应触发计数 —— 防止误报。
func TestBusinessError_SuccessPathNotCounted(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)
	r.GET("/v1/list", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": []string{"a", "b"}}) // 无 status 字段
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/list", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}

	families, _ := reg.Gather()
	for _, mf := range families {
		if mf.GetName() == "dmwork_http_business_error_total" && len(mf.GetMetric()) > 0 {
			t.Errorf("expected no business_error_total samples on 200, got %d series",
				len(mf.GetMetric()))
		}
	}
}

// TestBusinessError_TruncatedBodyFallsBack 当 body 超过 bodyCaptureLimit
// 被截断时, JSON 解析失败 -> 用 HTTP status 兜底, 不应漏报或误报。
func TestBusinessError_TruncatedBodyFallsBack(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)
	r.GET("/v1/big", func(c *gin.Context) {
		// 构造 > 1 KiB 的错误响应,且把 status 字段塞到末尾(map 顺序不定也
		// 大概率被截断), HTTP 状态显式 400 提供兜底信号。
		big := make([]string, 200)
		for i := range big {
			big[i] = "padpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpad"
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"items":  big,
			"msg":    "validation failed",
			"status": 400,
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/big", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := counterValue(t, reg, "dmwork_http_business_error_total",
		map[string]string{"path": "/v1/big", "code": "400"})
	if got != 1 {
		t.Errorf("expected fallback counter=1 for truncated body, got %v", got)
	}
}

// TestBusinessError_UnmatchedRouteUses404 未匹配路由 gin 返回 404,
// path label 应走 "unmatched", code 走 HTTP status 兜底。
func TestBusinessError_UnmatchedRouteUses404(t *testing.T) {
	r, _, reg := newRouterWithMetrics(t)

	req := httptest.NewRequest(http.MethodGet, "/never-registered", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := counterValue(t, reg, "dmwork_http_business_error_total",
		map[string]string{"path": "unmatched", "code": "404"})
	if got != 1 {
		t.Errorf("expected counter=1 for {path=unmatched, code=404}, got %v", got)
	}
}

func TestNewHTTPMetrics_PanicsOnDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = metrics.NewHTTPMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	_ = metrics.NewHTTPMetrics(reg)
}
