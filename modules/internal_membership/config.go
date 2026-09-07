package internal_membership

import (
	"errors"
)

// Environment knobs and wire constants.
//
// A distinct token per internal consumer is the repository convention (see
// modules/internal_resolve/config.go and modules/bot_mention/config.go): it lets
// one consumer be revoked or rotated without disturbing the rest, and bounds the
// blast radius when a single value leaks.
const (
	// MembershipInternalTokenEnv gates this module's endpoints. The Loop/Fleet
	// control plane presents the value in X-Internal-Token on every call, and
	// the same variable name is used on the Fleet side for the credential it
	// sends — one secret, one name, so an operator cannot mis-pair them.
	//
	// Exported so main.go can feed the value into the cross-capability
	// exclusion checks without a second copy of the literal drifting out of
	// sync with this constant.
	MembershipInternalTokenEnv = "OCTO_MEMBERSHIP_INTERNAL_TOKEN"

	// Sibling fixed internal-token envs this module refuses to collide with, so
	// one leaked value can never grant two capabilities.
	//
	// This local check is DEFENCE IN DEPTH, not the complete guarantee. Each
	// module historically hand-rolls its own subset (notify checks one sibling,
	// bot_mention two, internal_resolve three), so the set of pairs actually
	// covered is asymmetric. main.go performs the exhaustive pairwise check
	// across every fixed internal-token env at startup; that is what makes the
	// invariant hold. Keep both: this one fails the capability closed, the
	// startup one fails the process loudly.
	notifyInternalTokenEnv     = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalTokenEnv = "OCTO_DOCS_BOT_MENTION_TOKEN"
	driveInternalTokenEnv      = "OCTO_DRIVE_INTERNAL_TOKEN"

	// internalTokenHeader is the wire header carrying the credential. Same
	// value as modules/notify, modules/bot_mention and modules/internal_resolve
	// — one convention across octo-server internal APIs.
	internalTokenHeader = "X-Internal-Token"

	// minInternalTokenBytes matches the repository-wide floor for new internal
	// credentials (modules/internal_resolve, modules/bot_mention). A one-byte
	// value would otherwise enable the capability.
	minInternalTokenBytes = 32

	// maxRequestBodyBytes bounds the verify body. The largest legitimate request
	// is maxBatchUIDs uids plus two ids; 16 KiB leaves generous headroom while
	// closing a body-size DoS window even for a caller holding a valid token.
	maxRequestBodyBytes = 16 * 1024

	// maxBatchUIDs / maxBatchProjectIDs are STRUCTURAL limits, enforced on top
	// of the byte cap rather than instead of it. `.octospec/rules/trust-boundary.md`
	// calls for exactly this pairing: a byte cap alone still admits a
	// well-formed-but-pathological payload, and both batch queries turn their
	// input straight into an SQL `IN` list.
	//
	// 50 is the bound both sides of the Loop integration contract already state,
	// so it is a contract value, not a tuning knob.
	maxBatchUIDs       = 50
	maxBatchProjectIDs = 50

	// membershipRateLimitTag is the Redis keyspace tag for the strict per-IP
	// bucket: StrictIPRateLimitMiddleware uses it as the key prefix
	// `ratelimit:strict:<tag>:`. It must not overlap any existing tag
	// ("login" / "register" / "bot_register" / "usersecret_resolve" /
	// "internal_resolve_bot_owner" / …) or quotas merge across unrelated routes.
	membershipRateLimitTag = "internal_membership"

	// Per-IP strict rate-limit knobs, deploy-tunable.
	//
	// Deliberately looser than internal_resolve's 2 rps / burst 60, because the
	// failure modes differ. These endpoints sit on the PEER'S AUTHORIZATION
	// PATH: throttling them does not slow a background poll, it makes the peer
	// fail authorization closed and deny end users. The token is the access
	// control here; the IP bucket is a DoS floor, so it is sized to be
	// comfortably above legitimate load rather than snug against it.
	//
	// Sizing: the peer batches (up to 50 ids per call) and caches epochs for at
	// most 60 s, so even a few thousand active projects amount to a handful of
	// calls per second from a small set of egress IPs. 20 rps with burst 400
	// absorbs a cold-cache reconcile spike without the operator having to know
	// these numbers exist.
	envMembershipIPRPS   = "DM_MEMBERSHIP_INTERNAL_IP_RPS"
	envMembershipIPBurst = "DM_MEMBERSHIP_INTERNAL_IP_BURST"
	defMembershipIPRPS   = 20.0
	defMembershipIPBurst = 400
)

// resolveMembershipInternalToken loads the token and refuses to enable the
// capability when it is unset, too short, or equal to a sibling fixed internal
// token.
//
// Returned error messages are logger-safe: they name the ENV, never the value.
func resolveMembershipInternalToken(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New(MembershipInternalTokenEnv + " lookup unavailable; membership internal API disabled")
	}
	token := getenv(MembershipInternalTokenEnv)
	if token == "" {
		return "", errors.New(MembershipInternalTokenEnv + " not set; membership internal API will reject all requests")
	}
	// Length is checked BEFORE collisions: a short token is unusable whether or
	// not it collides, and reporting "collides with X" for a value too short to
	// authenticate would leak a sibling's configuration state for nothing.
	if len(token) < minInternalTokenBytes {
		return "", errors.New(MembershipInternalTokenEnv + " must be at least 32 bytes; membership internal API disabled")
	}
	for _, sibling := range []string{
		notifyInternalTokenEnv,
		docsNotifyInternalTokenEnv,
		botMentionInternalTokenEnv,
		driveInternalTokenEnv,
	} {
		if token == getenv(sibling) {
			return "", errors.New(MembershipInternalTokenEnv + " must differ from " + sibling + "; membership internal API disabled")
		}
	}
	return token, nil
}
