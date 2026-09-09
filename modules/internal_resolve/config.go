package internal_resolve

import (
	"errors"
)

// Environment variable names and shared constants.
//
// A distinct token per internal consumer is deliberate: it lets us revoke
// / rotate one consumer without disturbing the rest, and shrinks the blast
// radius if any single token leaks. See modules/notify/api.go:96-98 and
// modules/bot_mention/config.go:142-145 for the same "must differ from
// sibling tokens" hardening.
//
// The strongest cross-capability guard lives in main.go's
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions call, which is
// the single place that also sees the *dynamic* per-route notify tokens and
// callback secrets loaded from OCTO_CARD_ACTION_ROUTES. This module
// contributes OCTO_DRIVE_INTERNAL_TOKEN to that global check so a route-scoped
// token can never accidentally inherit the resolve-bot-owner capability, and
// vice versa. Locally we only guard the four fixed internal-token envs against
// intra-set collision (drive vs notify / docs-notify / bot-mention).
const (
	// DriveInternalTokenEnv gates the drive-facing resolve endpoints. The
	// docs-auto-mount polling loop in octo-drive presents this token in the
	// X-Internal-Token header on every call.
	//
	// Exported so main.go can feed the value into
	// cardactiondispatch.Registry.ValidateNotifyTokenExclusions alongside the
	// other three fixed internal-token envs, without a second literal string
	// drifting out of sync with this constant.
	DriveInternalTokenEnv = "OCTO_DRIVE_INTERNAL_TOKEN"

	// Sibling internal-token envs we forbid intra-set collision with, so a
	// single leaked value can't grant multiple fixed capabilities.
	notifyInternalTokenEnv  = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalToken = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalToken = "OCTO_DOCS_BOT_MENTION_TOKEN"
	// marketplaceInternalToken is modules/space.MarketplaceInternalTokenEnv,
	// which gates GET /v1/internal/spaces/:space_id/members/:uid/role.
	// Duplicated as a literal (like the three above) rather than imported, so
	// this module keeps zero production dependencies on modules/space;
	// api_test.go pins the two spellings together so they cannot drift.
	//
	// The check is symmetric on purpose — modules/space, modules/notify and
	// modules/bot_mention all run the mirror-image comparison. With only one
	// half, a deployment that set two tokens to the same value would disable
	// exactly one of the capabilities (an arbitrary winner) instead of failing
	// both closed.
	marketplaceInternalToken = "OCTO_MARKETPLACE_INTERNAL_TOKEN"

	// internalTokenHeader is the wire header carrying the credential. Same
	// value as modules/notify.InternalTokenHeader and modules/bot_mention's
	// internalTokenHeader — one convention across octo-server internal APIs.
	internalTokenHeader = "X-Internal-Token"

	// Body byte cap. resolve-bot-owner takes a single uid; 4KB is more than
	// enough and closes any body-size DoS window even with a valid token.
	maxRequestBodyBytes = 4 * 1024

	// minInternalTokenBytes is the minimum acceptable byte length for
	// OCTO_DRIVE_INTERNAL_TOKEN. Aligns with the repository-wide 32-byte
	// minimum enforced on new internal-route credentials — a one-byte value
	// would otherwise enable this endpoint. Matching the notify module keeps
	// operators from having to remember two different bars.
	minInternalTokenBytes = 32

	// Per-IP strict rate-limit knobs for /v1/internal/user/resolve-bot-owner.
	//
	// Deploy-tunable so operators can widen the ceiling when the octo-drive
	// consumer's worst-case per-egress-IP request rate goes above the default
	// without a code change + redeploy. The pattern mirrors
	// modules/usersecret/api.go:32-36 and modules/integration/api.go:94-99;
	// PR #711 review round 3 (yujiawei P1-2) specifically called this out —
	// a hardcoded ceiling with no escape hatch is a production hazard for
	// a service-to-service endpoint whose real request rate depends on the
	// consumer's ticker + cache behaviour, both of which live off-repo.
	//
	// Defaults: 2 rps (= 120 req/min), burst 60. These are ≈2× the
	// consumer's documented worst-case steady-state (a few dozen resolve
	// calls per 30 s poll tick under warm cache) with headroom for a
	// cold-cache reconcile spike. Choose deliberately: not the previous
	// round's 1 rps / burst 30, whose stated 10× ratio did not survive
	// arithmetic against its own steady-state estimate.
	envResolveBotOwnerIPRPS   = "DM_RESOLVE_BOT_OWNER_IP_RPS"
	envResolveBotOwnerIPBurst = "DM_RESOLVE_BOT_OWNER_IP_BURST"
	defResolveBotOwnerIPRPS   = 2.0
	defResolveBotOwnerIPBurst = 60

	// resolveBotOwnerRateLimitTag is the Redis keyspace tag for the strict
	// per-IP bucket. Distinct from every existing tag ("login" / "register" /
	// "bot_register" / "bot_heartbeat" / "usersecret_resolve" / …) so
	// quotas cannot cross-pollinate.
	resolveBotOwnerRateLimitTag = "internal_resolve_bot_owner"
)

// resolveDriveInternalToken loads OCTO_DRIVE_INTERNAL_TOKEN and refuses to
// enable the capability when it is unset, too short, or collides with any of
// the three sibling *fixed* internal-token envs. Cross-capability collision
// with the *dynamic* per-route notify tokens / callback secrets loaded from
// OCTO_CARD_ACTION_ROUTES is checked centrally by
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions in main.go — we
// expose DriveInternalTokenEnv above so that call can include our token in
// its argument list without duplicating the literal string.
//
// Returned error messages are logger-safe (no token values).
func resolveDriveInternalToken(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN lookup unavailable; drive internal API disabled")
	}
	token := getenv(DriveInternalTokenEnv)
	switch {
	case token == "":
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN not set; drive internal API will reject all requests")
	case len(token) < minInternalTokenBytes:
		// Length check goes BEFORE collision checks: a short token is
		// unusable regardless of whether it happens to collide, and we don't
		// want to leak "your token collides with X" for values too short to
		// authenticate anyway.
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must be at least 32 bytes; drive internal API disabled")
	case token == getenv(notifyInternalTokenEnv):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from NOTIFY_INTERNAL_TOKEN; drive internal API disabled")
	case token == getenv(docsNotifyInternalToken):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_DOCS_NOTIFY_TOKEN; drive internal API disabled")
	case token == getenv(botMentionInternalToken):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_DOCS_BOT_MENTION_TOKEN; drive internal API disabled")
	case token == getenv(marketplaceInternalToken):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_MARKETPLACE_INTERNAL_TOKEN; drive internal API disabled")
	default:
		return token, nil
	}
}
