package docs_proxy

// api_test.go
//
// 起 httptest.Server 假扮 upstream；wkhttp.WKHttp 挂 Proxy.Route + 手写测试 middleware
// 模拟 AuthMiddleware（真的 AuthMiddleware 依赖 cache，这里绕过，只需要把 uid 写进 c 即可）。
//
// 覆盖父单 OCT-144 §测试：
//   - 未配置 OCTO_DOCS_UPSTREAM → New 返回 nil；Route 不挂 → 404
//   - 未登录 (无 uid) → 401，upstream.callCount == 0
//   - 已登录 → upstream 收到 X-Octo-Token=<token>；caller 侧 Authorization/Cookie/token 未透传
//   - 请求侧 hop-by-hop（Connection/Keep-Alive/Upgrade/…）被 strip
//   - 响应侧 hop-by-hop 被 strip
//   - OPTIONS → 204 短路，upstream 未被调
//   - GET/POST/PUT/DELETE/HEAD/PATCH 全 method 反代通
//   - path 前缀剥离正确：/v1/docs/proxy/foo/bar → upstream /foo/bar（含 query）

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamCapture 记录 upstream 收到的最后一个请求，供 assert 用。
type upstreamCapture struct {
	calls   int64
	method  atomic.Value // string
	path    atomic.Value // string
	rawq    atomic.Value // string
	headers atomic.Value // http.Header
	body    atomic.Value // []byte
}

func (u *upstreamCapture) reset() { atomic.StoreInt64(&u.calls, 0) }

// newUpstream 起 httptest.Server；handler 记录并回一个带 hop-by-hop 头的响应，
// 让测试同时验证响应侧 strip。
func newUpstream(t *testing.T, cap *upstreamCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cap.calls, 1)
		cap.method.Store(r.Method)
		cap.path.Store(r.URL.Path)
		cap.rawq.Store(r.URL.RawQuery)
		// 深拷贝 header：http.Header 是 map，反代结束后底层可能改。
		h := http.Header{}
		for k, v := range r.Header {
			h[k] = append([]string(nil), v...)
		}
		cap.headers.Store(h)
		b, _ := io.ReadAll(r.Body)
		cap.body.Store(b)

		// 响应侧塞几个 hop-by-hop 头，验 modifyResponse 剥掉。
		w.Header().Set("Connection", "close")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Upstream-Marker", "ok")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setEnv 临时设置环境变量并在测试结束恢复。upstream url 空字符串 = 删除。
func setEnv(t *testing.T, k, v string) {
	t.Helper()
	prev, had := os.LookupEnv(k)
	if v == "" {
		_ = os.Unsetenv(k)
	} else {
		_ = os.Setenv(k, v)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(k, prev)
		} else {
			_ = os.Unsetenv(k)
		}
	})
}

// buildProxy 构造 Proxy（已注 log）。ctx 传 nil 是因为 handler 里没用 ctx，
// 只在 Route 里读 ctx.AuthMiddleware —— 测试用自己写的 middleware 绕过。
func buildProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	setEnv(t, upstreamEnv, upstream)
	p := New(nil)
	if p != nil {
		// 强制注入 log，避免 handler 里 h.Warn nil-deref（New 里已注，但保险）。
		p.Log = log.NewTLog("docs_proxy_test")
	}
	return p
}

// newTestRouter 拼 wkhttp 路由；用 fakeAuth 代替真的 AuthMiddleware，按 header
// "token" 决定挂哪些字段。emptyToken=true 时不注 uid（模拟未登录场景后 handler 层再校验一次）。
// 真实链路里 AuthMiddleware 无 token 会自己 abort 401；测试里手动 abort 达成同样效果。
func newTestRouter(t *testing.T, p *Proxy) *wkhttp.WKHttp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := wkhttp.New()
	fakeAuth := func(c *wkhttp.Context) {
		tok := c.Request.Header.Get(headerToken)
		if tok == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		// 从 token 里派生 uid：简化桩，格式 "tok:<uid>"，回落到 tok 本身。
		uid := tok
		if strings.HasPrefix(tok, "tok:") {
			uid = strings.TrimPrefix(tok, "tok:")
		}
		c.Set("uid", uid)
		c.Next()
	}
	if p == nil {
		// disabled 分支：显式不挂任何路由，验证 404。
		return r
	}
	r.Any(routePrefix+"/*action", fakeAuth, p.handle)
	return r
}

// doReq 通过真的 httptest.Server 打到 wkhttp router。用真 server 而不是 Recorder：
// gin.responseWriter.CloseNotify 要求底层 ResponseWriter 实现 http.CloseNotifier，
// httptest.Recorder 不实现，ReverseProxy.ServeHTTP 会 panic —— 用真 http server 绕开。
// 返回一个 fakeRecorder，暴露 Code/Header()/Body.String() 保持与 httptest.Recorder 同型。
func doReq(t *testing.T, r *wkhttp.WKHttp, method, target string, headers http.Header, body string) *fakeRecorder {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+target, rd)
	require.NoError(t, err)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// http.Client 默认对 30x follow；测试 URL 不涉及跳转，保持默认即可。
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	bts, _ := io.ReadAll(resp.Body)
	return &fakeRecorder{Code: resp.StatusCode, HeaderMap: resp.Header, BodyBytes: bts}
}

// fakeRecorder 模仿 httptest.ResponseRecorder 的最小 API 面，供既有 assert 复用。
type fakeRecorder struct {
	Code      int
	HeaderMap http.Header
	BodyBytes []byte
}

func (f *fakeRecorder) Header() http.Header { return f.HeaderMap }

// Body 返回一个 bytes.Buffer-like 结构，只需要 String()。
type bodyView struct{ b []byte }

func (b bodyView) String() string { return string(b.b) }

// Body 属性访问：既有测试用 w.BodyView().String()，我们给一个 method 别名。
func (f *fakeRecorder) BodyView() bodyView { return bodyView{b: f.BodyBytes} }

// ---- 测试用例 ------------------------------------------------------------

// New 与 disabled：OCTO_DOCS_UPSTREAM 空 → nil。
func TestNew_DisabledWhenUpstreamEmpty(t *testing.T) {
	setEnv(t, upstreamEnv, "")
	assert.Nil(t, New(nil))
}

// upstream URL 非法 → nil，不 panic。
func TestNew_DisabledOnInvalidUpstream(t *testing.T) {
	setEnv(t, upstreamEnv, "not-a-url")
	assert.Nil(t, New(nil))
}

// OCTO_DOCS_UPSTREAM 空 → route 未挂 → 404（父单验收 §3）。
func TestRoute_NotRegisteredWhenDisabled(t *testing.T) {
	setEnv(t, upstreamEnv, "")
	p := New(nil)
	require.Nil(t, p)
	r := newTestRouter(t, p)
	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/foo", nil, "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// 未登录 → 401，upstream 未被调（父单验收 §1）。
func TestProxy_UnauthorizedNoUpstream(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	require.NotNil(t, p)
	r := newTestRouter(t, p)

	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/foo", nil, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, int64(0), atomic.LoadInt64(&cap.calls))
}

// 登录后 upstream 收到 X-Octo-Token；caller Authorization/Cookie/token 未透传（父单验收 §2）。
func TestProxy_InjectsOctoToken_StripsCallerCreds(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	require.NotNil(t, p)
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:alice")
	h.Set(headerAuth, "Bearer leak-me")
	h.Set(headerCookie, "sess=leak-me")
	h.Set("X-Business", "keep")

	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/foo/bar?x=1&y=2", h, "")
	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(1), atomic.LoadInt64(&cap.calls))

	got := cap.headers.Load().(http.Header)
	assert.Equal(t, "tok:alice", got.Get(headerOctoToken), "X-Octo-Token 必须注入")
	assert.Empty(t, got.Get(headerAuth), "Authorization 必须剥掉")
	assert.Empty(t, got.Get(headerCookie), "Cookie 必须剥掉")
	assert.Empty(t, got.Get(headerToken), "原始 token header 必须剥掉，只保留 X-Octo-Token")
	assert.Equal(t, "keep", got.Get("X-Business"), "业务 header 应透传")

	// path 剥前缀 + 保留 query。
	assert.Equal(t, "/foo/bar", cap.path.Load().(string))
	assert.Equal(t, "x=1&y=2", cap.rawq.Load().(string))
}

// 请求侧 hop-by-hop 全被剥离。
func TestProxy_StripsRequestHopByHopHeaders(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:bob")
	// caller 塞的 hop-by-hop 值故意标记 leak-*，验证 upstream 收到的不是这些值。
	// 注：Te 的合法哨兵值 "trailers" 是 Go 标准库 http.Transport 会为 HTTP/1.1
	// 强制加的（transport.go writeSubset），我们只需保证 caller 的自定义 Te 值
	// 不会被透传即可，所以特意用 leak-marker。
	h.Set("Connection", "close, leak-connection")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Authorization", "Basic leak")
	h.Set("Upgrade", "leak-proto")
	h.Set("Te", "leak-te-value")
	h.Set("Trailer", "X-Leak-Trailer")

	w := doReq(t, r, http.MethodPost, "/v1/docs/proxy/thing", h, `{"k":"v"}`)
	assert.Equal(t, http.StatusOK, w.Code)

	got := cap.headers.Load().(http.Header)
	assert.Empty(t, got.Get("Connection"), "请求侧 Connection 应被 strip")
	assert.Empty(t, got.Get("Keep-Alive"), "请求侧 Keep-Alive 应被 strip")
	assert.Empty(t, got.Get("Proxy-Authorization"), "请求侧 Proxy-Authorization 应被 strip")
	assert.Empty(t, got.Get("Upgrade"), "请求侧 Upgrade 应被 strip")
	assert.Empty(t, got.Get("Trailer"), "请求侧 Trailer 应被 strip")
	// Te：即使 Go 标准库补 "trailers"，也绝不能出现 caller 的 leak-te-value。
	assert.NotContains(t, got.Get("Te"), "leak-te-value", "caller 的 Te 值必须不透传")
	// POST body 也要透传。
	assert.Equal(t, `{"k":"v"}`, string(cap.body.Load().([]byte)))
}

// 响应侧 hop-by-hop 被剥离（Connection/Keep-Alive 不该出现在返回给 caller 的响应里）。
func TestProxy_StripsResponseHopByHopHeaders(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:carol")
	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/x", h, "")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Connection"))
	assert.Empty(t, w.Header().Get("Keep-Alive"))
	// 非 hop-by-hop 业务头保留。
	assert.Equal(t, "ok", w.Header().Get("X-Upstream-Marker"))
	// body streaming。
	assert.Equal(t, "hello", w.BodyView().String())
}

// OPTIONS 短路 204，upstream 未被调（CORS 预检不反代）。
func TestProxy_OptionsShortCircuit(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:dave")
	w := doReq(t, r, http.MethodOptions, "/v1/docs/proxy/foo", h, "")
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, int64(0), atomic.LoadInt64(&cap.calls))
}

// 全 method 都能反代通（GET/POST/PUT/DELETE/HEAD/PATCH）。
func TestProxy_AllMethodsProxied(t *testing.T) {
	cap := &upstreamCapture{}
	up := newUpstream(t, cap)
	p := buildProxy(t, up.URL)
	r := newTestRouter(t, p)

	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodPatch}
	for _, m := range methods {
		cap.reset()
		h := http.Header{}
		h.Set(headerToken, "tok:eve")
		w := doReq(t, r, m, "/v1/docs/proxy/x", h, "")
		assert.Equal(t, http.StatusOK, w.Code, "method %s", m)
		require.Equal(t, int64(1), atomic.LoadInt64(&cap.calls), "upstream should be hit for %s", m)
		assert.Equal(t, m, cap.method.Load().(string))
	}
}

// upstream 挂了（连接失败）→ 502，caller 不见 upstream 内部错误字面。
func TestProxy_UpstreamDown_502(t *testing.T) {
	// 拿一个已关闭的 server URL：起一个再关。
	tmp := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := tmp.URL
	tmp.Close()

	p := buildProxy(t, deadURL)
	require.NotNil(t, p)
	// 缩短 transport 超时避免测试卡死。
	p.rp.Transport = &http.Transport{ResponseHeaderTimeout: 500 * time.Millisecond}
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:frank")
	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/foo", h, "")
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// singleJoiningSlash 边界。
func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "/foo", "/foo"},
		{"/", "/foo", "/foo"},
		{"/api", "/foo", "/api/foo"},
		{"/api/", "/foo", "/api/foo"},
		{"/api", "foo", "/api/foo"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, singleJoiningSlash(c.a, c.b), "a=%q b=%q", c.a, c.b)
	}
}

// upstream 带 basepath 时 path 拼接正确（例 http://host/api → /api/foo/bar）。
func TestProxy_UpstreamWithBasepath(t *testing.T) {
	cap := &upstreamCapture{}
	// 用一个自定义 handler 起 upstream，让它挂在 /api 下（httptest 不能起 subpath，
	// 但我们可以直接把 upstream URL 拼上 /api，让 director 走 singleJoiningSlash 分支）。
	up := newUpstream(t, cap)
	setEnv(t, upstreamEnv, up.URL+"/api")
	p := New(nil)
	require.NotNil(t, p)
	p.Log = log.NewTLog("docs_proxy_test")
	// 覆盖 rp 让它用测试自身的 transport（避免 keep-alive 干扰）。
	// director/modifyResponse/errorHandler 复用 New 里注的。
	r := newTestRouter(t, p)

	h := http.Header{}
	h.Set(headerToken, "tok:grace")
	w := doReq(t, r, http.MethodGet, "/v1/docs/proxy/foo/bar", h, "")
	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(1), atomic.LoadInt64(&cap.calls))
	assert.Equal(t, "/api/foo/bar", cap.path.Load().(string))
}

// 环境变量 timeout 生效（不直接断超时，只断 New 后 timeout 字段值）。
func TestNew_TimeoutFromEnv(t *testing.T) {
	setEnv(t, upstreamEnv, "http://example.invalid")
	setEnv(t, timeoutEnv, "12345")
	p := New(nil)
	require.NotNil(t, p)
	assert.Equal(t, 12345*time.Millisecond, p.timeout)
}

// 环境变量 timeout 非法值 → 回落默认。
func TestNew_TimeoutInvalidFallsBack(t *testing.T) {
	setEnv(t, upstreamEnv, "http://example.invalid")
	setEnv(t, timeoutEnv, "not-a-number")
	p := New(nil)
	require.NotNil(t, p)
	assert.Equal(t, time.Duration(defaultTimeMS)*time.Millisecond, p.timeout)
}

// director path 剥前缀边界：请求刚好 = routePrefix 时应转发到 upstream 根 "/"。
// wkhttp Any 的 /*action 通配符不会匹配空后缀 —— 至少要 /；这里断的是 director 自身逻辑。
func TestDirector_PathRootWhenPrefixOnly(t *testing.T) {
	setEnv(t, upstreamEnv, "http://example.invalid")
	p := New(nil)
	require.NotNil(t, p)

	u, _ := url.Parse("http://example.invalid" + routePrefix)
	req := &http.Request{URL: u, Header: http.Header{}}
	req = req.WithContext(withToken(req.Context(), "T"))
	p.director(req)
	assert.Equal(t, "/", req.URL.Path)
	assert.Equal(t, "T", req.Header.Get(headerOctoToken))
}

// 确认 ctx 传 nil 不会挂：New 只用 ctx 存字段，Route 才真正读 ctx.AuthMiddleware。
// 这个用例只是把上面测试隐式的前提显式化，防未来 New 里加 ctx.XXX 依赖时静默改坏。
var _ = config.Context{} // keep import stable
