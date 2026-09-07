package internal_membership_test

// Source guards for the boot-time wiring in main.go.
//
// The token-collision tests in config_test.go build their own environment, so
// they would keep passing even if main.go stopped wiring this module's token
// into the startup checks entirely. These guards close that loop.
//
// They are file greps rather than runtime assertions because the checks run
// inside installCardActionDispatch, which needs a real config, database and
// Redis to invoke. Coarser, but it gives tamper detection with no fixtures.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the current file")
	}
	// .../modules/internal_membership/main_wiring_test.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

func mainSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(raw)
}

// TestMainWiresMembershipTokenIntoNotifyExclusions guards the argument that
// checks this module's token against the DYNAMIC route-scoped notify tokens and
// callback secrets loaded from OCTO_CARD_ACTION_ROUTES. The module itself only
// sees the fixed internal-token envs and cannot detect that collision.
func TestMainWiresMembershipTokenIntoNotifyExclusions(t *testing.T) {
	src := mainSource(t)
	call := "ValidateNotifyTokenExclusions("
	start := strings.Index(src, call)
	if start < 0 {
		t.Fatal("main.go no longer calls ValidateNotifyTokenExclusions — if it moved, point this guard at the new location rather than deleting it")
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n\t); err != nil"); end > 0 {
		rest = rest[:end]
	}
	if !strings.Contains(rest, "internal_membership.MembershipInternalTokenEnv") {
		t.Error("main.go must pass internal_membership.MembershipInternalTokenEnv to " +
			"ValidateNotifyTokenExclusions, or a route-scoped credential could be reused " +
			"as the membership internal token")
	}
}

// TestMainReportsFixedTokenCollisions guards the startup visibility check for
// collisions between fixed internal-token envs.
//
// It reports rather than refuses: fixed-vs-fixed collisions disable the affected
// capability module-locally and the server still boots, which is the posture
// TestCardActionDispatchScopesBotMentionTokenCollisionFailures pins. The value
// here is that the module-local checks are asymmetric — notify checks one
// sibling, bot_mention two, internal_resolve three — so a pair neither side
// covers would otherwise leave two capabilities on one credential with nothing
// saying so.
func TestMainReportsFixedTokenCollisions(t *testing.T) {
	src := mainSource(t)
	if !strings.Contains(src, "fixedInternalTokenCollisions(os.Getenv)") {
		t.Error("main.go must call fixedInternalTokenCollisions at startup; " +
			"without it, a collision between two fixed internal-token envs that no " +
			"module-local check covers would go unreported")
	}
	if !strings.Contains(src, "internal_membership.MembershipInternalTokenEnv") {
		t.Error("the membership token env must be part of fixedInternalTokenEnvs")
	}
}
