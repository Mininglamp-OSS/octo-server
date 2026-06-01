package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	mysql "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// ThirdAuthcodeRedisPrefix 与 user 模块 ThirdAuthcodePrefix 一致,
// 复用前端短码轮询取登录态的现有约定。注意:不能改名,前端协议公开。
const ThirdAuthcodeRedisPrefix = "thirdlogin:authcode:"

const (
	// stateTTL OIDC authorize → callback 之间 state 的有效期;
	// 覆盖 IdP 同意页 + 网络往返,同时压缩 state 复用攻击窗口。
	stateTTL = 5 * time.Minute
	// thirdAuthcodeTTL 前端短码轮询拿 LoginRespJSON 的窗口。
	// 登录响应仅在 callback 成功时落 Redis,容量影响可忽略。
	thirdAuthcodeTTL = 5 * time.Minute
	// maxAuditDetail audit 表 reason 列写入的最大长度,防止 IdP 返回的
	// 任意字段(如 ?error=...)灌爆审计字段或污染下游 dashboard。
	maxAuditDetail    = 256
	defaultDeviceFlag = uint8(0) // APP
	maxAuditUID       = 64
)

// authcodeRe 限制前端短码字符集:[a-zA-Z0-9_-],防 Redis key 注入 / 跨 user 覆盖。
//
// ThirdAuthcode key 空间(thirdlogin:authcode:*)与 GitHub OAuth 共用,authcode
// 由前端生成并直接拼到 Redis key 后段,不校验会让攻击者构造 authcode 覆盖
// 别人的登录 payload。
var authcodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// authcodeWriter Redis ThirdAuthcode 写入抽象,生产用 ctx.GetRedisConn(),测试用内存。
type authcodeWriter interface {
	SetAuthcode(ctx context.Context, authcode, payload string, ttl time.Duration) error
}

// auditWriter 审计写入抽象,best-effort:写失败仅记 log,不阻塞 callback 返回。
type auditWriter interface {
	InsertAudit(m *AuditModel) error
}

// rtRevoker logout / 状态同步路径上对 RT 的批量吊销抽象。
// 生产实现是 *DB,测试可注入内存 fake 断言调用。
type rtRevoker interface {
	RevokeRefreshByUID(uid string) (int64, error)
}

// OIDC OIDC 登录模块。
//
// 字段全部包内可见:测试在 New 后可替换 stateStore / authcode 为内存实现。
type OIDC struct {
	ctx *config.Context
	log.Log

	cfg        *Config
	client     *Client
	service    *Service
	db         *DB
	store      identityStore
	stateStore StateStore
	authcode   authcodeWriter
	audit      auditWriter
	killer     sessionKiller
	revoker    rtRevoker
	// idTokens 缓存登录时验签过的 id_token,供 logout 当 RP-Initiated Logout 的
	// id_token_hint。nil 时 logout 不生成 end_session_url(降级为仅清本地)。
	idTokens idTokenStore
	worker   *SyncWorker
	tickLock *RedisTickLock
	cbGuard  *CallbackGuard
	bind     *BindService // 自助绑定(P0);Bind.Enabled=false 时为 nil,handler 不挂载
	// bindStore 单独持引用便于 Close 时关连接池。bind.store 是 BindStore 接口,
	// 接口本身没 Close,production impl(*redisBindStore)有独立 redis.Client,
	// 不关会泄漏。
	bindStore BindStore

	// verification 由 Init() 注入(user.IService 的子集),OIDC callback 拿到 IdP
	// identity_verification claims 后调用 UpsertVerificationFromOIDC 写 user_verification。
	//
	// 小接口而非直接持 user.IService 是为了让 api_test 里的 newTestOIDC 可以注入
	// fake,和已有 fakeUserLookup / fakeIdentityStore 的风格一致。nil 时 callback
	// 不会尝试写库,等价于该 IdP 没返 identity_verification scope(fail-open,不阻断登录)。
	verification verificationUpserter
}

// verificationUpserter OIDC callback 写 user_verification 的最小依赖接口。
//
// 生产路径下由 user.IService 直接实现(user.Service 已加 UpsertVerificationFromOIDC);
// 测试可注入 fake 断言参数。
type verificationUpserter interface {
	UpsertVerificationFromOIDC(ctx context.Context, uid string, claims user.OIDCVerificationClaims) error
}

// New 构造 OIDC 模块(生产路径)。
//
// Enabled=false 时只挂 Route 占位,handler 一律返回 404,避免漏配置时静默通过。
// Discovery 失败不阻塞启动,handler 自检后返回 500,跟进运维告警即可。
func New(ctx *config.Context) *OIDC {
	cfg, err := LoadConfig()
	o := &OIDC{
		ctx: ctx,
		Log: log.NewTLog("OIDC"),
	}
	if err != nil {
		o.Error("加载 OIDC 配置失败", zap.Error(err))
		return o
	}
	o.cfg = cfg
	if !cfg.Enabled {
		return o
	}
	db := NewDB(ctx)
	o.store = identityStoreAdapter{db: db}
	o.db = db
	o.stateStore = newRedisStateStore(ctx)
	o.authcode = redisAuthcode{ctx: ctx}
	o.audit = db
	o.revoker = db
	o.killer = ctxKiller{ctx: ctx}
	o.cbGuard = NewCallbackGuard(
		ctx.GetRedisConn(),
		callbackGuardThresholdFromEnv(),
		callbackGuardWindowFromEnv(),
	)

	cctx, cancel := context.WithTimeout(context.Background(), cfg.Provider.HTTPTimeout)
	defer cancel()
	client, cerr := NewClient(cctx, ClientConfig{
		Issuer:       cfg.Provider.Issuer,
		ClientID:     cfg.Provider.ClientID,
		ClientSecret: cfg.Provider.ClientSecret,
		RedirectURI:  cfg.Provider.RedirectURI,
		Scopes:       cfg.Provider.Scopes,
		HTTPTimeout:  cfg.Provider.HTTPTimeout,
		ClockSkew:    cfg.Provider.ClockSkew,
	})
	if cerr != nil {
		o.Error("OIDC Discovery 失败,handlers 将返回 500", zap.Error(cerr))
		_ = o.Close()
		o.stateStore = nil
		return o
	}
	o.client = client
	// id_token 缓存(RP-Initiated Logout)仅在功能"确实可用"时启用:既配了回跳地址
	// (PostLogoutRedirectURI),又拿得到*合法 https* 的 end_session 端点(discovery 或
	// override)。端点仅"非空"不够 —— 非法/非 https 端点下 logout 也出不了 URL
	// (buildEndSessionURL 会拒),此时建池存 PII id_token 纯属浪费。放在 client 之后才能判
	// 端点可用性。密钥已在 LoadConfig 校验为 32B,构造失败仅记日志降级,不影响登录主流程。
	if cfg.Provider.PostLogoutRedirectURI != "" {
		endpoint := o.endSessionEndpoint()
		switch {
		case endpoint == "" || validateLogoutURL("end_session_endpoint", endpoint) != nil:
			// 配了回跳地址却拿不到可用端点:打 Info 让运维可见"为什么 RP-logout 没生效"。
			o.Info("RP-Initiated Logout 已禁用:end_session 端点不可用(discovery 未提供且未配 override,或非 https)",
				zap.String("endpoint", endpoint))
		default:
			if enc, eerr := NewEncryptor(cfg.Provider.RefreshTokenEncryptionKey); eerr != nil {
				o.Error("构造 id_token Encryptor 失败,RP-Initiated Logout 禁用", zap.Error(eerr))
			} else {
				o.idTokens = newRedisIDTokenStore(ctx, enc)
			}
		}
	}
	return o
}

// Init 在所有模块初始化完成后调用(register.Module.Start),
// 此时 user 模块的 IService 已通过 register.GetService 可用。
func (o *OIDC) Init() error {
	if o.cfg == nil || !o.cfg.Enabled {
		return nil
	}
	// Discovery 失败时 client=nil,handler 入口已 fail-fast 返 500,
	// 此处构造 service 也用不到,直接早返回省一次跨模块查询。
	if o.client == nil {
		return nil
	}
	raw := register.GetService("user")
	if raw == nil {
		return fmt.Errorf("oidc: Init: user service not registered")
	}
	userSvc, ok := raw.(user.IService)
	if !ok {
		return fmt.Errorf("oidc: Init: expected user.IService, got %T", raw)
	}
	o.service = newService(o.cfg.Provider, o.store, newUserAdapter(userSvc, o.db))
	// user.IService 已在本 PR 加 UpsertVerificationFromOIDC,直接作为 verificationUpserter 使用。
	// 单测场景下 o.verification 可由 newTestOIDC 提前塞入 fake,跳过此赋值。
	if o.verification == nil {
		o.verification = userSvc
	}

	// 自助绑定(P0):Bind.Enabled=true 时构造 BindService + 注入 user.IService。
	// Bind.Enabled=false 时 o.bind=nil,bindRoutes 不挂任何路由(零生产影响)。
	if err := validateBindConfigAgainstProvider(o.cfg); err != nil {
		return fmt.Errorf("oidc: Init: %w", err)
	}
	if o.cfg.Bind.Enabled {
		o.bindStore = newRedisBindStore(o.ctx)
		// userSvc 已经实现 BindAuthenticator(三个方法在 user.IService 内),
		// Go 鸭子类型直接传即可。BindLocator 用 oidc.DB 适配:复用同一连接池。
		locator := dbBindLocator{db: o.db}
		o.bind = newBindService(o.cfg.Bind, o.bindStore, userSvc, locator)
		// Confirm 路径需要 identity 写入 + IssueSession 签发,复用 *Service 已经
		// 持有的 store(identityStore) 和 users(userLookup)。两者都在 newService
		// 内完成构造,Init 顺序保证 o.service 此时非 nil。
		o.bind.identity = o.store
		o.bind.users = o.service.users
	}

	// SyncWorker:Aegis 侧账号状态变更(封号/改密/登出)→ DMWork 主动感知。
	// Interval=0 视为禁用,适合本地开发 / DB 还没准备好 RT 行的早期阶段。
	if o.cfg.Provider.SyncInterval > 0 && o.db != nil && o.killer != nil {
		enc, err := NewEncryptor(o.cfg.Provider.RefreshTokenEncryptionKey)
		if err != nil {
			return fmt.Errorf("oidc: Init: encryptor: %w", err)
		}
		// 注入 Redis tick lock:多实例同 tick 只一个跑,IdP 流量降到 1/N。
		o.tickLock = newRedisTickLock(o.ctx)
		o.worker = NewSyncWorker(SyncWorkerConfig{
			Interval:    o.cfg.Provider.SyncInterval,
			Concurrency: o.cfg.Provider.SyncConcurrency,
		}, o.db, enc, clientRefresher{c: o.client}, o.killer, o.audit, o.tickLock)
		// YUJ-405:RT 轮转成功后用新 access_token 调 /userinfo 同步实名 claims。
		// 覆盖所有 OIDC 登录过的用户,最多 SyncInterval 延迟感知 Aegis 侧实名变化。
		// ui/verif 同时就位:o.client 已完成 Discovery,userSvc 已确认实现 IService。
		o.worker.WithVerificationSync(o.client, userSvc)
		o.worker.Start(context.Background())
	}
	return nil
}

// legacyProviderPathID Route 在 provider ID 与之不同时额外挂一组路径作为前端兼容,
// 保证已发布的 web 客户端在后端 PR 合入当天仍能登录。一个迭代后随老前端下线一并删除。
const legacyProviderPathID = "aegis"

// Route 路由注册。Enabled=false 时所有端点返回 404,避免漏配置静默通过。
//
// 路径段从 cfg.Provider.ID 取,默认 "oidc";老前端硬编码的 "/aegis" 路径在 ID
// 不为 "aegis" 时同时挂载作为兼容入口,迁移完成后删除 legacyProviderPathID 即可。
//
// authorize/callback 是公开端点(IdP 重定向到 callback 时不带 dmwork token);
// logout 必须 AuthMiddleware 校验后拿 uid 才能踢线 + 吊销 RT,所以单独分组。
func (o *OIDC) Route(r *wkhttp.WKHttp) {
	id := ""
	if o.cfg != nil {
		id = o.cfg.Provider.ID
	}
	if id == "" {
		id = "oidc"
	}
	o.routeAt(r, id)
	if id != legacyProviderPathID {
		o.routeAt(r, legacyProviderPathID)
	}
}

func (o *OIDC) routeAt(r *wkhttp.WKHttp, pathID string) {
	base := "/v1/auth/oidc/" + pathID
	pub := r.Group(base)
	if o.cfg == nil || !o.cfg.Enabled {
		// disabled 路径三个端点都返 404,所以 logout 挂在 pub 而非 authed 没有
		// 安全影响 —— 不挂 AuthMiddleware 反而避免在 OIDC 关闭时给 /logout 引入
		// 跨模块的 token 校验依赖,行为更"完全关闭"。
		pub.GET("/authorize", o.disabled)
		pub.GET("/callback", o.disabled)
		pub.POST("/logout", o.disabled)
		return
	}
	pub.GET("/authorize", o.authorize)
	pub.GET("/callback", o.callback)
	authed := r.Group(base, o.ctx.AuthMiddleware(r))
	authed.POST("/logout", o.logout)
	// 自助绑定(P0):Bind.Enabled=false 时 bindRoutes 自身 no-op,生产路径完
	// 全不挂这些 endpoint;true 时挂 4 个 bind/* 端点,callback 接管由 PR4 引入。
	o.bindRoutes(pub)
}

func (o *OIDC) disabled(c *wkhttp.Context) {
	c.AbortWithStatus(http.StatusNotFound)
}

// authorize 生成 state/nonce/PKCE,落 StateStore,302 跳 IdP。
//
// 查询参数:
//   - authcode (必填): 前端生成的短码,callback 完成后用作 ThirdAuthcode Redis key
//   - return_to (可选): 登录后跳转地址,host 必须命中白名单或为相对路径
//   - flag     (可选): 设备标志,默认 0=APP
func (o *OIDC) authorize(c *wkhttp.Context) {
	metricAuthorizeTotal.Inc()
	if o.client == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("oidc client not initialized"))
		return
	}
	authcode := c.Query("authcode")
	if !authcodeRe.MatchString(authcode) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("authcode invalid"))
		return
	}
	cleanReturnTo, err := ValidateReturnTo(c.Query("return_to"), o.cfg.Provider.ReturnToHosts)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg(err.Error()))
		return
	}
	state, err := NewRandomString(32)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg(err.Error()))
		return
	}
	nonce, err := NewRandomString(32)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg(err.Error()))
		return
	}
	verifier, challenge, err := NewPKCEPair()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg(err.Error()))
		return
	}
	deviceFlag := defaultDeviceFlag
	if v := c.Query("flag"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 && n < 256 {
			deviceFlag = uint8(n)
		}
	}
	sd := &StateData{
		Provider:       o.cfg.Provider.ID,
		CodeVerifier:   verifier,
		Nonce:          nonce,
		IP:             util.GetClientPublicIP(c.Request),
		UserAgent:      c.Request.UserAgent(),
		ReturnTo:       cleanReturnTo,
		ClientAuthcode: authcode,
		DeviceFlag:     deviceFlag,
	}
	if err := o.stateStore.Save(c.Request.Context(), state, sd, stateTTL); err != nil {
		o.Error("保存 OIDC state 失败", zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("save state"))
		return
	}
	authURL, err := o.client.AuthCodeURL(state, nonce, challenge)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg(err.Error()))
		return
	}
	// EventAuthorize 不携带 uid(此时尚未拿到 IdP claims),仅用于审计统计:
	// state 数 / 异常 ip 高频起步 / authcode 复用 等运维向问题。
	o.writeAudit("", EventAuthorize, sd, "")
	c.Redirect(http.StatusFound, authURL)
}

// callback 验证 state → 换 token → 验签 → ResolveOrLink → IssueSession →
// 写 ThirdAuthcode Redis(前端短码轮询)→ 跳回 return_to。
//
// 任何步骤失败都把"0"写到 Redis key,前端按 GitHub 模式拿到 "0" 即视为登录失败,
// 与 api_github.go:161 保持一致,前端无需新代码。
func (o *OIDC) callback(c *wkhttp.Context) {
	traceID := newTraceID()
	clientIP := util.GetClientPublicIP(c.Request)
	start := time.Now()

	// result 在每个分支显式置位,defer 集中上报 callback 计数 + duration。
	// 默认 "other_fail",任何意外路径(panic 之外)都被归入此桶,触发告警时
	// 优先排查未在 callbackResultLabels() 枚举的新分支。
	result := "other_fail"
	defer func() {
		metricCallbackTotal.WithLabelValues(result).Inc()
		metricCallbackDuration.Observe(time.Since(start).Seconds())
	}()

	if o.client == nil {
		// result 默认即 "other_fail",此分支无需显式置位
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("oidc client not initialized"))
		return
	}

	// IP 限流前置:同一 IP 短时间内累计失败过多,直接 429 拒绝,
	// 不再消费 state、不再调 IdP,失败计数不再 +1(否则锁定窗口被自身续期成永久锁)。
	if o.cbGuard != nil {
		if cerr := o.cbGuard.Check(clientIP); cerr != nil {
			result = "rate_limited"
			o.Warn("OIDC callback 触达 IP 失败阈值,拒绝",
				zap.String("trace_id", traceID),
				zap.String("ip", clientIP))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, errMsg("too many failed callbacks, retry later"))
			return
		}
	}

	state := c.Query("state")
	if state == "" {
		result = "state_invalid"
		metricStateConsumeTotal.WithLabelValues("miss").Inc()
		o.cbGuard.RecordFailureLogged(clientIP)
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("state required"))
		return
	}

	sd, err := o.stateStore.Consume(c.Request.Context(), state)
	if err != nil {
		result = "state_invalid"
		metricStateConsumeTotal.WithLabelValues("miss").Inc()
		o.cbGuard.RecordFailureLogged(clientIP)
		o.Warn("OIDC state 校验失败",
			zap.String("trace_id", traceID),
			zap.String("ip", clientIP),
			zap.Error(err))
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("state invalid"))
		return
	}
	metricStateConsumeTotal.WithLabelValues("ok").Inc()

	// IdP 自身报错(用户拒绝授权 / 配置错误)。
	// 不计 IP 失败:用户在 IdP 端点了"拒绝"不是攻击;state 已消费,replay 不可能。
	// oerr 是 IdP 返回的任意字符串,截断到 maxAuditDetail 防灌爆。
	if oerr := c.Query("error"); oerr != "" {
		result = "idp_error"
		if len(oerr) > maxAuditDetail {
			oerr = oerr[:maxAuditDetail]
		}
		o.Warn("OIDC callback IdP 报错",
			zap.String("trace_id", traceID),
			zap.String("idp_error", oerr))
		o.failWithAuthcode(c.Request.Context(), sd, nil, fmt.Errorf("idp error: %s", oerr))
		o.redirectAfterCallback(c, sd, true)
		return
	}

	code := c.Query("code")
	if code == "" {
		result = "missing_code"
		o.cbGuard.RecordFailureLogged(clientIP)
		o.failWithAuthcode(c.Request.Context(), sd, nil, errors.New("missing code"))
		o.redirectAfterCallback(c, sd, true)
		return
	}

	tok, err := o.client.Exchange(c.Request.Context(), code, sd.CodeVerifier)
	if err != nil {
		// 不计 IP 失败:state 已消费,replay 同一对 (state, code) 行不通;
		// Exchange 故障多半是 IdP 抖动 / 网络问题,不是 IP 行为可控的攻击信号。
		result = "exchange_fail"
		o.Warn("OIDC callback Exchange 失败",
			zap.String("trace_id", traceID),
			zap.Error(err))
		o.failWithAuthcode(c.Request.Context(), sd, nil, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}

	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		result = "verify_fail"
		o.cbGuard.RecordFailureLogged(clientIP)
		o.failWithAuthcode(c.Request.Context(), sd, nil, errors.New("id_token missing from token response"))
		o.redirectAfterCallback(c, sd, true)
		return
	}

	claims, err := o.client.VerifyIDToken(c.Request.Context(), rawID)
	if err != nil {
		result = "verify_fail"
		o.cbGuard.RecordFailureLogged(clientIP)
		o.Warn("OIDC callback VerifyIDToken 失败",
			zap.String("trace_id", traceID),
			zap.Error(err))
		o.failWithAuthcode(c.Request.Context(), sd, nil, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.Info("OIDC callback id_token verified",
		zap.String("trace_id", traceID),
		zap.String("sub_hash", subHash(claims.Subject)),
		zap.String("email", maskEmail(claims.Email)),
		zap.Bool("email_verified", claims.EmailVerified))

	if claims.Nonce != sd.Nonce {
		result = "nonce_mismatch"
		o.cbGuard.RecordFailureLogged(clientIP)
		o.Warn("OIDC callback nonce 不匹配",
			zap.String("trace_id", traceID),
			zap.String("sub_hash", subHash(claims.Subject)))
		o.failWithAuthcode(c.Request.Context(), sd, claims, errors.New("nonce mismatch"))
		o.redirectAfterCallback(c, sd, true)
		return
	}

	// 部分 IdP(如 Aegis / OCTO)只在 /userinfo 暴露 email/phone/name,ID Token 仅含 sub。
	// 自动绑定历史账号必须依赖 email/phone;name 缺失会让新建用户落到随机汉名兜底
	// (modules/user/api.go createUserWithRespAndTx 的 Names[] 分支),用户体验极差。
	// 所以缺啥就拉一次 /userinfo 补啥(issue #1307)。
	// userinfo 失败不阻断登录,只是失去自动绑定能力,等价于 IdP 没返这些 claim。
	//
	// identity_verification scope 类似(YUJ-382 + codex review 多轮):部分 Aegis
	// 部署把 5 个字段全放 ID Token,另一些只在 /userinfo 暴露,还有一些只放部分
	// 字段。为覆盖所有部署形态:只要 scope 已配置 **且** 任一必需字段未就位
	// (IsVerified=false、VerifiedAt=0、VerifiedProvider 空、LegalName 空),
	// 都触发 /userinfo 合并。代价:未实名用户每次登录多一跳 /userinfo 请求
	// (IdP 端幂等、本地 http 超时兜底已有),换 Phase 1 直切方案在生产各种
	// 部署形态下稳定生效。
	needUserInfo := claims.Email == "" || claims.PhoneNumber == "" || claims.Name == ""
	if !needUserInfo && hasIdentityVerificationScope(o.cfg.Provider.Scopes) &&
		!hasCompleteVerificationClaims(claims) {
		needUserInfo = true
	}
	if needUserInfo {
		ui, uerr := o.client.UserInfo(c.Request.Context(), tok)
		if uerr != nil {
			o.Warn("OIDC callback userinfo 拉取失败,跳过补全",
				zap.String("trace_id", traceID),
				zap.Error(uerr))
		} else if ui.Subject != claims.Subject {
			// 安全检查:userinfo sub 必须等于 ID Token sub,否则视为账号串台,直接拒绝
			result = "verify_fail"
			o.cbGuard.RecordFailureLogged(clientIP)
			o.failWithAuthcode(c.Request.Context(), sd, claims,
				fmt.Errorf("userinfo sub mismatch: idtoken=%s userinfo=%s",
					subHash(claims.Subject), subHash(ui.Subject)))
			o.redirectAfterCallback(c, sd, true)
			return
		} else {
			if claims.Email == "" {
				claims.Email = ui.Email
				claims.EmailVerified = ui.EmailVerified
			}
			if claims.PhoneNumber == "" {
				claims.PhoneNumber = ui.PhoneNumber
				claims.PhoneVerified = ui.PhoneVerified
			}
			if claims.Name == "" {
				claims.Name = ui.Name
			}
			// identity_verification 合并:只在 ID Token 对应字段为空(或 is_verified 未置)
			// 时才取 userinfo 的,避免 IdP 两边不一致时静默覆盖(ID Token 是签名权威,
			// 优先保留)。IsVerified 本身只有 true 能向 false 走过 IdP 明确撤销的语义,
			// 所以 userinfo 的 true 可以覆盖 ID Token 的 false(更新语义)。
			if ui.IsVerified {
				claims.IsVerified = true
			}
			if claims.VerifiedAt == 0 {
				claims.VerifiedAt = ui.VerifiedAt
			}
			if claims.VerifiedProvider == "" {
				claims.VerifiedProvider = ui.VerifiedProvider
			}
			if claims.LegalName == "" {
				claims.LegalName = ui.LegalName
			}
			if claims.LegalEmail == "" {
				claims.LegalEmail = ui.LegalEmail
			}
		}
	}

	res, err := o.service.ResolveOrLink(c.Request.Context(), claims)
	if err != nil {
		// PR4 自助绑定接管:autolink 失败时,若 Bind.Enabled + issuer 在 allowlist
		// 内 + 错误是可绑定类型(ErrUnknownUser/ErrConflictNeedManual),引导用户走
		// 自助绑定流程。其它失败 / flag off / issuer 不在白名单都退回旧路径,确保
		// NFR-6 一键回滚(关 flag + 重启)语义生效。
		if o.bind.ShouldHandle(err, claims) {
			// 把 ResolveOrLink 的 err 类型固化到 BindSession.IssueReason —— Create
			// 路径用它拒绝 manual_conflict 来源的建号请求,Info 路径用它回填
			// create_blocked。BindReasonManualConflict 仅在多账号冲突时落地;
			// 其他可接管错误统一按 BindReasonUnknownUser(自助建号合法来源)签发。
			reason := BindReasonUnknownUser
			if errors.Is(err, ErrConflictNeedManual) {
				reason = BindReasonManualConflict
			}
			jti, ierr := o.bind.IssueWithReason(c.Request.Context(), claims, sd, reason)
			if ierr == nil {
				result = "bind_pending" // 已在 callbackResultLabels 注册
				// bind 接管时尚不知 uid,先按 jti 暂存 id_token,confirm/create 后迁移到 uid。
				o.saveBindIDTokenHint(c.Request.Context(), jti, rawID)
				o.writeAudit("bind:"+subHash(jti), EventBindIssued, sd, "")
				o.redirectToBindPage(c, sd, jti)
				return
			}
			// Issue 失败:不让"bind 引擎抖动"把整条 OIDC 登录拖死,继续退回旧路径。
			// 失败原因记 warn,运维通过 oidc_bind_request_total 看不到这一脚 ——
			// 是有意的,这种"bind 接管异常但回落"应该看 callback_total{result=resolve_fail}。
			o.Warn("OIDC bind Issue failed, falling back to legacy fail path",
				zap.String("trace_id", traceID), zap.Error(ierr))
		}
		result = "resolve_fail"
		o.failWithAuthcode(c.Request.Context(), sd, claims, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}

	zone := extractZone(claims.PhoneNumber)
	phone := extractPhone(claims.PhoneNumber)
	if claims.PhoneNumber != "" && phone == "" {
		// 非 +86 号码 extractPhone 当前直接丢弃,记 warn 让运维知道
		// "OIDC 登录手机号没绑上"不是 IdP 没返,而是 dmwork 的解析限制。
		o.Warn("OIDC phone number dropped: only +86 supported",
			zap.String("idp_phone", claims.PhoneNumber))
	}
	issueReq := IssueSessionReq{
		UID:        res.UID,
		CreateUser: res.IsNew,
		Name:       claims.Name,
		Email:      claims.Email,
		Phone:      phone,
		Zone:       zone,
		DeviceFlag: sd.DeviceFlag,
		PublicIP:   sd.IP,
		// res.IsNew=true 进入 user.externalLoginCreate;TrustedSSOCreate=true
		// 让 user 模块绕过 register.off 全局开关。
		//
		// callback 路径的信任锚(与 /bind/create 走 IssuerAllowlist 是**不同**的
		// trust chain,不要混):
		//   1. o.client.VerifyIDToken 用 cfg.Provider.Issuer discovery 出来的
		//      IdP 公钥验签 → claims.Issuer 必然等于 cfg.Provider.Issuer
		//      (不等则验签直接失败,根本走不到这里),等同于"size=1 的
		//      隐式 issuer allowlist";
		//   2. Service.ResolveOrLink 只在 cfg.Provider.AllowNewUser=true
		//      时才返 IsNew=true,这是运维通过 DM_OIDC_PROVIDER_ALLOW_NEW_USER
		//      显式开的 bool。
		// 两条合在一起 = "运维显式信任的单一 Provider.Issuer 自动建号" —— 与
		// 公开注册入口(email/phone signup / GitHub/Gitee OAuth)的不可控外部
		// 输入语义不同,bypass register.off 的运维授权是显式的。
		//
		// 与 /bind/create 行为对称(都"OIDC 通道下让运维显式控制建号"),但
		// 信任链的具体机制不同 —— /bind/create 用 IssuerAllowlist 兜底
		// (多 issuer 配置 + bind_token 显式同意),callback 用单 Provider.Issuer
		// 签名 + AllowNewUser flag。
		TrustedSSOCreate: res.IsNew,
	}
	sessResp, err := o.service.IssueSession(c.Request.Context(), issueReq)
	if err != nil {
		result = "issue_fail"
		o.failWithAuthcode(c.Request.Context(), sd, claims, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}

	// 新建用户:user 模块创建后,补写 oidc identity 绑定行(uid 是 user 模块回填的)。
	//
	// 并发竞态处理:同 (issuer, sub) 的两个 callback 同时进来,ResolveOrLink 都
	// 返回 IsNew=true,各自创建一个 user。UNIQUE KEY uk_issuer_subject 保证只
	// 有一行 identity。输家的 user 已落库无法回滚 → 把输家的会话改签到赢家 uid,
	// 用户体验正确(两个 tab 都登成同一个账号),ghost user 留给审计 + 后台合并。
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
				recovered := o.recoverFromIdentityRace(c.Request.Context(), claims, sd, sessResp, issueReq, err)
				if recovered == nil {
					result = "identity_insert_fail"
					// 竞态恢复失败:writeAudit 已在 recover 内部记录,这里只补 ThirdAuthcode "0"
					if e := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
						o.Error("写 ThirdAuthcode 失败(race-recover fail path)",
							zap.String("trace_id", traceID), zap.Error(e))
					}
					o.redirectAfterCallback(c, sd, true)
					return
				}
				result = "race_recovered"
				sessResp = recovered
			} else {
				result = "identity_insert_fail"
				o.Error("写 identity 绑定失败(非竞态)",
					zap.String("trace_id", traceID),
					zap.String("sub_hash", subHash(claims.Subject)),
					zap.Error(err))
				o.failWithAuthcode(c.Request.Context(), sd, claims, fmt.Errorf("bind identity: %w", err))
				o.redirectAfterCallback(c, sd, true)
				return
			}
		}
	}

	// existing user 重复登录:刷新 identity 行的 last_login_at 和最新 claims 字段。
	// 之前这一步缺失,导致 existing user 的 last_login_at 永远 NULL。
	if !res.IsNew && res.UID != "" {
		if existing, err := o.store.Get(claims.Issuer, claims.Subject); err == nil && existing != nil {
			if uerr := o.store.UpdateLogin(existing.Id,
				claims.Email, boolToInt(claims.EmailVerified),
				claims.PhoneNumber, boolToInt(claims.PhoneVerified)); uerr != nil {
				o.Error("更新 identity login info 失败", zap.Error(uerr))
			}
		}
	}

	// Aegis OIDC 直切(YUJ-382 / Aegis OIDC Phase 1):若 IdP 返回 identity_verification
	// claims,登录时顺手 upsert user_verification 表。权威写入口从 dmwork-verify-service
	// 的 HMAC 回调迁移到 oidc callback,前端协议/表 schema 均无变化。
	//
	// **失败只告警不阻断登录**:实名状态刷不了是 P2,用户登不进系统是 P0。
	// 不满足条件(未配 upserter / is_verified=false / legal_name 空)直接跳过,不报错。
	//
	// YUJ-413 R5 Critical #1 修复 — 写库时序 + LoginRespJSON patch:
	// IssueSession 已在 sessResp.LoginRespJSON 里调过 applyRealnameToLoginResp,
	// 但此时 user_verification 还没有这次 upsert 的行(首次实名 / 值变化场景),
	// 所以 JSON 里的 realname 字段是 stale 的。下面 upsert 成功后,我们在这里
	// 用刚写进去的 claims 值 in-place patch 一次 JSON —— 保证 SetAuthcode 缓存
	// 的 payload 就是最新的,客户端首次 fresh login 就能拿到正确的实名态,
	// 不必依赖 Custom Tabs 回跳后的 GET /v1/user/current 二次刷新。
	if o.verification != nil && claims.IsVerified.Bool() && claims.LegalName != "" && sessResp.UID != "" {
		vclaims := user.OIDCVerificationClaims{
			Subject:          claims.Subject,
			VerifiedProvider: claims.VerifiedProvider,
			VerifiedAt:       claims.VerifiedAt.Int64(),
			LegalName:        claims.LegalName,
			LegalEmail:       claims.LegalEmail,
		}
		if verr := o.verification.UpsertVerificationFromOIDC(c.Request.Context(), sessResp.UID, vclaims); verr != nil {
			o.Warn("OIDC callback upsert verification failed (非致命,不阻断登录)",
				zap.String("trace_id", traceID),
				zap.String("sub_hash", subHash(claims.Subject)),
				zap.String("provider", claims.VerifiedProvider),
				zap.Error(verr))
		} else {
			// upsert 成功才 patch — 失败时 stale 和 DB 保持一致,客户端后续
			// GET /v1/user/current 会看到真实(仍 stale 的)状态。
			if patched, perr := patchLoginRespJSONWithRealname(
				sessResp.LoginRespJSON,
				claims.LegalName,
				claims.VerifiedAt.Int64(),
			); perr != nil {
				o.Warn("OIDC callback patch LoginRespJSON realname failed (非致命,客户端可用 /user/current 兜底)",
					zap.String("trace_id", traceID), zap.Error(perr))
			} else {
				sessResp.LoginRespJSON = patched
			}
		}
	}

	if err := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, sessResp.LoginRespJSON, thirdAuthcodeTTL); err != nil {
		result = "set_authcode_fail"
		// 写 LoginRespJSON 失败,前端轮询永远拿不到 token,会傻等到 TTL 超时。
		// 立刻补 "0" 让前端尽早感知,并在 redirect URL 拼 ?oidc_error=1。
		o.Error("写 ThirdAuthcode 失败",
			zap.String("trace_id", traceID), zap.Error(err))
		if e := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
			o.Error("回写 ThirdAuthcode \"0\" 也失败,前端将等到 TTL 超时",
				zap.String("trace_id", traceID), zap.Error(e))
		}
		o.writeAudit(sessResp.UID, EventCallbackFail, sd, "set authcode failed: "+err.Error())
		o.redirectAfterCallback(c, sd, true)
		return
	}
	result = "ok"
	// 成功路径清场:防止 IP 长尾累积导致历史失败 + 偶发 state 过期把用户误锁。
	o.cbGuard.ResetLogged(clientIP)
	// 缓存验签过的 id_token,供后续 logout 当 RP-Initiated Logout 的 id_token_hint。
	// best-effort:存失败只告警,不影响登录(logout 时退回"仅清本地")。日志不打 token。
	if o.idTokens != nil && sessResp.UID != "" {
		if serr := o.idTokens.Save(c.Request.Context(), sessResp.UID, rawID, o.cfg.Provider.IDTokenTTL); serr != nil {
			o.Warn("OIDC callback 缓存 id_token 失败(不影响登录,仅 RP-logout 降级)",
				zap.String("trace_id", traceID), zap.Error(serr))
		}
	}
	o.writeAudit(sessResp.UID, EventCallbackOK, sd, "")
	o.redirectAfterCallback(c, sd, false)
}

// Close 释放 OIDC 模块持有的资源(redisStateStore 连接池 + SyncWorker goroutine)。
//
// 注册到 register.Module.Stop,框架优雅退出时调用。可被多次调用(幂等):
//   - New() 内 Discovery 失败会调一次清理 stateStore
//   - 之后 framework shutdown 又会调一次,此时 stateStore 已 nil,早返回
func (o *OIDC) Close() error {
	if o.worker != nil {
		o.worker.Stop()
		o.worker = nil
	}
	if o.tickLock != nil {
		if err := o.tickLock.Close(); err != nil {
			o.Error("关闭 OIDC sync tick lock 失败", zap.Error(err))
		}
		o.tickLock = nil
	}
	// bindStore 独立 redis.Client(与 stateStore 同模式),Bind.Enabled=true
	// 时由 Init 创建。优雅退出/Discovery 失败兜底清理都要关,否则 fd 泄漏。
	if rbs, ok := o.bindStore.(*redisBindStore); ok {
		if err := rbs.Close(); err != nil {
			o.Error("关闭 OIDC bind store 失败", zap.Error(err))
		}
		o.bindStore = nil
	}
	// idTokens 独立 redis.Client(RP-Initiated Logout),New() 在 enabled 时创建。
	// 与 bindStore 同样需在关闭路径释放,否则连接池 fd 泄漏。放在 stateStore nil
	// 早返回之前,保证 stateStore 已被置 nil 的二次 Close 仍能关掉 idTokens。
	if ridt, ok := o.idTokens.(*redisIDTokenStore); ok {
		if err := ridt.Close(); err != nil {
			o.Error("关闭 OIDC id_token store 失败", zap.Error(err))
		}
		o.idTokens = nil
	}
	if o.stateStore == nil {
		return nil
	}
	if rss, ok := o.stateStore.(*redisStateStore); ok {
		return rss.Close()
	}
	return nil
}

// logout 撤销本地登录态:踢全部设备 + 吊销该 UID 名下所有未吊销 RT + 审计。
//
// 前置条件:路由已挂 AuthMiddleware,c.GetLoginUID() 有值。无 uid 视为未登录。
// 任何步骤失败都按"尽力而为"处理:踢线失败仍尝试吊销 RT,反之亦然,最终都返 200。
// 理由:logout 客户端关心的是"我点了登出,本地已清空状态",对幂等性要求高于完美吊销。
// 真正的兜底由 SyncWorker 的下次轮询补足(refresh 失败也会触发踢线)。
//
// IdP 端 RP-Initiated Logout(/end_session)的跳转地址由后端拼好后随 200 响应返回
// (end_session_url 字段),前端做顶层跳转。后端收口的原因:本架构 code→token 在
// 服务端完成,前端无法自行*构造* id_token_hint(它不经手 token 交换),也不应散落
// end_session 端点 / 参数 / 回跳白名单这些 IdP 细节 —— 由后端给出单次性 URL 最稳妥。
// 真正终止 IdP 会话仍依赖浏览器顶层跳转携带 IdP 域 cookie,所以后端只给 URL,不代理跳转。
//
// 信任模型说明:end_session_url 里必然带 id_token_hint(RFC 规定的 front-channel 参数),
// 因此该 id_token 会暴露到前端 JS、浏览器历史、Referer 及 IdP 访问日志 —— 这是
// RP-Initiated Logout 协议固有的。可接受:它是单次性、不可重放的登出提示(octo-server
// 自身从不把它当 bearer/assertion 复用),取出即原子作废(luaGetDel)。
//
// 配置缺失(未配 PostLogoutRedirectURI / 无 end_session 端点 / 无缓存的 id_token)时
// 省略 end_session_url,前端降级为仅清本地 —— 纯增量,不影响存量行为。
func (o *OIDC) logout(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, errMsg("login required"))
		return
	}
	traceID := newTraceID()
	ctx := c.Request.Context()

	// kickFailed/revokeFailed 单独记账:logout 整体最终都返 200(best-effort 语义),
	// 但指标要区分"成功 / 踢线失败 / 吊销失败",方便定位 IM 或 DB 链路问题。
	kickFailed := false
	revokeFailed := false
	if o.killer != nil {
		if err := o.killer.Kick(ctx, uid); err != nil {
			kickFailed = true
			o.Error("OIDC logout 踢线失败",
				zap.String("trace_id", traceID),
				zap.Error(err), zap.String("uid", uid))
		}
	}
	if o.revoker != nil {
		if _, err := o.revoker.RevokeRefreshByUID(uid); err != nil {
			revokeFailed = true
			o.Error("OIDC logout 吊销 RT 失败",
				zap.String("trace_id", traceID),
				zap.Error(err), zap.String("uid", uid))
		}
	}
	// 两个失败标签独立计数 —— 同一次 logout 可能 kick 和 revoke 都失败,
	// 早期 switch/case 写法会把 revoke_fail 吃掉。Counter sum 仍可能 > 总请求数,
	// 但每个失败维度的趋势准确,运维查"哪条链路在抖"时不会漏报。
	switch {
	case kickFailed && revokeFailed:
		metricLogoutTotal.WithLabelValues("kick_fail").Inc()
		metricLogoutTotal.WithLabelValues("revoke_fail").Inc()
	case kickFailed:
		metricLogoutTotal.WithLabelValues("kick_fail").Inc()
	case revokeFailed:
		metricLogoutTotal.WithLabelValues("revoke_fail").Inc()
	default:
		metricLogoutTotal.WithLabelValues("ok").Inc()
	}
	o.writeAudit(uid, EventLogout, &StateData{
		IP:        util.GetClientPublicIP(c.Request),
		UserAgent: c.Request.UserAgent(),
	}, "")

	resp := map[string]interface{}{"status": 200}
	// 拼 IdP end_session 跳转地址(RP-Initiated Logout)。任一前置缺失时返回空串,
	// 此时省略字段,前端降级为仅清本地。end_session_url 含 id_token,不写日志。
	if endSessionURL := o.buildEndSessionURL(ctx, uid); endSessionURL != "" {
		resp["end_session_url"] = endSessionURL
		// 响应体含 id_token_hint,禁止任何缓存(OAuth/OIDC 安全 BCP、RFC 6749 §5.1)。
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
	}
	c.JSON(http.StatusOK, resp)
}

// endSessionEndpoint 解析 IdP 的 RP-Initiated Logout 端点:config override 优先,
// 否则取 Discovery 解析值。两者皆空时返回空串(IdP 未声明且未配 override)。
func (o *OIDC) endSessionEndpoint() string {
	if o.cfg != nil && o.cfg.Provider.EndSessionURL != "" {
		return o.cfg.Provider.EndSessionURL
	}
	if o.client != nil {
		return o.client.EndSessionEndpoint()
	}
	return ""
}

// buildEndSessionURL 构造 RP-Initiated Logout 跳转地址,带 id_token_hint +
// post_logout_redirect_uri。任一前置不满足返回空串(调用方据此省略字段、前端降级):
//   - 未配置 PostLogoutRedirectURI(运维写死的回跳页,同时充当白名单);
//   - idTokens 未注入 / 取不到该 uid 的 id_token(非 OIDC 登录 / 已过期 / 已消费);
//   - 无可用 end_session 端点。
//
// 取出 id_token 即一次性消费(Take 内部删除)。不带 state(Aegis Discovery 未声明)。
//
// 顺序很关键:先解析+校验端点,再消费 id_token。原因有二:
//   - 端点非法时不白烧 token(GETDEL 不可逆),logout 仍可重试拿到 end_session_url;
//   - end_session 端点可能来自 discovery(非 config override,未经启动期校验),这里统一
//     过一道 https 校验 —— 防 IdP 万一下发 http:// 把带 id_token 的 URL 降级到非 https。
func (o *OIDC) buildEndSessionURL(ctx context.Context, uid string) string {
	if o.cfg == nil || o.cfg.Provider.PostLogoutRedirectURI == "" || o.idTokens == nil {
		return ""
	}
	endpoint := o.endSessionEndpoint()
	if endpoint == "" {
		return ""
	}
	// 校验 + 解析端点(在消费 token 之前)。validateLogoutURL 强制绝对 https
	// (dev 可 OCTO_OIDC_LOGOUT_ALLOW_INSECURE=1),覆盖 discovery 与 override 两种来源。
	if err := validateLogoutURL("end_session_endpoint", endpoint); err != nil {
		o.Error("OIDC logout end_session 端点不合法,跳过 IdP 登出", zap.String("endpoint", endpoint), zap.Error(err))
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		// 只记端点本身,不记含 id_token 的完整 URL。
		o.Error("OIDC logout 解析 end_session 端点失败", zap.String("endpoint", endpoint), zap.Error(err))
		return ""
	}
	idToken, err := o.idTokens.Take(ctx, uid)
	if err != nil {
		// 取 id_token 失败不阻断 logout(本地已踢线+吊销),仅降级跳过 IdP 跳转。
		o.Warn("OIDC logout 取 id_token 失败,跳过 end_session 跳转", zap.Error(err))
		return ""
	}
	if idToken == "" {
		return ""
	}
	q := u.Query()
	q.Set("id_token_hint", idToken)
	q.Set("post_logout_redirect_uri", o.cfg.Provider.PostLogoutRedirectURI)
	u.RawQuery = q.Encode()
	return u.String()
}

// saveBindIDTokenHint 在自助绑定接管(callback bind_pending)时,按 bind token(jti)
// 暂存验签过的 id_token,TTL 对齐 bind session —— bind 路径的 callback 还不知道最终
// uid,无法直接按 uid 存。confirm/create 成功后由 promoteBindIDToken 迁移到 uid 名下。
// 仅在 RP-Initiated Logout 启用(idTokens!=nil)时生效;best-effort,失败不阻断绑定。
func (o *OIDC) saveBindIDTokenHint(ctx context.Context, jti, rawID string) {
	if o.idTokens == nil || jti == "" || rawID == "" {
		return
	}
	if err := o.idTokens.Save(ctx, bindIDTokenKey(jti), rawID, o.cfg.Bind.TokenTTL); err != nil {
		o.Warn("OIDC bind 暂存 id_token 失败(不影响绑定,仅 RP-logout 降级)", zap.Error(err))
	}
}

// promoteBindIDToken 把 bind 接管阶段按 jti 暂存的 id_token 迁移到已确定的 uid 名下,
// 供后续 logout 当 id_token_hint。一次性消费 jti 暂存项(Take 内部删除);无值时静默
// (非 OIDC bind 登录 / 已过期 / 功能未启用)。best-effort,失败不阻断绑定完成。
func (o *OIDC) promoteBindIDToken(ctx context.Context, jti, uid string) {
	if o.idTokens == nil || jti == "" || uid == "" {
		return
	}
	raw, err := o.idTokens.Take(ctx, bindIDTokenKey(jti))
	if err != nil {
		o.Warn("OIDC bind 取暂存 id_token 失败,跳过 RP-logout 缓存", zap.Error(err))
		return
	}
	if raw == "" {
		return
	}
	if err := o.idTokens.Save(ctx, uid, raw, o.cfg.Provider.IDTokenTTL); err != nil {
		o.Warn("OIDC bind 迁移 id_token 到 uid 失败", zap.Error(err))
	}
}

func (o *OIDC) failWithAuthcode(ctx context.Context, sd *StateData, claims *IDTokenClaims, err error) {
	uid := ""
	if claims != nil {
		// 审计 uid 列存 SHA-256 短哈希前缀,既能事后关联同一 IdP 用户,
		// 又避免明文 sub 泄漏到审计表。前缀固定 "sub:" 与历史落库格式兼容(老行
		// 是明文截断,新行是哈希),排查时按 prefix 过滤即可。
		uid = "sub:" + subHash(claims.Subject)
		if len(uid) > maxAuditUID {
			uid = uid[:maxAuditUID]
		}
	}
	o.Warn("OIDC callback 失败", zap.String("audit_uid", uid), zap.Error(err))
	o.writeAudit(uid, EventCallbackFail, sd, err.Error())
	if sd == nil || sd.ClientAuthcode == "" {
		return
	}
	if e := o.authcode.SetAuthcode(ctx, sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
		o.Error("写 ThirdAuthcode 失败(fail path)", zap.Error(e))
	}
}

// recoverFromIdentityRace 处理新建用户时 identity unique-key 冲突。
//
// 场景:同 (issuer, sub) 的两个 callback 并发到达,ResolveOrLink 都返回 IsNew=true,
// 各自调 IssueSession 创建了 user。UNIQUE KEY 让只有一行 identity 落库,
// 输家 user 已 commit 无法回滚。
//
// 成功路径:把输家会话改签到赢家 uid,返回赢家 session。两个 tab 都登成同一账号,UX 正确;
// 输家创建的 dmwork user 是 ghost(无 OIDC 绑定),由审计日志 + 后台合并清理。
//
// 失败路径(查不到赢家 / 赢家会话签发失败)返回 nil,caller 必须走 failWithAuthcode
// 写 "0" 让前端提示重试。**绝不能把 ghost session 写到 ThirdAuthcode**——那等于
// 给前端发了一个无 OIDC 绑定的孤立账号 token,后续依赖 identity 的业务全部空转。
func (o *OIDC) recoverFromIdentityRace(
	ctx context.Context,
	claims *IDTokenClaims,
	sd *StateData,
	original *IssueSessionResp,
	origReq IssueSessionReq,
	insertErr error,
) *IssueSessionResp {
	existing, qerr := o.store.Get(claims.Issuer, claims.Subject)
	if qerr != nil || existing == nil {
		o.Error("写 identity 绑定失败且无法定位赢家", zap.Error(insertErr),
			zap.String("ghost_uid", original.UID))
		o.writeAudit(original.UID, EventCallbackFail, sd,
			"insert identity (ghost orphan): "+insertErr.Error())
		return nil
	}
	if existing.UID == original.UID {
		// 异常:UNIQUE 冲突但赢家就是自己?数据已就位,当作正常返回。
		return original
	}
	winnerReq := origReq
	winnerReq.UID = existing.UID
	winnerReq.CreateUser = false
	winnerSess, err := o.service.IssueSession(ctx, winnerReq)
	if err != nil {
		o.Error("identity 竞态后赢家会话签发失败", zap.Error(err),
			zap.String("ghost_uid", original.UID),
			zap.String("winner_uid", existing.UID))
		o.writeAudit(original.UID, EventCallbackFail, sd,
			"race-recover failed; ghost="+original.UID+" winner="+existing.UID+": "+err.Error())
		return nil
	}
	o.Warn("identity 并发写入冲突,会话已改签到赢家;ghost user 待人工合并",
		zap.String("ghost_uid", original.UID),
		zap.String("winner_uid", existing.UID),
		zap.String("issuer", claims.Issuer),
		zap.String("sub_hash", subHash(claims.Subject)))
	o.writeAudit(original.UID, EventCallbackFail, sd,
		"identity race ghost="+original.UID+" winner="+existing.UID)
	return winnerSess
}

// patchLoginRespJSONWithRealname 把刚写入 user_verification 的三个实名字段
// in-place patch 进 sessResp.LoginRespJSON。YUJ-413 R5 Critical #1 修复:
//
// OIDC callback 的原始时序是:
//  1. IssueSession → user.execLogin → newLoginUserDetailResp →
//     applyRealnameToLoginResp(读旧 user_verification)→ 生成 LoginRespJSON
//  2. UpsertVerificationFromOIDC(写入新实名行)
//  3. SetAuthcode(把 1 的 LoginRespJSON 缓存给前端)
//
// 首次实名 / 实名字段值变化时,第 1 步读到的是旧值(或缺失),第 3 步缓存的
// JSON 就和 DB 现状不一致,客户端 fresh login 拿到的是 stale 态 ——
// 直接违反"fresh login 后 self 实名字段可用"契约。
//
// 本函数在 upsert 成功后被调用,用已知的 claims 值替换 JSON 里的对应 key。
// 用 claims 而不是再查一次 DB,一是省 round trip,二是语义最确定(就是刚写
// 进去的那行)。
//
// 字段名严格对齐 loginUserDetailResp:
//
//	realname_verified      = true
//	real_name              = realName  (空则不写)
//	realname_verified_at   = verifiedAt (<=0 则不写)
//
// 传入 JSON 空 / 非法时返回原值 + 非 nil err,调用方自行决定是否回退。
func patchLoginRespJSONWithRealname(jsonStr, realName string, verifiedAt int64) (string, error) {
	if jsonStr == "" {
		return jsonStr, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return jsonStr, fmt.Errorf("oidc: unmarshal LoginRespJSON: %w", err)
	}
	m["realname_verified"] = true
	if realName != "" {
		m["real_name"] = realName
	}
	if verifiedAt > 0 {
		m["realname_verified_at"] = verifiedAt
	}
	b, err := json.Marshal(m)
	if err != nil {
		return jsonStr, fmt.Errorf("oidc: marshal patched LoginRespJSON: %w", err)
	}
	return string(b), nil
}

// writeAudit best-effort 审计:写失败只记 log,不阻塞调用方。
//
// 审计写到 DB 是为了事后追溯(例如 ghost user 排查、异常登录排查);
// 写失败不应该干扰用户登录体验,所以不返错。
func (o *OIDC) writeAudit(uid string, event AuditEvent, sd *StateData, reason string) {
	if o.audit == nil {
		return
	}
	m := &AuditModel{UID: uid, Event: event, Reason: reason}
	if sd != nil {
		m.IP = sd.IP
		m.UserAgent = sd.UserAgent
	}
	if err := o.audit.InsertAudit(m); err != nil {
		o.Error("写 OIDC 审计失败", zap.Error(err), zap.String("event", string(event)))
	}
}

// fallbackReturnTo 没配 return_to 时回根路径,确保 302 总能成立。
func fallbackReturnTo(rt string) string {
	if rt == "" {
		return "/"
	}
	return rt
}

// redirectToBindPage 自助绑定触发时的 302 跳转。把 jti + 原 authcode + 清洗后
// 的 return_to 拼到 BindConfig.RedirectBase 上。
//
// 设计:
//   - 用 url.Parse + Query API 拼参,避免手拼 query 在 RedirectBase 自带 ? 时
//     出 ?token=xxx?return_to=yyy 的 bug;
//   - return_to 走 ValidateReturnTo 二次校验(纵深防御,与 redirectAfterCallback
//     一致);非法时直接落空,前端按未提供处理;
//   - RedirectBase 为空时(漏配置)记 error 并退回 redirectAfterCallback 失败
//     路径,**绝不**裸跳 302 到空字符串(那会变 referrer 漏洞);
//   - 不向 URL 拼任何 claims 内容(SR-7),客户端通过 /bind/info?token=... 拉脱敏。
func (o *OIDC) redirectToBindPage(c *wkhttp.Context, sd *StateData, jti string) {
	base := o.cfg.Bind.RedirectBase
	if base == "" {
		o.Error("OIDC bind redirect: OCTO_OIDC_BIND_REDIRECT_BASE not configured, falling back",
			zap.String("jti_hash", subHash(jti)))
		o.failBindRedirect(c, sd)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		o.Error("OIDC bind redirect: invalid RedirectBase",
			zap.String("base", base), zap.Error(err))
		o.failBindRedirect(c, sd)
		return
	}
	q := target.Query()
	q.Set("token", jti)
	// provider 段在 bind API 路径里 (/v1/auth/oidc/<provider>/bind/*),前端从
	// query 取出后拼回 API URL;缺失时前端兜底到 legacyProviderPathID="aegis"。
	if o.cfg.Provider.ID != "" {
		q.Set("provider", o.cfg.Provider.ID)
	}
	if sd != nil && sd.ClientAuthcode != "" {
		q.Set("authcode", sd.ClientAuthcode)
	}
	// 二次校验 return_to (纵深防御:即便 RedirectBase 是可信前端域,我们也
	// 不应把任意原 return_to 透过)。
	if sd != nil {
		if cleaned, verr := ValidateReturnTo(sd.ReturnTo, o.cfg.Provider.ReturnToHosts); verr == nil && cleaned != "" {
			q.Set("return_to", cleaned)
		}
	}
	target.RawQuery = q.Encode()
	// Referrer-Policy: no-referrer 仅保护这一跳:浏览器从 callback URL 跳到
	// bind 页时不会把 callback 的 ?code=... &state=... 经 Referer 泄漏给 bind
	// 页 host。bind 页加载之后,其内部子资源是否泄漏"含 token/authcode 的
	// bind 页 URL",取决于 bind 页**自己**的 Referrer-Policy(响应头或 meta),
	// 后端无法跨域强制。前端 host 应同步下发 Referrer-Policy: no-referrer
	// 作为纵深防御。
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, target.String())
}

// failBindRedirect 跳转到 bind 页失败(漏配 RedirectBase / 非法 URL)时的兜底:
// 先把 ThirdAuthcode 写 "0",让原发起设备的前端轮询立即拿到失败信号(否则要等
// 5min TTL 才会感知,用户会卡在加载态);再走 redirectAfterCallback 失败路径。
//
// 写 "0" 失败仅 log:此时已经在异常路径,继续 redirect 比 panic 更可控。
func (o *OIDC) failBindRedirect(c *wkhttp.Context, sd *StateData) {
	if o.authcode != nil && sd != nil && sd.ClientAuthcode != "" {
		if e := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
			o.Error("OIDC bind redirect fallback: write ThirdAuthcode \"0\" failed",
				zap.Error(e))
		}
	}
	o.redirectAfterCallback(c, sd, true)
}

// redirectAfterCallback 统一 callback 完成后的 302 跳转。
//
// 做两件事:
//  1. **纵深防御**:对从 StateStore 取出的 sd.ReturnTo 二次校验,即便 Redis 被
//     污染攻击者也无法构造 open redirect。authorize 阶段已校验过,这里是兜底。
//  2. **失败信号**:failed=true 时在 URL 拼 ?oidc_error=1,前端轮询拿到 "0" 时
//     可结合 query 提示用户重试,而不是傻等 ThirdAuthcode 1 分钟超时。
func (o *OIDC) redirectAfterCallback(c *wkhttp.Context, sd *StateData, failed bool) {
	target, err := ValidateReturnTo(sd.ReturnTo, o.cfg.Provider.ReturnToHosts)
	if err != nil {
		o.Warn("callback return_to 二次校验失败,回退根路径", zap.Error(err))
		target = ""
	}
	target = fallbackReturnTo(target)
	if failed {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target = target + sep + "oidc_error=1"
	}
	// 与 redirectToBindPage 同语义:防止 callback URL(IdP 回填的 code/state +
	// 我们注入的 oidc_error 标记)在跳到 return_to 那一跳经 Referer 泄漏。code
	// 是单次消费的,但 state 与时间窗内的 code 组合对反查仍有价值。无论成功还
	// 是失败 callback 都走这条路径,所以统一加上。
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, target)
}

func errMsg(msg string) map[string]string { return map[string]string{"msg": msg} }

// isDuplicateKeyError 判断 MySQL error 1062 (duplicate entry)。
// 只有 unique-key 冲突才走 recoverFromIdentityRace,其他 DB 错误(网络超时、
// 磁盘满等)应当 fail fast,避免误建 ghost user。
func isDuplicateKeyError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

// redisAuthcode 生产路径下的 ThirdAuthcode 写入实现。
type redisAuthcode struct{ ctx *config.Context }

// SetAuthcode 走 dmwork-lib 共享 Redis 连接,该 wrapper 不支持 context 取消。
// 用 goroutine + select 给 SetAndExpire 套硬超时,避免 Redis 网络阻塞拖死整条 callback。
//
// 泄漏预算:done channel 是 buffered(1),goroutine 写入后必退出,不会永久阻塞。
// 前提:dmwork-lib GetRedisConn() 底层有 socket ReadTimeout/WriteTimeout(通常由
// go-redis Options 或连接池配置),否则 Redis 网络分区时 goroutine 会持续存活
// 直到 TCP keepalive 超时。在 dmwork 的默认部署中 redis.Options 由 main.go 显式
// 设了 ReadTimeout=3s,所以此处 goroutine 寿命上限 = 3s + 网络 RTT。
func (r redisAuthcode) SetAuthcode(ctx context.Context, authcode, payload string, ttl time.Duration) error {
	timeout := 3 * time.Second
	done := make(chan error, 1)
	go func() {
		done <- r.ctx.GetRedisConn().SetAndExpire(ThirdAuthcodeRedisPrefix+authcode, payload, ttl)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("oidc: SetAuthcode timeout after %s", timeout)
	}
}

// ctxKiller 生产路径下的 sessionKiller 实现 —— 委托给 dmwork-lib 的
// QuitUserDevice(uid, -1):内部统一删 token Redis + 用新 token 重签 IM,
// WuKongIM 老连接的 transport 验证失败后自然断开,达成"踢全部设备"。
type ctxKiller struct{ ctx *config.Context }

func (k ctxKiller) Kick(_ context.Context, uid string) error {
	return k.ctx.QuitUserDevice(uid, -1)
}

func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 1 {
		return email
	}
	return email[:1] + "***" + email[at:]
}

// hasIdentityVerificationScope 判断配置的 scopes 是否包含 identity_verification。
//
// 用于决定"ID Token 里的实名字段不完整时要不要再跑一趟 /userinfo 兜底" ——
// 只有明确配置了 identity_verification 的部署才值得多这一跳 HTTP;否则(老部署
// /不跑实名的 IdP)保持原有"email/phone/name 缺才 fetch"的最小干预语义。
func hasIdentityVerificationScope(scopes []string) bool {
	for _, s := range scopes {
		if s == "identity_verification" {
			return true
		}
	}
	return false
}

// hasCompleteVerificationClaims 判断 ID Token 里的 identity_verification claims
// 是否已经齐备到可以直接走 upsert。四个必需字段都就位才算 "完整":
//
//   - IsVerified=true:Aegis 明确标记该 sub 已实名
//   - VerifiedAt > 0  :实名时间戳有效(UpsertVerificationFromOIDC 会拒 0)
//   - VerifiedProvider:allowlist 校验源
//   - LegalName       :实名姓名非空(upsert 的真正写入字段)
//
// 任一缺失则认为 ID Token 里的实名信息不可靠,需要 /userinfo 合并。LegalEmail
// 允许空,不在完整性判断内。
func hasCompleteVerificationClaims(c *IDTokenClaims) bool {
	if c == nil {
		return false
	}
	return c.IsVerified.Bool() && c.VerifiedAt > 0 && c.VerifiedProvider != "" && c.LegalName != ""
}
