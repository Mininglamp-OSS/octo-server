package project

import (
	"strings"
	"testing"
)

// The lifecycle event secret must not double as any other capability credential.
//
// main.go's registry sees this pair too, but it only LOGS — both capabilities
// stay live. A logged collision on a security credential is a collision that
// ships, so the refusal has to exist on at least one side. These tests pin that
// this side is one of them.

func fakeEnv(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// TestLifecycleSecretIsRefusedWhenItCollides covers every sibling, not a
// representative one: a list-driven check that only tested its first entry would
// pass with the other nine dropped.
func TestLifecycleSecretIsRefusedWhenItCollides(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	for _, sibling := range lifecycleSecretSiblings {
		t.Run(sibling, func(t *testing.T) {
			got, err := resolveLifecycleEventSecret(fakeEnv(map[string]string{
				LifecycleEventSecretEnv: secret,
				sibling:                 secret,
			}))
			if err == nil {
				t.Fatalf("sharing a value with %s must be refused", sibling)
			}
			if got != "" {
				t.Error("the secret must be DROPPED, not returned with a warning: " +
					"lifecycleEventsEnabled() requires a non-empty secret, and that is what " +
					"turns the refusal into a disabled feed rather than a log line")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error must name ENVs, never the value: %q", err)
			}
			if !strings.Contains(err.Error(), sibling) {
				t.Errorf("the error must name the colliding env, got %q", err)
			}
		})
	}
}

// TestLifecycleSecretSurvivesDistinctSiblings is the other half. A refusal that
// also fires on distinct values would take the feed down on every deployment
// that configures more than one capability.
func TestLifecycleSecretSurvivesDistinctSiblings(t *testing.T) {
	env := map[string]string{LifecycleEventSecretEnv: strings.Repeat("a", 32)}
	for i, sibling := range lifecycleSecretSiblings {
		env[sibling] = strings.Repeat("b", 30) + string(rune('A'+i))
	}
	got, err := resolveLifecycleEventSecret(fakeEnv(env))
	if err != nil {
		t.Fatalf("distinct values must be accepted, got %v", err)
	}
	if got != strings.Repeat("a", 32) {
		t.Fatalf("the secret must come back unchanged, got %q", got)
	}
}

// TestUnsetSiblingsAreNotCollisions: an unconfigured capability is disabled, not
// colliding. Treating several empty strings as equal would disable the feed on
// every deployment that has not turned everything else on.
func TestUnsetSiblingsAreNotCollisions(t *testing.T) {
	got, err := resolveLifecycleEventSecret(fakeEnv(map[string]string{
		LifecycleEventSecretEnv: strings.Repeat("a", 32),
	}))
	if err != nil {
		t.Fatalf("unset siblings must not read as collisions, got %v", err)
	}
	if got == "" {
		t.Fatal("the secret must survive")
	}
	// And an unset secret with unset siblings is simply off, not an error.
	got, err = resolveLifecycleEventSecret(fakeEnv(map[string]string{}))
	if err != nil || got != "" {
		t.Fatalf("an unset secret is off, not an error: %q %v", got, err)
	}
}

// TestLifecycleSecretSiblingsMatchTheProvisioningList keeps the two module-local
// lists from drifting apart.
//
// They guard the same property from opposite ends — a provisioning secret must
// differ from the lifecycle secret, and vice versa — so the lifecycle list must
// be the provisioning sibling list, minus the two provisioning envs it checks
// intrinsically, plus those two (which ARE siblings from this side) and minus
// its own env. Concretely: every env the provisioning check refuses, this one
// refuses too.
func TestLifecycleSecretSiblingsMatchTheProvisioningList(t *testing.T) {
	inLifecycle := make(map[string]bool, len(lifecycleSecretSiblings))
	for _, e := range lifecycleSecretSiblings {
		inLifecycle[e] = true
	}
	for _, e := range []string{
		siblingNotifyTokenEnv,
		siblingDocsNotifyTokenEnv,
		siblingBotMentionTokenEnv,
		siblingDriveInternalToken,
		siblingMembershipTokenEnv,
		siblingWebhookSecretEnv,
		siblingMailGatewaySecret,
		siblingGRPCAuthTokenEnv,
	} {
		if !inLifecycle[e] {
			t.Errorf("%s is refused for a provisioning secret but not for the lifecycle "+
				"secret; one leaked value would still grant two capabilities", e)
		}
	}
	// And the fleet provisioning secret, which the provisioning path checks against
	// the rest rather than through that list.
	//
	// One secret, not two: #887 stopped giving Drive its own per-target secret and
	// pointed provisioning at the existing OCTO_DRIVE_INTERNAL_TOKEN, which is
	// already covered by siblingDriveInternalToken above. Asserting a second one
	// here would be asserting an env nothing reads.
	for _, e := range []string{ProvisionFleetSecretEnv} {
		if !inLifecycle[e] {
			t.Errorf("%s must be a lifecycle sibling: both secrets go to the same peer, and "+
				"one value for both means a leak from either grants both", e)
		}
	}
	if inLifecycle[LifecycleEventSecretEnv] {
		t.Error("the list must not contain the env it guards; it would refuse every value")
	}
}

// TestProvisioningRefusesTheLifecycleSecret is the reciprocal direction, proven
// against the real config loader rather than by reading the list.
func TestProvisioningRefusesTheLifecycleSecret(t *testing.T) {
	const shared = "0123456789abcdef0123456789abcdef"
	cfg, problems := loadProvisioningConfig(fakeEnv(map[string]string{
		"OCTO_PROJECT_PROVISION_TARGETS":   "fleet",
		"OCTO_PROJECT_PROVISION_FLEET_URL": "https://fleet.invalid/internal/workspaces/ensure",
		ProvisionFleetSecretEnv:            shared,
		LifecycleEventSecretEnv:            shared,
	}))
	if len(problems) == 0 {
		t.Fatal("a provisioning secret equal to the lifecycle secret must be refused")
	}
	if len(cfg.Targets) != 0 {
		t.Error("the target must be dropped, not merely reported: a reported-but-live target " +
			"is one credential serving two capabilities")
	}
	joined := ""
	for _, p := range problems {
		joined += p.Error() + "\n"
	}
	if !strings.Contains(joined, LifecycleEventSecretEnv) {
		t.Errorf("the problem must name the colliding env, got %q", joined)
	}
	if strings.Contains(joined, shared) {
		t.Errorf("the problem must never carry the value: %q", joined)
	}
}
