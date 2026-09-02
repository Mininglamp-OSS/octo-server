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
	"time"

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
	// 外呼之前的第一道:这是不是一张**我们自己签的业务 JWT**?
	//
	// 顺序与 modules/integration 的 oidcAuth 一致,而且必须一致 —— 那边靠 HMAC
	// 验签认出这一类,这边原先根本没有验签这一步,于是同一张 token 在两个端点上
	// 得到相反的处理。守卫挂在两个消费者中的一个,是这个改动反复踩的形态。
	//
	// 判据是 HMAC 而不是形态:一张 token 要么带着我方密钥下的合法签名,要么没有。
	// 所以这道检查不会误伤 JWT 形态的上游不透明 token(造不出合法签名)。
	// 密钥**缺失**时 o.bearerJWT 为 nil,C3 凭据不可能存在,行为与之前完全一致。
	// 密钥**无效**是另一回事,由上面 bearerJWTErr 那段处理。
	// 构造失败态先处理:密钥配了但无效(比如 31 字节)时验签器为 nil,而客户端
	// 业务后端拿的是**同一个值**在签 —— HMAC 不在乎密钥长度,32 字节是我方准入
	// 策略。所以那张 token 带着在我方配置密钥下合法的签名,转发等于把签名材料和
	// 载荷 PII 一起送进第三方访问日志。
	//
	// 归属问题在这个状态下**没有答案**,所以拒绝**每一个**凭据(含上游凭据),
	// 与 modules/integration 同一标准。"没配密钥"仍是合法部署形态,不走这里。
	if o.bearerJWTErr != nil {
		metricExchangeResult.WithLabelValues("verifier_unavailable").Inc()
		o.Error("OIDC exchange: bearer verifier failed to construct; refusing every credential "+
			"rather than forwarding to the upstream IdP",
			zap.String("trace_id", traceID), zap.Error(o.bearerJWTErr))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	// 密钥**缺失**时无法做任何归属判定,而这个 provider 的 access_token 是不透明串,
	// 所以 JWT 形态的凭据在这条路上不可能合法 —— 转发它只可能泄漏。见 jwt_shape.go。
	//
	// 上一轮只关了"密钥无效"。否掉那半的是同一条论证:客户端持有并使用**它自己的**
	// 密钥,跟我方配没配无关。这次改的是谓词,两扇门一起。
	if UnverifiableJWTMustNotBeForwarded(o.bearerJWT != nil, o.provider.Capabilities(), req.AccessToken) {
		metricExchangeResult.WithLabelValues("unverifiable_jwt").Inc()
		o.Warn("OIDC exchange: refused a JWT-shaped credential with no bearer verifier configured",
			zap.String("trace_id", traceID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}
	if o.bearerJWT != nil {
		if _, berr := o.bearerJWT.VerifyForRedemption(req.AccessToken, time.Now()); berr == nil ||
			!IsForeignToken(berr) {
			// berr == nil:是一张有效业务 JWT,但发错了端点(该发 /exchange-jwt)。
			// !IsForeignToken:HMAC 已匹配,确定是我们签的,只是按自身条件被拒。
			//
			// 两种都不能回落上游:那条路把凭据放在 URL query 上,转发等于把载荷里的
			// PII 和一份在我方密钥下合法的签名一起送进第三方的访问日志。
			//
			// 对客户端回与其它失败相同的码(反枚举),真实原因只进日志。
			metricExchangeResult.WithLabelValues("own_business_jwt").Inc()
			o.Warn("OIDC exchange: refused a business JWT presented to the wrong endpoint",
				zap.String("trace_id", traceID), zap.Error(berr))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
			return
		}
	}

	// 第二道:排除**我方自己签发**的其它凭据(会话 token / uk_ / bf_ / app_)。
	//
	// 这条路撞上的概率比 integration 那两个端点还高:/exchange 与 /exchange-jwt
	// 请求体一模一样、字段都叫 access_token(见文件头),发错端点只会得到一个
	// 无差别 401 —— 而在 plain-OAuth2 下,凭据那时已经进了上游的 URL query。
	switch kind, cerr := o.ownCred.Classify(c.Request.Context(), req.AccessToken); {
	case cerr != nil:
		// 判不出归属(会话存储不可用)。不能回落上游:那等于把守卫的失败方向
		// 设成泄漏,而且不可观测 —— 客户端只看到 401,凭据已经出去了。
		metricExchangeResult.WithLabelValues("provenance_undecided").Inc()
		o.Error("OIDC exchange: cannot establish credential provenance; refusing rather "+
			"than forwarding to the upstream IdP",
			zap.String("trace_id", traceID), zap.Error(cerr))
		// 走 i18n 门面而不是裸 JSON:这是普通的运维故障 500,不属于本文件顶部
		// 那条"协议端点原样响应"的豁免(那条是给浏览器/原生重定向入口的)。
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	case kind != OwnCredentialNone:
		// 我方凭据不是对本端点的身份断言。只记类别,不记凭据本身。
		metricExchangeResult.WithLabelValues("own_credential").Inc()
		o.Warn("OIDC exchange: refused a credential this service issued",
			zap.String("trace_id", traceID), zap.String("own_credential_kind", string(kind)))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

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
