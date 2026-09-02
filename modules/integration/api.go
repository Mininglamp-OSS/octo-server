package integration

import (
	"context"
	"errors"
	"strings"
	"sync"

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
	provider      oidc.AuthProvider
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
		// 由 provider 解释这个凭据,不由本模块猜。
		//
		// **不能按 token 长得像不像 JWT 来分流**:plain-OAuth2 的 access_token 是
		// 不透明串,完全可能恰好是 JWT 形态(上游内部用什么编码是它的事),
		// 按形态猜会把一个未经我方验证的载荷当成身份来源。
		claims, err := it.provider.IdentityFromClientCredential(c.Request.Context(), raw)
		if err != nil {
			it.Warn("OIDC integration token verify failed",
				zap.String("provider_kind", string(it.provider.Kind())), zap.Error(err))
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
