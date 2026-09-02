package space_test

// Source-guard test for the boot-time wiring in main.go.
//
// The repo-root main_marketplace_token_test.go collision cases build their own
// argument list, so they would still pass if the production argument in main.go's
// ValidateNotifyTokenExclusions call were deleted. This test closes that loop
// by asserting the production source itself passes the marketplace internal
// token into the exclusion gate.
//
// It is a source-level grep rather than a runtime assertion because the
// exclusion call happens inside installCardActionDispatch, which needs a real
// config/DB/redis rig to invoke. Coarser, but it gives tamper-detection with
// zero extra fixtures. Copied from
// modules/internal_resolve/main_wiring_test.go with the needle swapped.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMainWiresMarketplaceInternalTokenIntoValidateNotifyTokenExclusions
// guards the argument. If a refactor moves the call out of main.go, update
// this test's target — do not delete it, the semantics still matter.
func TestMainWiresMarketplaceInternalTokenIntoValidateNotifyTokenExclusions(t *testing.T) {
	root, err := spaceRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	mainPath := filepath.Join(root, "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}

	// Locate the call and extract its argument block up to the balanced
	// closing paren. A regex is unreliable here because the argument list
	// contains nested calls like os.Getenv(...) whose ')' would close the
	// match too early.
	needle := "registry.ValidateNotifyTokenExclusions("
	start := strings.Index(string(src), needle)
	if start < 0 {
		t.Fatalf("main.go no longer calls registry.ValidateNotifyTokenExclusions; " +
			"if this call moved, move this test's target too — the invariant " +
			"(the marketplace token flows into the central exclusion " +
			"gate) must be pinned somewhere")
	}
	i := start + len(needle)
	depth := 1
	var args strings.Builder
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
			args.WriteByte(src[i])
		}
	}
	if depth != 0 {
		t.Fatalf("main.go: could not find balanced closing paren for ValidateNotifyTokenExclusions call")
	}

	// Grep for the fully qualified reference to the exported constant so a
	// future refactor that copy-pastes a stale literal cannot silently pass.
	wantRef := "space.MarketplaceInternalTokenEnv"
	if !strings.Contains(args.String(), wantRef) {
		t.Fatalf("main.go: registry.ValidateNotifyTokenExclusions(...) no longer includes %s\n"+
			"Args block:\n%s\n\n"+
			"OCTO_MARKETPLACE_INTERNAL_TOKEN authorizes reading a uid's role in any "+
			"Space and must be checked against the dynamic per-route notify "+
			"tokens / callback secrets loaded from OCTO_CARD_ACTION_ROUTES. Removing "+
			"this argument reopens the one-credential-two-capabilities hazard.",
			wantRef, args.String())
	}
}

// spaceRepoRoot walks up from this test file until it finds go.mod.
func spaceRepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", spaceSourceErr("cannot determine caller file")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", spaceSourceErr("go.mod not found walking up from test file")
		}
		dir = parent
	}
}

type spaceSourceErr string

func (e spaceSourceErr) Error() string { return string(e) }
