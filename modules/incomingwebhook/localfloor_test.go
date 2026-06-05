package incomingwebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Unit tests for the Redis-independent local push floor. These need no DB /
// Redis / WuKongIM — they exercise the in-memory token bucket directly, so they
// run in plain `go test` and document the limiter contract.

// countAllowed fires n calls and returns how many were allowed. Used by the
// default-config tests, where the 200 rps default refill makes an exact "burst
// then exactly one reject" assertion timing-sensitive on slow CI; a range check
// (full burst passes, but not 2× burst) is refill-robust instead.
func countAllowed(f *localFloor, n int) int {
	allowed := 0
	for i := 0; i < n; i++ {
		if f.allow() {
			allowed++
		}
	}
	return allowed
}

// TestLocalFloor_AllowsBurstThenBlocks: with a tiny refill rate, exactly `burst`
// calls pass before the bucket empties and further calls are rejected.
func TestLocalFloor_AllowsBurstThenBlocks(t *testing.T) {
	t.Setenv(envLocalFloorBurst, "2")
	t.Setenv(envLocalFloorRPS, "0.001") // ~never refills within the test

	f := newLocalFloor()
	assert.True(t, f.allow(), "1st call within burst must pass")
	assert.True(t, f.allow(), "2nd call within burst must pass")
	assert.False(t, f.allow(), "3rd call must be rejected once burst is spent")
	assert.False(t, f.allow(), "still rejected while the bucket is empty")
}

// TestLocalFloor_ZeroEnvDoesNotDisable documents the fail-safe: the shared env
// parsers coerce 0 / negative / unparseable values to the generous default, so
// setting the env to "0" does NOT disable the floor — it stays enabled at the
// default ceiling (a default-sized burst passes, one past it is still blocked).
func TestLocalFloor_ZeroEnvDoesNotDisable(t *testing.T) {
	t.Setenv(envLocalFloorRPS, "0")
	t.Setenv(envLocalFloorBurst, "0")

	f := newLocalFloor()
	allowed := countAllowed(f, 2*defaultLocalFloorBurst)
	assert.GreaterOrEqual(t, allowed, defaultLocalFloorBurst, "coerced-to-default burst must all pass")
	assert.Less(t, allowed, 2*defaultLocalFloorBurst, "env 0 did not disable the floor; it stays bounded at the default")
}

// TestLocalFloor_ConfiguredAtConstruction: env is read once when the floor is
// built, so two floors constructed under different env get independent limits.
func TestLocalFloor_ConfiguredAtConstruction(t *testing.T) {
	t.Setenv(envLocalFloorBurst, "1")
	t.Setenv(envLocalFloorRPS, "0.001")
	tight := newLocalFloor()
	assert.True(t, tight.allow(), "tight floor: first call gets the lone token")
	assert.False(t, tight.allow(), "tight floor: second call is rejected")

	t.Setenv(envLocalFloorBurst, "100")
	loose := newLocalFloor()
	for i := 0; i < 100; i++ {
		assert.Truef(t, loose.allow(), "loose floor built later must allow its own burst (i=%d)", i)
	}
}

// TestLocalFloor_DefaultIsBackstop: with no env override the default bucket is
// large enough that ordinary traffic never trips it (it only acts as a Redis-
// outage backstop). A burst of default size all passes.
func TestLocalFloor_DefaultIsBackstop(t *testing.T) {
	f := newLocalFloor()
	allowed := countAllowed(f, 2*defaultLocalFloorBurst)
	assert.GreaterOrEqual(t, allowed, defaultLocalFloorBurst, "the full default burst must pass")
	assert.Less(t, allowed, 2*defaultLocalFloorBurst, "default floor is bounded; not everything passes")
}
