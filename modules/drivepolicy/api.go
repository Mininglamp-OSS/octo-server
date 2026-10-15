package drivepolicy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

// Module is the module handle registered with the wkhttp router. Field layout
// matches the octo-server convention (see modules/internal_resolve/api.go:48).
type Module struct {
	ctx           *config.Context
	internalToken string
	log.Log

	// policy resolves the currently-effective drive upload policy together with
	// its availability. The second result is false when the process-wide
	// SystemSettings snapshot has never loaded successfully, which the handler
	// must surface as 503 rather than a valid-looking 200 (fail-closed). Nil in
	// production, where the handler falls back to
	// common.EnsureSystemSettings(ctx).DriveUploadPolicyAvail(). Tests inject a
	// stub here so the handler can be exercised without MySQL / a *SystemSettings
	// with a live snapshot, and so a single test can prove the handler reads
	// the policy exactly once per request.
	policy func() (common.DriveUploadPolicy, bool)
}

// New loads OCTO_DRIVE_INTERNAL_TOKEN at construction. When it is unset, too
// short, or collides with a sibling fixed internal-token env, the reason is
// logged and every request is refused (fail-closed via internalAuthMiddleware).
// The module still mounts, so a misconfigured deployment fails closed on the
// endpoint rather than failing to boot the whole server — the same posture
// modules/internal_resolve and modules/internal_membership take.
//
// ctx is retained so Route can construct the rate-limit Redis client the same
// way modules/internal_resolve does, without threading extra plumbing.
func New(ctx *config.Context) *Module {
	logger := log.NewTLog("DrivePolicy")
	token, tokenErr := resolveDrivePolicyToken(os.Getenv)
	if tokenErr != nil {
		// Cleared explicitly rather than relying on every error path in
		// resolveDrivePolicyToken returning "". The middleware refuses on an
		// empty token, but making fail-closed a property of one line here
		// rather than of five separate return statements means a future edit
		// that returns a partially-validated value cannot enable the endpoint
		// with an error already logged.
		token = ""
		logger.Error(tokenErr.Error())
	}
	return &Module{
		ctx:           ctx,
		internalToken: token,
		Log:           logger,
	}
}

// ipRateLimit builds the per-endpoint IP strict limiter.
//
// Values are deploy-tunable via envDrivePolicyIPRPS / envDrivePolicyIPBurst
// (see config.go), sanitized against the defaults so a NaN / +Inf / non-positive
// env typo cannot silently disable this security control. ParseRPSFromEnv lets
// both NaN and +Inf through, and they surface as Lua script errors that the
// limiter treats as fail-open; ratelimit's sanitizers exist for exactly this.
//
// One-shot construction (once per Route call) matches modules/internal_resolve
// and modules/internal_membership. The client is never closed because Route()
// runs exactly once per process and the limiter lives as long as the router.
func (m *Module) ipRateLimit(r *wkhttp.WKHttp) wkhttp.HandlerFunc {
	rlRedis := octoredis.NewInstrumentedClient(m.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	rps := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envDrivePolicyIPRPS, defDrivePolicyIPRPS),
		defDrivePolicyIPRPS,
	)
	burst := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envDrivePolicyIPBurst, defDrivePolicyIPBurst),
		defDrivePolicyIPBurst,
	)
	limiter := r.StrictIPRateLimitMiddleware(
		context.Background(), rlRedis, drivePolicyRateLimitTag, rps, burst,
	)
	return countRateLimited(limiter)
}

// countRateLimited wraps a rate-limit handler so the rate_limited outcome lands
// on the served_total metric without moving the limiter out of its position at
// the head of the chain. The limiter calls c.Next() itself when it admits the
// request and aborts (without Next) when it rejects, so a non-admitted request
// returns here with the chain already aborted at 429.
//
// Extracted from ipRateLimit so a hermetic test can drive the real rejection
// branch (429 → metric) with an injected limiter — the real limiter needs Redis,
// and the pass-through stubs used by the route-order tests never produce a 429,
// which is exactly the coverage gap reviewer yujiawei flagged (PR #930 P2).
func countRateLimited(limiter wkhttp.HandlerFunc) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		limiter(c)
		if c.IsAborted() && c.Writer.Status() == http.StatusTooManyRequests {
			servedTotal.WithLabelValues(resultRateLimited).Inc()
		}
	}
}

// Route mounts the internal drive upload-policy endpoint under /v1/internal,
// gated by X-Internal-Token.
//
// Middleware EXECUTION ORDER on this route (matters):
//  1. per-endpoint IP strict rate limit — MUST run FIRST so IP-abusive traffic
//     never reaches the token compare (a Redis Lua round trip) or the DB path.
//  2. internalAuthMiddleware — X-Internal-Token constant-time compare +
//     fail-closed on unset.
//
// Both middlewares are mounted on the CONCRETE GET route, NOT the group. Gin
// runs group handlers ahead of route handlers, so auth-on-group + limiter-on-
// route would execute as auth → limiter: a missing / wrong token would abort
// before ever consuming the strict-IP bucket, and token-probing traffic would
// fall back to the wider global bucket. modules/internal_resolve and
// modules/internal_membership document having been bitten by exactly this
// ordering; route_runtime_order_test.go pins the observed order here too.
func (m *Module) Route(r *wkhttp.WKHttp) {
	ipLimit := m.ipRateLimit(r)
	internal := r.Group("/v1/internal")
	internal.GET(
		"/drive/upload-policy",
		ipLimit,
		m.internalAuthMiddleware(),
		m.getUploadPolicy,
	)
}

// internalAuthMiddleware fails closed when the token is unset (matches
// modules/internal_resolve and modules/notify) and uses constant-time comparison
// to defeat timing side-channels. Every failure is counted so an operator can
// see token probing / a rotation mismatch on the metrics surface.
func (m *Module) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := c.GetHeader(internalTokenHeader)
		if m.internalToken == "" || !internalTokenMatches(token, m.internalToken) {
			servedTotal.WithLabelValues(resultUnauthorized).Inc()
			internalAuthFailTotal.Inc()
			respondUnauthorized(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// internalTokenMatches reports whether presented equals the configured secret.
//
// Both sides are folded through SHA-256 first so the constant-time comparison
// always runs over two fixed 32-byte digests. Comparing the raw, variable-length
// byte slices instead would let subtle.ConstantTimeCompare return on its very
// first length check (it is only constant-time for equal-length inputs), so a
// repeat-timing observer could recover the *configured* token's length before
// any content comparison happened (reviewer yujiawei, PR #930 P2). Hashing also
// keeps the comparison cost independent of how much of the secret matches.
//
// The early "unset token" refusal lives in the caller: an unset secret is a
// configuration state, not a secret whose length must be protected.
func internalTokenMatches(presented, configured string) bool {
	got := sha256.Sum256([]byte(presented))
	want := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// getUploadPolicy serves the currently-effective policy as the 6-field wire
// body defined by the shared contract §3.3 (DriveUploadPolicy).
//
// 200 only when the policy is authoritative. If the process-wide settings
// snapshot has never loaded successfully, the effective policy is unknown —
// the handler answers 503 instead of publishing a compile-time default with a
// valid-looking fingerprint, because the drive side would otherwise raise its
// limit to a value no admin ever configured. A later successful auto-reload
// self-heals (the snapshot is swapped in-place), so this is transient.
//
// The policy is read exactly once per request (single snapshot read, see
// common.SystemSettings.DriveUploadPolicyAvail) so the six fields, the version,
// and the availability flag can never disagree.
func (m *Module) getUploadPolicy(c *wkhttp.Context) {
	policy, available := m.currentPolicy()
	if !available {
		servedTotal.WithLabelValues(resultUnavailable).Inc()
		m.Warn("drive upload policy unavailable: settings snapshot never loaded")
		respondUnavailable(c)
		return
	}
	servedTotal.WithLabelValues(resultOk).Inc()
	policyVersionInfo.Set(policy.PolicyVersion)
	m.Debug("drive upload policy served",
		zap.String("policy_version", policy.PolicyVersion),
		zap.Int("max_size_kb", policy.MaxSizeKB))
	c.JSON(http.StatusOK, policy)
}

// currentPolicy resolves the effective policy, honoring the test-injected
// stub when present. Kept as a method so the handler stays a single call site.
func (m *Module) currentPolicy() (common.DriveUploadPolicy, bool) {
	if m.policy != nil {
		return m.policy()
	}
	return common.EnsureSystemSettings(m.ctx).DriveUploadPolicyAvail()
}
