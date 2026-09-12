package main

import (
	"os"
	"path/filepath"
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

// TestFixedInternalTokenRegistryIsCompleteBySweep checks that the registry is
// COMPLETE, not merely self-consistent.
//
// The version this replaces built its expectation as a hand copy of
// fixedInternalTokenEnvs, so both sides of the assertion came from the same
// place: it pinned that the list equals itself, and a capability that was never
// registered passed silently. Two reviewers found the same three that way —
// TS_WEBHOOK_SECRET_KEY, OCTO_MAIL_GATEWAY_SECRET and TS_GRPC_AUTH_TOKEN — and a
// membership token equal to the first grants membership reads plus webhook
// forgery with nothing anywhere saying so.
//
// So the expectation now comes from the TREE. Every credential-shaped env
// literal in non-test Go source must be classified: in the registry, or in the
// annotated exclusion list below with a reason. A new one is neither, so it
// fails until someone decides which it is — which is the only version of this
// test that can catch the next omission.
func TestFixedInternalTokenRegistryIsCompleteBySweep(t *testing.T) {
	// Not capability credentials. Each reason is load-bearing: "it is a secret"
	// is not the criterion — "one value that authenticates one service-to-service
	// capability" is.
	excluded := map[string]string{
		"DM_OIDC_AEGIS_CLIENT_SECRET":     "OAuth client secret for one relying party, not a capability of this binary",
		"DM_OIDC_PROVIDER_CLIENT_SECRET":  "OAuth client secret, provider side; same reason",
		"DM_OIDC_RT_ENC_KEY":              "encryption key for refresh tokens, not an auth credential",
		"DM_PUSH_APNS_KEY_ID":             "an APNs key IDENTIFIER, not a secret",
		"DM_USERSECRET_RESOLVE_IP_BURST":  "rate-limit knob; matches only because USERSECRET contains SECRET",
		"DM_USERSECRET_RESOLVE_IP_RPS":    "rate-limit knob; same",
		"OCTO_MASTER_KEY":                 "signing key for sticker upload handles",
		"OCTO_OIDC_BEARER_JWT_SECRET":     "JWT signing secret",
		"OCTO_OIDC_BIND_TOKEN_TTL_SEC":    "a duration; matches on TOKEN",
		"OCTO_OIDC_PROVIDER_ID_TOKEN_TTL": "a duration; same",
		"OCTO_PII_ENCRYPTION_SECRET":      "column encryption key",
		"OCTO_SEARCH_CURSOR_HMAC":         "cursor signing key",
		"OCTO_USER_API_KEY_SECRET":        "key-encryption key for user API keys",
		"TS_CACHE_TOKENEXPIRE":            "a duration; matches on TOKEN",
		// Found only after the pattern was widened. Both are real single-env
		// credentials of THIS binary, and both were invisible to the previous
		// pattern — which is the reason the widening happened.
		"OCTO_SEARCH_OS_PASSWORD": "OpenSearch cluster password: a credential for an " +
			"infrastructure dependency, not a capability of this binary that a peer authenticates to",
		"SPEECH_API_KEY": "third-party speech vendor API key, outbound only; sharing it with an " +
			"inbound token would be a vendor-account problem, not a capability collision here",
		"DM_OIDC_RESET_PASSWORD_URL": "a URL; matches on PASSWORD. The widened pattern is " +
			"expected to catch shapes like this — one annotated line is the price of not " +
			"missing a real credential",
	}

	registered := make(map[string]bool, len(fixedInternalTokenEnvs))
	for _, e := range fixedInternalTokenEnvs {
		registered[e] = true
	}

	found := sweepCredentialEnvLiterals(t)
	if len(found) < 24 {
		t.Fatalf("the sweep found only %d env literals; it is probably not walking the tree, "+
			"and a guard that reads nothing passes for the wrong reason", len(found))
	}

	for env, where := range found {
		switch {
		case registered[env]:
		case excluded[env] != "":
		default:
			t.Errorf("%s (%s) is a credential-shaped env that is neither in "+
				"fixedInternalTokenEnvs nor in this test's exclusion list. If it authenticates "+
				"a service-to-service capability, register it — otherwise one leaked value can "+
				"grant two capabilities with nothing detecting it. If it does not, add it to "+
				"`excluded` with the reason.", env, where)
		}
	}

	// The exclusion list must not rot either: an entry naming an env that no
	// longer exists is a stale exemption someone could reuse by accident.
	for env := range excluded {
		if _, ok := found[env]; !ok {
			t.Errorf("%s is excluded but no longer appears in the tree; drop the exemption", env)
		}
	}

	// And every registered env must actually exist, so the registry cannot drift
	// into naming envs nothing reads.
	for _, env := range fixedInternalTokenEnvs {
		if _, ok := found[env]; !ok {
			t.Errorf("%s is in fixedInternalTokenEnvs but appears in no non-test source file", env)
		}
	}
}

// credentialEnvLiteral matches env literals that could be a deployment-configured
// credential. Deliberately over-broad — it also catches durations and identifiers
// — because the cost of a false positive is one line in `excluded` with a reason,
// and the cost of a false negative is an unregistered capability credential.
//
// Both halves were too narrow for a round, and the failure mode is the one that
// matters: a guard that cannot EXPRESS a form of the thing it guards reports
// green for it forever.
//
//   - The keyword set lacked PASSWORD, so OCTO_SEARCH_OS_PASSWORD
//     (modules/messages_search) was unclassifiable in either direction.
//   - The prefix list (TS|OCTO|DM|NOTIFY) excluded SPEECH_API_KEY
//     (modules/voice_adapter) for having a prefix nobody had thought of — which is
//     exactly what a new module's env looks like.
//
// So the prefix restriction is gone: any SCREAMING_SNAKE literal carrying a
// credential keyword is swept. That widens the false-positive set, which is the
// cheap direction.
var credentialEnvLiteral = regexp.MustCompile(
	`"([A-Z][A-Z0-9_]*(?:SECRET|TOKEN|KEY|HMAC|PASSWORD|PASSPHRASE|CREDENTIAL|SALT)[A-Z0-9_]*)"`)

// sweepCredentialEnvLiterals returns every credential-shaped env literal in
// non-test Go source, mapped to the first file it was seen in.
//
// Test files are skipped: their fixtures name envs that exist only inside the
// test (OCTO_SHARED_NOTIFY_TOKEN, OCTO_TEST_CALLBACK_SECRET, the S3 test keys),
// and classifying those would be busywork with no security content.
func sweepCredentialEnvLiterals(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range credentialEnvLiteral.FindAllStringSubmatch(string(data), -1) {
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return found
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
			own:    []string{project.ProvisionFleetSecretEnv, project.DriveInternalTokenEnv},
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
