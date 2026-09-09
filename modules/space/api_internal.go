package space

import (
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	"go.uber.org/zap"
)

// Internal single-subject Space role lookup for octo-marketplace.
//
// GET /v1/internal/spaces/:space_id/members/:uid/role answers exactly one
// question — "what is this uid's space_member.role in this Space right now?" —
// and returns nothing else. It is a service-to-service endpoint: no user token,
// no Space middleware; the only credential is X-Internal-Token bound to
// OCTO_MARKETPLACE_INTERNAL_TOKEN.
//
// # Why a single-subject lookup and not a roster
//
// An earlier revision of this branch exposed
// GET /v1/internal/spaces/:space_id/admins, which returned {uid, name, role}
// for every owner/admin of any Space the caller could name. Two defects killed
// it:
//
//   - It leaked verified legal names cross-tenant. The roster reused
//     MemberDetailModel.DisplayName(), whose fallback chain reaches
//     user_verification.real_name — the legal name returned by CAS / WeCom /
//     Feishu. That chain is correct for queryMembers, whose audience is other
//     members of the same Space. On a cross-tenant service endpoint it handed
//     any token holder the leadership roster, by legal name, of every org whose
//     space_id it could guess or obtain.
//   - Empty-vs-non-empty was itself an existence oracle. The roster claimed an
//     unknown Space was indistinguishable from an admin-less one, but every
//     real Space has at least one role=2 creator, so "[]" meant "no such Space"
//     in practice.
//
// The consumer needed the roster for two things, and neither actually requires
// one:
//
//   - Delivery targeting for the IM approval card. octo-server already owns
//     space_member and already delivers the card, so the consumer can ask for
//     role-targeted delivery instead of learning who the admins are — see
//     NotifyReq.target_role in modules/notify and ActiveAdminUIDs in
//     admin_targets.go.
//   - Re-verifying the operator who clicked approve/deny. The card-action
//     callback carries only an asserted operator_uid (the dispatch is an async
//     queue with retries and a DLQ, so there is no user token to forward), and
//     marketplace must independently confirm that uid still holds the role.
//     That is this endpoint: one subject, one integer.
//
// Deliberately lives in modules/space (not modules/internal_resolve) because
// the space_member isolation semantics belong here; internal_resolve
// additionally carries route-shape source assertions that pin its own route.
const (
	// MarketplaceInternalTokenEnv gates this endpoint. Exported so main.go can
	// feed the value into
	// cardactiondispatch.Registry.ValidateNotifyTokenExclusions by qualified
	// reference instead of a duplicated literal — the repo-root
	// main_marketplace_token_test.go and this package's main_wiring_test.go
	// assert the qualified form, and a literal would silently drift from this
	// constant.
	//
	// RENAMED from OCTO_MARKETPLACE_ADMIN_LIST_TOKEN when the roster endpoint
	// was deleted: the credential no longer authorizes listing anything, and an
	// env name that describes a capability the process does not have is a
	// standing invitation to misconfigure it. The name now matches its sibling
	// OCTO_DRIVE_INTERNAL_TOKEN (one consumer, one internal capability set).
	MarketplaceInternalTokenEnv = "OCTO_MARKETPLACE_INTERNAL_TOKEN"

	// Sibling *fixed* internal-token envs we forbid intra-set collision with,
	// so one leaked value can never grant two fixed capabilities. Mirrors the
	// same local const set in modules/internal_resolve/config.go,
	// modules/bot_mention/config.go and modules/notify/config.go — the names
	// are duplicated on purpose rather than imported, so no module has to
	// depend on another just to know a string. Every one of those modules runs
	// the mirror-image comparison, so a shared value fails ALL the colliding
	// capabilities closed instead of picking an arbitrary winner.
	//
	// Collision with the *dynamic* per-route notify tokens / callback secrets
	// loaded from OCTO_CARD_ACTION_ROUTES cannot be seen from here; that check
	// happens centrally in main.go (see MarketplaceInternalTokenEnv above).
	notifyInternalTokenEnv            = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv        = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalTokenEnv        = "OCTO_DOCS_BOT_MENTION_TOKEN"
	driveInternalTokenEnvForExclusion = "OCTO_DRIVE_INTERNAL_TOKEN"

	// marketplaceInternalTokenHeader is the wire header carrying the
	// credential. Same value as modules/notify.InternalTokenHeader and
	// modules/internal_resolve's internalTokenHeader — one convention across
	// octo-server internal APIs.
	marketplaceInternalTokenHeader = "X-Internal-Token"

	// minMarketplaceInternalTokenBytes is the repository-wide 32-byte floor
	// for internal-route credentials (see modules/internal_resolve and
	// modules/notify). A one-byte value would otherwise enable the endpoint.
	minMarketplaceInternalTokenBytes = 32

	// maxSpaceIDBytes matches the space.space_id / space_member.space_id
	// column width (VARCHAR(40), modules/space/sql/20260307000002). Anything
	// longer cannot identify a row, so reject it as a malformed parameter
	// instead of running a query that can only miss.
	maxSpaceIDBytes = 40

	// maxMemberUIDBytes matches the space_member.uid / user.uid column width
	// (VARCHAR(40)). Same reasoning as maxSpaceIDBytes.
	maxMemberUIDBytes = 40

	// Per-IP strict rate-limit knobs. Deploy-tunable so operators can widen
	// the ceiling without a redeploy when the marketplace consumer's
	// per-egress-IP rate rises; same pattern as
	// modules/internal_resolve/config.go and modules/integration/api.go.
	//
	// Defaults: 2 rps (=120 req/min), burst 20. The consumer calls this once
	// per approval-card click (a human action), so the steady state is a
	// handful of calls per minute; the default is ~10x that with burst room
	// for a batch of callbacks landing together after a retry sweep.
	envSpaceMemberRoleIPRPS   = "DM_SPACE_MEMBER_ROLE_IP_RPS"
	envSpaceMemberRoleIPBurst = "DM_SPACE_MEMBER_ROLE_IP_BURST"
	defSpaceMemberRoleIPRPS   = 2.0
	defSpaceMemberRoleIPBurst = 20

	// spaceMemberRoleRateLimitTag is the Redis keyspace tag for the strict
	// per-IP bucket (key prefix `ratelimit:strict:<tag>:`). It must be unique
	// repo-wide or two unrelated routes share one quota; the invariant is
	// enforced by TestStrictIPRateLimitTagsAreUniqueRepoWide in
	// ratelimit_tags_test.go at the repo root, which reads every
	// StrictIPRateLimitMiddleware call site off disk instead of comparing
	// against a hand-maintained list.
	spaceMemberRoleRateLimitTag = "space_internal_member_role"
)

// resolveMarketplaceInternalToken loads OCTO_MARKETPLACE_INTERNAL_TOKEN and
// refuses to enable the capability when it is unset, too short, or collides
// with any sibling fixed internal-token env. Modeled on
// modules/internal_resolve/config.go resolveDriveInternalToken.
//
// A collision disables THIS capability and logs the reason; the process still
// boots. Every module owning one of the sibling envs runs the mirror-image
// comparison, so a shared value fails both colliding capabilities closed
// instead of picking an arbitrary winner. Collision with the *dynamic*
// per-route credentials from OCTO_CARD_ACTION_ROUTES is a separate check and IS
// fatal — it happens in main.go, the only place both sets are visible.
//
// Returned error messages are logger-safe: they never contain token values.
func resolveMarketplaceInternalToken(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New(MarketplaceInternalTokenEnv +
			" lookup unavailable; space internal API disabled")
	}
	token := getenv(MarketplaceInternalTokenEnv)
	switch {
	case token == "":
		return "", errors.New(MarketplaceInternalTokenEnv +
			" not set; space internal API will reject all requests")
	case len(token) < minMarketplaceInternalTokenBytes:
		// Length check goes BEFORE the collision checks: a short token is
		// unusable regardless of whether it happens to collide, and we must
		// not leak "your token collides with X" for a value that could never
		// authenticate anyway.
		return "", errors.New(MarketplaceInternalTokenEnv +
			" must be at least 32 bytes; space internal API disabled")
	case token == getenv(notifyInternalTokenEnv):
		return "", errors.New(MarketplaceInternalTokenEnv +
			" must differ from " + notifyInternalTokenEnv +
			"; space internal API disabled")
	case token == getenv(docsNotifyInternalTokenEnv):
		return "", errors.New(MarketplaceInternalTokenEnv +
			" must differ from " + docsNotifyInternalTokenEnv +
			"; space internal API disabled")
	case token == getenv(botMentionInternalTokenEnv):
		return "", errors.New(MarketplaceInternalTokenEnv +
			" must differ from " + botMentionInternalTokenEnv +
			"; space internal API disabled")
	case token == getenv(driveInternalTokenEnvForExclusion):
		return "", errors.New(MarketplaceInternalTokenEnv +
			" must differ from " + driveInternalTokenEnvForExclusion +
			"; space internal API disabled")
	default:
		return token, nil
	}
}

// sanitizedSpaceMemberRoleRPS / sanitizedSpaceMemberRoleBurst resolve the
// deploy-tunable limiter knobs for Route().
//
// The Sanitize* wrap is load-bearing, not decoration: wkhttp.ParseRPSFromEnv
// accepts NaN and +Inf (strconv.ParseFloat lets both through and the helper
// only rejects n <= 0), and those values slip past wkhttp's own <=0 check into
// the Redis Lua script, where they surface as script errors that documented
// behaviour maps to the FAIL-OPEN path. A typo like
// `DM_SPACE_MEMBER_ROLE_IP_RPS=NaN` would therefore silently disable a
// load-bearing security control on an un-user-authed endpoint. Same fix as
// modules/internal_resolve/api.go:100-113 and modules/bot_api/ratelimit.go.
//
// Extracted as functions so the composition is unit-testable without Redis.
func sanitizedSpaceMemberRoleRPS() float64 {
	return ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envSpaceMemberRoleIPRPS, defSpaceMemberRoleIPRPS),
		defSpaceMemberRoleIPRPS,
	)
}

func sanitizedSpaceMemberRoleBurst() int {
	return ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envSpaceMemberRoleIPBurst, defSpaceMemberRoleIPBurst),
		defSpaceMemberRoleIPBurst,
	)
}

// spaceMemberRoleData is the entire success payload. `role` is a *pointer* so
// "not a member" serializes as an explicit JSON null rather than being dropped
// or defaulting to 0 — 0 is a REAL role (plain member) and must never collide
// with absence. No name, no username, no short_no, no real_name: this endpoint
// is deliberately PII-free.
type spaceMemberRoleData struct {
	// Role uses the native space_member.role encoding — 0=member, 1=admin,
	// 2=owner (modules/space/sql/20260307000002_space_legacy01.sql). nil means
	// "no active membership row is visible", which covers a non-member, an
	// unknown Space, and a disbanded Space indistinguishably (see
	// getSpaceMemberRole).
	Role *int `json:"role"`
}

// spaceMemberRoleEnvelope keeps the `{ "data": ... }` success envelope the
// marketplace client decodes. wkhttp's c.Response writes the value verbatim,
// so the wrapper has to be explicit here.
type spaceMemberRoleEnvelope struct {
	Data spaceMemberRoleData `json:"data"`
}

// queryActiveMemberRole returns (role, true, nil) when uid holds an active
// membership row in an ACTIVE space, and (0, false, nil) otherwise.
//
// Divergences from queryMember (db.go), each deliberate:
//   - INNER JOIN space ON s.status=1 — disbanding a Space only flips
//     space.status; its space_member rows stay status=1. Without the join a
//     disbanded org's roles would keep answering. Same hardening as
//     modules/user/api.go queryUserSpaceContext (v3.3.1 §A.1).
//   - SELECT sm.role only, with no join to user / user_verification, so no
//     name can leak through this path even by accident.
func (d *DB) queryActiveMemberRole(spaceID, uid string) (int, bool, error) {
	var roles []int
	_, err := d.session.SelectBySql(`
		SELECT sm.role
		FROM space_member sm
		INNER JOIN space s ON s.space_id=sm.space_id AND s.status=1
		WHERE sm.space_id=? AND sm.uid=? AND sm.status=1
		LIMIT 1
	`, spaceID, uid).Load(&roles)
	if err != nil {
		return 0, false, err
	}
	if len(roles) == 0 {
		return 0, false, nil
	}
	return roles[0], true, nil
}

// marketplaceInternalTokenMiddleware fails closed when the token is unset or
// was rejected at construction time (matches
// modules/internal_resolve.internalAuthMiddleware and modules/notify/api.go),
// and uses a constant-time comparison so the compare cannot be turned into a
// byte-at-a-time oracle.
func (s *Space) marketplaceInternalTokenMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		provided := c.GetHeader(marketplaceInternalTokenHeader)
		if s.marketplaceInternalToken == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(s.marketplaceInternalToken)) != 1 {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// getSpaceMemberRole handles
// GET /v1/internal/spaces/:space_id/members/:uid/role.
//
// Contract:
//   - 200 { "data": { "role": 2 } }    — active member of an active Space.
//   - 200 { "data": { "role": null } } — no active membership row is visible.
//   - 400 err.shared.param.invalid on a missing / oversized space_id or uid.
//   - 401 err.shared.auth.token_invalid on X-Internal-Token failure (middleware).
//   - 500 err.shared.internal on a query failure.
//
// ABSENCE IS ONE ANSWER, ON PURPOSE. "user is not a member", "space does not
// exist" and "space was disbanded" all produce byte-identical
// `{"data":{"role":null}}`. A 404-vs-200 split (or any distinct body) would
// turn one shared service token into a cross-tenant Space-existence oracle,
// which is exactly the defect that retired the roster endpoint this replaced.
// 200 + a nullable field also matches how the sibling internal endpoint signals
// absence: modules/internal_resolve resolveBotOwner answers 200 with robot=0
// for an unknown uid rather than 404.
func (s *Space) getSpaceMemberRole(c *wkhttp.Context) {
	spaceID := strings.TrimSpace(c.Param("space_id"))
	if spaceID == "" || len(spaceID) > maxSpaceIDBytes {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil,
			i18n.Details{"field": "space_id"})
		return
	}
	uid := strings.TrimSpace(c.Param("uid"))
	if uid == "" || len(uid) > maxMemberUIDBytes {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil,
			i18n.Details{"field": "uid"})
		return
	}

	role, found, err := s.db.queryActiveMemberRole(spaceID, uid)
	if err != nil {
		s.Error("space member-role query failed",
			zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", uid))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if !found {
		c.Response(spaceMemberRoleEnvelope{Data: spaceMemberRoleData{Role: nil}})
		return
	}
	c.Response(spaceMemberRoleEnvelope{Data: spaceMemberRoleData{Role: &role}})
}
