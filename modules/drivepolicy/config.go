package drivepolicy

import "errors"

// Environment knobs and wire constants for /v1/internal/drive/upload-policy.
//
// The endpoint reuses OCTO_DRIVE_INTERNAL_TOKEN rather than minting a dedicated
// one (contract-freeze 06 §2): the same peer (octo-drive) already presents this
// token inbound on the resolve endpoints, so reusing it inherits the existing
// conflict registry / rotation surface instead of adding a new set of literals.
// Like modules/notify and modules/internal_membership, this module copies the
// token env literal rather than importing internal_resolve (no production
// dependency on a sibling package just to learn a string); a pinning test binds
// the two spellings together so they cannot drift.
const (
	// driveInternalTokenEnv is modules/internal_resolve.DriveInternalTokenEnv.
	// Duplicated as a literal — internal_resolve (canonical), internal_membership,
	// project/config_provisioning and main.go all carry the same spelling.
	driveInternalTokenEnv = "OCTO_DRIVE_INTERNAL_TOKEN"

	// Sibling fixed internal-token envs we forbid intra-set collision with, so a
	// single leaked value can't grant multiple fixed capabilities. Same set
	// internal_resolve checks. Comparison is byte-exact on the raw env values.
	notifyInternalTokenEnv      = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv  = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalTokenEnv  = "OCTO_DOCS_BOT_MENTION_TOKEN"
	marketplaceInternalTokenEnv = "OCTO_MARKETPLACE_INTERNAL_TOKEN"

	// internalTokenHeader is the wire header carrying the credential. Same value
	// as modules/notify, modules/bot_mention, modules/internal_membership and
	// modules/internal_resolve — one convention across octo-server internal APIs.
	internalTokenHeader = "X-Internal-Token"

	// minInternalTokenBytes matches the repository-wide 32-byte floor for new
	// internal-route credentials. A one-byte value would otherwise enable this
	// endpoint.
	minInternalTokenBytes = 32

	// Per-IP strict rate-limit knobs for /v1/internal/drive/upload-policy.
	// Deploy-tunable so operators can widen the ceiling when the octo-drive
	// consumer's worst-case per-egress-IP request rate exceeds the default
	// without a code change + redeploy (same pattern as internal_resolve).
	//
	// Defaults: 2 rps (= 120 req/min), burst 60. The drive consumer caches the
	// policy for ≤60s per instance, so steady-state is a few calls per minute;
	// the headroom absorbs a cold-cache reconcile spike.
	envDrivePolicyIPRPS   = "DM_DRIVE_UPLOAD_POLICY_IP_RPS"
	envDrivePolicyIPBurst = "DM_DRIVE_UPLOAD_POLICY_IP_BURST"
	defDrivePolicyIPRPS   = 2.0
	defDrivePolicyIPBurst = 60

	// drivePolicyRateLimitTag is the Redis keyspace tag for the strict per-IP
	// bucket. Distinct from every existing tag so quotas cannot cross-pollinate.
	drivePolicyRateLimitTag = "drive_upload_policy"
)

// resolveDrivePolicyToken loads OCTO_DRIVE_INTERNAL_TOKEN and refuses to enable
// the capability when it is unset, too short, or collides with any sibling fixed
// internal-token env. On any of those the endpoint rejects every request (401)
// while the process boots normally (fail-closed, matching internal_resolve).
//
// Returned error messages are logger-safe (no token values).
func resolveDrivePolicyToken(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN lookup unavailable; drive upload-policy API disabled")
	}
	token := getenv(driveInternalTokenEnv)
	switch {
	case token == "":
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN not set; drive upload-policy API will reject all requests")
	case len(token) < minInternalTokenBytes:
		// Length check precedes collision checks: a short token is unusable
		// regardless of whether it happens to collide, and we don't want to leak
		// "your token collides with X" for a value too short to authenticate.
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must be at least 32 bytes; drive upload-policy API disabled")
	case token == getenv(notifyInternalTokenEnv):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from NOTIFY_INTERNAL_TOKEN; drive upload-policy API disabled")
	case token == getenv(docsNotifyInternalTokenEnv):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_DOCS_NOTIFY_TOKEN; drive upload-policy API disabled")
	case token == getenv(botMentionInternalTokenEnv):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_DOCS_BOT_MENTION_TOKEN; drive upload-policy API disabled")
	case token == getenv(marketplaceInternalTokenEnv):
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_MARKETPLACE_INTERNAL_TOKEN; drive upload-policy API disabled")
	default:
		return token, nil
	}
}
