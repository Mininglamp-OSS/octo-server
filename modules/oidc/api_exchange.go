package oidc

// api_exchange.go — POST /v1/auth/oidc/<id>/exchange
//
// Token-exchange entry point for bearer / native clients that complete SSO
// themselves (system browser → local callback server) and hand us the
// IdP-issued access_token in exchange for our own session token.
//
// vs /callback:/callback is the browser redirect flow (code → token → userinfo,
// result handed back via ThirdAuthcode polling)./exchange is the direct JSON
// API for clients that already hold an IdP access_token; it returns the
// session JSON in the response body.
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

	// Identity 回调 IdP /userinfo,解私有信封,完成三条信任边界校验,返回可信
	// claims(Issuer/Subject 均非空)或 error。
	claims, err := o.provider.Identity(c.Request.Context(), &TokenSet{AccessToken: req.AccessToken})
	if err != nil {
		metricExchangeResult.WithLabelValues("identity_fail").Inc()
		o.Warn("OIDC exchange: identity rejected",
			zap.String("trace_id", traceID), zap.String("ip", clientIP), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	res, err := o.service.ResolveOrLink(c.Request.Context(), claims)
	if err != nil {
		metricExchangeResult.WithLabelValues("resolve_fail").Inc()
		o.Warn("OIDC exchange: resolve failed",
			zap.String("trace_id", traceID), zap.String("ip", clientIP),
			zap.String("sub", subHash(claims.Subject)), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	zone := extractZone(claims.PhoneNumber)
	phone := extractPhone(claims.PhoneNumber)
	if claims.PhoneNumber != "" && phone == "" {
		o.Warn("OIDC exchange phone number dropped: only +86 supported",
			zap.String("trace_id", traceID), zap.String("idp_phone", claims.PhoneNumber))
	}

	issueReq := IssueSessionReq{
		UID:              res.UID,
		CreateUser:       res.IsNew,
		Name:             claims.Name,
		Email:            claims.Email,
		Phone:            phone,
		Zone:             zone,
		DeviceFlag:       deviceFlag,
		PublicIP:         clientIP,
		TrustedSSOCreate: res.IsNew,
	}
	sessResp, err := o.service.IssueSession(c.Request.Context(), issueReq)
	if err != nil {
		metricExchangeResult.WithLabelValues("issue_fail").Inc()
		o.Error("OIDC exchange: issue session failed",
			zap.String("trace_id", traceID), zap.String("ip", clientIP),
			zap.String("sub", subHash(claims.Subject)), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	// 新用户补写 identity 行。并发首登竞态由 UNIQUE KEY uk_issuer_subject +
	// recoverFromIdentityRace 兜底(同 callback 路径)。
	if res.IsNew && sessResp.UID != "" {
		if err := o.store.Insert(&IdentityModel{
			UID:           sessResp.UID,
			Issuer:        claims.Issuer,
			Subject:       claims.Subject,
			Email:         claims.Email,
			EmailVerified: boolToInt(claims.EmailVerified),
			Phone:         claims.PhoneNumber,
			PhoneVerified: boolToInt(claims.PhoneVerified),
			LinkedAt:      time.Now(),
		}); err != nil {
			if isDuplicateKeyError(err) {
				if recovered := o.recoverFromIdentityRace(c.Request.Context(), claims, sd, sessResp, issueReq, err); recovered == nil {
					metricExchangeResult.WithLabelValues("identity_insert_fail").Inc()
					o.writeAudit(sessResp.UID, EventExchangeFail, sd, "identity insert race unrecovered")
					httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
					return
				}
				metricExchangeResult.WithLabelValues("race_recovered").Inc()
			} else {
				metricExchangeResult.WithLabelValues("identity_insert_fail").Inc()
				o.Error("OIDC exchange: insert identity failed",
					zap.String("trace_id", traceID), zap.Error(err))
				httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
				return
			}
		}
	}

	o.writeAudit(sessResp.UID, EventExchangeOK, sd, "")
	metricExchangeResult.WithLabelValues("ok").Inc()

	// 与 /bind/create 返回形状对齐:status / uid / login_resp(JSON 字符串,客户端
	// 直接解出 token 字段即可)。
	c.JSON(http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"uid":        sessResp.UID,
		"login_resp": sessResp.LoginRespJSON,
	})
}
