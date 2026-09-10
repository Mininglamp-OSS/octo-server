package main

// Boot-time config guard tests for OCTO_MARKETPLACE_INTERNAL_TOKEN.
//
// The token authorizes reading a uid's role in any Space. If it ever equalled
// one of the *dynamic* per-route notify tokens or callback secrets loaded from
// OCTO_CARD_ACTION_ROUTES, a single leaked value would grant both the Space role
// lookup AND route notify — the "one credential / one capability" invariant this
// repository enforces would be broken. That cross-capability check can only
// happen where the dynamic route specs are visible, i.e.
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions in main.go.
//
// These tests reproduce main.go's argument list against a synthetic getenv and
// assert the gate actually trips. modules/space/main_wiring_test.go complements
// them by asserting the production call site still passes our token — without
// that, these tests would keep passing after someone deleted the argument from
// main.go, because they build their own argument list.
//
// # Why this lives in package main and not in modules/space
//
// It was written as modules/space/boot_config_test.go, mirroring
// modules/internal_resolve/boot_config_test.go. That location breaks the whole
// modules/space package: importing internal/cardactiondispatch transitively
// pulls in modules/group, modules/user and modules/thread, whose init() register
// their modules — so module.Setup() in that package's TestMain then runs their
// migrations against the shared `test` database and 20191106000002_group_legacy01
// fails with "Table 'group' already exists" against the fixture table TestMain
// pre-creates for the space migrations. The panic takes down every test in the
// package, not just this file.
//
// package main already links the entire module graph, so the import costs
// nothing here. These tests only consume exported identifiers
// (space.MarketplaceInternalTokenEnv is exported for exactly this reason), so
// the guard is unchanged in strength by the move.

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
)

// marketplaceTestRouteSpec is a locally-owned minimal valid RouteSpec, kept
// inline so this file couples to nothing beyond the exported RouteSpec /
// NewRegistry surface.
func marketplaceTestRouteSpec() cardactiondispatch.RouteSpec {
	return cardactiondispatch.RouteSpec{
		SenderUID:      "notification",
		Owner:          "marketplace",
		ActionType:     "marketplace.plugin_review.decision",
		URL:            "https://marketplace.internal/v1/card-actions/decide",
		SecretEnv:      "OCTO_MARKETPLACE_CARD_ACTION_SECRET",
		Timeout:        3 * time.Second,
		MaxAttempts:    3,
		BaseBackoff:    time.Second,
		MaxBackoff:     30 * time.Second,
		MaxInFlight:    4,
		CallbackFormat: cardactiondispatch.CallbackFormatLegacy,
	}
}

// validateMarketplaceTokenAsMainDoes reproduces main.go's
// ValidateNotifyTokenExclusions argument list. Keep the shape identical to
// installCardActionDispatch — if main.go grows another fixed internal token, add
// it here too.
func validateMarketplaceTokenAsMainDoes(registry *cardactiondispatch.Registry, getenv func(string) string) error {
	return registry.ValidateNotifyTokenExclusions(
		getenv("NOTIFY_INTERNAL_TOKEN"),
		getenv("OCTO_DOCS_NOTIFY_TOKEN"),
		getenv("OCTO_DOCS_BOT_MENTION_TOKEN"),
		getenv("OCTO_DRIVE_INTERNAL_TOKEN"),
		getenv(space.MarketplaceInternalTokenEnv),
	)
}

// TestMarketplaceInternalTokenCollidesWithRouteNotifyToken: the marketplace
// token must not equal a route's notify_token_env value.
func TestMarketplaceInternalTokenCollidesWithRouteNotifyToken(t *testing.T) {
	spec := marketplaceTestRouteSpec()
	spec.NotifyTokenEnv = "MY_ROUTE_TOKEN"

	sharedTok := strings.Repeat("a", 32)
	getenv := func(key string) string {
		switch key {
		case spec.NotifyTokenEnv, space.MarketplaceInternalTokenEnv:
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
	if err := validateMarketplaceTokenAsMainDoes(registry, getenv); err == nil {
		t.Fatalf("ValidateNotifyTokenExclusions() = nil; expected rejection when %s == a "+
			"route's notify_token_env value — one leaked value would grant both the "+
			"Space role lookup and route notify", space.MarketplaceInternalTokenEnv)
	}
}

// TestMarketplaceInternalTokenCollidesWithCallbackSecret: same guard must
// cover the route callback secrets, not only notify tokens.
func TestMarketplaceInternalTokenCollidesWithCallbackSecret(t *testing.T) {
	spec := marketplaceTestRouteSpec()
	spec.SecretEnv = "MY_ROUTE_SECRET"
	spec.NotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"

	sharedTok := strings.Repeat("b", 32)
	getenv := func(key string) string {
		switch key {
		case spec.SecretEnv, space.MarketplaceInternalTokenEnv:
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
	if err := validateMarketplaceTokenAsMainDoes(registry, getenv); err == nil {
		t.Fatalf("ValidateNotifyTokenExclusions() = nil; expected rejection when %s == a "+
			"route's callback secret env value", space.MarketplaceInternalTokenEnv)
	}
}

// TestMarketplaceInternalTokenPassesWhenUnique is the negative control: a
// clean deployment with distinct values must boot. Guards against an
// overzealous future check that would reject valid configuration.
func TestMarketplaceInternalTokenPassesWhenUnique(t *testing.T) {
	spec := marketplaceTestRouteSpec()
	spec.NotifyTokenEnv = "MY_ROUTE_TOKEN"
	getenv := func(key string) string {
		switch key {
		case spec.NotifyTokenEnv:
			return strings.Repeat("r", 32)
		case spec.SecretEnv:
			return strings.Repeat("s", 32)
		case "OCTO_DRIVE_INTERNAL_TOKEN":
			return strings.Repeat("d", 32)
		case space.MarketplaceInternalTokenEnv:
			return strings.Repeat("m", 32) // distinct from every other value
		default:
			return ""
		}
	}
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{spec}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	if err := validateMarketplaceTokenAsMainDoes(registry, getenv); err != nil {
		t.Fatalf("ValidateNotifyTokenExclusions() = %v; expected nil for a unique "+
			"%s", err, space.MarketplaceInternalTokenEnv)
	}
}
