package oidc

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// auditReason 审计 reason 列的安全写入:
//   - 截断到 maxAuditDetail(api.go:38)防灌爆 + 防潜在的密码/凭据字面值
//     被反向代理/客户端塞进 err.Error() 路径(纵深防御);
//   - 空字符串原样返回,调用方决定是否带 reason。
//
// **不**做敏感词过滤:那是黑名单思路,容易遗漏;靠"截断 + reason 字面来自
// service 层而非用户输入"的契约。service 层的错误消息(bind_service.go)
// 由代码控制,不会拼用户原文。
func auditReason(s string) string {
	if len(s) > maxAuditDetail {
		return s[:maxAuditDetail]
	}
	return s
}

// bindMetricRecord 在 handler 入口 defer,统一上报:
//   - bind_request_total{endpoint, result}
//   - bind_request_duration_seconds{endpoint}
//
// result 由 handler 写到 *string 上;默认 "internal_error" 保证未显式置位
// 的路径(panic 之外)也被归到 5xx 桶,告警可定位。
type bindMetricRecord struct {
	endpoint string
	result   string
	start    time.Time
}

func startBindMetric(endpoint string) *bindMetricRecord {
	return &bindMetricRecord{endpoint: endpoint, result: "internal_error", start: time.Now()}
}

func (r *bindMetricRecord) finish() {
	metricBindRequestTotal.WithLabelValues(r.endpoint, r.result).Inc()
	metricBindRequestDuration.WithLabelValues(r.endpoint).Observe(time.Since(r.start).Seconds())
}

// guardBindReady 在每个 bind handler 入口检查 o.bind 是否就绪。
//
// 不就绪的真实路径:cfg.Bind.Enabled=true 但 Discovery 失败导致 Init 早返
// (api.go:170)—— o.bind 仍是 nil,但 cfg.Bind.Enabled 让 bindRoutes 把 5 个
// 端点挂上了路由。任一 handler 第一行调 o.bind.* 都 nil pointer panic
// 影响整个进程。
//
// 返 true 表示已经写入 503 响应并把 metric.result 置位,handler 应立即 return。
// 503 而非 500:服务"暂时不可用",运维修复 Discovery / 重启进程后即可恢复,
// 与 5xx 报警系统语义对齐(transient 而非 bug)。
func (o *OIDC) guardBindReady(c *wkhttp.Context, m *bindMetricRecord) bool {
	if o.bind != nil {
		return false
	}
	m.result = "not_ready"
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, errMsg("bind service not ready"))
	return true
}

// bindRouteGroup 把 wkhttp.RouterGroup 暴露的最小路由能力抽出来,让测试可以
// 用 gin.Engine + 薄 adapter 跑 bindRoutes,避免拉起 wkhttp 全套中间件。
//
// 接口签名严格对齐 wkhttp.RouterGroup 的 GET/POST 形态 —— 后者的 method 是:
//
//	GET(relativePath string, handlers ...HandlerFunc)
//
// 所以 production 路径直接传 wkhttp.RouterGroup 即可,无需 adapter。
type bindRouteGroup interface {
	GET(relativePath string, handlers ...wkhttp.HandlerFunc)
	POST(relativePath string, handlers ...wkhttp.HandlerFunc)
}

// bindRoutes 挂载自助绑定的 HTTP 端点。
//
// 设计:
//   - Bind.Enabled=false 时整个函数 no-op,production 配 disabled provider
//     时连"路由不存在"都成立(404 由 gin 默认 router 兜底)
//   - 不带 AuthMiddleware:bind_token 自身就是单次消费认证凭据(SR-1),
//     调用方还没有 dmwork session 才需要走这套流程
//   - AllowCreate=false 时 /bind/create 不挂(D8:404 比 403 更彻底)
func (o *OIDC) bindRoutes(g bindRouteGroup) {
	// 仅由 routeAt 在 o.cfg 非 nil 时调用,o == nil / o.cfg == nil 不可达。
	if !o.cfg.Bind.Enabled {
		return
	}
	g.GET("/bind/info", o.bindInfo)
	g.POST("/bind/verify/password", o.bindVerifyPassword)
	g.POST("/bind/verify/otp/send", o.bindOTPSend)
	g.POST("/bind/verify/otp/check", o.bindOTPCheck)
	g.POST("/bind/confirm", o.bindConfirm)
	if o.cfg.Bind.AllowCreate {
		g.POST("/bind/create", o.bindCreate)
	}
}

// bindInfo GET /bind/info?token=...  → 脱敏身份信息 + 可用方法(FR-2)。
//
// 失败码语义:
//   - 400 token 缺失 / 格式非法
//   - 410 token 已过期 / 已消费(单次性 + 5min TTL)
//   - 500 服务端错误(claims snapshot 解码失败等内部异常)
func (o *OIDC) bindInfo(c *wkhttp.Context) {
	m := startBindMetric("info")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	token := c.Query("token")
	if !authcodeRe.MatchString(token) {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token invalid"))
		return
	}
	info, err := o.bind.Info(c.Request.Context(), token)
	if err != nil {
		m.result = bindResultFromErr(err)
		o.handleBindLookupErr(c, "bind/info", token, err)
		return
	}
	m.result = "ok"
	c.JSON(http.StatusOK, info)
}

// bindVerifyPassword POST /bind/verify/password  {token, identifier, password}
//
// 失败码:
//   - 400 入参格式非法(token 不合规 / identifier/password 空)
//   - 401 账号或密码错(包括 user_not_found / password_mismatch,不区分以防枚举)
//   - 410 token 已过期/未知
//   - 429 验证尝试超 VerifyMax(SR-2.1)
//   - 500 内部错误
func (o *OIDC) bindVerifyPassword(c *wkhttp.Context) {
	m := startBindMetric("verify_password")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	var req struct {
		Token      string `json:"token"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("invalid request body"))
		return
	}
	if !authcodeRe.MatchString(req.Token) || req.Identifier == "" || req.Password == "" {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token/identifier/password required"))
		return
	}
	err := o.bind.VerifyPassword(c.Request.Context(), req.Token, req.Identifier, req.Password)
	if err != nil {
		m.result = bindResultFromErr(err)
		// 审计 reason 用通用文案,具体 reason 仅写 zap.Error(在 handleBindVerifyErr)
		// — 防止 oidc_audit_log 反查"用户存在 vs 密码错"差异。
		o.writeAudit("bind:"+subHash(req.Token), EventBindVerifyFail, stateFromCtx(c), "verify rejected")
		o.handleBindVerifyErr(c, "bind/verify/password", req.Token, err)
		return
	}
	m.result = "ok"
	o.writeAudit("bind:"+subHash(req.Token), EventBindVerifyOK, stateFromCtx(c), "password")
	c.JSON(http.StatusOK, map[string]string{"status": "verified"})
}

// bindOTPSend POST /bind/verify/otp/send  {token}
//
// 失败码:
//   - 400 token 非法 / claims 无可用 phone(FR-3.3:phone 来自 claims,不存在则该手段不可用)
//   - 410 token 已过期/未知
//   - 429 发送次数超 OTPSendMax(SR-2.1)
//   - 500 内部错误(底层 SMSService 异常)
func (o *OIDC) bindOTPSend(c *wkhttp.Context) {
	m := startBindMetric("otp_send")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.BindJSON(&req); err != nil || !authcodeRe.MatchString(req.Token) {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token required"))
		return
	}
	err := o.bind.SendSMS(c.Request.Context(), req.Token)
	if err != nil {
		m.result = bindResultFromErr(err)
		// SR-6 审计完整性:成功有 EventBindOTPSend,失败也要落审计,否则 SMS
		// provider 异常等场景在 oidc_audit_log 留不下任何痕迹,SOC 只能看
		// Prometheus 反推。reason 走通用文案,具体 err 走 zap。
		o.writeAudit("bind:"+subHash(req.Token), EventBindOTPSendFail, stateFromCtx(c), "otp send failed")
		o.handleBindOTPSendErr(c, req.Token, err)
		return
	}
	m.result = "ok"
	o.writeAudit("bind:"+subHash(req.Token), EventBindOTPSend, stateFromCtx(c), "")
	c.JSON(http.StatusOK, map[string]string{"status": "sent"})
}

// bindOTPCheck POST /bind/verify/otp/check  {token, code}
//
// 失败码与 bindVerifyPassword 同构:401 通用拒绝,429 计数超限,410 不存在/过期。
func (o *OIDC) bindOTPCheck(c *wkhttp.Context) {
	m := startBindMetric("otp_check")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	var req struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	if err := c.BindJSON(&req); err != nil {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("invalid request body"))
		return
	}
	if !authcodeRe.MatchString(req.Token) || req.Code == "" {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token/code required"))
		return
	}
	err := o.bind.VerifySMS(c.Request.Context(), req.Token, req.Code)
	if err != nil {
		m.result = bindResultFromErr(err)
		// 同 verify_password 路径:reason 通用化,具体 err 走 zap.Error。
		// 防止 oidc_audit_log 反查"phone 不存在" vs "OTP 错"差异。
		o.writeAudit("bind:"+subHash(req.Token), EventBindVerifyFail, stateFromCtx(c), "verify rejected")
		o.handleBindVerifyErr(c, "bind/verify/otp/check", req.Token, err)
		return
	}
	m.result = "ok"
	o.writeAudit("bind:"+subHash(req.Token), EventBindVerifyOK, stateFromCtx(c), "sms_otp")
	c.JSON(http.StatusOK, map[string]string{"status": "verified"})
}

// handleBindLookupErr Get/Info 路径上的统一错误码翻译。
// 单独抽出来是因为多个 handler 共用同一组语义(token 不存在 → 410)。
func (o *OIDC) handleBindLookupErr(c *wkhttp.Context, path, token string, err error) {
	if errors.Is(err, ErrBindNotFound) {
		c.AbortWithStatusJSON(http.StatusGone, errMsg("token expired or not found"))
		return
	}
	o.Warn("OIDC bind lookup error",
		zap.String("path", path), zap.String("token_hash", subHash(token)), zap.Error(err))
	c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("internal error"))
}

// handleBindVerifyErr verify(密码 / 短信)路径上的统一错误码翻译。
// 不向客户端泄漏具体 reason —— 401 通用文案防账号枚举(与登录路径一致)。
func (o *OIDC) handleBindVerifyErr(c *wkhttp.Context, path, token string, err error) {
	switch {
	case errors.Is(err, ErrBindNotFound):
		c.AbortWithStatusJSON(http.StatusGone, errMsg("token expired or not found"))
	case errors.Is(err, ErrBindRateLimited):
		c.AbortWithStatusJSON(http.StatusTooManyRequests, errMsg("too many attempts, try later"))
	case errors.Is(err, ErrBindStatusConflict):
		// status 不可推:多半是重复 verify,客户端应当跳过直接走 confirm。
		c.AbortWithStatusJSON(http.StatusConflict, errMsg("already verified"))
	case errors.Is(err, ErrBindConflictNeedManual):
		// 多 dmwork 账号对应同 phone:自助流程无法判定,提示走 Admin 兜底。
		c.AbortWithStatusJSON(http.StatusConflict, errMsg("multiple accounts matched; contact support"))
	case errors.Is(err, ErrBindNoPhone):
		// claims 无 phone 但客户端硬调 /verify/otp/check —— 业务前提不满足。
		// metric (bindResultFromErr) 已归到 bad_request,HTTP 同步 400 保持一致。
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("sms not available for this account"))
	case errors.Is(err, ErrBindMethodDisabled):
		// 运维通过 OCTO_OIDC_BIND_METHODS 关了该方法 —— 客户端不应再 retry。
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("verification method disabled"))
	case errors.Is(err, ErrBindAuthRejected):
		// 业务拒绝(密码错 / OTP 错 / phone 不命中):统一 401 防账号枚举。
		// 具体 reason 走 zap,客户端只看到通用文案。
		o.Info("OIDC bind verify rejected",
			zap.String("path", path), zap.String("token_hash", subHash(token)), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusUnauthorized, errMsg("invalid credentials"))
	default:
		// 未分类 → 500。bindResultFromErr 同步落 internal_error,告警阈值对齐。
		o.Error("OIDC bind verify internal error",
			zap.String("path", path), zap.String("token_hash", subHash(token)), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("internal error"))
	}
}

// bindConfirm POST /bind/confirm  {token}
//
// 用户在确认页点"绑定"后调用(FR-4.1)。串行步骤:
//  1. service.Confirm 写 user_oidc_identity + 调 IssueSession 拿 LoginRespJSON
//  2. authcode.SetAuthcode 回填原发起设备的 ThirdAuthcode(FR-6.3 跨设备)
//  3. 同时把 LoginRespJSON 直接返给当前设备,前端可选择走 ThirdAuthcode 或
//     response body(若当前设备 == 发起设备,二者等价)
//
// 失败码:
//   - 400 token 缺失/非法
//   - 401 状态机不在 verified(用户没完成二次验证就 confirm)
//   - 409 已绑定(uk_uid_issuer / uk_issuer_subject 命中,提示直接登录)
//   - 410 token 已过期/未知
//   - 429 confirm 次数超 ConfirmMax(SR-2.1)
//   - 500 内部错误
func (o *OIDC) bindConfirm(c *wkhttp.Context) {
	m := startBindMetric("confirm")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.BindJSON(&req); err != nil || !authcodeRe.MatchString(req.Token) {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token required"))
		return
	}
	ctx := c.Request.Context()
	resp, err := o.bind.Confirm(ctx, req.Token)
	if err != nil {
		m.result = bindResultFromErr(err)
		o.handleBindConfirmErr(c, req.Token, err)
		return
	}
	m.result = "ok"
	// bind 完成,迁移 callback 阶段按 jti 暂存的 id_token 到 uid,供 logout 用。
	o.promoteBindIDToken(ctx, req.Token, resp.UID)
	// 回填原发起设备的 ThirdAuthcode key,让 A 设备的轮询能拿到 LoginRespJSON
	// (FR-6.3 跨设备流转)。同设备时这一步与 response body 等价,前端任选其一。
	// 写失败不致命:用户在 B 设备(当前)已经拿到了 LoginRespJSON。
	if resp.SD != nil && resp.SD.ClientAuthcode != "" && o.authcode != nil {
		if e := o.authcode.SetAuthcode(ctx, resp.SD.ClientAuthcode,
			resp.IssueResp.LoginRespJSON, thirdAuthcodeTTL); e != nil {
			o.Warn("OIDC bind confirm: write ThirdAuthcode failed (non-fatal)",
				zap.String("trace_id", newTraceID()), zap.Error(e))
		}
	}
	o.writeAudit(resp.UID, EventBindConfirmOK, stateFromCtx(c), "")
	c.JSON(http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"login_resp": resp.IssueResp.LoginRespJSON,
		"uid":        resp.UID,
	})
}

// handleBindConfirmErr Confirm 路径的错误码翻译,与 verify 路径区分:
// 已绑定 → 409("已经绑定,直接用 OIDC 登录")。
//
// 所有分支(含 4xx)都写 EventBindConfirmFail 审计 —— SR-6 完整性:攻击者拿到
// 已 verified 的 token 反复 confirm 探测"already_bound vs status_conflict vs
// expired"的差异,在 metric 上能看到(`oidc_bind_request_total{result=...}`),
// 但 oidc_audit_log 之前只有 default(500)分支落库,SOC 反查时间序列丢半条。
// 不同分支的 reason 字面值固定(不带用户输入),不会扩大 attack surface。
func (o *OIDC) handleBindConfirmErr(c *wkhttp.Context, token string, err error) {
	tokenHash := subHash(token)
	switch {
	case errors.Is(err, ErrBindNotFound):
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), "token expired or not found")
		c.AbortWithStatusJSON(http.StatusGone, errMsg("token expired or not found"))
	case errors.Is(err, ErrBindRateLimited):
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), "rate limited")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, errMsg("too many confirm attempts"))
	case errors.Is(err, ErrBindStatusConflict):
		// 状态不是 verified —— 用户跳过了二次验证,或并发 confirm 撞上(AC-6)。
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), "status conflict")
		c.AbortWithStatusJSON(http.StatusUnauthorized, errMsg("verify before confirm"))
	case errors.Is(err, ErrBindAlreadyBound):
		// 重试 confirm 命中已写 identity 的常见路径(IssueSession 失败后 retry);
		// 文案引导用户回 OIDC 入口而不是放弃。
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), "already bound")
		c.AbortWithStatusJSON(http.StatusConflict,
			errMsg("identity already bound; sign in again via OIDC to continue"))
	case errors.Is(err, ErrBindAuthRejected):
		// TOCTOU 复核失败:verify→confirm 之间账号被停用/进入冷静期。
		// 401 + 通用文案与 verify 路径反枚举一致(不暴露"账号被运维停用"差异);
		// audit reason 写明区分让 SOC 在 oidc_audit_log 里能区分本因。
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), "candidate uid not bindable")
		c.AbortWithStatusJSON(http.StatusUnauthorized, errMsg("invalid credentials"))
	default:
		o.Error("OIDC bind confirm failed (internal)",
			zap.String("token_hash", tokenHash), zap.Error(err))
		// 截断 err.Error() —— 即便我们写到 audit 的是自家 error message,也防
		// 未来误把第三方 wrap 进来意外注入大字符串/凭据片段。256 字符上限和
		// 其他 callback 失败审计一致(api.go:38)。
		o.writeAudit("bind:"+tokenHash, EventBindConfirmFail, stateFromCtx(c), auditReason(err.Error()))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("internal error"))
	}
}

// stateFromCtx 从 HTTP 请求里抽出 IP/UA 拼一个最小 StateData 给审计写。
// 不复用 callback 阶段的 sd(那个在 bind_token 签发时就固化在 BindSession 里);
// 这里关心的是"用户当前在哪台设备完成的 confirm",对运维事后追溯有意义。
func stateFromCtx(c *wkhttp.Context) *StateData {
	return &StateData{
		IP:        clientIP(c),
		UserAgent: c.Request.UserAgent(),
	}
}

// clientIP 抽出来避免对 util 包的多余依赖;直接读 Request.RemoteAddr 兜底。
// 生产路径前置代理时由 util.GetClientPublicIP 透 X-Forwarded-For,这里测试
// 走 httptest 不会有代理,RemoteAddr 即真实 IP。
func clientIP(c *wkhttp.Context) string {
	// 透 util 而非 net.SplitHostPort:与 callback 路径行为对齐,避免反向代理头
	// 解析差异引入审计字段不一致。
	return util.GetClientPublicIP(c.Request)
}

// bindResultFromErr 把 BindService 返回的 error 翻译成 metric result label。
// 错误码 → label 映射与 handle*Err 翻 HTTP 状态码同源(同一个 errors.Is 序),
// 保证 Grafana 上的 rate_limited 计数和 429 计数一致。
func bindResultFromErr(err error) string {
	switch {
	case errors.Is(err, ErrBindNotFound):
		return "not_found"
	case errors.Is(err, ErrBindRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrBindStatusConflict):
		return "conflict"
	case errors.Is(err, ErrBindAlreadyBound):
		return "conflict"
	case errors.Is(err, ErrBindNoPhone):
		return "bad_request"
	case errors.Is(err, ErrBindMethodDisabled):
		return "bad_request"
	case errors.Is(err, ErrBindConflictNeedManual):
		return "conflict"
	case errors.Is(err, ErrBindAuthRejected):
		return "unauthorized"
	case errors.Is(err, ErrBindCreateClaimsIncomplete):
		return "claims_incomplete"
	case errors.Is(err, ErrBindCreateConflictNeedManual):
		return "conflict_need_manual"
	default:
		// 未分类 = 内部异常。dashboard 上看到 internal_error 持续升高就是
		// 该报警的信号(DB/Redis/底层 SMSService 故障),与 401 不再混淆。
		return "internal_error"
	}
}

// bindCreate POST /bind/create  {token}
//
// 用 bind_token 里固化的 SSO claims 直接建号 + 写 identity + 签发会话。
// 复用 AllowNewUser=true 路径语义(IssueSession{CreateUser:true}),但走 bind_token 护栏。
//
// 失败码:
//   - 400 token 缺失/格式非法
//   - 409 status 冲突或 identity 已存在(race recover 路径)
//   - 410 token 已过期/未知
//   - 422 claims 既无 verified email 也无 verified phone
//   - 429 同一 token 已 create 过(单次性)
//   - 500 内部错误
func (o *OIDC) bindCreate(c *wkhttp.Context) {
	m := startBindMetric("create")
	defer m.finish()

	if o.guardBindReady(c, m) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.BindJSON(&req); err != nil || !authcodeRe.MatchString(req.Token) {
		m.result = "bad_request"
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token required"))
		return
	}
	ctx := c.Request.Context()
	resp, err := o.bind.Create(ctx, req.Token)
	if err != nil {
		m.result = bindResultFromErr(err)
		o.handleBindCreateErr(c, req.Token, err)
		return
	}
	m.result = "ok"
	// bind 建号完成,迁移 callback 阶段按 jti 暂存的 id_token 到 uid,供 logout 用。
	o.promoteBindIDToken(ctx, req.Token, resp.UID)
	if resp.SD != nil && resp.SD.ClientAuthcode != "" && o.authcode != nil {
		if e := o.authcode.SetAuthcode(ctx, resp.SD.ClientAuthcode,
			resp.IssueResp.LoginRespJSON, thirdAuthcodeTTL); e != nil {
			o.Warn("OIDC bind create: write ThirdAuthcode failed (non-fatal)",
				zap.String("trace_id", newTraceID()), zap.Error(e))
		}
	}
	o.writeAudit(resp.UID, EventBindCreated, stateFromCtx(c), "")
	c.JSON(http.StatusOK, map[string]interface{}{
		"status":     "ok",
		"login_resp": resp.IssueResp.LoginRespJSON,
		"uid":        resp.UID,
	})
}

// handleBindCreateErr /bind/create 路径的错误码翻译。
// 所有分支都写 EventBindCreateFail 审计。
func (o *OIDC) handleBindCreateErr(c *wkhttp.Context, token string, err error) {
	tokenHash := subHash(token)
	switch {
	case errors.Is(err, ErrBindNotFound):
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "token expired or not found")
		c.AbortWithStatusJSON(http.StatusGone, errMsg("token expired or not found"))
	case errors.Is(err, ErrBindRateLimited):
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "rate limited")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, errMsg("too many create attempts"))
	case errors.Is(err, ErrBindStatusConflict):
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "status conflict")
		c.AbortWithStatusJSON(http.StatusConflict, errMsg("token status conflict"))
	case errors.Is(err, ErrBindAlreadyBound):
		// Identity 行已存在(并发 /bind/create 同 (issuer,sub) 触发了竞态)。
		// 赢家已签发会话;此处只引导客户端重发起 OIDC 登录以拾取赢家会话。
		// Ghost user 清理(输家创建的 dmwork user)不在本 PR 范围,见 plan §4/§15。
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "already bound")
		c.AbortWithStatusJSON(http.StatusConflict,
			errMsg("identity already bound; sign in via OIDC to continue"))
	case errors.Is(err, ErrBindCreateClaimsIncomplete):
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "claims incomplete")
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, errMsg("claims missing required fields"))
	case errors.Is(err, ErrBindCreateConflictNeedManual):
		// manual-conflict token:claims 命中多条 dmwork 账号,/bind/create
		// 不允许重复建号,引导走 Admin 人工合并兜底(与 verify 多匹配同语义)。
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), "conflict need manual")
		c.AbortWithStatusJSON(http.StatusConflict,
			errMsg("account conflict needs manual resolution"))
	default:
		o.Error("OIDC bind create failed (internal)",
			zap.String("token_hash", tokenHash), zap.Error(err))
		o.writeAudit("bind:"+tokenHash, EventBindCreateFail, stateFromCtx(c), auditReason(err.Error()))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("internal error"))
	}
}

// dbBindLocator 生产路径下的 BindLocator 实现:走 oidc.DB 直接查 user 表。
//
// 直接 SQL 不走 user.IService 是因为:
//   - 单条 QueryByUsername 不值得在 user.IService 多暴露一个方法;
//   - 本路径只需 uid,SQL 查询最直接;
//   - 数据库约束保证 username 唯一,无需上层做多匹配兜底。
type dbBindLocator struct {
	db *DB
}

func (l dbBindLocator) UIDByUsername(username string) (string, error) {
	if username == "" {
		return "", nil
	}
	if l.db == nil {
		// nil db 是 Init 路径配置 bug,不是"用户不存在"的业务条件。
		// 必须返 error,否则上层会把它当 user_not_found 静默吞掉,运维感知不到。
		return "", fmt.Errorf("oidc bind locator: db not initialised")
	}
	// 过滤条件与 QueryUIDsByEmail/Phone 保持一致(is_destroy=0 AND status<>0):
	// 排除冷静期/已注销/被封禁账号,避免 verify 通过后 confirm 时 IssueSession
	// 才拒绝,残留 user_oidc_identity 脏数据让该用户后续 OIDC 登录持续失败。
	// 与 user.VerifyPasswordByUID 的 IsDestroyDone || Status==0 检查互补
	// (前者更严:还排除 is_destroy=1 冷静期账号,因为 5min bind_token 期内
	// 不应该让正在冷静撤销的账号被绑到新 OIDC 身份)。
	var uids []string
	if _, err := l.db.session.Select("uid").From("user").
		Where("username=? AND is_destroy=0 AND status<>0", username).
		Limit(1).Load(&uids); err != nil {
		return "", fmt.Errorf("oidc bind locator: query user by username: %w", err)
	}
	if len(uids) == 0 {
		return "", nil
	}
	return uids[0], nil
}

// UIDsByPhone 与 service.userLookup.UIDsByPhone 等价 —— oidc.DB 已有同名方法,
// 这里只是 BindLocator 接口的桥接,语义透传。
//
// 多返回是因为 dmwork user 表的 (zone, phone) 无强唯一约束,VerifySMS 在多
// 匹配场景会走 ErrBindConflictNeedManual 兜底,不在本层做去重。
func (l dbBindLocator) UIDsByPhone(zone, phone string) ([]string, error) {
	if zone == "" || phone == "" {
		return nil, nil
	}
	if l.db == nil {
		return nil, fmt.Errorf("oidc bind locator: db not initialised")
	}
	return l.db.QueryUIDsByPhone(zone, phone)
}

// handleBindOTPSendErr 区分三种语义:
//   - 业务前提不满足(claims 无 phone)→ 400,前端不应 retry,引导走密码路径;
//   - token 不存在/限流 → 410 / 429;
//   - SMSService 内部异常 → 500,前端可 retry。
//
// 不把所有失败折叠成 400 是因为运维需要从 5xx 比例感知 SMS 链路抖动 —— 折叠
// 会让 SMS provider 故障被 4xx 报表掩盖。
func (o *OIDC) handleBindOTPSendErr(c *wkhttp.Context, token string, err error) {
	switch {
	case errors.Is(err, ErrBindNotFound):
		c.AbortWithStatusJSON(http.StatusGone, errMsg("token expired or not found"))
	case errors.Is(err, ErrBindRateLimited):
		c.AbortWithStatusJSON(http.StatusTooManyRequests, errMsg("too many otp sends, try later"))
	case errors.Is(err, ErrBindNoPhone):
		// 业务前提不满足:前端应改走密码手段,不要 retry SMS。
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("sms not available for this account"))
	case errors.Is(err, ErrBindMethodDisabled):
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("verification method disabled"))
	default:
		// SMSService 内部 / 网络异常:5xx 报给客户端,运维 dashboard 可见。
		o.Error("OIDC bind otp send failed (internal)",
			zap.String("token_hash", subHash(token)), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("sms send failed"))
	}
}
