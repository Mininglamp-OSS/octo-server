// Cross-service bot endpoints introduced by the fleet split (PR-A.2).
//
// Both endpoints sit in bot_provision rather than botfather because they are
// the new contract surface between octo-server and octo-fleet/daemon —
// keeping them here makes the eventual deprecation of botfather's older
// runtime/bot APIs cleaner.
//
//   POST /v1/bot/mint        — web-callable, session-auth, mints a bot
//                              OBO and returns {bot_uid}. bot_token
//                              stays in server's robot table.
//   GET  /v1/bot/:uid/token  — daemon-callable, api_key Bearer (uk_ prefix),
//                              returns {bot_token}. Authz: caller's api_key
//                              uid must equal the bot's creator_uid AND the
//                              bot must be a member of the api_key's bound
//                              space. (Pre-v2 file-top doc described a JWT
//                              path that no longer exists — JWT teardown
//                              landed in 决策一+二 Phase 4.)
package bot_provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
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

func (a *BotProvision) mintBot(c *wkhttp.Context) {
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
// Endpoint: GET /v1/bot/:uid/token — bot owner uses api_key Bearer to
// mint a bot session token. Gates (in order):
//   - Authorization: Bearer uk_<key> (api_key path; resolveAPIKey
//     returns callerUID + callerSpace from the verified key context).
//   - bot row exists with status=1 (admin-disabled bots are invisible
//     to this path; mirrors /v1/auth/verify-bot).
//   - bot.creator_uid == callerUID (only the bot's creator can mint).
//   - bot is a member of callerSpace via space_member (cross-space
//     filter; prevents an api_key bound to SpaceB from minting a
//     bot_token for a bot whose membership only exists in SpaceA).
func (a *BotProvision) botToken(c *wkhttp.Context) {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		c.ResponseErrorWithStatus(errors.New("missing Bearer token"), http.StatusUnauthorized)
		return
	}
	apiKey := strings.TrimPrefix(auth, "Bearer ")
	callerUID, callerSpace, err := a.resolveAPIKey(apiKey)
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
	// status=1 filter: disabled bots (status=0) must not leak their token —
	// admin disable is the kill switch and shouldn't be bypassable via the
	// daemon path. Aligns with the sibling /v1/auth/verify-bot which also
	// filters status=1.
	//
	// v2 cross-space filter (reviewer server#290 P2): bot must be a member
	// of the caller api_key's bound space. Without the space join, an
	// api_key bound to SpaceB whose owner is also the bot's creator in
	// SpaceA could pull SpaceA's bot_token, bypassing the user-space
	// trust boundary that 决策二 established. bot is itself a user, its
	// space membership lives in space_member; mirrors the join pattern in
	// botfather/db.go:71.
	_, err = a.ctx.DB().SelectBySql(
		`SELECT r.bot_token, r.creator_uid FROM robot r
		 INNER JOIN space_member sm ON sm.uid=r.robot_id AND sm.space_id=? AND sm.status=1
		 WHERE r.robot_id=? AND r.status=1`,
		callerSpace, botUID,
	).Load(&r)
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
	// generateBotToken. Inlined here to keep bot_provision self-contained
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
//
// Also registers 410 Gone stubs for removed legacy auth endpoints so old
// callers get a deterministic deprecation signal instead of a 404 from an
// unregistered path. The previous JWT/JWKS pair lived under these paths
// and was dropped in 决策一+二 Phase 4; the stubs document the change for
// any straggler client that hasn't been updated.
func (a *BotProvision) Route(r *wkhttp.WKHttp) {
	authGroup := r.Group("/v1", a.ctx.AuthMiddleware(r))
	authGroup.POST("/bot/mint", a.mintBot)

	// /v1/bot/:uid/token validates api_key inline (no session middleware
	// here — caller is daemon, not browser).
	//
	// Rate-limit (v3 §2.1): mounted with the same 1000 req/min/IP "verify"
	// bucket as /v1/auth/verify-*. Reasoning: this endpoint *returns* a
	// live bot_token on the happy path, so it's strictly more sensitive
	// than the verify-* siblings (which only confirm a credential).
	// Sharing the "verify" tag keeps a single IP keyspace for all
	// credential-touching paths. Network-level ACL (nginx internal-IP
	// allowlist / X-Internal-Key) is the documented primary control;
	// the limiter is defense-in-depth.
	rlCtx := context.Background()
	rlRedis := rd.NewClient(octoredis.MustBuildOptions(a.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	}))
	verifyLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "verify", 1000.0/60, 100)
	r.GET("/v1/bot/:uid/token", verifyLimit, a.botToken)

	// Removed legacy auth endpoints (决策一+二 Phase 4). 410 Gone so a
	// straggler client distinguishes "endpoint was removed by design" from
	// "wrong URL / typo". Each stub returns a stable JSON body pointing at
	// the replacement path so client owners can fix in place.
	//
	// v3.3.2 (Jerry-Xin R4 nit, caster local review): Sunset/Deprecation
	// headers dropped. v3.3.1 hardcoded both dates wrong — Sunset said
	// "Fri, 13 Jun 2026" but that day is actually Saturday; Deprecation
	// said "@1749427200" which resolves to 2025-06-09, off by a year.
	// Fixing the dates would just invite the next off-by-one when push
	// slips. There's no deploy-pipeline substitution mechanism in this
	// repo to keep them current, and zero current consumers depend on
	// either header. The 410 status + structured JSON body remain — that
	// is the actionable signal for both humans and automated clients.
	// If we ever want Sunset back, deploy-time substitution must land
	// first (separate PR).
	r.GET("/.well-known/jwks.json", gone410Handler(
		"JWKS endpoint removed — fleet/matter no longer verify JWTs locally. "+
			"Use POST /v1/auth/verify (session) or /v1/auth/verify-api-key (daemon) instead.",
	))
	r.POST("/v1/auth/token", gone410Handler(
		"Token exchange endpoint removed — daemon no longer exchanges api_key for a JWT. "+
			"Send api_key directly as Authorization: Bearer to fleet/matter; they will "+
			"call /v1/auth/verify-api-key for validation.",
	))
}

// gone410Handler returns a wkhttp handler that always responds with HTTP 410
// Gone + a structured JSON body describing why the endpoint was removed and
// where to migrate. Sunset/Deprecation headers were considered (RFC 8594 /
// draft-ietf-httpapi-deprecation) but dropped in v3.3.2 — see Route()
// comment above for the reasoning.
func gone410Handler(reason string) func(*wkhttp.Context) {
	return func(c *wkhttp.Context) {
		c.AbortWithStatusJSON(http.StatusGone, gin.H{
			"error":   "gone",
			"message": reason,
		})
	}
}
