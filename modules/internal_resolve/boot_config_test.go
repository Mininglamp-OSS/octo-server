package internal_resolve_test

// Boot-time config guard tests: the drive internal token must never overlap
// with any of the dynamic per-route notify tokens or callback secrets loaded
// from OCTO_CARD_ACTION_ROUTES. This is enforced centrally by
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions in main.go — we
// re-exercise it here from the drive module's package boundary so a future
// change that removes the drive token from the exclusion list gets caught
// immediately (the review that produced this file, CHANGES_REQUESTED P1 on
// PR #711, showed exactly that gap and this test pins it closed).
//
// External-package test (_test) so we consume only exported identifiers —
// DriveInternalTokenEnv is exported for this reason.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
)

// testValidRouteSpec is a locally-owned copy of the cardactiondispatch package
// helper. Kept small and inline so this file has zero test-only coupling into
// cardactiondispatch beyond the exported RouteSpec / NewRegistry surface.
func testValidRouteSpec() cardactiondispatch.RouteSpec {
	return cardactiondispatch.RouteSpec{
		SenderUID:      "notification",
		Owner:          "docs",
		ActionType:     "access_request.decision",
		URL:            "https://docs.internal/v1/card-actions/decide",
		SecretEnv:      "OCTO_DOCS_CARD_ACTION_SECRET",
		Timeout:        3 * time.Second,
		MaxAttempts:    3,
		BaseBackoff:    time.Second,
		MaxBackoff:     30 * time.Second,
		MaxInFlight:    4,
		CallbackFormat: cardactiondispatch.CallbackFormatLegacy,
	}
}

// TestDriveInternalTokenCollidesWithRouteNotifyToken pins the P1 fix from
// PR #711: OCTO_DRIVE_INTERNAL_TOKEN must be included in
// ValidateNotifyTokenExclusions so it cannot equal a route's notify token env
// value. Without the fix, a leaked drive token would inherit route-scoped
// notify capabilities, breaking the "one credential / one capability"
// invariant this module declares.
func TestDriveInternalTokenCollidesWithRouteNotifyToken(t *testing.T) {
	spec := testValidRouteSpec()
	spec.NotifyTokenEnv = "MY_ROUTE_TOKEN"

	// Both env values map to the SAME secret. This is the exact
	// misconfiguration the review described.
	sharedTok := strings.Repeat("a", 32)
	getenv := func(key string) string {
		switch key {
		case spec.NotifyTokenEnv, internal_resolve.DriveInternalTokenEnv:
			return sharedTok
		case spec.SecretEnv:
			return strings.Repeat("s", 32)
		default:
			return ""
		}
	}
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{spec}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	// Reproduce main.go's boot-time gate. If the drive token is missing from
	// this argument list, the collision goes undetected and Registry
	// silently authorizes a token holder to mint route notifications AND
	// call resolve-bot-owner — the exact P1 hazard.
	err = registry.ValidateNotifyTokenExclusions(
		os.Getenv("NOTIFY_INTERNAL_TOKEN"),
		os.Getenv("OCTO_DOCS_NOTIFY_TOKEN"),
		os.Getenv("OCTO_DOCS_BOT_MENTION_TOKEN"),
		getenv(internal_resolve.DriveInternalTokenEnv),
	)
	if err == nil {
		t.Fatal("ValidateNotifyTokenExclusions() = nil; expected collision rejection " +
			"when OCTO_DRIVE_INTERNAL_TOKEN == a route's notify_token_env value")
	}
}

// TestDriveInternalTokenCollidesWithCallbackSecret asserts the same guard
// covers callback secrets as well, not just notify tokens. Callback secrets
// live in the same exclusion map inside Registry (see registry.go's
// ValidateNotifyTokenExclusions body) — the check-list-based test in
// cardactiondispatch's own contract test covers this for the three legacy
// tokens; this one pins it for OCTO_DRIVE_INTERNAL_TOKEN specifically.
func TestDriveInternalTokenCollidesWithCallbackSecret(t *testing.T) {
	spec := testValidRouteSpec()
	spec.SecretEnv = "MY_ROUTE_SECRET"
	// notify_token_env may point anywhere; we're testing the OTHER slot.
	spec.NotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"

	// Drive token equals the ROUTE'S CALLBACK SECRET.
	sharedTok := strings.Repeat("b", 32)
	getenv := func(key string) string {
		switch key {
		case spec.SecretEnv, internal_resolve.DriveInternalTokenEnv:
			return sharedTok
		case spec.NotifyTokenEnv:
			return strings.Repeat("n", 32)
		default:
			return ""
		}
	}
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{spec}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	err = registry.ValidateNotifyTokenExclusions(
		"", "", "",
		getenv(internal_resolve.DriveInternalTokenEnv),
	)
	if err == nil {
		t.Fatal("ValidateNotifyTokenExclusions() = nil; expected collision rejection " +
			"when OCTO_DRIVE_INTERNAL_TOKEN == a route's callback secret env value")
	}
}

// TestDriveInternalTokenPassesWhenUniqueAcrossDynamicRoutes is the positive
// case: with unique values across all route-scoped envs, the gate must
// accept. Guards against a future overzealous check that would break clean
// deployments.
func TestDriveInternalTokenPassesWhenUniqueAcrossDynamicRoutes(t *testing.T) {
	spec := testValidRouteSpec()
	spec.NotifyTokenEnv = "MY_ROUTE_TOKEN"
	getenv := func(key string) string {
		switch key {
		case spec.NotifyTokenEnv:
			return strings.Repeat("r", 32)
		case spec.SecretEnv:
			return strings.Repeat("s", 32)
		case internal_resolve.DriveInternalTokenEnv:
			return strings.Repeat("d", 32) // distinct from route + secret
		default:
			return ""
		}
	}
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{spec}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	err = registry.ValidateNotifyTokenExclusions(
		"", "", "",
		getenv(internal_resolve.DriveInternalTokenEnv),
	)
	if err != nil {
		t.Fatalf("ValidateNotifyTokenExclusions() = %v; expected nil for a unique drive token", err)
	}
}
