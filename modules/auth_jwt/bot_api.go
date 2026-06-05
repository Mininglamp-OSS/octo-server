// Cross-service bot endpoints introduced by the fleet split (PR-A.2).
//
// Both endpoints sit in auth_jwt rather than botfather because they are
// the new contract surface between octo-server and octo-fleet/daemon —
// keeping them here makes the eventual deprecation of botfather's older
// runtime/bot APIs cleaner.
//
//   POST /v1/bot/mint        — web-callable, session-auth, mints a bot
//                              OBO and returns {bot_uid}. bot_token
//                              stays in server's robot table.
//   GET  /v1/bot/:uid/token  — daemon-callable, JWT-auth (scope=daemon),
//                              returns {bot_token}. Authz check: the
//                              daemon's JWT.sub must equal the bot's
//                              creator_uid (owner-on-behalf-of model).
package auth_jwt

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"go.uber.org/zap"
)

// ---------- POST /v1/bot/mint (web → server) ----------

type mintRequest struct {
	DisplayName string `json:"display_name"`
	SpaceID     string `json:"space_id"`
	// BotToken — optional. If empty, server generates a random one.
	// Callers may supply their own so the token namespace stays caller-side.
	BotToken string `json:"bot_token"`
}

type mintResponse struct {
	BotUID string `json:"bot_uid"`
}

func (a *AuthJWT) mintBot(c *wkhttp.Context) {
	// Caller is browser; reuse octo-lib session middleware semantics:
	// AuthMiddleware would have set "uid"; if not present we 401.
	uid := c.GetLoginUID()
	if uid == "" {
		c.ResponseErrorWithStatus(errors.New("login required"), http.StatusUnauthorized)
		return
	}
	var req mintRequest
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(fmt.Errorf("invalid body: %w", err))
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		c.ResponseError(errors.New("display_name required"))
		return
	}
	if strings.TrimSpace(req.SpaceID) == "" {
		c.ResponseError(errors.New("space_id required"))
		return
	}
	// PR-D.1 #2: caller must actually be in the target space before
	// MintBotOBO drops a bot into space_member there. Previously this
	// was unchecked — any logged-in user could mint a bot in any space
	// they knew the id of and then use that bot to observe groups.
	if err := a.assertSpaceMember(uid, req.SpaceID); err != nil {
		c.ResponseErrorWithStatus(err, http.StatusForbidden)
		return
	}
	botToken := req.BotToken
	if botToken == "" {
		var err error
		botToken, err = generateBfToken()
		if err != nil {
			c.ResponseError(fmt.Errorf("gen token: %w", err))
			return
		}
	}
	res, err := botfather.MintBotOBO(a.ctx, uid, req.SpaceID, req.DisplayName, botToken)
	if err != nil {
		a.Error("MintBotOBO failed", zap.Error(err))
		c.ResponseError(fmt.Errorf("mint: %w", err))
		return
	}
	c.JSON(http.StatusOK, mintResponse{BotUID: res.BotUID})
}

// ---------- GET /v1/bot/:uid/token (daemon → server) ----------

// botToken validates a daemon api_key (uk_ Bearer) and returns the
// bot_token iff the caller is the bot's creator.
//
// 合并 plan 决策一+二 Phase 4: 改用 api_key 直连 (不再 JWT exchange).
// SQL 逻辑跟 /v1/auth/verify-api-key (modules/user/api.go) 同款 — 复用
// auth_jwt.resolveAPIKey 现有的两步走 (api_key → uid+space_id → membership).
func (a *AuthJWT) botToken(c *wkhttp.Context) {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		c.ResponseErrorWithStatus(errors.New("missing Bearer token"), http.StatusUnauthorized)
		return
	}
	apiKey := strings.TrimPrefix(auth, "Bearer ")
	callerUID, _, _, err := a.resolveAPIKey(apiKey, "", "")
	if err != nil {
		c.ResponseErrorWithStatus(errors.New("invalid api_key"), http.StatusUnauthorized)
		return
	}
	botUID := c.Param("uid")
	if botUID == "" {
		c.ResponseError(errors.New("uid required"))
		return
	}
	type row struct {
		BotToken   string `db:"bot_token"`
		CreatorUID string `db:"creator_uid"`
	}
	var r row
	_, err = a.ctx.DB().Select("bot_token", "creator_uid").From("robot").
		Where("robot_id=?", botUID).Load(&r)
	if err != nil {
		a.Error("query robot for token", zap.Error(err), zap.String("bot_uid", botUID))
		c.ResponseError(errors.New("lookup failed"))
		return
	}
	if r.BotToken == "" {
		c.ResponseErrorWithStatus(errors.New("bot not found"), http.StatusNotFound)
		return
	}
	if r.CreatorUID != callerUID {
		// Caller's api_key uid must equal the bot's creator. Anything
		// else means a daemon for one user is asking for another user's
		// bot — clean 403, no info leak about whether the bot exists.
		c.ResponseErrorWithStatus(errors.New("not authorized for this bot"), http.StatusForbidden)
		return
	}
	c.JSON(http.StatusOK, map[string]string{
		"bot_uid":   botUID,
		"bot_token": r.BotToken,
	})
}

// generateBfToken produces a `bf_<32hex>` token matching IM /newbot style.
func generateBfToken() (string, error) {
	// Reuse the same RNG-derived hex format as runtime/bot.go's
	// generateBotToken. Inlined here to keep auth_jwt self-contained
	// and avoid importing the soon-to-be-deprecated runtime module.
	b := make([]byte, 16)
	if _, err := readRand(b); err != nil {
		return "", err
	}
	return "bf_" + hexEncode(b), nil
}

// Tiny indirection so we can unit-test without crypto/rand dep here.
var readRand = defaultReadRand
var hexEncode = defaultHexEncode

// Route mounts the two bot endpoints. JWT exchange + JWKS endpoints have
// been removed (合并 plan 决策一+二 Phase 4) — daemon/web now hit
// fleet/matter directly with api_key/session tokens.
//
//	POST /v1/bot/mint        — web session auth (octo-lib session middleware)
//	GET  /v1/bot/:uid/token  — daemon api_key Bearer (validated inline)
func (a *AuthJWT) Route(r *wkhttp.WKHttp) {
	authGroup := r.Group("/v1", a.ctx.AuthMiddleware(r))
	authGroup.POST("/bot/mint", a.mintBot)

	// /v1/bot/:uid/token validates api_key inline (no session middleware
	// here — caller is daemon, not browser).
	r.GET("/v1/bot/:uid/token", a.botToken)
}