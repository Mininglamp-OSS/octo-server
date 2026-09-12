package internal_membership

// Rate-limit env sanitization tests for the strict per-IP bucket.
//
// wkhttp's ParseRPSFromEnv accepts NaN and +Inf: both parse through ParseFloat,
// and NaN passes its `> 0` gate because every comparison with NaN is false. Such
// a value reaches the limiter's Redis Lua script, errors there, and the limiter
// is FAIL-OPEN on Redis errors — so a single typo in a deploy manifest silently
// removes the DoS floor from an authorization endpoint, with nothing in the logs
// saying the control is gone.
//
// ipRateLimit therefore wraps both parses in ratelimit.SanitizeRPS /
// SanitizeBurst. It is correct today and nothing pinned it, so a refactor that
// dropped the wrap would go green. modules/internal_resolve carries the same
// file for the same reason, after a round there rated the gap a blocker.

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
)

func TestMembershipRateLimitSanitizersFallBackOnPathologicalEnvs(t *testing.T) {
	cases := map[string]string{
		"NaN":          "NaN",
		"+Inf":         "+Inf",
		"zero":         "0",
		"negative":     "-1",
		"not a number": "twenty",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			const envKey = "TEST_MEMBERSHIP_RPS_SANITIZE"
			t.Setenv(envKey, value)

			got := ratelimit.SanitizeRPS(
				wkhttp.ParseRPSFromEnv(envKey, defMembershipIPRPS),
				defMembershipIPRPS,
			)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("sanitized RPS is still %v — the sanitizer was bypassed, and the "+
					"limiter fails OPEN when its Lua script errors on such a value", got)
			}
			if got != defMembershipIPRPS {
				t.Fatalf("sanitized RPS = %v, want the compiled-in default %v", got, defMembershipIPRPS)
			}
		})
	}
}

func TestMembershipBurstSanitizerFallsBackOnZero(t *testing.T) {
	const envKey = "TEST_MEMBERSHIP_BURST_SANITIZE"
	t.Setenv(envKey, "0")

	got := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(envKey, defMembershipIPBurst),
		defMembershipIPBurst,
	)
	if got != defMembershipIPBurst {
		t.Fatalf("sanitized burst = %v, want the compiled-in default %v", got, defMembershipIPBurst)
	}
}

// TestMembershipRateLimitSanitizersPassThroughValidOverrides is the other half:
// a sanitizer that clamped everything to the default would pass every test above
// while quietly removing the operator's ability to tune the bucket.
func TestMembershipRateLimitSanitizersPassThroughValidOverrides(t *testing.T) {
	const rpsKey = "TEST_MEMBERSHIP_RPS_VALID"
	const burstKey = "TEST_MEMBERSHIP_BURST_VALID"
	t.Setenv(rpsKey, "5")
	t.Setenv(burstKey, "77")
	defer os.Unsetenv(rpsKey)
	defer os.Unsetenv(burstKey)

	if got := ratelimit.SanitizeRPS(wkhttp.ParseRPSFromEnv(rpsKey, defMembershipIPRPS), defMembershipIPRPS); got != 5.0 {
		t.Fatalf("sanitized RPS = %v, want the operator's 5", got)
	}
	if got := ratelimit.SanitizeBurst(wkhttp.ParseBurstFromEnv(burstKey, defMembershipIPBurst), defMembershipIPBurst); got != 77 {
		t.Fatalf("sanitized burst = %v, want the operator's 77", got)
	}
}

// TestIPRateLimitStillComposesTheSanitizers is the source guard.
//
// The behavioural tests above exercise the composition; they cannot see whether
// ipRateLimit still USES it, because building the real limiter needs Redis. This
// names the wrap so deleting it fails here rather than in production, where the
// symptom is silence.
func TestIPRateLimitStillComposesTheSanitizers(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func (m *Module) ipRateLimit(")
	if start < 0 {
		t.Fatal("ipRateLimit not found — if it moved, point this guard at it rather than deleting it")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}

	for _, want := range []string{"ratelimit.SanitizeRPS(", "ratelimit.SanitizeBurst("} {
		if !strings.Contains(body, want) {
			t.Errorf("ipRateLimit must wrap the parsed env in %s — ParseRPSFromEnv lets NaN and "+
				"+Inf through, and the limiter fails OPEN when they reach its Lua script", want)
		}
	}
}
