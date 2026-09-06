package oidc

// api_exchange_jwt.go — POST /v1/auth/oidc/<id>/exchange-jwt
//
// non-browser client (native app)完成 SSO 后拿到的是 客户端业务后端自签的
// HS256 JWT(不是上游 OIDC access_token,也不是我方 session)。本端点做本地 HS256
// 验签 + 过期校验,成功后签发我方 session token。
//
// 与 /exchange(上游 OIDC access_token → IdP /userinfo → 身份)的区别:
//   - 本端点不外呼,纯本地计算,性能/可靠性不依赖 IdP;
//   - issuer 使用独立命名空间(上游 issuer + "#bearer-jwt" 后缀),
//     与上游 OIDC flow 的 identity 行互不干扰;
//   - subject 是 客户端 userId 数字转字符串(不是上游 IdP sub);
//   - 不携带 email/phone/verified 字段,AutoLink 天然 fail-closed。
//
// 为什么不用同一个 /exchange 端点自动识别:形态自动路由会在日志/监控里把两种
// 完全不同的身份来源混在一起,排错困难;且一旦识别错误(比如有人塞一个长得
// 像 JWT 的上游 OIDC access_token),可能误把 token 当 bearer JWT 解(HS256 对任意
// 输入都能"算出"一个 MAC,只是不会通过正确密钥的校验),属于无端扩大攻击面。
// 分成两个端点,配置/限流/metric label/审计字段都独立,边界清晰。

import (
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// exchangeJWTRequest 与 /exchange 同形,字段名都叫 access_token(客户端不需要
// 区分底层是哪种 token,只管把本地存的 accessToken POST 过来即可)。
type exchangeJWTRequest struct {
	AccessToken string `json:"access_token"`
	DeviceFlag  *uint8 `json:"flag,omitempty"`
}

// exchangeJWT POST /v1/auth/oidc/<id>/exchange-jwt
//
// 入: {access_token: "<客户端 HS256 JWT>", flag?: uint8}
// 出: 200 {status:"ok", uid, login_resp}
//
//	| 400 请求格式错/空 token
//	| 401 验签失败/过期/claims 非法(反枚举:统一返同一个码)
//	| 500 密钥未配置(启动期配置错误,不给攻击者探测面)
//
// 反枚举与 /exchange 保持一致:任何无法确立身份的失败都返 401 同一个错误码,
// 具体原因只进 zap 日志。客户端看到 401 应重新走 SSO。
func (o *OIDC) exchangeJWT(c *wkhttp.Context) {
	metricBearerExchangeTotal.Inc()

	// service 在 Init() 里构造且以 o.provider!=nil 为前置;运维配置错误导致
	// provider 构造失败(Discovery 不通/BaseURL 错)时 o.provider==o.service==nil,
	// 此时任何登录路径都无法工作,返回明确的 500 而不是 nil panic。
	if o.provider == nil || o.service == nil {
		// 门面而非裸 JSON:原生客户端调的 JSON 端点,不属于 api.go 的浏览器重定向豁免。
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	// 配置缺失是运维错误,500 而非 401:区别"客户端给了坏 token" vs "我们自己没配好"。
	if o.bearerJWT == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	var req exchangeJWTRequest
	// 复用 /exchange 的 body 上限(16KB):JWT 通常 <2KB,留出 flag 字段裕量。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, exchangeMaxBodyBytes)
	if err := c.BindJSON(&req); err != nil {
		metricBearerExchangeResult.WithLabelValues("bad_request").Inc()
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeRequestInvalid, nil, nil)
		return
	}
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	if req.AccessToken == "" {
		metricBearerExchangeResult.WithLabelValues("bad_request").Inc()
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

	// 本地验签:HS256 + exp + userId>0。不外呼。
	//
	// 全部校验收在 verifyBearerJWT 一处(它内部调通用 VerifyHS256JWT 再追加
	// userId>0 这条本路径约束)。handler 不重复实现任何一条 —— 双写过的话,
	// 改一处漏另一处就是一个静默放行 userId=0 的洞,且 bearer_jwt_test.go
	// 的用例会变成只覆盖不上生产的副本。
	//
	// 失败原因(签名/过期/格式 vs userId 缺失)由 err 携带,进日志区分;
	// 对客户端一律回同一个 401 码,不做失败原因枚举(anti-enumeration)。
	now := time.Now()
	rj, err := o.bearerJWT.VerifyForRedemption(req.AccessToken, now)
	if err != nil {
		metricBearerExchangeResult.WithLabelValues("token_rejected").Inc()
		o.Warn("OIDC exchange-jwt: token rejected",
			zap.String("trace_id", traceID), zap.String("ip", clientIP), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	// 新鲜度判定。验签只回答"这张 token 是不是我们签的、有没有过期",能不能**用它
	// 换一个新会话**是另一个问题 —— 由兑换台账按兑换行为回答(首次兑换上限 F +
	// 空闲窗口 T,见 redemption_ledger.go)。
	//
	// 拒绝时对客户端仍是与验签失败**同一个** 401 码:两者都意味着"重新走一遍 SSO",
	// 而区分它们只会告诉调用方"这张 token 曾经是有效的",那正是反枚举要挡的信息。
	// 真实原因进 metric 与日志。
	outcome := o.admitRedemption(c.Request.Context(), req.AccessToken, rj, now, traceID)
	metricBearerRedemptionTotal.WithLabelValues(string(outcome)).Inc()
	if !outcome.admitted() {
		metricBearerExchangeResult.WithLabelValues("redeem_refused").Inc()
		o.Warn("OIDC exchange-jwt: redemption refused by the ledger",
			zap.String("trace_id", traceID), zap.String("ip", clientIP),
			zap.String("outcome", string(outcome)),
			zap.Duration("token_age", now.Sub(rj.IssuedAt).Round(time.Second)))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrOIDCExchangeTokenRejected, nil, nil)
		return
	}

	o.completeExchange(c, verifiedIdentity{
		claims:     rj.Claims,
		deviceFlag: deviceFlag,
		clientIP:   clientIP,
		traceID:    traceID,
		state:      sd,
		// domainAccount 人类可读,便于与上游对账;userId 是数字串,对账时无用。
		// toIdentityClaims 把它映射到 Name(不是主键,仅作显示与审计)。
		auditDetail: rj.Claims.Name,
	}, exchangeFlavour{
		logName:   "exchange-jwt",
		result:    metricBearerExchangeResult,
		eventOK:   EventBearerExchangeOK,
		eventFail: EventBearerExchangeFail,
	})
}
