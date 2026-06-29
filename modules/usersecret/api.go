package usersecret

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// maxAuditQueryRunes resolve 审计 query 列写入上限(按字符,非字节),防灌爆 + 不落明文 key。
// 列是 VARCHAR(128)(utf8mb4 下按字符计长),故 128 runes 必然能塞下;按 rune 边界
// 截断避免切断多字节 UTF-8 码点导致插入失败 / 存入非法 UTF-8(P1.4)。
const maxAuditQueryRunes = 128

// resolve 端点 per-IP 限流默认值。resolve 经 bf_ bot token 鉴权且锚定 owner,
// 不挂用户态 SharedUIDRateLimiter(读不到登录 uid),改用 per-IP 严格限流挡
// token 探测 + 审计表写放大(R3:每次调用都写审计行,含坏 token)。阈值给得
// 偏宽:正常 use-time resolve 量很低,但同 egress IP 下可能聚合多 bot,故
// 50 rps / burst 200 留余量,异常放大才触顶。可经环境变量覆盖。
const (
	envResolveIPRPS    = "DM_USERSECRET_RESOLVE_IP_RPS"
	envResolveIPBurst  = "DM_USERSECRET_RESOLVE_IP_BURST"
	defResolveIPRPS    = 50.0
	defResolveIPBurst  = 200
	resolveRateLimitNS = "usersecret_resolve" // Redis keyspace tag
)

// rateRedisOnce 让限流 redis client 在进程内单例化,避免每次 New() 都开新连接池
// (参考 incomingwebhook / pkg/wkhttp.SharedUIDRateLimiter 的同款约定)。
var (
	rateRedisOnce   sync.Once
	rateRedisClient *rd.Client
)

func sharedRateRedis(cfg *config.Config) *rd.Client {
	rateRedisOnce.Do(func() {
		// 经 octoredis.MustBuildOptions 构造,确保 RedisTLS 启用(托管 TLS Redis)时
		// TLSConfig 不被遗漏,否则限流 client 连不上、fail-open 静默关掉防护。
		// PoolSize 显式设 10:令牌桶 Lua 是短事务,与其它限流 client 全局约定一致。
		rateRedisClient = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = 10
		})
	})
	return rateRedisClient
}

// API 是 usersecret 模块的 HTTP 入口 + 依赖容器。
type API struct {
	ctx *config.Context
	log.Log

	store   secretStore
	svc     *service
	enc     *encryptor
	enabled bool // 主密钥就绪才挂载写接口 / resolve
}

// New 构造模块。主密钥缺失不阻断进程启动:enabled=false,路由层返回 5xx,
// 运维补齐 OCTO_USER_API_KEY_SECRET 后重启即恢复(与 oidc 漏配的降级口径一致)。
func New(ctx *config.Context) *API {
	a := &API{
		ctx:   ctx,
		Log:   log.NewTLog("UserSecret"),
		store: newStore(ctx),
	}
	enc, err := newEncryptor()
	if err != nil {
		a.Error("usersecret 主密钥未就绪,写接口/resolve 将返回内部错误", zap.Error(err))
		return a
	}
	a.enc = enc
	a.svc = newService(a.store, enc)
	a.enabled = true
	return a
}

// Route 注册路由。
//
//   - /v1/manager/secrets/*  用户态 CRUD(AuthMiddleware,owner = 当前登录用户)。
//   - /v1/bot/secrets/resolve  channel 插件 use-time 解析(bf_ bot token 鉴权)。
func (a *API) Route(r *wkhttp.WKHttp) {
	// 认证 CRUD 组在 AuthMiddleware 之后挂 SharedUIDRateLimiter:per-login-user 桶
	// (ratelimit:uid:{uid}),给创建/轮换加密 key 这类敏感写路径做按用户的频控,
	// 与 incomingwebhook / app_bot 等认证管理路由口径一致。
	mgr := r.Group("/v1/manager/secrets", a.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, a.ctx))
	{
		mgr.POST("", a.create)           // 新建
		mgr.GET("", a.list)              // 列表(脱敏)
		mgr.PUT("/:secret_id", a.update) // 换 key / 重命名
		mgr.DELETE("/:secret_id", a.delete)
	}

	// resolve:bot token 鉴权,不挂用户 AuthMiddleware。但它会返明文且每次调用
	// (含坏 token)都写一条审计行,故必须挂 per-IP 严格限流挡 token 探测 + 审计
	// 写放大(R3 blocker)。复用 StrictIPRateLimitMiddleware(与 login/sms/搜索等
	// 敏感端点同款),独立 tag 隔离 keyspace;Redis 故障时 fail-open(放行 + 告警)。
	rps := wkhttp.ParseRPSFromEnv(envResolveIPRPS, defResolveIPRPS)
	burst := wkhttp.ParseBurstFromEnv(envResolveIPBurst, defResolveIPBurst)
	resolveLimit := r.StrictIPRateLimitMiddleware(
		context.Background(), sharedRateRedis(a.ctx.GetConfig()), resolveRateLimitNS, rps, burst)
	r.POST("/v1/bot/secrets/resolve", resolveLimit, a.resolve)
}

// guardReady 写接口 / resolve 入口统一检查主密钥就绪。未就绪返回 5xx 并 return true。
func (a *API) guardReady(c *wkhttp.Context) bool {
	if a.enabled && a.svc != nil {
		return false
	}
	respondErr(c, errcode.ErrUserSecretResolveFailed)
	return true
}

// ---------------- CRUD (user token) ----------------

func (a *API) create(c *wkhttp.Context) {
	if a.guardReady(c) {
		return
	}
	owner := c.GetLoginUID()
	if owner == "" {
		respondErr(c, errcode.ErrUserSecretUnauthorized)
		return
	}
	var req struct {
		DisplayName string `json:"display_name"`
		Kind        string `json:"kind"`
		Key         string `json:"key"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
		return
	}
	view, err := a.svc.create(owner, req.DisplayName, req.Kind, req.Key)
	if err != nil {
		a.handleWriteErr(c, "create", err)
		return
	}
	c.ResponseWithStatus(http.StatusCreated, view)
}

func (a *API) list(c *wkhttp.Context) {
	if a.guardReady(c) {
		return
	}
	owner := c.GetLoginUID()
	if owner == "" {
		respondErr(c, errcode.ErrUserSecretUnauthorized)
		return
	}
	views, err := a.svc.list(owner, c.Query("kind"))
	if err != nil {
		a.Error("usersecret list 失败", zap.Error(err))
		respondErr(c, errcode.ErrUserSecretResolveFailed)
		return
	}
	c.Response(map[string]interface{}{"secrets": views})
}

// update 同时承载「换 key」与「重命名」:
//   - body 含 key      → 换 key(只更新密文,secret_id/display_name 不变)。
//   - body 含 display_name → 重命名(secret_id/密文不变)。
//   - 两者都给 → 先重命名再换 key(见下方实现注释);都不给 → 400。
func (a *API) update(c *wkhttp.Context) {
	if a.guardReady(c) {
		return
	}
	owner := c.GetLoginUID()
	if owner == "" {
		respondErr(c, errcode.ErrUserSecretUnauthorized)
		return
	}
	secretID := c.Param("secret_id")
	var req struct {
		Key         *string `json:"key"`
		DisplayName *string `json:"display_name"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
		return
	}
	if req.Key == nil && req.DisplayName == nil {
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
		return
	}
	var view secretView
	var err error
	// 先 rename 再换 key:rename 撞名是最常见的失败,放前面可在不改动密文的
	// 前提下早退,避免「key 已轮换但 rename 撞名返 409」的部分生效困惑。
	if req.DisplayName != nil {
		if view, err = a.svc.rename(owner, secretID, *req.DisplayName); err != nil {
			a.handleWriteErr(c, "rename", err)
			return
		}
	}
	if req.Key != nil {
		if view, err = a.svc.updateKey(owner, secretID, *req.Key); err != nil {
			a.handleWriteErr(c, "update_key", err)
			return
		}
	}
	c.Response(view)
}

func (a *API) delete(c *wkhttp.Context) {
	if a.guardReady(c) {
		return
	}
	owner := c.GetLoginUID()
	if owner == "" {
		respondErr(c, errcode.ErrUserSecretUnauthorized)
		return
	}
	if err := a.svc.delete(owner, c.Param("secret_id")); err != nil {
		a.handleWriteErr(c, "delete", err)
		return
	}
	c.ResponseOK()
}

// handleWriteErr 把 service 层错误映射到错误码。内部错误记 zap,不上 wire 细节。
func (a *API) handleWriteErr(c *wkhttp.Context, op string, err error) {
	switch {
	case errors.Is(err, errInvalidInput):
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
	case errors.Is(err, errDuplicateName):
		respondErr(c, errcode.ErrUserSecretDuplicateName)
	case errors.Is(err, errNotFound):
		respondErr(c, errcode.ErrUserSecretNotFound)
	default:
		a.Error("usersecret 写操作失败", zap.String("op", op), zap.Error(err))
		respondErr(c, errcode.ErrUserSecretResolveFailed)
	}
}

// ---------------- resolve (bot token) ----------------

func (a *API) resolve(c *wkhttp.Context) {
	if a.guardReady(c) {
		return
	}
	// 鉴权:bf_ bot token,绑 owner。认不出合法 bot → 401(不区分原因防枚举)。
	caller, callerErr := a.authResolveCaller(c)
	if callerErr != nil {
		a.Error("usersecret resolve 鉴权查询失败", zap.Error(callerErr))
		// 鉴权查询本身报错(DB 异常)也留痕:安全模块的鉴权链路任何失败都要审计。
		a.writeResolveAudit(c, caller, "", resolveOutcome{result: resultInternalError})
		respondErr(c, errcode.ErrUserSecretResolveFailed)
		return
	}
	if caller == nil || caller.OwnerUID == "" {
		// 鉴权失败(无/非法 bot token)必须留痕 —— 安全模块的越权探测线索。
		a.writeResolveAudit(c, caller, "", resolveOutcome{result: resultUnauthorized})
		respondErr(c, errcode.ErrUserSecretUnauthorized)
		return
	}

	var req struct {
		Query string `json:"query"` // secret_id 或 display_name
	}
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.Query) == "" {
		// 入参非法也留痕(已鉴权的 caller 发了坏请求)。标 request_invalid,
		// 别和真实 not_found 混。
		a.writeResolveAudit(c, caller, req.Query, resolveOutcome{result: resultRequestInvalid})
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
		return
	}

	outcome, err := a.svc.resolve(caller.OwnerUID, req.Query)
	a.writeResolveAudit(c, caller, req.Query, outcome)

	switch {
	case err == nil && outcome.result == resultOK:
		// 唯一命中:返明文。明文只进响应体,绝不进日志/审计。
		c.Response(map[string]interface{}{
			"secret_id": outcome.secretID,
			"value":     outcome.plaintext,
		})
	case errors.Is(err, errAmbiguous):
		// 歧义:走统一 i18n 错误信封,候选列表(脱敏)经 details.candidates 返回,
		// 让上层消歧;不返明文。保留 error.http_status(422)与本地化 message。
		respondErrWithDetails(c, errcode.ErrUserSecretAmbiguous, i18n.Details{
			"candidates": outcome.candidates,
		})
	case errors.Is(err, errNotFound):
		respondErr(c, errcode.ErrUserSecretNotFound)
	case errors.Is(err, errInvalidInput):
		respondErr(c, errcode.ErrUserSecretRequestInvalid)
	default:
		a.Error("usersecret resolve 失败", zap.String("result", outcome.result), zap.Error(err))
		respondErr(c, errcode.ErrUserSecretResolveFailed)
	}
}

// authResolveCaller 从 Authorization: Bearer <bf_token> 解析调用方身份。
// 复用 bot 凭证体系:bf_ token 已绑 creator_uid(owner),resolve 只返该 owner 的 key。
func (a *API) authResolveCaller(c *wkhttp.Context) (*botIdentity, error) {
	token := extractBearer(c)
	if token == "" || !strings.HasPrefix(token, "bf_") {
		return nil, nil
	}
	return a.store.queryBotByToken(token)
}

func extractBearer(c *wkhttp.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// writeResolveAudit best-effort 写 resolve 审计。失败仅记日志,不阻塞返回。
// 不记录明文/密文/query 全文(query 截断)。caller 为 nil(鉴权失败)时仍留痕,
// owner/caller_id 留空,result 由调用方标明(如 unauthorized)。
func (a *API) writeResolveAudit(c *wkhttp.Context, caller *botIdentity, query string, outcome resolveOutcome) {
	q := query
	// 按 rune 边界截断:VARCHAR(128) 按字符计长,CJK 在 UTF-8 占 3-4 字节,
	// 直接按字节切会切断码点 → 插入失败或存入非法 UTF-8(P1.4)。
	if r := []rune(q); len(r) > maxAuditQueryRunes {
		q = string(r[:maxAuditQueryRunes])
	}
	result := outcome.result
	if result == "" {
		result = resultNotFound
	}
	candidates := len(outcome.candidates)
	audit := &resolveAuditModel{
		CallerKind: "user_bot",
		Query:      q,
		SecretID:   outcome.secretID,
		Result:     result,
		Candidates: candidates,
		IP:         c.ClientIP(),
	}
	if caller != nil {
		audit.OwnerUID = caller.OwnerUID
		audit.CallerID = caller.RobotID
	}
	if err := a.store.insertResolveAudit(audit); err != nil {
		a.Error("usersecret resolve 审计写入失败", zap.Error(err))
	}
}
