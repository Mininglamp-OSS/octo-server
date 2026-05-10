package app_bot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// AppBotTokenPrefix is the token prefix for App Bots.
	AppBotTokenPrefix = "app_"
	// AppBotUIDPrefix is the UID prefix for App Bots.
	AppBotUIDPrefix = "app_"
	// AppBotUIDSuffix is the UID suffix for App Bots.
	AppBotUIDSuffix = "_bot"
)

// Status values for App Bot.
const (
	StatusDraft       = 0
	StatusPublished   = 1
	StatusUnpublished = 2
)

const (
	// spaceRoleAdmin is the minimum role value for space admin/owner.
	// 0=member, 1=admin, 2=owner (consistent with space module semantics).
	spaceRoleAdmin = 1
)

// Reserved IDs that cannot be used as App Bot IDs.
var reservedIDs = map[string]bool{
	"system":       true,
	"filehelper":   true,
	"botfather":    true,
	"notification": true,
}

// idPattern validates App Bot ID format.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,29}$`)

// Not exported — external access goes through SetAppBotResolver closure.

// AppBot is the App Bot management module.
type AppBot struct {
	ctx         *config.Context
	db          *appBotDB
	registry    *Registry
	userService user.IService
	log.Log
}

// NewAppBot creates the App Bot module.
func NewAppBot(ctx *config.Context) *AppBot {
	ab := &AppBot{
		ctx:         ctx,
		db:          newAppBotDB(ctx),
		registry:    NewRegistry(),
		userService: user.NewService(ctx),
		Log:         log.NewTLog("AppBot"),
	}

	// Register App Bot identity resolver in user module (breaks circular import)
	user.SetAppBotResolver(func(uid string) string {
		spec := ab.registry.FindByUID(uid)
		if spec == nil {
			return ""
		}
		return spec.DisplayName
	})

	// Eagerly initialize auth registry so operations never encounter nil.
	// loadRegistryFromDB will populate this same adapter from DB.
	authRegistry := bot_api.NewAppBotRegistryAdapter()
	bot_api.SetAppBotRegistry(authRegistry)

	// Populate registry from DB in background
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ab.Error("loadRegistryFromDB panic", zap.Any("recover", r))
			}
		}()
		ab.loadRegistryFromDB(authRegistry)
	}()

	return ab
}

// Route registers all App Bot management routes.
func (ab *AppBot) Route(r *wkhttp.WKHttp) {
	// Platform Admin API (requires login, super admin check in handlers)
	adminAPI := r.Group("/v1/admin/app_bot", ab.ctx.AuthMiddleware(r))
	{
		adminAPI.POST("", ab.createPlatformBot)
		adminAPI.GET("", ab.listPlatformBots)
		adminAPI.GET("/:id", ab.getBotDetail)
		adminAPI.PUT("/:id", ab.updateBot)
		adminAPI.DELETE("/:id", ab.deleteBot)
		adminAPI.POST("/:id/token", ab.rotateToken)
		adminAPI.POST("/:id/token/reveal", ab.revealToken)
		adminAPI.POST("/:id/publish", ab.publishBot)
		adminAPI.POST("/:id/unpublish", ab.unpublishBot)
	}

	// Space Admin API (requires login, space admin check in handlers)
	spaceAPI := r.Group("/v1/space/:space_id/app_bot", ab.ctx.AuthMiddleware(r))
	{
		spaceAPI.POST("", ab.createSpaceBot)
		spaceAPI.GET("", ab.listSpaceBots)
		spaceAPI.GET("/:id", ab.getBotDetail)
		spaceAPI.PUT("/:id", ab.updateBot)
		spaceAPI.DELETE("/:id", ab.deleteBot)
		spaceAPI.POST("/:id/token", ab.rotateToken)
		spaceAPI.POST("/:id/token/reveal", ab.revealToken)
		spaceAPI.POST("/:id/publish", ab.publishBot)
		spaceAPI.POST("/:id/unpublish", ab.unpublishBot)
	}

	// User discovery API (authenticated user)
	r.GET("/v1/app_bot/available", ab.ctx.AuthMiddleware(r), ab.discoverBots)

	// User opt-in: establish friend relationship with App Bot
	r.POST("/v1/app_bot/apply", ab.ctx.AuthMiddleware(r), ab.applyBot)
}

// checkSpaceAdmin verifies the logged-in user is admin/owner of the given space.
func (ab *AppBot) checkSpaceAdmin(c *wkhttp.Context, spaceID string) error {
	loginUID := c.GetLoginUID()
	var member struct {
		Role int `db:"role"`
	}
	count, err := ab.ctx.DB().SelectBySql(
		"SELECT role FROM space_member WHERE space_id=? AND uid=? AND status=1 LIMIT 1", spaceID, loginUID,
	).Load(&member)
	if err != nil || count == 0 || member.Role < spaceRoleAdmin {
		return errors.New("no permission: requires space admin")
	}
	return nil
}

// ==================== Registry ====================

// Registry is an in-memory store for published App Bots.
type Registry struct {
	mu    sync.RWMutex
	byUID map[string]*AppBotSpec
	byID  map[string]*AppBotSpec
}

// AppBotSpec is the in-memory representation of a published App Bot.
type AppBotSpec struct {
	ID          string
	UID         string
	DisplayName string
	Description string
	Avatar      string
	Scope       string
	SpaceID     string
	Token       string
	CreatedBy   string
}

// NewRegistry creates a new Registry.
func NewRegistry() *Registry {
	return &Registry{
		byUID: make(map[string]*AppBotSpec),
		byID:  make(map[string]*AppBotSpec),
	}
}

// FindByUID looks up an App Bot by UID.
func (r *Registry) FindByUID(uid string) *AppBotSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byUID[uid]
}

// FindByID looks up an App Bot by ID.
func (r *Registry) FindByID(id string) *AppBotSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// Add adds or updates an App Bot in the registry.
func (r *Registry) Add(spec *AppBotSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byUID[spec.UID] = spec
	r.byID[spec.ID] = spec
}

// Update atomically replaces an App Bot spec in the registry.
func (r *Registry) Update(spec *AppBotSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byUID[spec.UID] = spec
	r.byID[spec.ID] = spec
}

// Remove removes an App Bot from the registry.
func (r *Registry) Remove(id, uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byUID, uid)
	delete(r.byID, id)
}

// loadRegistryFromDB loads all published App Bots into memory.
func (ab *AppBot) loadRegistryFromDB(authRegistry *bot_api.AppBotRegistryAdapter) {
	var bots []*appBotModel
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		bots, err = ab.db.queryPublishedBots()
		if err == nil {
			break
		}
		ab.Error("load app_bot registry failed, retrying",
			zap.Error(err), zap.Int("attempt", attempt))
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		ab.Error("load app_bot registry failed after 3 attempts, giving up", zap.Error(err))
		return
	}

	for _, bot := range bots {
		ab.registry.Add(&AppBotSpec{
			ID:          bot.ID,
			UID:         bot.UID,
			DisplayName: bot.DisplayName,
			Description: bot.Description,
			Avatar:      ab.ctx.GetConfig().GetAvatarPath(bot.UID),
			Scope:       bot.Scope,
			SpaceID:     bot.SpaceID,
			Token:       bot.Token,
			CreatedBy:   bot.CreatedBy,
		})
		authRegistry.Add(bot.Token, &bot_api.AppBotRegistrySpec{
			UID:     bot.UID,
			Scope:   bot.Scope,
			SpaceID: bot.SpaceID,
		})

		// Ensure user record exists (repair for bots created before avatar fix)
		_ = ab.userService.AddUser(&user.AddUserReq{
			UID:      bot.UID,
			Username: bot.UID,
			Name:     bot.DisplayName,
			ShortNo:  bot.UID,
			Phone:    "",
			Zone:     "",
			Robot:    1,
		})
	}

	ab.Info("App Bot registry loaded", zap.Int("count", len(bots)))
}

// ==================== Admin API Handlers ====================

// createPlatformBot handles POST /v1/admin/app_bot.
func (ab *AppBot) createPlatformBot(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	ab.createBot(c, "platform", "")
}

// createSpaceBot handles POST /v1/space/:space_id/app_bot.
func (ab *AppBot) createSpaceBot(c *wkhttp.Context) {
	spaceID := c.Param("space_id")
	if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
		return
	}
	ab.createBot(c, "space", spaceID)
}

func (ab *AppBot) createBot(c *wkhttp.Context, scope, spaceID string) {
	var req struct {
		ID          string `json:"id" binding:"required"`
		DisplayName string `json:"display_name" binding:"required"`
		Description string `json:"description"`
		Avatar      string `json:"avatar"`
		WelcomeMsg  string `json:"welcome_msg"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	// Validate ID format
	if !idPattern.MatchString(req.ID) {
		c.ResponseError(errors.New("id must match ^[a-z0-9][a-z0-9_-]{0,29}$"))
		return
	}
	if reservedIDs[req.ID] {
		c.ResponseError(errors.New("id is reserved"))
		return
	}

	uid := fmt.Sprintf("%s%s%s", AppBotUIDPrefix, req.ID, AppBotUIDSuffix)

	// Note: cross-table uniqueness is enforced inside insertAppBot's transaction.
	// No outer pre-check needed — the transactional recheck is the authoritative safety net.

	// Generate token
	token, err := generateAppBotToken()
	if err != nil {
		ab.Error("generate token failed", zap.Error(err))
		c.ResponseError(errors.New("generate token failed"))
		return
	}

	loginUID := c.GetLoginUID()
	bot := &appBotModel{
		ID:          req.ID,
		UID:         uid,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Avatar:      req.Avatar,
		Scope:       scope,
		SpaceID:     spaceID,
		Status:      StatusDraft,
		Token:       token,
		WelcomeMsg:  req.WelcomeMsg,
		CreatedBy:   loginUID,
	}

	if err := ab.db.insertAppBot(bot); err != nil {
		if errors.Is(err, ErrIDAlreadyInUse) {
			c.ResponseError(errors.New("id already in use"))
			return
		}
		ab.Error("insert app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("create app bot failed"))
		return
	}

	// Register IM token for Bot
	resp, tokenErr := ab.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         uid,
		Token:       token,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		// Rollback DB
		ab.db.deleteAppBot(req.ID)
		ab.Error("register IM token failed", zap.Any("error", tokenErr), zap.String("uid", uid))
		c.ResponseError(errors.New("register IM token failed"))
		return
	}

	// Create user record so SDK can resolve avatar/name for message rows.
	// This is a hard dependency — without it, avatar and permission checks fail (404).
	if err := ab.userService.AddUser(&user.AddUserReq{
		UID:      uid,
		Username: uid,
		Name:     req.DisplayName,
		ShortNo:  uid,
		Phone:    "",
		Zone:     "",
		Robot:    1,
	}); err != nil {
		// Rollback: remove app_bot record and invalidate IM token
		ab.db.deleteAppBot(req.ID)
		revokeToken, tokenErr := generateAppBotToken()
		if tokenErr != nil {
			revokeToken = fmt.Sprintf("REVOKED-%s-%d", uid, time.Now().UnixNano())
		}
		ab.ctx.UpdateIMToken(config.UpdateIMTokenReq{
			UID:         uid,
			Token:       revokeToken,
			DeviceFlag:  config.APP,
			DeviceLevel: config.DeviceLevelMaster,
		})
		ab.Error("create user record for app bot failed, rolled back", zap.Error(err), zap.String("uid", uid))
		c.ResponseError(errors.New("create app bot failed: user record creation failed"))
		return
	}

	c.Response(gin.H{
		"id":    req.ID,
		"uid":   uid,
		"token": token,
	})
}

// listPlatformBots handles GET /v1/admin/app_bot.
func (ab *AppBot) listPlatformBots(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		c.ResponseError(err)
		return
	}
	pageIndex, pageSize := c.GetPage()
	keyword := c.Query("keyword")
	statusStr := c.Query("status")
	var statusFilter *int
	if statusStr != "" {
		s, err := strconv.Atoi(statusStr)
		if err == nil {
			statusFilter = &s
		}
	}
	bots, total, err := ab.db.queryBotsByScope("platform", "", pageIndex, pageSize, keyword, statusFilter)
	if err != nil {
		ab.Error("query platform bots failed", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	c.Response(gin.H{"count": total, "list": ab.toBotListResp(bots)})
}

// listSpaceBots handles GET /v1/space/:space_id/app_bot.
func (ab *AppBot) listSpaceBots(c *wkhttp.Context) {
	spaceID := c.Param("space_id")
	if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
		return
	}
	pageIndex, pageSize := c.GetPage()
	keyword := c.Query("keyword")
	statusStr := c.Query("status")
	var statusFilter *int
	if statusStr != "" {
		s, err := strconv.Atoi(statusStr)
		if err == nil {
			statusFilter = &s
		}
	}
	bots, total, err := ab.db.queryBotsByScope("space", spaceID, pageIndex, pageSize, keyword, statusFilter)
	if err != nil {
		ab.Error("query space bots failed", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	c.Response(gin.H{"count": total, "list": ab.toBotListResp(bots)})
}

// getBotDetail handles GET /v1/admin/app_bot/:id and GET /v1/space/:space_id/app_bot/:id.
func (ab *AppBot) getBotDetail(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	tokenDisplay := ""
	if len(bot.Token) > 4 {
		tokenDisplay = "****" + bot.Token[len(bot.Token)-4:]
	} else {
		tokenDisplay = "****"
	}

	c.Response(gin.H{
		"id":           bot.ID,
		"uid":          bot.UID,
		"display_name": bot.DisplayName,
		"description":  bot.Description,
		"avatar":       ab.ctx.GetConfig().GetAvatarPath(bot.UID),
		"welcome_msg":  bot.WelcomeMsg,
		"scope":        bot.Scope,
		"space_id":     bot.SpaceID,
		"status":       bot.Status,
		"token":        tokenDisplay,
		"created_by":   bot.CreatedBy,
		"created_at":   bot.CreatedAt,
		"updated_at":   bot.UpdatedAt,
	})
}

// updateBot handles PUT /v1/admin/app_bot/:id and PUT /v1/space/:space_id/app_bot/:id.
func (ab *AppBot) updateBot(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	var req struct {
		DisplayName *string `json:"display_name"`
		Description *string `json:"description"`
		Avatar      *string `json:"avatar"`
		WelcomeMsg  *string `json:"welcome_msg"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	if spaceID != "" {
		existing, qerr := ab.db.queryBotByID(id)
		if qerr != nil || existing == nil {
			c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
			return
		}
		if existing.Scope != "space" || existing.SpaceID != spaceID {
			c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
			return
		}
	}

	updates := make(map[string]interface{})
	if req.DisplayName != nil {
		updates["display_name"] = *req.DisplayName
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Avatar != nil {
		updates["avatar"] = *req.Avatar
	}
	if req.WelcomeMsg != nil {
		updates["welcome_msg"] = *req.WelcomeMsg
	}
	if len(updates) == 0 {
		c.ResponseError(errors.New("nothing to update"))
		return
	}

	if err := ab.db.updateAppBot(id, updates); err != nil {
		ab.Error("update app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("update failed"))
		return
	}

	// Update registry if published
	bot, _ := ab.db.queryBotByID(id)
	if bot != nil && bot.Status == StatusPublished {
		ab.registry.Add(&AppBotSpec{
			ID:          bot.ID,
			UID:         bot.UID,
			DisplayName: bot.DisplayName,
			Description: bot.Description,
			Avatar:      ab.ctx.GetConfig().GetAvatarPath(bot.UID),
			Scope:       bot.Scope,
			SpaceID:     bot.SpaceID,
			Token:       bot.Token,
			CreatedBy:   bot.CreatedBy,
		})
		ab.syncAuthRegistry(bot.Token, bot.UID, bot.Scope, bot.SpaceID)
	}

	// Sync display_name to user table (for SDK avatar/name resolution)
	if req.DisplayName != nil && bot != nil {
		name := *req.DisplayName
		if err := ab.userService.UpdateUser(user.UserUpdateReq{UID: bot.UID, Name: &name}); err != nil {
			ab.Warn("sync display_name to user table failed", zap.Error(err), zap.String("uid", bot.UID))
		}
	}

	c.ResponseOK()
}

// deleteBot handles DELETE /v1/admin/app_bot/:id and DELETE /v1/space/:space_id/app_bot/:id.
func (ab *AppBot) deleteBot(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	// Delete from DB first; only after success remove from registries
	if err := ab.db.deleteAppBot(id); err != nil {
		ab.Error("delete app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("delete failed"))
		return
	}

	// Note: user record is intentionally preserved after bot deletion to maintain
	// referential integrity with message history (from_uid), conversation records, etc.
	// The bot is effectively dead: IM token invalidated, registry cleared, app_bot record deleted.

	// Remove from both registries
	ab.registry.Remove(bot.ID, bot.UID)
	ab.removeAuthRegistry(bot.Token)

	// Invalidate IM token on delete — all bots get IM token at creation time,
	// so we must always rotate to random to revoke access regardless of status.
	randomToken, err := generateAppBotToken()
	if err != nil {
		ab.Error("generateAppBotToken failed during bot deletion, using revocation fallback", zap.Error(err))
		randomToken = fmt.Sprintf("REVOKED-%s-%d", bot.UID, time.Now().UnixNano())
	}
	ab.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         bot.UID,
		Token:       randomToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})

	c.ResponseOK()
}

// rotateToken handles POST /v1/admin/app_bot/:id/token and POST /v1/space/:space_id/app_bot/:id/token.
func (ab *AppBot) rotateToken(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	newToken, err := generateAppBotToken()
	if err != nil {
		ab.Error("generate token failed", zap.Error(err))
		c.ResponseError(errors.New("generate token failed"))
		return
	}

	// Update DB with optimistic lock (WHERE token=oldToken prevents TOCTOU race)
	if err := ab.db.rotateAppBotToken(id, bot.Token, newToken); err != nil {
		if errors.Is(err, ErrTokenRotationConflict) {
			c.ResponseError(errors.New("token was rotated by another request, please retry"))
			return
		}
		ab.Error("update token failed", zap.Error(err))
		c.ResponseError(errors.New("update token failed"))
		return
	}

	// Update IM token
	resp, tokenErr := ab.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         bot.UID,
		Token:       newToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		// Rollback
		if rbErr := ab.db.updateAppBot(id, map[string]interface{}{"token": bot.Token}); rbErr != nil {
			ab.Error("rotateToken rollback failed — DB and IM tokens may be inconsistent",
				zap.String("bot_id", id), zap.Error(rbErr))
		}
		ab.Error("register new IM token failed", zap.Any("error", tokenErr))
		c.ResponseError(errors.New("register IM token failed"))
		return
	}

	// Update both registries atomically (single lock acquisition per registry)
	// NOTE: Updates to local registry and auth registry use separate locks.
	// Brief inconsistency window (microseconds to low milliseconds under GC) is acceptable:
	// - During this window, requests with the OLD token may get a brief auth failure
	//   (old token removed from auth registry, DB already has new token → fallback returns nil).
	// - Requests with the NEW token succeed immediately via auth registry or DB fallback.
	// - This is acceptable: token rotation is admin-initiated, brief disruption expected.
	// - This avoids a coordination lock that would couple two independent modules.
	if bot.Status == StatusPublished {
		ab.registry.Update(&AppBotSpec{
			ID:          bot.ID,
			UID:         bot.UID,
			DisplayName: bot.DisplayName,
			Description: bot.Description,
			Avatar:      ab.ctx.GetConfig().GetAvatarPath(bot.UID),
			Scope:       bot.Scope,
			SpaceID:     bot.SpaceID,
			Token:       newToken,
			CreatedBy:   bot.CreatedBy,
		})
		ab.updateAuthRegistry(bot.Token, newToken, bot.UID, bot.Scope, bot.SpaceID)
	}

	c.Response(gin.H{"token": newToken})
}

// revealToken handles POST /v1/admin/app_bot/:id/token/reveal and POST /v1/space/:space_id/app_bot/:id/token/reveal.
func (ab *AppBot) revealToken(c *wkhttp.Context) {
	spaceID := c.Param("space_id")
	id := c.Param("id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	ab.Info("token revealed",
		zap.String("bot_id", id),
		zap.String("operator", c.GetLoginUID()),
		zap.String("scope", bot.Scope),
		zap.String("space_id", spaceID),
	)

	c.Response(gin.H{"token": bot.Token})
}

// publishBot handles POST /v1/admin/app_bot/:id/publish and POST /v1/space/:space_id/app_bot/:id/publish.
func (ab *AppBot) publishBot(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}
	if bot.Status == StatusPublished {
		c.ResponseOK()
		return
	}

	if err := ab.db.updateAppBot(id, map[string]interface{}{"status": StatusPublished}); err != nil {
		ab.Error("publish app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("publish failed"))
		return
	}

	// Add to both registries
	ab.registry.Add(&AppBotSpec{
		ID:          bot.ID,
		UID:         bot.UID,
		DisplayName: bot.DisplayName,
		Description: bot.Description,
		Avatar:      ab.ctx.GetConfig().GetAvatarPath(bot.UID),
		Scope:       bot.Scope,
		SpaceID:     bot.SpaceID,
		Token:       bot.Token,
		CreatedBy:   bot.CreatedBy,
	})
	ab.syncAuthRegistry(bot.Token, bot.UID, bot.Scope, bot.SpaceID)

	c.ResponseOK()
}

// unpublishBot handles POST /v1/admin/app_bot/:id/unpublish and POST /v1/space/:space_id/app_bot/:id/unpublish.
func (ab *AppBot) unpublishBot(c *wkhttp.Context) {
	id := c.Param("id")
	spaceID := c.Param("space_id")

	if spaceID != "" {
		if err := ab.checkSpaceAdmin(c, spaceID); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": err.Error()})
			return
		}
	} else {
		if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
			c.ResponseError(err)
			return
		}
	}

	bot, err := ab.db.queryBotByID(id)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}

	if spaceID != "" && (bot.Scope != "space" || bot.SpaceID != spaceID) {
		c.JSON(http.StatusNotFound, gin.H{"msg": "bot not found"})
		return
	}
	if bot.Status == StatusUnpublished {
		c.ResponseOK()
		return
	}

	if err := ab.db.updateAppBot(id, map[string]interface{}{"status": StatusUnpublished}); err != nil {
		ab.Error("unpublish app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("unpublish failed"))
		return
	}

	// Remove from both registries
	ab.registry.Remove(bot.ID, bot.UID)
	ab.removeAuthRegistry(bot.Token)

	c.ResponseOK()
}

// syncAuthRegistry adds an app bot to the bot_api auth registry.
func (ab *AppBot) syncAuthRegistry(token, uid, scope, spaceID string) {
	if r, ok := bot_api.GetAppBotRegistry().(*bot_api.AppBotRegistryAdapter); ok && r != nil {
		r.Add(token, &bot_api.AppBotRegistrySpec{
			UID:     uid,
			Scope:   scope,
			SpaceID: spaceID,
		})
	}
}

// removeAuthRegistry removes an app bot from the bot_api auth registry.
func (ab *AppBot) removeAuthRegistry(token string) {
	if r, ok := bot_api.GetAppBotRegistry().(*bot_api.AppBotRegistryAdapter); ok && r != nil {
		r.Remove(token)
	}
}

// updateAuthRegistry atomically swaps a spec from oldToken to newToken in the bot_api auth registry.
func (ab *AppBot) updateAuthRegistry(oldToken, newToken, uid, scope, spaceID string) {
	if r, ok := bot_api.GetAppBotRegistry().(*bot_api.AppBotRegistryAdapter); ok && r != nil {
		r.Update(oldToken, newToken, &bot_api.AppBotRegistrySpec{
			UID:     uid,
			Scope:   scope,
			SpaceID: spaceID,
		})
	}
}

// ==================== Discovery API ====================

// discoverBots handles GET /v1/app_bot/available.
func (ab *AppBot) discoverBots(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceIDFilter := c.Query("space_id")

	bots, err := ab.db.queryAvailableBots(loginUID, spaceIDFilter)
	if err != nil {
		ab.Error("query available bots failed", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}

	result := make([]gin.H, 0, len(bots))
	for _, bot := range bots {
		result = append(result, gin.H{
			"id":           bot.ID,
			"uid":          bot.UID,
			"display_name": bot.DisplayName,
			"description":  bot.Description,
			"avatar":       ab.ctx.GetConfig().GetAvatarPath(bot.UID),
			"scope":        bot.Scope,
		})
	}
	c.JSON(http.StatusOK, result)
}

// ==================== Helpers ====================

func (ab *AppBot) toBotListResp(bots []*appBotModel) []gin.H {
	result := make([]gin.H, 0, len(bots))
	for _, bot := range bots {
		result = append(result, gin.H{
			"id":           bot.ID,
			"uid":          bot.UID,
			"display_name": bot.DisplayName,
			"avatar":       ab.ctx.GetConfig().GetAvatarPath(bot.UID),
			"status":       bot.Status,
			"scope":        bot.Scope,
			"created_at":   bot.CreatedAt,
		})
	}
	return result
}

// applyBot handles POST /v1/app_bot/apply — user opt-in to establish friend relationship with an App Bot.
func (ab *AppBot) applyBot(c *wkhttp.Context) {
	var req struct {
		RobotUID string `json:"robot_uid" binding:"required"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	loginUID := c.GetLoginUID()

	// Validate robot_uid format: must match app_*_bot pattern
	if !strings.HasPrefix(req.RobotUID, AppBotUIDPrefix) || !strings.HasSuffix(req.RobotUID, AppBotUIDSuffix) {
		c.ResponseError(errors.New("invalid robot_uid format"))
		return
	}

	// Rate limit: 10 apply requests per minute per user
	rateLimitKey := fmt.Sprintf("app_bot_apply_rate:%s", loginUID)
	count, redisErr := ab.ctx.GetRedisConn().Incr(rateLimitKey)
	if redisErr != nil {
		ab.Error("rate limit Redis error, denying request", zap.Error(redisErr))
		c.ResponseError(errors.New("服务繁忙，请稍后再试"))
		return
	}
	if count == 1 {
		// First request in this window — set TTL (fixed window, not sliding).
		// If SetExpire fails, DEL the key to prevent permanent rate-limiting.
		if expErr := ab.ctx.GetRedisConn().SetExpire(rateLimitKey, time.Minute); expErr != nil {
			ab.Error("rate limit SetExpire failed, deleting key to prevent permanent block",
				zap.Error(expErr), zap.String("key", rateLimitKey))
			ab.ctx.GetRedisConn().Del(rateLimitKey)
		}
	}
	if count > 10 {
		c.ResponseError(errors.New("请求过于频繁，请稍后再试"))
		return
	}

	// Query App Bot
	bot, err := ab.db.queryBotByUID(req.RobotUID)
	if err != nil {
		ab.Error("query app_bot failed", zap.Error(err))
		c.ResponseError(errors.New("\u67e5\u8be2\u673a\u5668\u4eba\u5931\u8d25"))
		return
	}
	if bot == nil || bot.Status != StatusPublished {
		c.ResponseError(errors.New("\u673a\u5668\u4eba\u4e0d\u5b58\u5728\u6216\u672a\u53d1\u5e03"))
		return
	}

	// Space bot: verify user is space member (fail-closed if SpaceID is unexpectedly empty)
	if bot.Scope == "space" {
		if bot.SpaceID == "" {
			c.ResponseError(errors.New("internal error: space bot missing space_id"))
			return
		}
		var memberCount int
		err = ab.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
			bot.SpaceID, loginUID,
		).LoadOne(&memberCount)
		if err != nil {
			ab.Error("query space membership failed", zap.Error(err))
			c.ResponseError(errors.New("\u67e5\u8be2\u7a7a\u95f4\u6210\u5458\u5931\u8d25"))
			return
		}
		if memberCount == 0 {
			c.ResponseError(errors.New("\u4f60\u4e0d\u662f\u8be5\u7a7a\u95f4\u7684\u6210\u5458"))
			return
		}
	}

	// Idempotent: already friends → return OK
	isFriend, err := ab.userService.IsFriend(loginUID, req.RobotUID)
	if err != nil {
		ab.Error("check friend failed", zap.Error(err))
		c.ResponseError(errors.New("\u68c0\u67e5\u597d\u53cb\u5173\u7cfb\u5931\u8d25"))
		return
	}
	if isFriend {
		c.Response(gin.H{"status": "approved", "message": "\u5df2\u7ecf\u662f\u597d\u53cb\u4e86"})
		return
	}

	// Establish bidirectional friend relationship
	// 1. AddFriend both directions (rollback first if second fails)
	err = ab.userService.AddFriend(loginUID, &user.FriendReq{UID: loginUID, ToUID: req.RobotUID})
	if err != nil {
		ab.Error("add friend (user->bot) failed", zap.Error(err))
		c.ResponseError(errors.New("\u521b\u5efa\u597d\u53cb\u5173\u7cfb\u5931\u8d25"))
		return
	}
	err = ab.userService.AddFriend(req.RobotUID, &user.FriendReq{UID: req.RobotUID, ToUID: loginUID})
	if err != nil {
		ab.Error("add friend (bot->user) failed", zap.Error(err))
		// Rollback: remove the first direction to avoid half-state
		if _, rbErr := ab.ctx.DB().DeleteFrom("friend").Where("uid=? AND to_uid=?", loginUID, req.RobotUID).Exec(); rbErr != nil {
			ab.Error("friend rollback failed — one-directional friend record may remain",
				zap.String("uid", loginUID), zap.String("toUID", req.RobotUID), zap.Error(rbErr))
		}
		c.ResponseError(errors.New("\u521b\u5efa\u597d\u53cb\u5173\u7cfb\u5931\u8d25"))
		return
	}

	// 2. Fix friend version for SDK incremental sync
	ab.fixFriendVersion(loginUID, req.RobotUID)
	ab.fixFriendVersion(req.RobotUID, loginUID)

	// 3. IM Whitelist (bidirectional, with space prefix if applicable)
	userChannelID := loginUID
	botChannelID := req.RobotUID
	spaceID := space.GetCommonSpaceID(ab.ctx, loginUID, req.RobotUID)
	if spaceID != "" {
		userChannelID = fmt.Sprintf("s%s_%s", spaceID, loginUID)
		botChannelID = fmt.Sprintf("s%s_%s", spaceID, req.RobotUID)
	}
	if wlErr := ab.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   userChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{req.RobotUID},
	}); wlErr != nil {
		ab.Warn("IMWhitelistAdd (user channel) failed", zap.String("channelID", userChannelID), zap.Error(wlErr))
	}
	if wlErr := ab.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   botChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{loginUID},
	}); wlErr != nil {
		ab.Warn("IMWhitelistAdd (bot channel) failed", zap.String("channelID", botChannelID), zap.Error(wlErr))
	}

	// 4. Notify client (CMDFriendAccept)
	cmdParam := map[string]interface{}{
		"to_uid":   loginUID,
		"from_uid": req.RobotUID,
	}
	if spaceID != "" {
		cmdParam["space_id"] = spaceID
	}
	_ = ab.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{loginUID, req.RobotUID},
		Param:       cmdParam,
	})

	// 5. Send welcome message (use bot's custom welcome_msg if configured)
	// Sent as normal text message (chat bubble from bot), not Tip (gray centered system text)
	welcomeContent := "\u6211\u4eec\u5df2\u7ecf\u662f\u597d\u53cb\u4e86\uff0c\u53ef\u4ee5\u5f00\u59cb\u804a\u5929\u4e86\uff01"
	if bot.WelcomeMsg != "" {
		welcomeContent = bot.WelcomeMsg
	}
	msgPayload := map[string]interface{}{
		"content": welcomeContent,
		"type":    common.Text,
	}
	if spaceID != "" {
		msgPayload["space_id"] = spaceID
	}
	payload, _ := json.Marshal(msgPayload)
	_ = ab.ctx.SendMessage(&config.MsgSendReq{
		FromUID:     req.RobotUID,
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
		Header: config.MsgHeader{
			RedDot: 1,
		},
	})

	c.Response(gin.H{"status": "approved", "message": "\u5df2\u81ea\u52a8\u901a\u8fc7\uff0c\u53ef\u4ee5\u5f00\u59cb\u804a\u5929"})
}

// fixFriendVersion updates friend version for SDK incremental sync.
func (ab *AppBot) fixFriendVersion(uid, toUID string) {
	_, err := ab.ctx.DB().UpdateBySql(
		"UPDATE friend SET version=(SELECT v FROM (SELECT IFNULL(MAX(version),0)+1 AS v FROM friend WHERE uid=?) t) WHERE uid=? AND to_uid=? AND version=0",
		uid, uid, toUID,
	).Exec()
	if err != nil {
		ab.Warn("fix friend version failed", zap.Error(err))
	}
}

// generateAppBotToken generates a secure random App Bot token.
func generateAppBotToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return AppBotTokenPrefix + hex.EncodeToString(b), nil
}

