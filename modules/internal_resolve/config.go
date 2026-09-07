package internal_resolve

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-server/pkg/internaltoken"
)

// Environment variable names and shared constants.
//
// A distinct token per internal consumer is deliberate: it lets us revoke
// / rotate one consumer without disturbing the rest, and shrinks the blast
// radius if any single token leaks.
//
// The intra-set guard — "OCTO_DRIVE_INTERNAL_TOKEN must differ from every
// other fixed internal-token env" — is no longer hand-rolled here. It lives in
// pkg/internaltoken, which owns the registry of those envs and compares the
// one being resolved against every OTHER registered entry, so a capability
// added tomorrow is covered without this file changing. See that package's doc
// for why coverage-by-convention was replaced.
//
// The strongest cross-capability guard still lives in main.go's
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions call, which is
// the single place that also sees the *dynamic* per-route notify tokens and
// callback secrets loaded from OCTO_CARD_ACTION_ROUTES. main.go now sources
// its argument list from internaltoken.Values, so this module's token reaches
// that check through the registry rather than through a literal argument.
const (
	// DriveInternalTokenEnv gates the drive-facing resolve endpoints. The
	// docs-auto-mount polling loop in octo-drive presents this token in the
	// X-Internal-Token header on every call.
	//
	// Re-exported from the shared registry so callers (main.go, this module's
	// tests) keep a single name for it and no second literal can drift.
	DriveInternalTokenEnv = internaltoken.DriveInternalTokenEnv

	// marketplaceInternalToken is modules/space.MarketplaceInternalTokenEnv.
	// Kept as a local literal (and a local check below) only until that env is
	// absorbed into pkg/internaltoken; see the follow-up commit on PR #853.
	marketplaceInternalToken = "OCTO_MARKETPLACE_INTERNAL_TOKEN"

	// internalTokenHeader is the wire header carrying the credential. Owned by
	// pkg/internaltoken alongside the env names — one convention across every
	// octo-server internal API, and one place to change it.
	internalTokenHeader = internaltoken.Header

	// Body byte cap. resolve-bot-owner takes a single uid; 4KB is more than
	// enough and closes any body-size DoS window even with a valid token.
	maxRequestBodyBytes = 4 * 1024

	// minInternalTokenBytes is the minimum acceptable byte length for
	// OCTO_DRIVE_INTERNAL_TOKEN, enforced by internaltoken.Resolve. Aliased
	// here so the module's own tests can state the bar without duplicating the
	// number.
	minInternalTokenBytes = internaltoken.DefaultMinBytes

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

// resolveDriveInternalToken loads OCTO_DRIVE_INTERNAL_TOKEN through the shared
// registry, which refuses to enable the capability when the value is unset, too
// short, or equal to ANY other registered fixed internal-token env. Refusal
// yields the empty string, so ratelimitedInternalTokenAuth below fails closed.
//
// Cross-capability collision with the *dynamic* per-route notify tokens /
// callback secrets loaded from OCTO_CARD_ACTION_ROUTES is checked centrally by
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions in main.go.
//
// Returned error messages are logger-safe (no token values) — pinned by
// pkg/internaltoken's TestErrorsNeverContainTokenValue.
func resolveDriveInternalToken(getenv func(string) string) (string, error) {
	token, err := internaltoken.Resolve(DriveInternalTokenEnv, getenv)
	if err != nil {
		return "", err
	}
	// Mirror-image half for OCTO_MARKETPLACE_INTERNAL_TOKEN (#827). That env is
	// registered AFTER this one, so the registry's precedence rule alone would
	// leave this capability serving; #827 made that pair symmetric on purpose.
	// Absorbed into the registry by the next commit.
	if getenv(marketplaceInternalToken) == token {
		return "", errors.New("OCTO_DRIVE_INTERNAL_TOKEN must differ from OCTO_MARKETPLACE_INTERNAL_TOKEN; drive internal API disabled")
	}
	return token, nil
}
