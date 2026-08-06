package internal_resolve_test

// Source-guard test for the boot-time wiring in main.go.
//
// The review that produced this file (PR #711, lml2468 08:42Z) pointed out
// that boot_config_test.go's collision cases would still pass even if the
// production argument in main.go's ValidateNotifyTokenExclusions call were
// deleted — because those tests build their own argument list. This test
// closes that loop by asserting the production source itself passes the
// drive token to the exclusions call.
//
// It is a source-level grep rather than a runtime assertion because the
// exclusion call happens inside installCardDispatch, which requires a real
// config/DB/redis rig to invoke. A file grep is coarser but gives us
// tamper-detection with zero extra fixtures.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

	// The critical argument: the drive token env value. Grep for the fully
	// qualified reference to the exported constant so a future refactor that
	// unqualifies the import or copy-pastes a stale literal cannot silently
	// pass.
	wantRef := "internal_resolve.DriveInternalTokenEnv"
	if !strings.Contains(args, wantRef) {
		t.Fatalf("main.go: registry.ValidateNotifyTokenExclusions(...) no longer includes %s\n"+
			"Args block:\n%s\n\n"+
			"P1 from PR #711 review requires the drive token to be checked against "+
			"the dynamic per-route notify tokens / callback secrets loaded from "+
			"OCTO_CARD_ACTION_ROUTES. Removing this argument reopens the "+
			"one-credential-two-capabilities hazard.",
			wantRef, args)
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
