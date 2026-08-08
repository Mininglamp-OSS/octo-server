package internal_resolve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// identityResolver is the narrow surface this module needs from
// modules/botidentity. Extracted as an interface so unit tests can inject a
// fake that returns any Kind (in particular, `KindAppBot`) without touching
// MySQL. The production implementation is *botidentity.Resolver.
//
// Why botidentity, not a hand-rolled `SELECT ... FROM robot` snapshot: the
// authoritative live-bot identity set in this repository spans TWO tables —
// `robot` (user bots) and `app_bot` (platform / space app bots). An earlier
// draft of this endpoint used a robot-only query and therefore emitted the
// contract shape "not a bot" for a published app_bot uid — see the resolver's
// package comment for why user.robot is not authoritative. modules/botidentity
// already reads both tables in one snapshot AND enforces the
// cross-table-uniqueness invariant (ErrAmbiguousIdentity), so consumers do not
// have to hand-code either concern. The same resolver is consumed by
// modules/message, modules/cardtrust and internal/carddispatch — using it
// here keeps every bot-classification site pointed at one source of truth.
type identityResolver interface {
	// Resolve returns nil when uid is not an active bot identity, matching
	// modules/botidentity.Resolver.Resolve semantics. Callers must inspect
	// (*Identity).Kind — KindUserBot vs KindAppBot — before deriving a
	// creator uid, because app-bot lifecycle intentionally has no human
	// creator (its scope is `platform` or `space`, see botidentity.Kind).
	Resolve(uid string) (*botidentity.Identity, error)
}

// Module is the module handle registered with the wkhttp router. Field names
// match the octo-server convention (see modules/bot_mention/api.go:32).
type Module struct {
	ctx           *config.Context
	identities    identityResolver
	internalToken string
	log.Log
}

// New wires the module against the repository-wide botidentity.Resolver. The
// token is loaded from the environment at construction time; if it is unset,
// too short, or collides with a sibling internal token, the module logs the
// reason and refuses all requests (fail-closed via internalAuthMiddleware).
//
// ctx is retained so Route can construct the rate-limit Redis client the
// same way modules/user/api.go:196 does — deferring that construction to
// Route matches the convention across the repository and gives us access to
// the runtime config without threading extra plumbing.
func New(ctx *config.Context) *Module {
	logger := log.NewTLog("InternalResolve")
	token, tokenErr := resolveDriveInternalToken(os.Getenv)
	if tokenErr != nil {
		logger.Error(tokenErr.Error())
	}
	return &Module{
		ctx:           ctx,
		identities:    botidentity.New(ctx),
		internalToken: token,
		Log:           logger,
	}
}

// resolveBotOwnerIPRateLimit builds the per-endpoint IP rate-limit middleware.
//
// Values are deploy-tunable via envResolveBotOwnerIPRPS / envResolveBotOwnerIPBurst
// (see config.go), sanitized against defaults so a NaN / +Inf / non-positive
// env typo cannot silently disable this security control. Defaults are 2 rps
// + burst 60 — roughly 2× the octo-drive consumer's documented worst-case
// steady-state (a few dozen resolve calls per 30 s poll tick under warm
// cache) with headroom for a cold-cache reconcile spike.
//
// One-shot construction (once per Route call) matches modules/user/api.go's
// login/register/sms shape and modules/usersecret/api.go's resolve shape.
func (m *Module) resolveBotOwnerIPRateLimit(r *wkhttp.WKHttp) wkhttp.HandlerFunc {
	rlRedis := octoredis.NewInstrumentedClient(m.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	// Sanitize the env-derived values before construction:
	// wkhttp.ParseRPSFromEnv accepts NaN and +Inf (strconv.ParseFloat lets
	// both through and the helper only rejects n <= 0), so those slip past
	// wkhttp.newKeyedLimiter's own <=0 check into the Redis Lua script,
	// where they surface as script errors that documented behaviour maps to
	// the fail-open path. A typo like `DM_RESOLVE_BOT_OWNER_IP_RPS=NaN`
	// would therefore silently disable this security control. ratelimit's
	// dedicated sanitizers exist for exactly this hazard; use them the
	// same way modules/bot_api/ratelimit.go:159-162 does.
	rps := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envResolveBotOwnerIPRPS, defResolveBotOwnerIPRPS),
		defResolveBotOwnerIPRPS,
	)
	burst := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envResolveBotOwnerIPBurst, defResolveBotOwnerIPBurst),
		defResolveBotOwnerIPBurst,
	)
	// Tag is a stable, distinct string — StrictIPRateLimitMiddleware uses it
	// as the Redis key prefix `ratelimit:strict:<tag>:`, so overlapping with
	// any existing tag ("login" / "register" / "bot_register" /
	// "usersecret_resolve" / …) would merge quotas across unrelated routes.
	return r.StrictIPRateLimitMiddleware(
		context.Background(), rlRedis, resolveBotOwnerRateLimitTag, rps, burst,
	)
}

// Route mounts internal endpoints under /v1/internal, gated by X-Internal-Token.
// Route mounts internal endpoints under /v1/internal, gated by X-Internal-Token.
// The path shape (`/v1/internal/user/resolve-bot-owner`) matches issue #710.
//
// Middleware **execution order** on this route (matters):
//  1. per-endpoint IP strict rate limit — MUST run FIRST so IP-abusive
//     traffic never reaches the token-compare (Redis Lua) or the DB path.
//     Aligns with `.octospec/rules/rate-limit.md` (load_bearing:true) and
//     with the repository convention for un-user-authed sensitive routes
//     (login / register / sms / bot_register / bot_heartbeat).
//  2. internalAuthMiddleware — X-Internal-Token constant-time compare +
//     fail-closed on unset.
//
// Both middlewares are mounted on the **concrete POST route**, NOT the
// group. Gin combines group handlers ahead of route handlers, so putting
// auth on the group and ipLimit on the route would give an execution order
// of `auth → ipLimit → handler` — a missing / invalid token would then
// abort before ever consuming the strict-IP bucket, and token-probing
// traffic would fall back to the wider global bucket. This exact
// ordering trap was flagged by PR #711 review round 4 (yujiawei P1
// middleware order).
//
// The concrete POST also anchors both middlewares to a single route: a
// future second endpoint added under the same /v1/internal group makes an
// explicit choice about its own auth + limiter rather than silently
// inheriting this one.
func (m *Module) Route(r *wkhttp.WKHttp) {
	ipLimit := m.resolveBotOwnerIPRateLimit(r)
	internal := r.Group("/v1/internal")
	internal.POST(
		"/user/resolve-bot-owner",
		ipLimit,
		m.internalAuthMiddleware(),
		m.resolveBotOwner,
	)
}

// internalAuthMiddleware fails closed when the token is unset (matches
// modules/notify/api.go:207-232) and uses constant-time comparison to defeat
// timing side-channels (same guarantee as modules/bot_mention/api.go:79).
func (m *Module) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := c.GetHeader(internalTokenHeader)
		if m.internalToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(m.internalToken)) != 1 {
			respondUnauthorized(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// resolveBotOwnerRequest is the wire body. `uid` is mandatory; we deliberately
// do not accept batch input in v1 — the drive-side TTL cache absorbs most
// duplicate lookups and a batch endpoint would only pay off past the cache.
type resolveBotOwnerRequest struct {
	UID string `json:"uid"`
}

// resolveBotOwnerResponse is the wire body. Field shape matches issue #710:
//   - robot=1 && bot_creator_uid!="" → user-bot; caller routes doc to the
//     creator's pane.
//   - robot=1 && bot_creator_uid=="" → published app-bot (platform/space
//     scope, no human creator) OR a robot row that has no creator_uid set;
//     caller skips auto-mount for the ownerless case.
//   - robot=0                        → uid is not a live bot (human, unknown,
//     or soft-deleted); caller treats it as a regular owner uid.
type resolveBotOwnerResponse struct {
	UID           string `json:"uid"`
	Robot         int    `json:"robot"`
	BotCreatorUID string `json:"bot_creator_uid"`
}

// resolveBotOwner is the single handler.
//
// Contract:
//   - 200 { uid, robot, bot_creator_uid } on success (see resolveBotOwnerResponse).
//   - 400 err.shared.param.invalid when the body / uid is malformed.
//   - 401 err.shared.auth.token_invalid on X-Internal-Token failure (middleware).
//   - 500 err.shared.internal on any lookup failure OR the
//     botidentity cross-table ambiguity guard tripping.
//
// Note: we do NOT return 404 for "uid unknown". The design deliberately maps
// unknown uid to robot=0 — the drive polling loop's job is "is this bot? if so
// who owns it?", not "does this user exist?". Distinguishing unknown from
// human would leak existence to a caller that does not need it.
//
// Bot classification: this endpoint delegates to modules/botidentity.Resolver
// so a published app_bot uid is correctly reported as robot=1 (rather than
// misclassified as robot=0 = plain owner, which a robot-only query would do).
// See modules/botidentity/resolver.go's package comment for why user.robot is
// not authoritative for this decision.
func (m *Module) resolveBotOwner(c *wkhttp.Context) {
	req, err := decodeResolveRequest(c)
	if err != nil {
		respondInvalidParam(c, "body")
		return
	}
	uid := strings.TrimSpace(req.UID)
	if uid == "" || len(uid) > 256 {
		respondInvalidParam(c, "uid")
		return
	}

	identity, err := m.identities.Resolve(uid)
	if err != nil {
		// ErrAmbiguousIdentity means the cross-table uniqueness invariant is
		// broken (uid live in BOTH robot AND app_bot). Fail closed with 500
		// rather than silently choosing one kind — see
		// modules/botidentity/resolver.go's Resolve for the same posture.
		m.Error("resolve-bot-owner: identity resolution failed",
			zap.Error(err), zap.String("uid", uid),
			zap.Bool("ambiguous", errors.Is(err, botidentity.ErrAmbiguousIdentity)))
		respondInternal(c)
		return
	}
	if identity == nil {
		c.Response(resolveBotOwnerResponse{UID: uid, Robot: 0, BotCreatorUID: ""})
		return
	}
	// Both bot kinds are `robot=1` on the wire; only `KindUserBot` carries
	// a human creator uid. `KindAppBot` intentionally has no creator field
	// (its scope is platform/space, not a user), so we map it to the
	// ownerless-bot shape — which the drive polling loop already handles
	// as "skip auto-mount". If botidentity ever grows a third Kind, this
	// switch will exhaustively expose the gap rather than silently return
	// `bot_creator_uid=""` for it.
	switch identity.Kind {
	case botidentity.KindUserBot:
		c.Response(resolveBotOwnerResponse{UID: uid, Robot: 1, BotCreatorUID: identity.CreatorUID})
	case botidentity.KindAppBot:
		c.Response(resolveBotOwnerResponse{UID: uid, Robot: 1, BotCreatorUID: ""})
	default:
		m.Error("resolve-bot-owner: unknown botidentity.Kind — refusing to guess",
			zap.String("uid", uid), zap.String("kind", string(identity.Kind)))
		respondInternal(c)
	}
}

// decodeResolveRequest reads and strictly parses the JSON body. Mirrors
// modules/bot_mention.decodeMentionRequest — bounded body, DisallowUnknownFields
// so a typo doesn't silently succeed, and rejection of trailing garbage.
func decodeResolveRequest(c *wkhttp.Context) (resolveBotOwnerRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var req resolveBotOwnerRequest
	if err := decoder.Decode(&req); err != nil {
		return resolveBotOwnerRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return resolveBotOwnerRequest{}, errors.New("resolve-bot-owner request contains multiple JSON values")
		}
		return resolveBotOwnerRequest{}, err
	}
	return req, nil
}
