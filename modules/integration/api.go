package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/oidc"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	pkgspace "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const (
	defaultIntegrationIPRateLimitRPS   = 2.0
	defaultIntegrationIPRateLimitBurst = 60
	integrationRateLimitPoolSize       = 10
)

var (
	integrationRateRedisOnce   sync.Once
	integrationRateRedisClient *rd.Client
)

type Integration struct {
	ctx    *config.Context
	db     *integrationDB
	oidcDB *oidc.DB
	// provider 按 provider kind 构造(见 oidc.NewAuthProvider)。
	//
	// 原本这里存的是 *oidc.Client,并且 New() 无条件做 Discovery —— 于是切到
	// 没有 Discovery 的 plain-OAuth2 kind 之后,本模块两个对外端点整体返回 500。
	// 改存抽象之后,"客户端出示的这个 Bearer 该怎么解释"由 provider 自己决定,
	// 本模块保持 kind 无关。
	provider oidc.AuthProvider

	// bearerJWT 业务后端自签 HS256 JWT 的验签器;未配置密钥时为 nil,
	// 表示这条凭据路径未启用(合法部署形态)。
	//
	// 桌面客户端手上只有这种凭据 —— 它既没有上游 access_token 也没有 id_token,
	// 却需要用本模块的两个端点。所以同一个 Authorization 头上要接受两类凭据。
	bearerJWT     *oidc.BearerJWTVerifier
	apiKeyService botfather.UserAPIKeyService
	groupService  group.IService
	rateRedis     *rd.Client
	log.Log
}

func New(ctx *config.Context) *Integration {
	it := &Integration{
		ctx:           ctx,
		db:            newIntegrationDB(ctx),
		oidcDB:        oidc.NewDB(ctx),
		apiKeyService: botfather.NewUserAPIKeyService(ctx),
		groupService:  group.NewService(ctx),
		rateRedis:     sharedIntegrationRateRedis(ctx.GetConfig()),
		Log:           log.NewTLog("Integration"),
	}
	cfg, err := oidc.LoadConfig()
	if err != nil {
		it.Error("加载 OIDC integration 配置失败", zap.Error(err))
		return it
	}
	if !cfg.Enabled {
		return it
	}
	// 分派与 modules/oidc 共用同一份实现:标准 OIDC 走 Discovery,plain-OAuth2
	// 直接从配置拼端点(不发网络请求)。在这里另抄一份 switch 就又造出一份
	// 会漂移的副本。
	cctx, cancel := context.WithTimeout(context.Background(), cfg.Provider.HTTPTimeout)
	defer cancel()
	res, err := oidc.NewAuthProvider(cctx, cfg.Provider, func(msg string, werr error) {
		it.Warn(msg, zap.Error(werr))
	})
	if err != nil {
		// fail-closed:构造失败意味着无法确立任何身份,两个端点必须继续拒绝请求。
		it.Error("初始化 OIDC integration provider 失败", zap.Error(err))
		return it
	}
	it.provider = res.Provider

	// 业务 JWT 验签器与 modules/oidc 共用同一份装配(密钥读取、强度校验、
	// issuer 命名空间派生)。在这里重写就又是一份会漂移的副本,而这里漂移的
	// 后果是命名空间不一致 —— 同一个人在两条路径下被认成两个账号,
	// 且 (issuer, subject) 落库后不可逆。
	bv, bverr := oidc.NewBearerJWTVerifier(cfg.Provider)
	if bverr != nil {
		// 配置错误(密钥太短 / issuer 派生失败)不能静默降级成"这条路径不可用":
		// 那样桌面端只会看到一个笼统的 401,真实原因只在启动日志里一闪而过。
		it.Error("初始化业务 JWT 验签器失败,桌面端凭据将不可用", zap.Error(bverr))
	} else if bv != nil {
		it.bearerJWT = bv
		it.Info("integration 接受业务 JWT 凭据",
			zap.String("issuer", bv.Issuer()), zap.Int("secret_bytes", bv.SecretLen()))
	}
	return it
}

func sharedIntegrationRateRedis(cfg *config.Config) *rd.Client {
	integrationRateRedisOnce.Do(func() {
		integrationRateRedisClient = octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = integrationRateLimitPoolSize
		})
	})
	return integrationRateRedisClient
}

func (it *Integration) Route(r *wkhttp.WKHttp) {
	ipLimit := r.StrictIPRateLimitMiddleware(
		context.Background(),
		it.rateRedis,
		"integration_oidc",
		wkhttp.ParseRPSFromEnv("DM_INTEGRATION_IP_RATELIMIT_RPS", defaultIntegrationIPRateLimitRPS),
		wkhttp.ParseBurstFromEnv("DM_INTEGRATION_IP_RATELIMIT_BURST", defaultIntegrationIPRateLimitBurst),
	)
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, it.ctx)
	base := r.Group("/v1/integrations/oidc", it.forceEnglish(), ipLimit)
	base.GET("/spaces", it.oidcAuth(), uidLimit, it.listSpaces)
	base.POST("/exchange", it.oidcAuth(), uidLimit, it.exchange)
	base.DELETE("/binding", it.userAPIKeyAuth(), uidLimit, it.deleteBinding)
	base.POST("/groups", it.userAPIKeyAuth(), uidLimit, it.createGroup)
	base.GET("/groups/:group_no", it.userAPIKeyAuth(), uidLimit, it.groupExists)

	manager := r.Group("/v1/manager", it.ctx.AuthMiddleware(r), uidLimit)
	manager.PUT("/integrations/oidc/client", it.upsertManagerClient)
}

func (it *Integration) forceEnglish() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		c.Request = c.Request.WithContext(i18n.WithLanguage(c.Request.Context(), i18n.LanguageDecision{
			Language: i18n.SourceLanguage,
			Source:   i18n.LanguageSourceTrustedHeader,
		}))
		c.Next()
	}
}

func (it *Integration) oidcAuth() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if it.provider == nil {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		raw := extractBearer(c)
		if raw == "" {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenMissing, nil, nil)
			c.Abort()
			return
		}
		// 两类凭据共用这一个 Authorization 头:桌面端持业务后端自签的 HS256 JWT,
		// 其余客户端持上游凭据(标准 OIDC 下是 id_token,plain-OAuth2 下是不透明
		// access_token)。
		//
		// **区分方式是验签,不是形态。** 一张 token 要么带着我方密钥下的合法
		// HMAC,要么没有 —— 这是确定性检验。按形态猜是不可接受的:不透明
		// access_token 完全可能恰好是 JWT 形态(上游内部用什么编码是它的事),
		// 猜错会把一个未经我方验证的载荷当成身份来源。
		//
		// 两个方向都不会误判:上游的不透明 token 不可能带出我方密钥下的合法签名;
		// 上游的 id_token 是 RS256,而 VerifyHS256JWT 把 alg 钉死 HS256 并显式
		// 拒绝 RS256,不走算法混淆那条路。
		//
		// 顺序:先本地验签(不外呼、无副作用),失败再问上游。反过来会让桌面端
		// 每个请求都白打一次 /userinfo。
		var claims *oidc.IdentityClaims
		var err error
		credential := "upstream"
		if it.bearerJWT != nil {
			if bc, bcErr := it.bearerJWT.Verify(raw, time.Now()); bcErr == nil {
				claims, credential = bc, "bearer_jwt"
			}
		}
		if claims == nil {
			claims, err = it.provider.IdentityFromClientCredential(c.Request.Context(), raw)
		}
		if claims == nil || err != nil {
			// 两条路径都没能确立身份 —— 对客户端一律回同一个错误(反枚举):
			// 不让调用方从响应里区分"凭据类型不对" vs "凭据本身无效"。
			it.Warn("OIDC integration token verify failed",
				zap.String("provider_kind", string(it.provider.Kind())),
				zap.Bool("bearer_jwt_enabled", it.bearerJWT != nil),
				zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}

		if strings.TrimSpace(claims.Subject) == "" {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		enabled, err := it.db.isClientEnabled(defaultClientID)
		if err != nil {
			it.Error("查询 integration client 失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		if !enabled {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationDisabled, nil, nil)
			c.Abort()
			return
		}
		if err := botfather.ValidateUserAPIKeySecret(); err != nil {
			it.Error("integration request blocked by invalid user api key secret", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}

		identity, err := it.oidcDB.QueryIdentityByIssuerSubject(claims.Issuer, claims.Subject)
		if err != nil {
			it.Error("查询 OIDC identity 失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		if identity == nil || identity.UID == "" {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationUserNotLinked, nil, nil)
			c.Abort()
			return
		}
		activeUser, err := it.db.isActiveUser(identity.UID)
		if err != nil {
			it.Error("查询 integration 本地用户状态失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		if !activeUser {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationUserNotLinked, nil, nil)
			c.Abort()
			return
		}

		c.Set("uid", identity.UID)
		// credential 进日志维度:排查"桌面端能进但 web 端不能"这类问题时,
		// 需要知道这次请求是哪条凭据路径确立的身份。
		it.Info("OIDC integration authenticated",
			zap.String("credential", credential), zap.String("issuer", claims.Issuer))
		c.Set("integration_principal", &oidcPrincipal{
			UID:     identity.UID,
			Subject: claims.Subject,
			Issuer:  claims.Issuer,
		})
		c.Next()
	}
}

func (it *Integration) userAPIKeyAuth() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := extractBearer(c)
		if token == "" || !strings.HasPrefix(token, botfather.UserAPIKeyPrefix) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		key, err := it.apiKeyService.AuthByKey(token)
		if err != nil {
			it.Error("integration binding 查询 uk_ 失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			c.Abort()
			return
		}
		if key == nil {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		if key.ClientID != defaultClientID {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		c.Set("uid", key.UID)
		c.Set("integration_api_key", key)
		c.Next()
	}
}

func (it *Integration) upsertManagerClient(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		return
	}

	var req managerIntegrationClientReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "body"})
		return
	}
	if req.Status == nil || (*req.Status != 0 && *req.Status != 1) {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "status"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultClientName
	}
	if len(name) > 100 {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "name"})
		return
	}
	if *req.Status == 1 {
		if err := botfather.ValidateUserAPIKeySecret(); err != nil {
			it.Error("integration client enable blocked by invalid user api key secret", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
	}
	if err := it.db.upsertClient(defaultClientID, name, *req.Status); err != nil {
		it.Error("写入 integration client 失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	c.Response(managerIntegrationClientResp{
		ClientID: defaultClientID,
		Name:     name,
		Status:   *req.Status,
		Enabled:  *req.Status == 1,
	})
}

func (it *Integration) listSpaces(c *wkhttp.Context) {
	principal, ok := getPrincipal(c)
	if !ok {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	spaces, err := it.db.querySpaces(principal.UID)
	if err != nil {
		it.Error("查询 integration spaces 失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	c.Response(spacesResp{
		UID:      principal.UID,
		ClientID: defaultClientID,
		Spaces:   spaces,
	})
}

func (it *Integration) exchange(c *wkhttp.Context) {
	principal, ok := getPrincipal(c)
	if !ok {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	var req exchangeReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "body"})
		return
	}
	req.SpaceID = strings.TrimSpace(req.SpaceID)
	if req.SpaceID == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "space_id"})
		return
	}

	spaceName, err := it.db.queryActiveSpaceName(req.SpaceID)
	if err != nil {
		it.Error("查询 exchange Space 失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if spaceName == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}

	member, err := pkgspace.CheckMembership(it.ctx.DB(), req.SpaceID, principal.UID)
	if err != nil {
		it.Error("校验 exchange Space 成员失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if !member {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		return
	}

	apiKey, err := it.apiKeyService.GetOrCreateForEnabledIntegrationClient(principal.UID, req.SpaceID, defaultClientID)
	if err != nil {
		if errors.Is(err, botfather.ErrIntegrationClientDisabled) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationDisabled, nil, nil)
			return
		}
		it.Error("签发 integration uk_ 失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	resp := exchangeResp{
		UID:       principal.UID,
		SpaceID:   req.SpaceID,
		SpaceName: spaceName,
		ClientID:  defaultClientID,
		APIKey:    apiKey,
	}
	if req.IncludeBots {
		bots, err := it.db.queryBots(principal.UID, req.SpaceID)
		if err != nil {
			it.Error("查询 integration bots 失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
		resp.Bots = bots
	}
	c.Response(resp)
}

func (it *Integration) deleteBinding(c *wkhttp.Context) {
	key, ok := getUserAPIKey(c)
	if !ok {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if err := it.db.revokeUserAPIKey(key.ID); err != nil {
		it.Error("撤销 integration uk_ 失败", zap.Int64("keyID", key.ID), zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	c.Response(gin.H{"revoked": true})
}

func extractBearer(c *wkhttp.Context) string {
	auth := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.Fields(auth)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func getPrincipal(c *wkhttp.Context) (*oidcPrincipal, bool) {
	v, ok := c.Get("integration_principal")
	if !ok {
		return nil, false
	}
	p, ok := v.(*oidcPrincipal)
	return p, ok && p != nil
}

func getUserAPIKey(c *wkhttp.Context) (*botfather.UserAPIKey, bool) {
	v, ok := c.Get("integration_api_key")
	if !ok {
		return nil, false
	}
	key, ok := v.(*botfather.UserAPIKey)
	return key, ok && key != nil
}
