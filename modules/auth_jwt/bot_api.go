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
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/go-jose/go-jose/v3/jwt"
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

func (a *AuthJWT) botToken(c *wkhttp.Context) {
	// Validate JWT inline (no AuthMiddleware on this route — daemon scope)
	cl, err := a.requireDaemonJWT(c)
	if err != nil {
		c.ResponseErrorWithStatus(err, http.StatusUnauthorized)
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
	if r.CreatorUID != cl.Subject {
		// Daemon's JWT subject must equal the bot's creator. Anything
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

// requireDaemonJWT parses and validates a Bearer token from the request,
// asserting scope=daemon. Returns parsed Claims.
func (a *AuthJWT) requireDaemonJWT(c *wkhttp.Context) (*Claims, error) {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, errors.New("missing Bearer token")
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	parsed, err := jwt.ParseSigned(tok)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var cl Claims
	if err := parsed.Claims(a.pubKey, &cl); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	// PR-A fix (齐乐 review #3): signature-only verification let expired
	// tokens through. Plan AU1 requires "JWT 过期 → 401" — enforce it
	// here, otherwise a daemon JWT past 30-day TTL could still mint a
	// bot_token forever.
	//
	// Note on clock skew: Time field below is exact wall-clock; we rely
	// on go-jose's DefaultLeeway (1 min) to tolerate small daemon ↔ server
	// drift on exp/iat/nbf. If a downstream operator overrides leeway to
	// 0 (e.g. via a global config), tight-clock daemons could see
	// spurious failures right around mint time — keep the default unless
	// you have a hard reason.
	if err := cl.Validate(jwt.Expected{
		Issuer: jwtIssuer,
		Time:   time.Now(),
	}); err != nil {
		return nil, fmt.Errorf("claims invalid: %w", err)
	}
	if cl.Scope != "daemon" {
		return nil, errors.New("daemon scope required")
	}
	return &cl, nil
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