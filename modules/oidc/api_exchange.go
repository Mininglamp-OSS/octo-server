package oidc

// api_exchange.go — POST /v1/auth/oidc/<id>/exchange
//
// Token-exchange entry point for bearer / native clients that complete SSO
// themselves (system browser → local callback server) and hand us an
// IdP-issued credential in exchange for our own session token.
//
// **Which credential depends on the provider kind, and the field name does not
// say so.** The JSON field is `access_token` (kept for wire compatibility), but
// the value is interpreted by the provider, not by this file:
//
//   - kind=oidc   → the value must be an **id_token**. It is the only credential
//     a client can hold that we can verify independently; an OAuth2 access_token
//     is an opaque string meant for a resource server, and accepting it as proof
//     of identity would mean not verifying anything.
//   - kind=oauth2 → the value is the opaque **access_token**, redeemed against
//     /userinfo.
//
// Getting this wrong yields the generic anti-enumeration 401 with no hint, so
// the mismatch is stated here rather than left to be discovered. Renaming the
// field would be a breaking change for existing clients; that trade-off is
// recorded in the task brief's Pending section.
//
// vs /callback:/callback is the browser redirect flow (code → token → userinfo,
// result handed back via ThirdAuthcode polling)./exchange is the direct JSON
// API for clients that already hold such a credential; it returns the session
// JSON in the response body.
//
// Both paths share Identity → ResolveOrLink → IssueSession; transport
// (redirect vs JSON) and anti-CSRF (state vs TLS channel) differ.

import (
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

const (
	// exchangeDefaultDeviceFlag exchange 缺省按 PC 设备处理(callback 默认 APP=0),
	// 因为 exchange 就是给原生客户端用的。
	exchangeDefaultDeviceFlag = uint8(2)
	// exchangeMaxBodyBytes 限制请求体大小。本端点 body 只有 access_token(<4KB) + flag,
	// 16KB 已经留出足够裕量;不限制会让无认证客户端发 GB 级 body 压垮内存/CPU。
	exchangeMaxBodyBytes = int64(16 << 10)

	// exchangeIPLimitRPS / exchangeIPLimitBurst 是 /exchange 与 /exchange-jwt 共用的
	// 端点级 per-IP 限流参数。写成常量而非 env,与 user 模块 login/register/sms 一致:
	// 这两个端点是登录等价入口,阈值属于安全基线,调它应当走 code review 而不是
	// 部署时旋钮。全局 500rps/1000burst 的 IP 桶对"每请求触发一次出站 HTTP 或 DB 写"
	// 的端点太宽,所以必须另设一层。
	exchangeIPLimitRPS   = 2.0
	exchangeIPLimitBurst = 10
)

type exchangeRequest struct {
	// AccessToken 名字来自 wire 兼容,**取值随 provider kind 而变**:
	// kind=oidc 时这里必须放 id_token,kind=oauth2 时放不透明 access_token。
	// 见文件头。
	AccessToken string `json:"access_token"`
	DeviceFlag  *uint8 `json:"flag,omitempty"`
}

// exchange POST /v1/auth/oidc/<id>/exchange
//
// 入: {access_token: string, flag?: uint8}
// 出: 200 {status:"ok", uid, login_resp}
//
//	| 400 请求格式错
//	| 401 token 被 IdP 拒绝(反枚举,所有失败统一此码)
//
// Anti-enumeration:任何无法确立身份的情况(IdP 返 401、信封失败、subject 空、
// 网络失败、ResolveOrLink 失败、IssueSession 失败)一律返同一个 401。具体原因
// 只进 zap 日志。客户端看到 401 应重新走 SSO。
//
// 不接入 bind 流程:bind 是浏览器跳转流的自助交互(选账号/输密码/收短信),
// 原生客户端的静默 exchange 没有这个交互上下文。
//
// Rate limit: 全局 per-IP 桶(main.go 的 RateLimitMiddleware)兜底之外,路由上
// 另挂了端点级 StrictIPRateLimitMiddleware("oidc_exchange", 见 Route)——
// 全局桶的 500rps 对一个每请求都外呼 IdP 并可能写库的登录端点太宽。
func (o *OIDC) exchange(c *wkhttp.Context) {
	metricExchangeTotal.Inc()

	if o.provider == nil || o.service == nil {
		// 构造阶段已打过 Error 日志;这里只返 500,不给攻击者探测端点状态的信息。
		// 这个直接 AbortWithStatusJSON 是和 api.go authorize/callback 同一条豁免
		// 路径的扩展:OAuth2/OIDC 协议端点的浏览器/原生重定向与 JSON 入口,不走
		// i18n 信封(api.go 的 14 处同理)。baseline 计数见 client_ip_source_test.go。
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("oidc not initialized"))
		return
	}

	var req exchangeRequest
	// 限制 body 大小,防止未认证请求用大 body 耗尽内存;两个 exchange 端点共用常量。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, exchangeMaxBodyBytes)
	if err := c.BindJSON(&req); err != nil {
		metricExchangeResult.WithLabelValues("bad_request").Inc()
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeRequestInvalid, nil, nil)
		return
	}
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	if req.AccessToken == "" {
		metricExchangeResult.WithLabelValues("bad_request").Inc()
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeRequestInvalid, nil, nil)
		return
	}

	deviceFlag := exchangeDefaultDeviceFlag
	if req.DeviceFlag != nil {
		deviceFlag = *req.DeviceFlag
	}
	clientIP := wkhttp.ClientIP(c.Request)
	traceID := newTraceID()
	sd := &StateData{IP: clientIP, DeviceFlag: deviceFlag}

	// 由 provider 按自己的协议事实解释这个客户端出示的凭据:标准 OIDC 下它是
	// id_token(验签),plain-OAuth2 下它是不透明 access_token(拉 /userinfo)。
	//
	// 早先这里硬编码成 &TokenSet{AccessToken: ...},于是在标准 OIDC kind 下
	// 永远失败(oidcProvider.Identity 要求 RawIDToken)—— 存量部署白得一个只会
	// 返 401 的无认证端点。上层不该知道该往哪个字段放,那是 provider 的事。
	claims, err := o.provider.IdentityFromClientCredential(c.Request.Context(), req.AccessToken)
	if err != nil {
		metricExchangeResult.WithLabelValues("identity_fail").Inc()
		o.Warn("OIDC exchange: identity rejected",
			zap.String("trace_id", traceID), zap.String("ip", clientIP), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	o.completeExchange(c, verifiedIdentity{
		claims:     claims,
		deviceFlag: deviceFlag,
		clientIP:   clientIP,
		traceID:    traceID,
		state:      sd,
		// subject 是长数字串,进审计无意义;它已经以 hash 形式进了日志。
		auditDetail: "",
	}, exchangeFlavour{
		logName:   "exchange",
		result:    metricExchangeResult,
		eventOK:   EventExchangeOK,
		eventFail: EventExchangeFail,
	})
}
