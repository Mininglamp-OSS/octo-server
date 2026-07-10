package docs_proxy

// 反向代理实现，把 /v1/docs/proxy/*path 转发到 OCTO_DOCS_UPSTREAM。
//
// 契约（与父单 OCT-136 §4 + FEAT-1 身份桥 + OCT-145 方案 C 内网信任头一致）：
//   - AuthMiddleware 已在 group 上：未登录一律 401，匿名不透传。
//   - 转发时注入 X-Octo-Token = 当前请求的 "token" header 原值（AuthMiddleware
//     校验过合法性），供 doc 侧 FEAT-1 反查 uid。
//   - OCT-145 方案 C：反代→doc 走内网信任通道，额外注入身份三元组
//     X-Octo-Uid / X-Octo-Name / X-Octo-Role，doc 侧直接消费，不再回打 userinfo。
//     注入前一律 Del 掉 caller 可能自带的同名头，防伪造身份。
//   - 剥离 caller 侧 Authorization / Cookie / 原始 token header，防止 leak 到 upstream。
//   - hop-by-hop headers（RFC 7230 §6.1 + Trailer/Upgrade）请求响应两侧都 strip。
//   - 响应 body 直接 streaming（httputil 默认行为），不整段缓存。
//   - CORS 预检 OPTIONS：直接放行，不反代（返回 204，Allow-* 由前置网关处理）。
//
// 部署侧硬约束（OCT-145 方案 C）：反代→doc 必须走内网，doc 侧只在内网监听。
// 三个身份头一旦被外网 caller 直接打到 doc 就是身份伪造缺口 —— 见 README.md。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 反代挂在 /v1/docs 下，具体前缀 /v1/docs/proxy。
// 路径映射：/v1/docs/proxy/<rest> → upstream <rest>（不带 /v1/docs/proxy 前缀）。
// 这样 doc 侧的 handler 看到的就是自己的原生路径（例 /v1/docs/foo/versions）。
const (
	routePrefix   = "/v1/docs/proxy"
	upstreamEnv   = "OCTO_DOCS_UPSTREAM"
	timeoutEnv    = "OCTO_DOCS_PROXY_TIMEOUT_MS"
	defaultTimeMS = 30_000

	// header 常量：常量而不是字面量是为了单测能引用同一份 key，避免测试与实现字面漂移。
	headerOctoToken = "X-Octo-Token"
	headerOctoUID   = "X-Octo-Uid"
	headerOctoName  = "X-Octo-Name"
	headerOctoRole  = "X-Octo-Role"
	headerToken     = "token"
	headerAuth      = "Authorization"
	headerCookie    = "Cookie"

	// role 值域：doc 侧按稳定字符串判权（superAdmin/admin/member），
	// 未识别一律降级为 member，避免把空串或未知字面吐给 doc 造成隐式提权。
	roleSuperAdmin = "superAdmin"
	roleAdmin      = "admin"
	roleMember     = "member"
)

// hopByHop 是 RFC 7230 §6.1 的 hop-by-hop headers，加 Trailer/Upgrade。
// 请求侧 strip 避免透传到 upstream；响应侧 strip 避免透传回 caller。
// httputil.ReverseProxy 只 strip 请求侧标准集合，响应侧不动，所以自己再兜底一遍。
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Transfer-Encoding",
	"Te",
	"Trailer",
	"Upgrade",
	"Proxy-Authorization",
	"Proxy-Authenticate",
}

// Proxy 是 docs_proxy handler。disabled=true 时 Route 不挂任何 endpoint。
type Proxy struct {
	ctx      *config.Context
	upstream *url.URL
	rp       *httputil.ReverseProxy
	timeout  time.Duration
	disabled bool
	log.Log
}

// New 读环境变量构造 Proxy。upstream 未配置 → 返回 nil（1module.go 依此判定 disable）。
// upstream 解析失败 → 也返回 nil 并 warn，避免启动崩溃。
func New(ctx *config.Context) *Proxy {
	upstream := strings.TrimSpace(os.Getenv(upstreamEnv))
	if upstream == "" {
		return nil
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		log.NewTLog("docs_proxy").Error(
			"OCTO_DOCS_UPSTREAM 解析失败，docs_proxy 未启用",
			zap.String("value", upstream), zap.Error(err),
		)
		return nil
	}
	timeout := time.Duration(defaultTimeMS) * time.Millisecond
	if v := strings.TrimSpace(os.Getenv(timeoutEnv)); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}

	p := &Proxy{
		ctx:      ctx,
		upstream: u,
		timeout:  timeout,
		Log:      log.NewTLog("docs_proxy"),
	}
	p.rp = &httputil.ReverseProxy{
		Director:       p.director,
		ErrorHandler:   p.errorHandler,
		ModifyResponse: p.modifyResponse,
		Transport: &http.Transport{
			// 单独一份 transport 是为了：(1) 自己控超时；(2) 不污染进程默认 Transport 的连接池。
			ResponseHeaderTimeout: timeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   16,
		},
	}
	return p
}

// Route 挂反代到 AuthMiddleware 下；disabled=true 时不挂任何路径。
// 用 wkhttp.WKHttp.Any 一次覆盖全 HTTP method（父单 §2：GET/POST/PUT/DELETE/HEAD/OPTIONS 全支持）。
// AuthMiddleware 作为 chain 前置：token 缺失/失效 → 401 abort，proxy handler 拿不到匿名请求。
// OPTIONS 在 handler 内 204 短路；wkhttp 顶层 CORSMiddleware 若已挂，preflight 更早就被截了。
func (p *Proxy) Route(r *wkhttp.WKHttp) {
	if p.disabled {
		return
	}
	r.Any(routePrefix+"/*action", p.ctx.AuthMiddleware(r), p.handle)
}

// handle 是 4 种 method 的统一入口；OPTIONS 直接 204 短路（CORS preflight 不反代）。
func (p *Proxy) handle(c *wkhttp.Context) {
	if c.Request.Method == http.MethodOptions {
		c.Status(http.StatusNoContent)
		return
	}
	// AuthMiddleware 已保证 uid 存在；再兜一次是防 middleware 顺序漂移导致 silent 透传匿名请求。
	if c.GetLoginUID() == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	// caller 的原始 token：AuthMiddleware 用它反查过 uid，拿来即当前用户 octo token。
	// 拿不到（middleware 侧写了 uid 但清了 header 的极端场景）→ 401 明确报错，别静默透传。
	token := c.Request.Header.Get(headerToken)
	if token == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	// OCT-145 方案 C：把已鉴权的登录态一并封进 context，director 里注入内网信任头。
	// name/role 允许为空（部分登录路径可能没写全）；role 用 normalizeRole 收敛值域。
	id := identity{
		token: token,
		uid:   c.GetLoginUID(),
		name:  c.GetLoginName(),
		role:  normalizeRole(c.GetLoginRole()),
	}
	c.Request = c.Request.WithContext(withIdentity(c.Request.Context(), id))
	p.rp.ServeHTTP(c.Writer, c.Request)
}

// director 改写请求指向 upstream；houseKeeping：
//  1. scheme/host 换成 upstream；
//  2. path 剥掉 /v1/docs/proxy 前缀（保留后续路径原样，包括查询串）；
//  3. 注入 X-Octo-Token，删掉 caller 侧敏感 header 与 hop-by-hop；
//  4. Host header 改成 upstream host（否则 upstream 侧虚拟主机会 misroute）。
func (p *Proxy) director(req *http.Request) {
	req.URL.Scheme = p.upstream.Scheme
	req.URL.Host = p.upstream.Host

	// 剥前缀：/v1/docs/proxy/foo/bar → /foo/bar；恰等于 /v1/docs/proxy → /（避免路径为空）。
	rest := strings.TrimPrefix(req.URL.Path, routePrefix)
	if rest == "" {
		rest = "/"
	}
	// upstream 若本身带 basepath（如 http://host/api），拼一次以保留。
	req.URL.Path = singleJoiningSlash(p.upstream.Path, rest)
	req.Host = p.upstream.Host

	// 敏感 header：caller 的 Authorization / Cookie / token 一律不透传，upstream 只信 X-Octo-Token。
	// Cookie 保留业务侧 Set-Cookie 响应，但请求侧不带过去（避免会话跨域串）。
	req.Header.Del(headerAuth)
	req.Header.Del(headerCookie)
	req.Header.Del(headerToken)
	// OCT-145 方案 C：三个身份头 Del 后 Set，防 caller 自带同名头伪造身份。
	// 三个 Del 一个都不能漏 —— 少 Del 一个就是伪造缺口。Set 走已鉴权的登录态。
	req.Header.Del(headerOctoUID)
	req.Header.Del(headerOctoName)
	req.Header.Del(headerOctoRole)
	// hop-by-hop 请求侧清理。
	for _, h := range hopByHop {
		req.Header.Del(h)
	}
	// 从 context 拿 handle 阶段落进来的登录态，token 是必有的，身份三元组按值注入
	// （uid/name 允许空串 —— 空串代表登录态里就没有，doc 侧自己兜底，不写空避免误覆盖）。
	if id, ok := identityFromContext(req.Context()); ok {
		if id.token != "" {
			req.Header.Set(headerOctoToken, id.token)
		}
		if id.uid != "" {
			req.Header.Set(headerOctoUID, id.uid)
		}
		if id.name != "" {
			req.Header.Set(headerOctoName, id.name)
		}
		// role 一定写：normalizeRole 保证一定是 superAdmin/admin/member 之一。
		req.Header.Set(headerOctoRole, id.role)
	}
	// User-Agent 空值时 http 客户端会自加默认 UA，为让 upstream 日志更清楚，显式打标。
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "octo-server/docs_proxy")
	}
}

// modifyResponse 响应侧 hop-by-hop strip；Set-Cookie 保留（caller 端设置 doc 侧 session 有用途）。
// 不缓冲 body：httputil.ReverseProxy 默认 Copy(w, resp.Body) streaming。
func (p *Proxy) modifyResponse(resp *http.Response) error {
	for _, h := range hopByHop {
		resp.Header.Del(h)
	}
	return nil
}

// errorHandler：upstream 超时/连接失败 → 502 Bad Gateway。默认 ReverseProxy 是 502，
// 但自定义一版是为了：(1) 打结构化日志方便定位；(2) 不把 upstream 内部错误字面回吐给 caller。
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	p.Warn("docs_proxy upstream 调用失败",
		zap.String("path", r.URL.Path),
		zap.String("method", r.Method),
		zap.Error(err),
	)
	// context canceled：caller 主动断，不算故障，回 499 会误报；沿用 502。
	if errors.Is(err, http.ErrAbortHandler) {
		return
	}
	w.WriteHeader(http.StatusBadGateway)
}

// ---- context key --------------------------------------------------------

// identity 是反代要注入 doc 的登录态快照（OCT-145 方案 C）。放 context 而不是
// 全局 map，避免并发请求相互覆盖；handle → director 单向传递，值语义无锁。
type identity struct {
	token string
	uid   string
	name  string
	role  string
}

type ctxKey struct{}

func withIdentity(parent context.Context, id identity) context.Context {
	return context.WithValue(parent, ctxKey{}, id)
}
func identityFromContext(c context.Context) (identity, bool) {
	v, ok := c.Value(ctxKey{}).(identity)
	return v, ok
}

// normalizeRole 把 wkhttp 现成的 role 字面收敛到稳定值域，避免把空串/未知字面
// 直接吐给 doc。superAdmin/admin 保留原样，其他一律降级 member（不提权）。
func normalizeRole(raw string) string {
	switch raw {
	case roleSuperAdmin:
		return roleSuperAdmin
	case roleAdmin:
		return roleAdmin
	default:
		return roleMember
	}
}

// singleJoiningSlash 抄 net/http/httputil 的私有工具：合并 basepath 与后续 path，
// 避免出现 // 或漏斜杠。
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
