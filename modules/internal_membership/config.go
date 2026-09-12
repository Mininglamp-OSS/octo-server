package internal_membership

import (
	"errors"
	"strconv"
	"time"
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

	// PeerMaxCacheAgeEnv is the second half of the enablement gate: the operator
	// declares, in seconds, the time bound the PEER places on decisions it caches
	// from these endpoints. Setting the token without it does not enable the
	// capability.
	//
	// # Why the release condition is an env and not a sentence
	//
	// member_epoch cannot be a sufficient invalidation signal, and not because of
	// any defect: it is keyed per PROJECT while one of the four axes that decide
	// the answer — a super-admin ban or an account destroy — is per USER. A ban
	// moves no epoch, so a cached grant keeps its agreement and a cached denial
	// survives the unban. Nothing on this side can close that; the bound has to
	// exist on the consumer. See docs/project-membership-cache-bound-proposal.md.
	//
	// That made the safety argument for shipping "the peer implements a TTL, and
	// the token stays unset until they do" — a promise living in a PR description
	// and a rollout document, with the only observable being a gauge nobody had a
	// reason to watch. This branch's whole discipline is that a mistake worth
	// making impossible should be made INEXPRESSIBLE rather than documented, and
	// the branch had not applied it to its own release condition. Now enabling the
	// capability requires stating the bound, and the stated value is published as
	// a metric so "did anyone actually agree a number" is answerable at any time.
	//
	// It carries the VALUE rather than being a yes/no acknowledgement because the
	// value is the contract. When §3.1 of the proposal lands and the answers start
	// carrying max_age_seconds on the wire, this is where that number comes from —
	// no second source to drift.
	//
	// This is NOT a claim that the peer honours it. The server cannot verify that
	// from here; §5 of the proposal specifies the measured gate (observe the
	// peer's re-verification interval) that eventually can. It is the difference
	// between an unstated assumption and a stated one.
	PeerMaxCacheAgeEnv = "OCTO_MEMBERSHIP_INTERNAL_PEER_MAX_CACHE_AGE_SECONDS"

	// peerMaxCacheAgeCeilingSeconds is where a "bound" stops being one.
	//
	// The value is the worst-case window in which a banned or destroyed account
	// keeps an authorization it no longer holds. Five minutes is already generous
	// for that — the proposal suggests starting at 30s and tuning against measured
	// _verify load — and past it the declaration would be a formality that reads as
	// a bound in the ConfigMap while behaving like none. Refusing an absurd value
	// is the point: an operator who "satisfies" the gate with 86400 has satisfied
	// nothing, and would have done it believing otherwise.
	peerMaxCacheAgeCeilingSeconds = 300

	// Sibling fixed internal-token envs this module refuses to collide with, so
	// one leaked value can never grant two capabilities.
	//
	// This local check is DEFENCE IN DEPTH, not the complete guarantee, and the
	// limits of the other layer are worth stating exactly rather than implying.
	// main.go's registry (fixedInternalTokenEnvs) does check every pair across
	// the credentials that live in ONE env var — but it REPORTS collisions at
	// ERROR level rather than refusing to boot, and it does not see
	// modules/bot_task's per-source bearer tokens (they live inside the
	// OCTO_BOT_TASK_SOURCES JSON registry, which dedupes only within itself) or
	// TS_GRPC_AUTH_TOKEN. So the unconditional claim "one leaked value can never
	// grant two capabilities" holds for the envs listed below and for the
	// registry's set; it does not hold binary-wide.
	//
	// That is why this list must cover EVERY env in main.go's registry, including
	// the two outbound provisioning secrets: the local check is the layer that
	// fails this capability CLOSED, and a pair only the central check sees leaves
	// both capabilities live with nothing but a log line. modules/project's
	// checkSecretExclusivity carries the reciprocal entry.
	notifyInternalTokenEnv     = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
	botMentionInternalTokenEnv = "OCTO_DOCS_BOT_MENTION_TOKEN"
	driveInternalTokenEnv      = "OCTO_DRIVE_INTERNAL_TOKEN"
	provisionFleetSecretEnv    = "OCTO_PROJECT_PROVISION_FLEET_SECRET"
	// Drive's provisioning credential is NOT a separate env any more: main's #887
	// pointed project provisioning at the existing OCTO_DRIVE_INTERNAL_TOKEN rather
	// than giving Drive its own per-target secret, so driveInternalTokenEnv above
	// already covers it. Re-adding OCTO_PROJECT_PROVISION_DRIVE_SECRET here would
	// make this module refuse a collision with an env nothing reads, which reads as
	// coverage and is not.
	webhookSecretEnv     = "TS_WEBHOOK_SECRET_KEY"
	mailGatewaySecretEnv = "OCTO_MAIL_GATEWAY_SECRET"
	grpcAuthTokenEnv     = "TS_GRPC_AUTH_TOKEN"
	marketplaceTokenEnv  = "OCTO_MARKETPLACE_INTERNAL_TOKEN"

	// internalTokenHeader is the wire header carrying the credential. Same
	// value as modules/notify, modules/bot_mention and modules/internal_resolve
	// — one convention across octo-server internal APIs.
	internalTokenHeader = "X-Internal-Token"

	// minInternalTokenBytes matches the repository-wide floor for new internal
	// credentials (modules/internal_resolve, modules/bot_mention). A one-byte
	// value would otherwise enable the capability.
	minInternalTokenBytes = 32

	// readBodyTimeout bounds the TIME spent reading a request body, which the byte
	// cap below does not: MaxBytesReader stops at 16 KiB but waits forever for
	// them. See boundBodyReadTime — 15 s is far beyond any real peer sending
	// <=16 KiB, so crossing it means the connection is stalled, not slow.
	readBodyTimeout = 15 * time.Second

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
	//
	// KNOWN TENSION, recorded rather than papered over. The bucket is consumed
	// BEFORE the token is checked — deliberately, so token probing cannot fall
	// back to the far wider global bucket — which means the quota is spent by
	// whoever shares the peer's observed source IP, not by whoever holds the
	// credential. Behind a shared NAT or ingress, an unauthenticated stranger can
	// drain the burst with garbage tokens and produce exactly the outcome this
	// sizing was chosen to avoid: the peer fails authorization closed and denies
	// end users.
	//
	// Loosening the numbers does not fix it, it only raises the cost of the
	// attack. The fix is a SECOND quota, keyed on the credential and applied
	// AFTER auth, which invalid-token traffic cannot touch — the pre-auth bucket
	// then only has to be an abuse floor. That is not built here: it needs a
	// per-consumer identity the current one-shared-token contract does not have,
	// which is the same open question as per-consumer scoping (see the brief).
	//
	// AND THE RESIDUAL IS WIDER THAN "the peer's egress is not shared", which is
	// what this comment used to claim. The bucket is not keyed on the observed
	// source address: octo-lib's limiter resolves the caller as X-Real-Ip, then
	// the rightmost X-Forwarded-For, then RemoteAddr, and nothing in this
	// repository calls SetTrustedProxies (see modules/qrcode/api.go, which
	// documents the default as 0.0.0.0/0). So anyone who can reach /v1/internal
	// can PIN X-Real-Ip to the peer's address and drain the burst — producing
	// exactly the deny-end-users outcome — or rotate the header and skip the
	// bucket entirely. Naming a deployment property ("their egress is clean")
	// does not bound a risk whose key the caller supplies.
	//
	// This is a pre-existing property of every strict-limited route in this
	// repository rather than something these endpoints introduce, which is why it
	// is recorded here instead of being fixed by hand-rolling a second limiter —
	// that would fork the shared middleware for one module. What makes it worth
	// writing down is that on THESE routes the failure mode is denial of the
	// peer's authorization path, not a slowed background poll. Confirming whether
	// an edge proxy normalizes those headers on /v1/internal is a deployment
	// question, and it is on the human-verify list.
	envMembershipIPRPS   = "DM_MEMBERSHIP_INTERNAL_IP_RPS"
	envMembershipIPBurst = "DM_MEMBERSHIP_INTERNAL_IP_BURST"
	defMembershipIPRPS   = 20.0
	defMembershipIPBurst = 400
)

// siblingFixedTokenEnvs is the list this module refuses to share a value with.
//
// It must stay equal to main.go's fixedInternalTokenEnvs minus this module's own
// env; TestSiblingListCoversTheCentralRegistry pins that, so adding a credential
// centrally cannot silently leave this local refusal a subset again.
var siblingFixedTokenEnvs = []string{
	notifyInternalTokenEnv,
	docsNotifyInternalTokenEnv,
	botMentionInternalTokenEnv,
	driveInternalTokenEnv,
	provisionFleetSecretEnv,
	webhookSecretEnv,
	mailGatewaySecretEnv,
	grpcAuthTokenEnv,
	// The Space internal API's token, which main's #827 added centrally without
	// reaching either module-local refusal list. It authorizes reading any uid's
	// role in any Space; shared with this module's token, one leaked value grants
	// the Space role lookup AND project membership reads.
	marketplaceTokenEnv,
}

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
	for _, sibling := range siblingFixedTokenEnvs {
		if token == getenv(sibling) {
			return "", errors.New(MembershipInternalTokenEnv + " must differ from " + sibling + "; membership internal API disabled")
		}
	}
	return token, nil
}

// resolvePeerCacheAgeBound reads the declared peer-side cache bound.
//
// Only consulted when the token resolved: with the capability off the declaration
// is irrelevant, and reporting it would be noise on every deployment that has
// correctly left this whole module disabled.
//
// Returned error messages name the ENV and the offending value. Unlike the token,
// the value here is not a secret — it is a duration an operator has to be able to
// read back out of a log line to fix it.
func resolvePeerCacheAgeBound(getenv func(string) string) (int, error) {
	if getenv == nil {
		return 0, errors.New(PeerMaxCacheAgeEnv + " lookup unavailable; membership internal API disabled")
	}
	raw := getenv(PeerMaxCacheAgeEnv)
	if raw == "" {
		return 0, errors.New(MembershipInternalTokenEnv + " is set but " + PeerMaxCacheAgeEnv +
			" is not; membership internal API disabled. These endpoints answer a peer that " +
			"caches authorization decisions, and member_epoch cannot invalidate an account " +
			"ban or destroy — the epoch is per project and the ban is per user. The peer " +
			"must bound its cached decisions by TIME; set this to that bound in seconds " +
			"(1.." + strconv.Itoa(peerMaxCacheAgeCeilingSeconds) + ", 30 is the suggested " +
			"starting point). See docs/project-membership-cache-bound-proposal.md")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(PeerMaxCacheAgeEnv + " must be a whole number of seconds, got " +
			strconv.Quote(raw) + "; membership internal API disabled")
	}
	if seconds < 1 || seconds > peerMaxCacheAgeCeilingSeconds {
		return 0, errors.New(PeerMaxCacheAgeEnv + " must be between 1 and " +
			strconv.Itoa(peerMaxCacheAgeCeilingSeconds) + " seconds, got " + strconv.Itoa(seconds) +
			"; membership internal API disabled. This value is the worst-case window in which " +
			"a banned or destroyed account keeps an authorization it no longer holds, so a " +
			"number large enough to be indistinguishable from no bound is refused rather than " +
			"accepted with a warning")
	}
	return seconds, nil
}
