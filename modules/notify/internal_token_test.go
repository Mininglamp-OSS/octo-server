package notify

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/internaltoken"
)

// This module owns two of the fixed internal-token envs. Both now resolve
// through pkg/internaltoken instead of the hand-rolled comparison that used to
// live in New().

// resolveOne runs one of this module's two envs through the production resolver
// (#827's resolveInternalTokens, which now delegates to pkg/internaltoken) and
// returns just that env's token, so the directional assertions below stay
// readable.
func resolveOne(t *testing.T, env string, getenv func(string) string) string {
	t.Helper()
	token, docsToken, _, _ := resolveInternalTokens(getenv)
	switch env {
	case internaltoken.NotifyInternalTokenEnv:
		return token
	case internaltoken.DocsNotifyTokenEnv:
		return docsToken
	default:
		t.Fatalf("resolveOne: %s is not owned by modules/notify", env)
		return ""
	}
}

func TestResolveNotifyTokenAcceptsDistinctValues(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case internaltoken.NotifyInternalTokenEnv:
			return "legacy-notify-token"
		case internaltoken.DocsNotifyTokenEnv:
			return "docs-notify-token"
		default:
			return ""
		}
	}
	if got := resolveOne(t, internaltoken.NotifyInternalTokenEnv, getenv); got != "legacy-notify-token" {
		t.Fatalf("legacy token = %q, want it enabled", got)
	}
	if got := resolveOne(t, internaltoken.DocsNotifyTokenEnv, getenv); got != "docs-notify-token" {
		t.Fatalf("docs token = %q, want it enabled", got)
	}
}

// TestResolveNotifyTokenHasNoLengthFloor pins the legacy waiver these two envs
// carry: values below internaltoken.DefaultMinBytes still enable the
// capability. Raising the bar would disable a live ingress on the next deploy
// of any installation running a shorter secret, so it is a rollout decision,
// not something this refactor makes silently. Deployments in the tree (see
// pilote2e) do run short values.
func TestResolveNotifyTokenHasNoLengthFloor(t *testing.T) {
	short := "short-token"
	if len(short) >= internaltoken.DefaultMinBytes {
		t.Fatalf("fixture is %d bytes; it must be under the %d-byte shared floor to test the waiver",
			len(short), internaltoken.DefaultMinBytes)
	}
	getenv := func(key string) string {
		if key == internaltoken.NotifyInternalTokenEnv {
			return short
		}
		return ""
	}
	if got := resolveOne(t, internaltoken.NotifyInternalTokenEnv, getenv); got != short {
		t.Fatalf("token = %q, want the short value accepted under the legacy waiver", got)
	}
}

func TestResolveNotifyTokenUnsetFailsClosed(t *testing.T) {
	getenv := func(string) string { return "" }
	for _, env := range []string{internaltoken.NotifyInternalTokenEnv, internaltoken.DocsNotifyTokenEnv} {
		if got := resolveOne(t, env, getenv); got != "" {
			t.Fatalf("%s unset resolved to %q; must be empty so authInternal rejects every request", env, got)
		}
	}
}

// TestResolveNotifyTokenCollisionDisablesTheJuniorCapability pins the
// precedence rule for this module's own pair, which is also the behaviour the
// hand-rolled check in New() had before the registry existed: with both envs
// set to the same value, the legacy ingress keeps serving and the docs
// capability — the newcomer that duplicated an existing secret — is the one
// that goes dark.
//
// Disabling both would satisfy the invariant too, but it would take a working
// production ingress offline on the next rolling deploy for a misconfiguration
// whose damage is already contained once either side is off. Boot succeeds
// either way; this is a module-local degradation, not a startup failure.
func TestResolveNotifyTokenCollisionDisablesTheJuniorCapability(t *testing.T) {
	const shared = "shared-internal-token-value-0000"
	getenv := func(key string) string {
		switch key {
		case internaltoken.NotifyInternalTokenEnv, internaltoken.DocsNotifyTokenEnv:
			return shared
		default:
			return ""
		}
	}
	if got := resolveOne(t, internaltoken.NotifyInternalTokenEnv, getenv); got != shared {
		t.Fatalf("legacy token = %q; the senior capability must keep serving", got)
	}
	if got := resolveOne(t, internaltoken.DocsNotifyTokenEnv, getenv); got != "" {
		t.Fatalf("docs token = %q on collision; the junior capability must fail closed", got)
	}
}

// TestResolveNotifyTokenCoversEverySibling is the coverage this module never
// had: New() used to compare its two envs only against each other, so a value
// shared with the bot-mention or drive capability went undetected here. Under
// the registry's precedence rule the junior side yields, so OCTO_DOCS_NOTIFY_TOKEN
// is now guarded against every env registered before it — and both of this
// module's envs are guarded from the other direction too, by the junior module
// disabling itself.
func TestResolveNotifyTokenCoversEverySibling(t *testing.T) {
	const shared = "shared-internal-token-value-0000"
	owned := []string{internaltoken.NotifyInternalTokenEnv, internaltoken.DocsNotifyTokenEnv}

	yielding := 0
	for _, subject := range owned {
		for _, sibling := range internaltoken.Envs() {
			if sibling == subject {
				continue
			}
			getenv := func(key string) string {
				if key == subject || key == sibling {
					return shared
				}
				return ""
			}
			// Branch on the registry's own rule. NOTIFY_INTERNAL_TOKEN is
			// registered first, so it yields to nothing by precedence — but it
			// still yields to a Mutual env, which is how #827's marketplace
			// pair behaves and why this cannot be a "seniors only" test.
			if internaltoken.Yields(subject, sibling) {
				yielding++
				t.Run(subject+"_yields_to_"+sibling, func(t *testing.T) {
					if got := resolveOne(t, subject, getenv); got != "" {
						t.Fatalf("%s = %q while sharing a value with %s; want it disabled",
							subject, got, sibling)
					}
				})
				continue
			}
			t.Run(subject+"_outranks_"+sibling, func(t *testing.T) {
				if got := resolveOne(t, subject, getenv); got != shared {
					t.Fatalf("%s = %q while sharing a value with the junior env %s; the senior "+
						"capability must keep serving", subject, got, sibling)
				}
			})
		}
	}
	if yielding == 0 {
		t.Fatal("neither notify env yields to anything; the guard would be vacuous")
	}
}

func TestNotifyOwnsItsEnvsInTheSharedRegistry(t *testing.T) {
	for _, env := range []string{internaltoken.NotifyInternalTokenEnv, internaltoken.DocsNotifyTokenEnv} {
		if !internaltoken.Registered(env) {
			t.Fatalf("%s is not in pkg/internaltoken's registry; it would bypass every collision check", env)
		}
	}
}
