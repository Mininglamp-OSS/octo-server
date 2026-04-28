package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	stateTTL         = 10 * time.Minute
	thirdAuthcodeTTL = 1 * time.Minute
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

	cctx, cancel := context.WithTimeout(context.Background(), cfg.Aegis.HTTPTimeout)
	defer cancel()
	client, cerr := NewClient(cctx, ClientConfig{
		Issuer:       cfg.Aegis.Issuer,
		ClientID:     cfg.Aegis.ClientID,
		ClientSecret: cfg.Aegis.ClientSecret,
		RedirectURI:  cfg.Aegis.RedirectURI,
		Scopes:       cfg.Aegis.Scopes,
		HTTPTimeout:  cfg.Aegis.HTTPTimeout,
		ClockSkew:    cfg.Aegis.ClockSkew,
	})
	if cerr != nil {
		o.Error("OIDC Discovery 失败,handlers 将返回 500", zap.Error(cerr))
		_ = o.Close()
		o.stateStore = nil
		return o
	}
	o.client = client
	return o
}

// Init 在所有模块初始化完成后调用(register.Module.Start),
// 此时 user 模块的 IService 已通过 register.GetService 可用。
func (o *OIDC) Init() error {
	if o.cfg == nil || !o.cfg.Enabled {
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
	o.service = newService(o.cfg.Aegis, o.store, newUserAdapter(userSvc, o.db))
	return nil
}

// Route 路由注册。Enabled=false 时所有端点返回 404,避免漏配置静默通过。
func (o *OIDC) Route(r *wkhttp.WKHttp) {
	g := r.Group("/v1/auth/oidc/aegis")
	if o.cfg == nil || !o.cfg.Enabled {
		g.GET("/authorize", o.disabled)
		g.GET("/callback", o.disabled)
		g.POST("/logout", o.disabled)
		return
	}
	g.GET("/authorize", o.authorize)
	g.GET("/callback", o.callback)
	g.POST("/logout", o.logout)
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
	if o.client == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("oidc client not initialized"))
		return
	}
	authcode := c.Query("authcode")
	if !authcodeRe.MatchString(authcode) {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("authcode invalid"))
		return
	}
	cleanReturnTo, err := ValidateReturnTo(c.Query("return_to"), o.cfg.Aegis.ReturnToHosts)
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
		Provider:       "aegis",
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
	c.Redirect(http.StatusFound, authURL)
}

// callback 验证 state → 换 token → 验签 → ResolveOrLink → IssueSession →
// 写 ThirdAuthcode Redis(前端短码轮询)→ 跳回 return_to。
//
// 任何步骤失败都把"0"写到 Redis key,前端按 GitHub 模式拿到 "0" 即视为登录失败,
// 与 api_github.go:161 保持一致,前端无需新代码。
func (o *OIDC) callback(c *wkhttp.Context) {
	if o.client == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, errMsg("oidc client not initialized"))
		return
	}
	state := c.Query("state")
	if state == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("state required"))
		return
	}
	statePreview := state
	if len(state) > 8 {
		statePreview = state[:8] + "..."
	}
	o.Debug("OIDC callback 开始", zap.String("state", statePreview))

	sd, err := o.stateStore.Consume(c.Request.Context(), state)
	if err != nil {
		o.Debug("OIDC callback state 无效", zap.Error(err))
		o.Warn("OIDC state 校验失败", zap.Error(err))
		c.AbortWithStatusJSON(http.StatusBadRequest, errMsg("state invalid"))
		return
	}
	authcodePreview := sd.ClientAuthcode
	if len(authcodePreview) > 8 {
		authcodePreview = authcodePreview[:8] + "..."
	}
	o.Debug("OIDC callback state 消费成功", zap.String("authcode", authcodePreview))

	// IdP 自身报错(用户拒绝授权 / 配置错误)
	if oerr := c.Query("error"); oerr != "" {
		o.Debug("OIDC callback IdP 报错", zap.String("error", oerr))
		o.failWithAuthcode(c.Request.Context(), sd, nil, fmt.Errorf("idp error: %s", oerr))
		o.redirectAfterCallback(c, sd, true)
		return
	}

	code := c.Query("code")
	if code == "" {
		o.Debug("OIDC callback 缺 code")
		o.failWithAuthcode(c.Request.Context(), sd, nil, errors.New("missing code"))
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.Debug("OIDC callback 开始 Exchange")

	tok, err := o.client.Exchange(c.Request.Context(), code, sd.CodeVerifier)
	if err != nil {
		o.Debug("OIDC callback Exchange 失败", zap.Error(err))
		o.failWithAuthcode(c.Request.Context(), sd, nil, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.Debug("OIDC callback Exchange 成功")

	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		o.Debug("OIDC callback id_token 缺失")
		o.failWithAuthcode(c.Request.Context(), sd, nil, errors.New("id_token missing from token response"))
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.Debug("OIDC callback 开始 VerifyIDToken")

	claims, err := o.client.VerifyIDToken(c.Request.Context(), rawID)
	if err != nil {
		o.Debug("OIDC callback VerifyIDToken 失败", zap.Error(err))
		o.failWithAuthcode(c.Request.Context(), sd, nil, err)
		o.redirectAfterCallback(c, sd, true)
		return
	}
	// TODO(an9xyz): remove after OIDC callback issue is resolved
	o.Debug("OIDC callback VerifyIDToken 成功",
		zap.String("sub", claims.Subject),
		zap.String("email", maskEmail(claims.Email)),
		zap.Bool("email_verified", claims.EmailVerified))

	if claims.Nonce != sd.Nonce {
		o.Debug("OIDC callback nonce 不匹配",
			zap.String("claims_nonce", claims.Nonce),
			zap.String("state_nonce", sd.Nonce))
		o.failWithAuthcode(c.Request.Context(), sd, claims, errors.New("nonce mismatch"))
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.Debug("OIDC callback nonce 匹配,开始 ResolveOrLink")

	res, err := o.service.ResolveOrLink(c.Request.Context(), claims)
	if err != nil {
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
	}
	sessResp, err := o.service.IssueSession(c.Request.Context(), issueReq)
	if err != nil {
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
					// 竞态恢复失败:writeAudit 已在 recover 内部记录,这里只补 ThirdAuthcode "0"
					if e := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
						o.Error("写 ThirdAuthcode 失败(race-recover fail path)", zap.Error(e))
					}
					o.redirectAfterCallback(c, sd, true)
					return
				}
				sessResp = recovered
			} else {
				o.Error("写 identity 绑定失败(非竞态)", zap.Error(err))
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

	if err := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, sessResp.LoginRespJSON, thirdAuthcodeTTL); err != nil {
		// 写 LoginRespJSON 失败,前端轮询永远拿不到 token,会傻等 1 分钟超时。
		// 立刻补 "0" 让前端尽早感知,并在 redirect URL 拼 ?oidc_error=1。
		o.Error("写 ThirdAuthcode 失败", zap.Error(err))
		if e := o.authcode.SetAuthcode(c.Request.Context(), sd.ClientAuthcode, "0", thirdAuthcodeTTL); e != nil {
			o.Error("回写 ThirdAuthcode \"0\" 也失败,前端将等到 TTL 超时", zap.Error(e))
		}
		o.writeAudit(sessResp.UID, EventCallbackFail, sd, "set authcode failed: "+err.Error())
		o.redirectAfterCallback(c, sd, true)
		return
	}
	o.writeAudit(sessResp.UID, EventCallbackOK, sd, "")
	o.redirectAfterCallback(c, sd, false)
}

// Close 释放 OIDC 模块持有的资源(目前只有 redisStateStore 的独立连接池)。
//
// 注册到 register.Module.Stop,框架优雅退出时调用。可被多次调用(幂等):
//   - New() 内 Discovery 失败会调一次清理 stateStore
//   - 之后 framework shutdown 又会调一次,此时 stateStore 已 nil,早返回
func (o *OIDC) Close() error {
	if o.stateStore == nil {
		return nil
	}
	if rss, ok := o.stateStore.(*redisStateStore); ok {
		return rss.Close()
	}
	return nil
}

// logout 撤销本地登录态。
//
// P1.2 范围:仅返回 200,IdP 端登出由前端按需调 IdP 的 /end_session。
// 后续 P1.3 SyncWorker 上线后,这里会同步把对应 RT 标记吊销。
func (o *OIDC) logout(c *wkhttp.Context) {
	c.JSON(http.StatusOK, map[string]interface{}{"status": 200})
}

func (o *OIDC) failWithAuthcode(ctx context.Context, sd *StateData, claims *IDTokenClaims, err error) {
	o.Warn("OIDC callback 失败", zap.Error(err))
	uid := ""
	if claims != nil {
		uid = "sub:" + claims.Subject
		if len(uid) > maxAuditUID {
			uid = uid[:maxAuditUID]
		}
	}
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
		zap.String("sub", claims.Subject))
	o.writeAudit(original.UID, EventCallbackFail, sd,
		"identity race ghost="+original.UID+" winner="+existing.UID)
	return winnerSess
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

// redirectAfterCallback 统一 callback 完成后的 302 跳转。
//
// 做两件事:
//  1. **纵深防御**:对从 StateStore 取出的 sd.ReturnTo 二次校验,即便 Redis 被
//     污染攻击者也无法构造 open redirect。authorize 阶段已校验过,这里是兜底。
//  2. **失败信号**:failed=true 时在 URL 拼 ?oidc_error=1,前端轮询拿到 "0" 时
//     可结合 query 提示用户重试,而不是傻等 ThirdAuthcode 1 分钟超时。
func (o *OIDC) redirectAfterCallback(c *wkhttp.Context, sd *StateData, failed bool) {
	target, err := ValidateReturnTo(sd.ReturnTo, o.cfg.Aegis.ReturnToHosts)
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

func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 1 {
		return email
	}
	return email[:1] + "***" + email[at:]
}
