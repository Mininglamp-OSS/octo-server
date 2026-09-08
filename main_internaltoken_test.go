package main

import (
	"os"
	"regexp"
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

// TestModuleLocalRefusalsCoverTheCentralRegistry closes the gap between the two
// layers, in the direction that matters.
//
// The central check DETECTS every pair but only logs, so a collision it finds
// leaves BOTH capabilities live. The module-local check is the layer that fails a
// capability CLOSED. A pair that only the central check sees is therefore a pair
// where a single leaked value grants two capabilities with nothing but an ERROR
// line standing in the way.
//
// The rebase that added the two provisioning secrets to the registry grew that
// gap without anything noticing: modules/internal_membership refused four
// siblings out of six, and modules/project's checkSecretExclusivity mirrored the
// omission from the other side. Both are complete now, and this is what keeps
// them complete when the registry next grows.
//
// Scope: only the two modules whose credentials this PR introduced or pairs
// with. The older modules (notify, bot_mention, internal_resolve) remain
// asymmetric — that is the documented pre-existing norm, and widening them is a
// change to those modules, not to this list.
func TestModuleLocalRefusalsCoverTheCentralRegistry(t *testing.T) {
	cases := []struct {
		name string
		file string
		// marker anchors the refusal LIST, so a constant that is declared and
		// never used does not satisfy the check.
		marker string
		// own is the module's own env, which it obviously does not list as a sibling.
		own []string
	}{
		{
			name:   "internal_membership",
			file:   "modules/internal_membership/config.go",
			marker: "var siblingFixedTokenEnvs = []string{",
			own:    []string{internal_membership.MembershipInternalTokenEnv},
		},
		{
			name:   "project provisioning",
			file:   "modules/project/config_provisioning.go",
			marker: "for _, siblingEnv := range []string{",
			own:    []string{project.ProvisionFleetSecretEnv, project.ProvisionDriveSecretEnv},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refused := refusedEnvs(t, tc.file, tc.marker)
			// Not vacuous: an empty or mis-anchored region would pass every
			// membership check below by simply having nothing to disagree with.
			if len(refused) < 4 {
				t.Fatalf("%s: found only %d refused envs (%v) — the region anchor %q is probably "+
					"stale, and a guard that reads nothing passes for the wrong reason",
					tc.file, len(refused), refused, tc.marker)
			}
			mine := make(map[string]bool, len(tc.own))
			for _, e := range tc.own {
				mine[e] = true
			}
			for _, env := range fixedInternalTokenEnvs {
				if mine[env] || refused[env] {
					continue
				}
				t.Errorf("%s does not refuse a collision with %s. The central registry only "+
					"LOGS that pair, so both capabilities would stay live on one leaked "+
					"value — the module-local refusal is the layer that fails closed.",
					tc.file, env)
			}
		})
	}
}

// refusedEnvs returns the env NAMES a module's refusal list actually names.
//
// It reads the list REGION rather than the whole file, and resolves the
// identifiers in it through the file's own `ident = "LITERAL"` declarations. A
// whole-file grep would pass on a file that declares a constant and never uses
// it — which is precisely the shape a careless edit leaves behind.
func refusedEnvs(t *testing.T, file, marker string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(raw)

	consts := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(src, -1) {
		consts[m[1]] = m[2]
	}

	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("%s: refusal-list anchor %q not found; point this guard at the new one rather than deleting it", file, marker)
	}
	region := src[at+len(marker):]
	if end := strings.Index(region, "}"); end >= 0 {
		region = region[:end]
	}

	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(region, -1) {
		out[m[1]] = true
	}
	// A trailing `// comment` after the entry is allowed: the alternative is a
	// spurious CI failure the first time someone annotates one of these lines.
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+),\s*(?://.*)?$`).FindAllStringSubmatch(region, -1) {
		if lit, ok := consts[m[1]]; ok {
			out[lit] = true
		}
	}
	return out
}
