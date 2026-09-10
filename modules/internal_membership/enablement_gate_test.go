package internal_membership

// The enablement gate, driven through New().
//
// # Why this exists
//
// The safety argument for shipping these endpoints with the account axis open is
// "the peer bounds its cached decisions by time, and the token stays unset until it
// does". For sixteen review rounds that argument lived in a PR description. A
// release condition that only a reviewer's memory enforces is exactly the shape
// this branch spent those rounds eliminating from the CODE — a hand-maintained
// thing that has to be remembered — so it is now enforced the same way: setting the
// token alone does not enable the capability.
//
// These cases drive New() rather than the resolver, because the property that
// matters is not "the resolver returns an error" — it is that the module comes out
// UNABLE TO AUTHENTICATE ANYONE when the declaration is missing. A resolver test
// would keep passing if a future edit logged the error and kept the token.

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// enabledEnv sets the token to a value that clears every other check, so each case
// below varies exactly one thing: the declared bound.
func enabledEnv(t *testing.T, bound string) {
	t.Helper()
	t.Setenv(MembershipInternalTokenEnv, testInternalToken)
	if bound == "" {
		// Setenv then unset, so a value inherited from the developer's shell cannot
		// make the "missing" case pass for the wrong reason.
		t.Setenv(PeerMaxCacheAgeEnv, "")
		return
	}
	t.Setenv(PeerMaxCacheAgeEnv, bound)
}

func TestTokenWithoutADeclaredPeerCacheBoundDoesNotEnableTheCapability(t *testing.T) {
	enabledEnv(t, "")

	m := New(nil)

	if m.internalToken != "" {
		t.Fatal("setting only " + MembershipInternalTokenEnv + " must NOT enable these " +
			"endpoints.\n\nThey answer a peer that caches authorization decisions, and " +
			"member_epoch cannot invalidate an account ban or destroy — the epoch is per " +
			"project and the ban is per user, so a cached grant keeps its agreement and a " +
			"cached denial survives the unban. The only bound on that axis is time, on the " +
			"consumer's side. " + PeerMaxCacheAgeEnv + " is where the operator states it, " +
			"and it is required precisely so enabling this halfway is not something a " +
			"deployment can do by accident.")
	}
	if m.peerCacheAgeSeconds != 0 {
		t.Fatalf("a refused declaration must leave no bound in force, got %d", m.peerCacheAgeSeconds)
	}
	if got := testutil.ToFloat64(peerMaxCacheAge); got != 0 {
		t.Fatalf("the published bound must be 0 when the capability is off, got %v", got)
	}
}

func TestDeclaringThePeerCacheBoundEnablesTheCapability(t *testing.T) {
	enabledEnv(t, "30")

	m := New(nil)

	if m.internalToken != testInternalToken {
		t.Fatal("a valid token beside a valid declared bound must enable the endpoints — " +
			"the gate must not be a way to turn the capability off permanently")
	}
	if m.peerCacheAgeSeconds != 30 {
		t.Fatalf("the declared bound must be carried on the module so the wire field in §3.1 "+
			"of the cache-bound proposal has one source when it lands; got %d", m.peerCacheAgeSeconds)
	}
	if got := testutil.ToFloat64(peerMaxCacheAge); got != 30 {
		t.Fatalf("the declared bound must be published, or 'what bound did we agree' has no "+
			"answer outside somebody's memory; got %v", got)
	}
}

// TestAnAbsurdOrMalformedPeerCacheBoundIsRefused is the half that makes the gate
// more than a checkbox.
//
// A gate satisfied by any string is satisfied by "0" and by a day, and an operator
// who set either would believe they had complied. Refusing is the only outcome that
// tells them otherwise.
func TestAnAbsurdOrMalformedPeerCacheBoundIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		value string
		why   string
	}{
		{"zero", "0", "0 seconds is not a short bound, it is no bound expressed as a number"},
		{"negative", "-30", "a negative duration cannot bound anything"},
		{"beyond the ceiling", "86400", "a day-long window is indistinguishable from unbounded " +
			"for a revoked account, and would read as a bound in the ConfigMap"},
		{"not a number", "30s", "a duration string is the most likely wrong spelling, and " +
			"silently coercing it would enable the capability under a value nobody chose"},
		{"empty-ish", "  ", "whitespace is what a half-edited ConfigMap value looks like"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enabledEnv(t, c.value)

			m := New(nil)

			if m.internalToken != "" {
				t.Fatalf("%s must be refused: %s", c.value, c.why)
			}
			if m.peerCacheAgeSeconds != 0 {
				t.Fatalf("a refused declaration must leave no bound in force, got %d",
					m.peerCacheAgeSeconds)
			}
		})
	}
}

// TestThePeerCacheBoundIsNotConsultedWhileTheCapabilityIsOff keeps the common
// deployment quiet.
//
// Every deployment that has correctly left this module disabled would otherwise get
// an ERROR line about a bound it does not need, on every boot — which is how a
// logger stops being read.
func TestThePeerCacheBoundIsNotConsultedWhileTheCapabilityIsOff(t *testing.T) {
	t.Setenv(MembershipInternalTokenEnv, "")
	t.Setenv(PeerMaxCacheAgeEnv, "")

	m := New(nil)

	if m.internalToken != "" {
		t.Fatal("no token means no capability")
	}
	if m.peerCacheAgeSeconds != 0 {
		t.Fatalf("no bound is in force when the capability is off, got %d", m.peerCacheAgeSeconds)
	}
}

// TestThePeerCacheBoundRefusalNamesTheEnvAndTheReason is what an operator actually
// has to work from: the endpoints answer 401 for "unset" and "wrong" alike, so the
// log line and the gauge are the only channel that distinguishes them.
func TestThePeerCacheBoundRefusalNamesTheEnvAndTheReason(t *testing.T) {
	_, err := resolvePeerCacheAgeBound(func(string) string { return "" })
	if err == nil {
		t.Fatal("a missing declaration must be an error")
	}
	for _, want := range []string{PeerMaxCacheAgeEnv, MembershipInternalTokenEnv, "disabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so an operator can act on it; got %q", want, err.Error())
		}
	}

	_, err = resolvePeerCacheAgeBound(func(string) string { return "30s" })
	if err == nil {
		t.Fatal("a malformed declaration must be an error")
	}
	// The value is echoed on purpose: unlike the token it is not a secret, and a
	// refusal that hides the offending value makes a typo unfindable.
	if !strings.Contains(err.Error(), "30s") {
		t.Errorf("the refusal must echo the offending value; got %q", err.Error())
	}
}
