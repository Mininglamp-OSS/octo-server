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
	ctx           *config.Context
	db            *integrationDB
	oidcDB        *oidc.DB
	oidcClient    *oidc.Client
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
	client, err := oidc.NewClient(context.Background(), oidc.ClientConfig{
		Issuer:       cfg.Provider.Issuer,
		ClientID:     cfg.Provider.ClientID,
		ClientSecret: cfg.Provider.ClientSecret,
		RedirectURI:  cfg.Provider.RedirectURI,
		Scopes:       cfg.Provider.Scopes,
		ClockSkew:    cfg.Provider.ClockSkew,
		HTTPTimeout:  cfg.Provider.HTTPTimeout,
	})
	if err != nil {
		it.Error("初始化 OIDC integration client 失败", zap.Error(err))
		return it
	}
	it.oidcClient = client
	return it
}

func sharedIntegrationRateRedis(cfg *config.Config) *rd.Client {
	integrationRateRedisOnce.Do(func() {
		integrationRateRedisClient = rd.NewClient(octoredis.MustBuildOptions(cfg, func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = integrationRateLimitPoolSize
		}))
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
		if it.oidcClient == nil {
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
		claims, err := it.oidcClient.VerifyIDToken(c.Request.Context(), raw)
		if err != nil {
			it.Warn("OIDC integration token verify failed", zap.Error(err))
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
