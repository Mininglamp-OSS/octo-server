package internal_resolve_test

// Source-guard test for the boot-time wiring in main.go.
//
// The review that produced this file (PR #711, lml2468 08:42Z) pointed out
// that boot_config_test.go's collision cases would still pass even if the
// production argument in main.go's ValidateNotifyTokenExclusions call were
// deleted — because those tests build their own argument list. This test
// closes that loop by asserting the production source itself feeds the drive
// token into the exclusions call.
//
// The call no longer takes a hand-written argument list: main.go passes
// internaltoken.Values(os.Getenv), so the wiring is now two claims instead of
// one — main.go sources its arguments from the shared registry, AND the
// registry contains the drive token. Both are checked below; together they are
// strictly stronger than grepping for a single literal argument, because they
// also hold for every capability registered after this one.
//
// The main.go half stays a source-level grep rather than a runtime assertion
// because the exclusion call happens inside installCardActionDispatch, which
// takes a real route config to reach. A file grep is coarser but gives us
// tamper-detection with zero extra fixtures.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
	"github.com/Mininglamp-OSS/octo-server/pkg/internaltoken"
)

// TestMainWiresDriveTokenIntoValidateNotifyTokenExclusions guards the exact
// argument the review required to be present. If a refactor moves the call
// out of main.go, update this test to the new location — do not delete it,
// the semantics still matter.
func TestMainWiresDriveTokenIntoValidateNotifyTokenExclusions(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	mainPath := filepath.Join(root, "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}

	// Locate the call and extract its argument block up to the balanced
	// closing paren. Regex-based (?s).*? isn't reliable here because the
	// argument list contains nested calls like os.Getenv(...) whose ')'
	// would otherwise close the match too early.
	needle := "registry.ValidateNotifyTokenExclusions("
	start := strings.Index(string(src), needle)
	if start < 0 {
		t.Fatalf("main.go no longer calls registry.ValidateNotifyTokenExclusions; " +
			"if this call moved, move this test's target too — the invariant " +
			"(drive token flows into the central exclusion gate) must be pinned somewhere")
	}
	// Balance parens starting after the opening one.
	i := start + len(needle)
	depth := 1
	args := ""
	for ; i < len(src) && depth > 0; i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				continue
			}
		}
		if depth > 0 {
			args += string(src[i])
		}
	}
	if depth != 0 {
		t.Fatalf("main.go: could not find balanced closing paren for ValidateNotifyTokenExclusions call")
	}

	// Claim 1: the argument list comes from the shared registry, fed by the real
	// process environment. Both halves matter — `internaltoken.Values(` alone
	// would still pass for a stub lookup that yields nothing, which would make
	// the whole gate a no-op. (pkg/internaltoken's Values panics on a nil
	// getenv for the same reason; this catches the non-nil stubs it cannot.)
	const wantSource = "internaltoken.Values(os.Getenv)"
	if !strings.Contains(strings.Join(strings.Fields(args), ""), strings.ReplaceAll(wantSource, " ", "")) {
		t.Fatalf("main.go: registry.ValidateNotifyTokenExclusions(...) no longer sources its arguments from %s\n"+
			"Args block:\n%s\n\n"+
			"P1 from PR #711 review requires the fixed internal-token envs to be checked "+
			"against the dynamic per-route notify tokens / callback secrets loaded from "+
			"OCTO_CARD_ACTION_ROUTES. A hand-written list reopens the "+
			"one-credential-two-capabilities hazard the moment someone forgets an entry.",
			wantSource, args)
	}

	// Claim 2: the drive token is actually in that registry. Without this, claim 1
	// could hold over an empty or drive-less registry and the P1 would be back.
	if !internaltoken.Registered(internal_resolve.DriveInternalTokenEnv) {
		t.Fatalf("%s is not in pkg/internaltoken's registry; main.go's exclusion gate "+
			"therefore never sees it, reopening the one-credential-two-capabilities hazard",
			internal_resolve.DriveInternalTokenEnv)
	}
}

// TestDriveTokenIsRegisteredWithSiblings pins the precondition this module used
// to hand-roll for itself: the drive token is in the shared registry alongside
// the other fixed internal-token envs, so Resolve compares it against its
// seniors and any capability added later yields to it. Enumerating the registry
// means an appended env needs no edit here.
//
// This asserts membership, not the comparison itself — that is
// TestResolveDriveInternalTokenRejectsSiblingCollision's job, and it branches on
// the precedence index because the guard is directional.
func TestDriveTokenIsRegisteredWithSiblings(t *testing.T) {
	envs := internaltoken.Envs()
	if len(envs) < 2 {
		t.Fatalf("registry holds %d envs; the cross-capability guard would be vacuous", len(envs))
	}
	found := false
	for _, env := range envs {
		if env == internal_resolve.DriveInternalTokenEnv {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s missing from internaltoken.Envs() = %v", internal_resolve.DriveInternalTokenEnv, envs)
	}
}

// repoRoot walks up from this test file until it finds go.mod.
func repoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errUnknownCaller
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errRepoRootNotFound
		}
		dir = parent
	}
}

// Package-scoped sentinel errors kept internal — no need to spread these.
var (
	errUnknownCaller    = mkErr("cannot determine caller file")
	errRepoRootNotFound = mkErr("go.mod not found walking up from test file")
)

type sourceErr string

func (e sourceErr) Error() string { return string(e) }
func mkErr(s string) sourceErr    { return sourceErr(s) }
