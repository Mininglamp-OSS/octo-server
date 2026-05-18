package oidc

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

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

// bindRoutes 挂载自助绑定的 4 个 HTTP 端点。
//
// 设计:
//   - Bind.Enabled=false 时整个函数 no-op,production 配 disabled provider
//     时连"路由不存在"都成立(404 由 gin 默认 router 兜底)
//   - 不带 AuthMiddleware:bind_token 自身就是单次消费认证凭据(SR-1),
//     调用方还没有 dmwork session 才需要走这套流程
//   - 不挂 /bind/confirm —— PR4 才接 callback 链路 + ThirdAuthcode 回填
func (o *OIDC) bindRoutes(g bindRouteGroup) {
	// 仅由 routeAt 在 o.cfg 非 nil 时调用,o == nil / o.cfg == nil 不可达。
	if !o.cfg.Bind.Enabled {
		return
	}
	g.GET("/bind/info", o.bindInfo)
	g.POST("/bind/verify/password", o.bindVerifyPassword)
	g.POST("/bind/verify/otp/send", o.bindOTPSend)
	g.POST("/bind/verify/otp/check", o.bindOTPCheck)
}

// bindInfo GET /bind/info?token=...  → 脱敏身份信息 + 可用方法(FR-2)。
//
// 失败码语义:
//   - 400 token 缺失 / 格式非法
//   - 410 token 已过期 / 已消费(单次性 + 5min TTL)
//   - 500 服务端错误(claims snapshot 解码失败等内部异常)
func (o *OIDC) bindInfo(c *wkhttp.Context) {
	token := c.Query("token")
	if !authcodeRe.MatchString(token) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token invalid"))
		return
	}
	info, err := o.bind.Info(c.Request.Context(), token)
	if err != nil {
		o.handleBindLookupErr(c, "bind/info", token, err)
		return
	}
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
	var req struct {
		Token      string `json:"token"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("invalid request body"))
		return
	}
	if !authcodeRe.MatchString(req.Token) || req.Identifier == "" || req.Password == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token/identifier/password required"))
		return
	}
	err := o.bind.VerifyPassword(c.Request.Context(), req.Token, req.Identifier, req.Password)
	if err != nil {
		o.handleBindVerifyErr(c, "bind/verify/password", req.Token, err)
		return
	}
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
	var req struct {
		Token string `json:"token"`
	}
	if err := c.BindJSON(&req); err != nil || !authcodeRe.MatchString(req.Token) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token required"))
		return
	}
	err := o.bind.SendSMS(c.Request.Context(), req.Token)
	if err != nil {
		o.handleBindOTPSendErr(c, req.Token, err)
		return
	}
	// 审计写入统一在 PR4 callback 接管阶段补齐所有 handler;PR3 此处不写,
	// 避免与其他三个 handler 不一致导致 dashboard 误读单边数据。
	c.JSON(http.StatusOK, map[string]string{"status": "sent"})
}

// bindOTPCheck POST /bind/verify/otp/check  {token, code}
//
// 失败码与 bindVerifyPassword 同构:401 通用拒绝,429 计数超限,410 不存在/过期。
func (o *OIDC) bindOTPCheck(c *wkhttp.Context) {
	var req struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("invalid request body"))
		return
	}
	if !authcodeRe.MatchString(req.Token) || req.Code == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("token/code required"))
		return
	}
	err := o.bind.VerifySMS(c.Request.Context(), req.Token, req.Code)
	if err != nil {
		o.handleBindVerifyErr(c, "bind/verify/otp/check", req.Token, err)
		return
	}
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
		zap.String("path", path), zap.String("token", token), zap.Error(err))
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
	default:
		// 业务层 VerifyPassword / VerifySMS 没有错误类型化为哨兵(密码错只返
		// "rejected: <reason>" 普通 error),此处统一 401 防账号枚举。
		o.Info("OIDC bind verify rejected",
			zap.String("path", path), zap.String("token", token), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusUnauthorized, errMsg("invalid credentials"))
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
	var uids []string
	if _, err := l.db.session.Select("uid").From("user").
		Where("username=? AND is_destroy=0", username).
		Limit(1).Load(&uids); err != nil {
		return "", fmt.Errorf("oidc bind locator: query user by username: %w", err)
	}
	if len(uids) == 0 {
		return "", nil
	}
	return uids[0], nil
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
	default:
		// SMSService 内部 / 网络异常:5xx 报给客户端,运维 dashboard 可见。
		o.Error("OIDC bind otp send failed (internal)",
			zap.String("token", token), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("sms send failed"))
	}
}
