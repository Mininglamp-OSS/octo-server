package main

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/internal_membership"
	"github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
	"github.com/Mininglamp-OSS/octo-server/modules/project"
)

// Tests for the exhaustive pairwise collision DETECTION across fixed
// internal-token envs.
//
// Detection, not enforcement, and the distinction is deliberate. This
// repository's posture — pinned by
// TestCardActionDispatchScopesBotMentionTokenCollisionFailures — is that a
// collision between two FIXED internal-token envs disables the affected
// capability module-locally and lets the server boot, while a collision with a
// DYNAMIC route credential fails startup. Escalating the fixed-vs-fixed case to
// a boot failure would refuse to start deployments that are running with the
// collision today, which is a rollout decision, not a bug fix.
//
// What this adds is visibility: the module-local checks are asymmetric — notify
// checks one sibling, bot_mention two, internal_resolve three — so a pair
// neither side checks leaves both capabilities enabled on one credential with
// nothing saying so. main.go is the only place that sees every env.

func envFromMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestFixedInternalTokenExclusionsAcceptDistinctValues(t *testing.T) {
	got := fixedInternalTokenCollisions(envFromMap(map[string]string{
		"NOTIFY_INTERNAL_TOKEN":                        strings.Repeat("a", 32),
		"OCTO_DOCS_NOTIFY_TOKEN":                       strings.Repeat("b", 32),
		"OCTO_DOCS_BOT_MENTION_TOKEN":                  strings.Repeat("c", 32),
		internal_resolve.DriveInternalTokenEnv:         strings.Repeat("d", 32),
		internal_membership.MembershipInternalTokenEnv: strings.Repeat("e", 32),
		project.ProvisionFleetSecretEnv:                strings.Repeat("f", 32),
		project.ProvisionDriveSecretEnv:                strings.Repeat("g", 32),
	}))
	if len(got) != 0 {
		t.Fatalf("distinct tokens must report no collision, got %v", got)
	}
}

func TestFixedInternalTokenExclusionsAllowUnsetEnvs(t *testing.T) {
	// The common deployment shape: most capabilities are off, so several envs
	// are empty. Empty must not count as a collision or nothing would boot.
	got := fixedInternalTokenCollisions(envFromMap(map[string]string{
		internal_membership.MembershipInternalTokenEnv: strings.Repeat("e", 32),
		project.ProvisionFleetSecretEnv:                strings.Repeat("f", 32),
		project.ProvisionDriveSecretEnv:                strings.Repeat("g", 32),
	}))
	if len(got) != 0 {
		t.Fatalf("unset siblings must not collide, got %v", got)
	}
	if got := fixedInternalTokenCollisions(envFromMap(map[string]string{})); len(got) != 0 {
		t.Fatalf("an entirely unconfigured deployment must report nothing, got %v", got)
	}
}

// TestFixedInternalTokenExclusionsCatchEveryPair is the point of the central
// check: it must reject a collision between ANY two envs, including pairs that
// no module-local switch covers.
func TestFixedInternalTokenExclusionsCatchEveryPair(t *testing.T) {
	shared := strings.Repeat("x", 32)
	for i := 0; i < len(fixedInternalTokenEnvs); i++ {
		for j := i + 1; j < len(fixedInternalTokenEnvs); j++ {
			a, b := fixedInternalTokenEnvs[i], fixedInternalTokenEnvs[j]
			t.Run(a+"_vs_"+b, func(t *testing.T) {
				got := fixedInternalTokenCollisions(envFromMap(map[string]string{
					a: shared,
					b: shared,
				}))
				if len(got) != 1 {
					t.Fatalf("%s and %s share a value and must be reported, got %v", a, b, got)
				}
				named := got[0][0] + " " + got[0][1]
				if !strings.Contains(named, a) || !strings.Contains(named, b) {
					t.Errorf("report should name both envs, got %v", got[0])
				}
				if strings.Contains(named, shared) {
					t.Errorf("report leaked the token value: %v", got[0])
				}
			})
		}
	}
}

func TestFixedInternalTokenExclusionsTolerateNoEnvAccessor(t *testing.T) {
	if got := fixedInternalTokenCollisions(nil); got != nil {
		t.Fatalf("a nil accessor must report nothing rather than panic, got %v", got)
	}
}

// TestFixedInternalTokenEnvsCoversEveryKnownCapability pins the registry itself.
// A new internal-token capability that forgets to register here gets no pairwise
// protection at all, and nothing else would notice.
func TestFixedInternalTokenEnvsCoversEveryKnownCapability(t *testing.T) {
	want := []string{
		"NOTIFY_INTERNAL_TOKEN",
		"OCTO_DOCS_NOTIFY_TOKEN",
		"OCTO_DOCS_BOT_MENTION_TOKEN",
		internal_resolve.DriveInternalTokenEnv,
		internal_membership.MembershipInternalTokenEnv,
		project.ProvisionFleetSecretEnv,
		project.ProvisionDriveSecretEnv,
	}
	if len(fixedInternalTokenEnvs) != len(want) {
		t.Fatalf("registry size changed: want %d, got %d (%v) — add the new capability to `want` too",
			len(want), len(fixedInternalTokenEnvs), fixedInternalTokenEnvs)
	}
	have := make(map[string]bool, len(fixedInternalTokenEnvs))
	for _, e := range fixedInternalTokenEnvs {
		have[e] = true
	}
	for _, e := range want {
		if !have[e] {
			t.Errorf("fixedInternalTokenEnvs is missing %s", e)
		}
	}
}
