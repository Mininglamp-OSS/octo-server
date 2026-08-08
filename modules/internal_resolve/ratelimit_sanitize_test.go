package internal_resolve

// Rate-limit env sanitization test: pin the fail-safe behaviour for
// pathological env values (NaN / +Inf / non-positive) that wkhttp's
// ParseRPSFromEnv accepts but that would silently disable the limiter if
// they reached the underlying Redis Lua script.
//
// PR #711 review round 4 (yujiawei P1 sanitize) pointed out this hazard.
// We use ratelimit.SanitizeRPS + SanitizeBurst on the parsed values so the
// module's fallback is deterministic: any bad env → the compiled-in
// defResolveBotOwner* defaults, NEVER a NaN / +Inf / <=0 value.

import (
	"math"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
)

// TestRateLimitSanitizersFallbackOnNaN pins the exact composition used in
// resolveBotOwnerIPRateLimit: ParseRPSFromEnv → SanitizeRPS. NaN parses
// through ParseFloat and passes ParseRPSFromEnv's own > 0 gate (NaN
// comparisons yield false → not <=0 → returned as-is), so without the
// SanitizeRPS wrap the pathological value would reach StrictIPRateLimitMiddleware.
func TestRateLimitSanitizersFallbackOnNaN(t *testing.T) {
	const envKey = "TEST_INTERNAL_RESOLVE_RPS_SANITIZE"
	t.Setenv(envKey, "NaN")

	got := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envKey, defResolveBotOwnerIPRPS),
		defResolveBotOwnerIPRPS,
	)
	if math.IsNaN(got) {
		t.Fatalf("sanitized RPS is still NaN — sanitizer bypassed. " +
			"Without this guard a `NaN` env would silently disable the strict-IP bucket.")
	}
	if got != defResolveBotOwnerIPRPS {
		t.Fatalf("sanitized RPS = %v, want fallback %v", got, defResolveBotOwnerIPRPS)
	}
}

func TestRateLimitSanitizersFallbackOnPlusInf(t *testing.T) {
	const envKey = "TEST_INTERNAL_RESOLVE_RPS_SANITIZE_INF"
	t.Setenv(envKey, "+Inf")

	got := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envKey, defResolveBotOwnerIPRPS),
		defResolveBotOwnerIPRPS,
	)
	if math.IsInf(got, 0) {
		t.Fatalf("sanitized RPS is still +Inf — sanitizer bypassed. " +
			"An infinite RPS is a disabled limiter with a lie in its logs.")
	}
	if got != defResolveBotOwnerIPRPS {
		t.Fatalf("sanitized RPS = %v, want fallback %v", got, defResolveBotOwnerIPRPS)
	}
}

func TestRateLimitSanitizersFallbackOnZero(t *testing.T) {
	// Zero would make the token bucket never refill; explicit fallback.
	const envKey = "TEST_INTERNAL_RESOLVE_RPS_SANITIZE_ZERO"
	t.Setenv(envKey, "0")

	got := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envKey, defResolveBotOwnerIPRPS),
		defResolveBotOwnerIPRPS,
	)
	if got != defResolveBotOwnerIPRPS {
		t.Fatalf("sanitized RPS = %v, want fallback %v for zero env", got, defResolveBotOwnerIPRPS)
	}
}

func TestRateLimitSanitizersFallbackBurstOnZero(t *testing.T) {
	const envKey = "TEST_INTERNAL_RESOLVE_BURST_SANITIZE_ZERO"
	t.Setenv(envKey, "0")

	got := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envKey, defResolveBotOwnerIPBurst),
		defResolveBotOwnerIPBurst,
	)
	if got != defResolveBotOwnerIPBurst {
		t.Fatalf("sanitized burst = %v, want fallback %v for zero env", got, defResolveBotOwnerIPBurst)
	}
}

func TestRateLimitSanitizersPassthroughValid(t *testing.T) {
	// A legitimate operator override MUST reach the limiter unchanged.
	const envKey = "TEST_INTERNAL_RESOLVE_RPS_VALID"
	t.Setenv(envKey, "10")
	// Guard against env leakage from a previous test — Setenv resets on
	// t.Cleanup, but nothing hurts to be explicit.
	defer os.Unsetenv(envKey)

	got := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(envKey, defResolveBotOwnerIPRPS),
		defResolveBotOwnerIPRPS,
	)
	if got != 10.0 {
		t.Fatalf("sanitized RPS = %v, want 10.0 (legitimate operator override must pass through)", got)
	}
}
