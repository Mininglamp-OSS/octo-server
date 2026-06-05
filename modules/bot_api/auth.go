package bot_api

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// BotKind identifies the type of authenticated bot.
const (
	BotKindUser = "user" // User Bot (bf_ token, robot table)
	BotKindApp  = "app"  // App Bot (app_ token, app_bot table)
)

// Context keys for bot identity.
const (
	CtxKeyRobotID       = "robot_id"
	CtxKeyBotKind       = "bot_kind"
	CtxKeyRobot         = "robot"         // *robotModel for User Bot
	CtxKeyAppBotScope   = "app_bot_scope" // "platform" | "space"
	CtxKeyAppBotSpaceID = "app_bot_space_id"
)

// authBot returns the unified Bot API authentication middleware.
// Routes by token prefix: bf_ → robot table, app_ → app_bot table.
func (ba *BotAPI) authBot() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := extractBotToken(c)
		if token == "" {
			respondBotAPIAuthFailed(c)
			return
		}

		if strings.HasPrefix(token, "app_") {
			// App Bot authentication
			ba.authAppBot(c, token)
		} else {
			// User Bot authentication (bf_ prefix or legacy tokens)
			ba.authUserBot(c, token)
		}
	}
}

// authUserBot authenticates a User Bot via robot table lookup.
func (ba *BotAPI) authUserBot(c *wkhttp.Context, token string) {
	robot, err := ba.db.queryRobotByBotToken(token)
	if err != nil {
		ba.Error("查询机器人失败", zap.Error(err))
		respondBotAPIAuthCheckFailed(c)
		return
	}
	if robot == nil {
		respondBotAPIAuthFailed(c)
		return
	}

	c.Set(CtxKeyRobotID, robot.RobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, robot)
	c.Next()
}

// authAppBot authenticates an App Bot.
// Primary path: O(1) in-memory Registry lookup (design doc §4.1).
// Fallback: DB query (covers startup race where registry not yet loaded).
func (ba *BotAPI) authAppBot(c *wkhttp.Context, token string) {
	// Try in-memory Registry first (O(1))
	if spec := ba.lookupAppBotRegistry(token); spec != nil {
		c.Set(CtxKeyRobotID, spec.UID)
		c.Set(CtxKeyBotKind, BotKindApp)
		c.Set(CtxKeyAppBotScope, spec.Scope)
		if spec.Scope == "space" {
			c.Set(CtxKeyAppBotSpaceID, spec.SpaceID)
		}
		c.Next()
		return
	}

	// Fallback to DB
	appBot, err := ba.db.queryAppBotByToken(token)
	if err != nil {
		ba.Error("查询App Bot失败", zap.Error(err))
		respondBotAPIAuthCheckFailed(c)
		return
	}
	if appBot == nil {
		respondBotAPIAuthFailed(c)
		return
	}

	// App Bot must be published (status=1) to serve API requests
	if appBot.Status != 1 {
		respondBotAPIBotUnavailable(c)
		return
	}

	c.Set(CtxKeyRobotID, appBot.UID)
	c.Set(CtxKeyBotKind, BotKindApp)
	c.Set(CtxKeyAppBotScope, appBot.Scope)
	if appBot.Scope == "space" {
		c.Set(CtxKeyAppBotSpaceID, appBot.SpaceID)
	}
	c.Next()
}

// lookupAppBotRegistry queries the global App Bot Registry (O(1) memory lookup).
// Returns nil if registry not initialized or token not found.
func (ba *BotAPI) lookupAppBotRegistry(token string) *AppBotRegistrySpec {
	reg := GetAppBotRegistry()
	if reg == nil {
		return nil
	}
	return reg.FindByToken(token)
}

// extractBotToken extracts the Bearer token from the Authorization header.
func extractBotToken(c *wkhttp.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// getRobotIDFromContext extracts the robot_id from gin context.
func getRobotIDFromContext(c *wkhttp.Context) string {
	v, _ := c.Get(CtxKeyRobotID)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// getRobotFromContext extracts the *robotModel from gin context (User Bot only).
func getRobotFromContext(c *wkhttp.Context) *robotModel {
	v, exists := c.Get(CtxKeyRobot)
	if !exists {
		return nil
	}
	rm, ok := v.(*robotModel)
	if !ok {
		return nil
	}
	return rm
}

// getBotKindFromContext returns the bot_kind from gin context.
func getBotKindFromContext(c *wkhttp.Context) string {
	v, _ := c.Get(CtxKeyBotKind)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
