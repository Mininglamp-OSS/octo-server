package internal_resolve_test

// Route wiring source-guard test: pin the production middleware shape and
// ordering in api.go's Route() function body at the source level.
//
// api_test.go builds a hermetic router that skips the strict-IP limiter (that
// layer needs a live Redis, verified out of band). PR #711 review round 3
// (yujiawei P2-3) flagged that no test exercised the real Route() path, so a
// reordering — putting auth before the limiter, or dropping either — could
// ship green.
//
// wkhttp does not expose the gin.Engine internals needed to walk the
// registered handler chain from Go code, so a topology assertion is out of
// reach without live infra. What we CAN pin at the source level:
//   - Route() invokes resolveBotOwnerIPRateLimit(r) (i.e. builds the limiter).
//   - Route() calls r.Group("/v1/internal", ..., m.internalAuthMiddleware()).
//   - Route() calls internal.POST("/user/resolve-bot-owner", ipLimit, m.resolveBotOwner).
//   - The ipLimit variable is threaded from the builder call into the POST
//     call, so the limiter really is mounted on the concrete route rather
//     than being built and dropped on the floor.
//
// If a future refactor drops the limiter, drops auth, reorders them, or moves
// the path string, one of these assertions fails.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRouteBodyPinsMiddlewareChain guards the exact contents of api.go's
// Route() function. If the function moves or the assertions no longer hold
// after a legitimate refactor, update this test to the new anchor points —
// do not delete it. The invariants (limiter mounted, auth mounted, path
// registered, limiter threaded through) still matter.
func TestRouteBodyPinsMiddlewareChain(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	apiPath := filepath.Join(root, "modules/internal_resolve/api.go")
	src, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatalf("read %s: %v", apiPath, err)
	}
	body := extractRouteBody(t, string(src))

	// The four contract points, each with an explicit failure message so a
	// future maintainer sees WHY the assertion exists.
	assertions := []struct {
		name        string
		mustHave    string
		whyItBreaks string
	}{
		{
			name:     "builds strict-IP rate limiter",
			mustHave: "m.resolveBotOwnerIPRateLimit(r)",
			whyItBreaks: "The per-endpoint IP rate limiter builder is missing. " +
				"`.octospec/rules/rate-limit.md` (load_bearing:true) requires it on " +
				"un-user-authed sensitive routes. Reintroduce the call.",
		},
		{
			name:     "creates the /v1/internal group WITHOUT middleware on the group",
			mustHave: `r.Group("/v1/internal")`,
			whyItBreaks: "Group must be constructed with NO middleware. Attaching auth " +
				"(or the limiter) to the group re-orders execution because Gin " +
				"combines group handlers ahead of route handlers — a missing token " +
				"would then abort before the strict-IP bucket is consumed, exposing " +
				"the endpoint to token-probing throttled only by the wider global " +
				"bucket. PR #711 review round 4 called this out explicitly.",
		},
		{
			name: "mounts ipLimit → auth → handler in that exact order on the POST",
			mustHave: `internal.POST(
		"/user/resolve-bot-owner",
		ipLimit,
		m.internalAuthMiddleware(),
		m.resolveBotOwner,
	)`,
			whyItBreaks: "The concrete POST must attach handlers in the order " +
				"ipLimit → auth → resolveBotOwner. Any other order lets " +
				"unauthenticated requests bypass the strict-IP bucket, defeating " +
				"the load-bearing rate-limit rule for un-user-authed endpoints. " +
				"If the sub-path itself moved, update this test AND every doc that " +
				"mentions /v1/internal/user/resolve-bot-owner.",
		},
	}

	for _, a := range assertions {
		if !strings.Contains(body, a.mustHave) {
			t.Errorf("Route() body assertion %q failed:\n"+
				"  missing substring: %q\n"+
				"  why it matters:   %s\n"+
				"  route body:\n%s",
				a.name, a.mustHave, a.whyItBreaks, indent(body))
		}
	}

	// Ordering: the limiter builder must appear BEFORE the POST call that
	// consumes ipLimit — otherwise ipLimit is nil at mount time or the code
	// is unreachable. Use raw index positions because they survive comment
	// and blank-line changes.
	limIdx := strings.Index(body, "m.resolveBotOwnerIPRateLimit(r)")
	postIdx := strings.Index(body, "internal.POST(")
	if limIdx >= 0 && postIdx >= 0 && !(limIdx < postIdx) {
		t.Errorf("Route() body order regression: limiter builder appears at or after " +
			"the POST registration; ipLimit would not be defined at mount time")
	}
}

// extractRouteBody returns the source between the opening `{` and the
// matching closing `}` of `func (m *Module) Route(r *wkhttp.WKHttp)`. Balanced
// on braces so any nested block inside the function does not close the match
// early.
func extractRouteBody(t *testing.T, src string) string {
	t.Helper()
	// Find the exact signature. If the receiver name or type changes for a
	// legitimate reason, update the needle here — do not remove the anchor.
	needle := "func (m *Module) Route(r *wkhttp.WKHttp) {"
	start := strings.Index(src, needle)
	if start < 0 {
		t.Fatalf("api.go no longer contains `%s`; if Route() moved or was renamed, "+
			"move this test's anchor with it — the wiring invariants still matter", needle)
	}
	// Walk from the opening brace, balancing.
	i := start + len(needle)
	depth := 1
	end := -1
	for ; i < len(src) && depth > 0; i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatalf("api.go: could not find balanced closing brace for Route()")
	}
	return src[start+len(needle) : end]
}

func findRepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errUnknownCallerRoute
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errRepoRootNotFoundRoute
		}
		dir = parent
	}
}

var (
	errUnknownCallerRoute    = routeErr("cannot determine caller file")
	errRepoRootNotFoundRoute = routeErr("go.mod not found walking up from test file")
)

type routeErr string

func (e routeErr) Error() string { return string(e) }

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "    | " + lines[i]
	}
	return strings.Join(lines, "\n")
}
