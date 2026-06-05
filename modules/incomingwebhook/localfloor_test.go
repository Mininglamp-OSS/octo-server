package incomingwebhook

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/time/rate"
)

// Unit tests for the Redis-independent local push floor. These need no DB /
// Redis / WuKongIM — they exercise the in-memory token buckets directly, so they
// run in plain `go test` and document the limiter contract.

// uniqueIP returns a distinct IP per index so the per-IP sub-limit never bites,
// isolating the GLOBAL floor under test in the global-cap assertions below.
func uniqueIP(i int) string { return fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256) }

// countAllowedGlobal fires n calls — each from a DISTINCT IP so only the global
// floor can reject — and returns how many were allowed. Used by the default-
// config tests, where the 200 rps default refill makes an exact "burst then
// exactly one reject" assertion timing-sensitive on slow CI; a range check (full
// burst passes, but not 2× burst) is refill-robust instead.
func countAllowedGlobal(f *localFloor, n int) int {
	allowed := 0
	for i := 0; i < n; i++ {
		if f.allow(uniqueIP(i)) {
			allowed++
		}
	}
	return allowed
}

// TestLocalFloor_AllowsBurstThenBlocks: with a tiny refill rate, exactly `burst`
// calls pass before the GLOBAL bucket empties and further calls are rejected.
// Distinct IPs keep the per-IP sub-limit out of the way.
func TestLocalFloor_AllowsBurstThenBlocks(t *testing.T) {
	t.Setenv(envLocalFloorBurst, "2")
	t.Setenv(envLocalFloorRPS, "0.001") // ~never refills within the test

	f := newLocalFloor()
	assert.True(t, f.allow("1.1.1.1"), "1st call within burst must pass")
	assert.True(t, f.allow("1.1.1.2"), "2nd call within burst must pass")
	assert.False(t, f.allow("1.1.1.3"), "3rd call must be rejected once burst is spent")
	assert.False(t, f.allow("1.1.1.4"), "still rejected while the bucket is empty")
}

// TestLocalFloor_ZeroEnvDoesNotDisable documents the fail-safe: the shared env
// parsers coerce 0 / negative / unparseable values to the generous default, so
// setting the env to "0" does NOT disable the floor — it stays enabled at the
// default ceiling (a default-sized burst passes, one past it is still blocked).
func TestLocalFloor_ZeroEnvDoesNotDisable(t *testing.T) {
	t.Setenv(envLocalFloorRPS, "0")
	t.Setenv(envLocalFloorBurst, "0")

	f := newLocalFloor()
	allowed := countAllowedGlobal(f, 2*defaultLocalFloorBurst)
	assert.GreaterOrEqual(t, allowed, defaultLocalFloorBurst, "coerced-to-default burst must all pass")
	assert.Less(t, allowed, 2*defaultLocalFloorBurst, "env 0 did not disable the floor; it stays bounded at the default")
}

// TestLocalFloor_ConfiguredAtConstruction: env is read once when the floor is
// built, so two floors constructed under different env get independent limits.
func TestLocalFloor_ConfiguredAtConstruction(t *testing.T) {
	t.Setenv(envLocalFloorBurst, "1")
	t.Setenv(envLocalFloorRPS, "0.001")
	tight := newLocalFloor()
	assert.True(t, tight.allow("1.1.1.1"), "tight floor: first call gets the lone token")
	assert.False(t, tight.allow("1.1.1.2"), "tight floor: second call is rejected")

	t.Setenv(envLocalFloorBurst, "100")
	loose := newLocalFloor()
	for i := 0; i < 100; i++ {
		assert.Truef(t, loose.allow(uniqueIP(i)), "loose floor built later must allow its own burst (i=%d)", i)
	}
}

// TestLocalFloor_DefaultIsBackstop: with no env override the default GLOBAL
// bucket is large enough that ordinary traffic never trips it (it only acts as a
// Redis-outage backstop). A burst of default size from distinct IPs all passes.
func TestLocalFloor_DefaultIsBackstop(t *testing.T) {
	f := newLocalFloor()
	allowed := countAllowedGlobal(f, 2*defaultLocalFloorBurst)
	assert.GreaterOrEqual(t, allowed, defaultLocalFloorBurst, "the full default burst must pass")
	assert.Less(t, allowed, 2*defaultLocalFloorBurst, "default floor is bounded; not everything passes")
}

// TestLocalFloor_SingleIPCannotStarveOthers is the #287 regression: a single IP
// flooding the endpoint may consume at most its per-IP share of the global
// floor, so it CANNOT drain the global bucket and cause collateral 429s for a
// valid push from a different IP. Per-IP budget is set tiny (burst 2, ~no
// refill) while the global budget is large, so the attacker is gated by the
// per-IP layer long before the global bucket is exhausted.
func TestLocalFloor_SingleIPCannotStarveOthers(t *testing.T) {
	t.Setenv(envLocalFloorRPS, "0.001")
	t.Setenv(envLocalFloorBurst, "100")
	t.Setenv(envLocalFloorPerIPRPS, "0.001")
	t.Setenv(envLocalFloorPerIPBurst, "2")

	f := newLocalFloor()

	const attacker = "203.0.113.7"
	attackerAllowed := 0
	for i := 0; i < 1000; i++ {
		if f.allow(attacker) {
			attackerAllowed++
		}
	}
	// The attacker is capped at its per-IP burst, NOT at the global burst.
	assert.LessOrEqual(t, attackerAllowed, 2, "attacker must be gated by its per-IP share, not the global floor")

	// A different IP's valid push still passes — the global floor was not drained.
	assert.True(t, f.allow("198.51.100.9"), "a valid push from another IP must not be starved by the attacker")
}

// TestLocalFloor_GlobalCapHoldsUnderDistributedFlood: the per-IP layer must not
// remove the global per-instance cap. Many distinct IPs each staying within
// their per-IP budget still collectively cannot exceed the global ceiling — the
// Redis-outage DB/WuKongIM protection the floor exists for.
func TestLocalFloor_GlobalCapHoldsUnderDistributedFlood(t *testing.T) {
	t.Setenv(envLocalFloorRPS, "0.001")
	t.Setenv(envLocalFloorBurst, "50")
	// Generous per-IP budget so each IP individually passes; the global cap is
	// the only thing that can reject here.
	t.Setenv(envLocalFloorPerIPRPS, "0.001")
	t.Setenv(envLocalFloorPerIPBurst, "10")

	f := newLocalFloor()
	allowed := 0
	for i := 0; i < 500; i++ {
		if f.allow(uniqueIP(i)) {
			allowed++
		}
	}
	assert.Equal(t, 50, allowed, "distributed flood is still bounded by the global per-instance cap")
}

// TestPerIPFloor_MemoryBounded verifies the two-generation map keeps live
// buckets bounded under a flood of distinct IPs (no background goroutine), so a
// Redis-down distributed attack cannot grow the map without bound.
func TestPerIPFloor_MemoryBounded(t *testing.T) {
	const maxEntries = 4
	p := newPerIPFloor(rate.Limit(1000), 1000, maxEntries)

	for i := 0; i < 100; i++ {
		p.allow(uniqueIP(i))
	}

	p.mu.Lock()
	live := len(p.cur) + len(p.prev)
	p.mu.Unlock()
	assert.LessOrEqual(t, live, 2*maxEntries, "live per-IP buckets must stay bounded by ~2*maxEntries")
}

// TestPerIPFloor_PromotionKeepsActiveIPAlive: an IP that keeps pushing across a
// generation rotation is promoted rather than evicted, so its bucket (and thus
// its accumulated rate-limit state) survives — otherwise a steady IP would get a
// fresh full burst on every rotation and the per-IP cap would be meaningless.
func TestPerIPFloor_PromotionKeepsActiveIPAlive(t *testing.T) {
	const maxEntries = 4
	// burst 1, ~no refill: the active IP gets exactly one token for its lifetime.
	p := newPerIPFloor(rate.Limit(0.001), 1, maxEntries)

	const active = "203.0.113.1"
	assert.True(t, p.allow(active), "active IP spends its single token")

	// Drive enough distinct IPs to force several rotations; touch `active` each
	// round so it keeps getting promoted into the current generation.
	for round := 0; round < 5; round++ {
		for i := 0; i < maxEntries; i++ {
			p.allow(fmt.Sprintf("round%d-%d", round, i))
		}
		assert.Falsef(t, p.allow(active), "active IP must stay throttled across rotation (round=%d)", round)
	}
}

// TestPerIPFloor_UnknownIPShareOneBucket: requests with an unresolved IP ("")
// share a single bucket rather than each getting a fresh one, so an
// unresolved-IP flood is throttled together instead of bypassing the per-IP cap.
func TestPerIPFloor_UnknownIPShareOneBucket(t *testing.T) {
	p := newPerIPFloor(rate.Limit(0.001), 2, 100)
	assert.True(t, p.allow(""), "1st unknown-IP call gets a token")
	assert.True(t, p.allow(""), "2nd unknown-IP call gets the last token")
	assert.False(t, p.allow(""), "3rd unknown-IP call is throttled — all share one bucket")

	p.mu.Lock()
	_, ok := p.cur[perIPUnknownKey]
	p.mu.Unlock()
	assert.True(t, ok, "unresolved IPs must collapse onto the shared unknown-IP key")
}
