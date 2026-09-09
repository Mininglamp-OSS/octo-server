package space_test

// Route wiring source-guard test: pin the production middleware shape and
// ordering of the internal role endpoint in api.go's Route() function body, at
// the source level.
//
// # Why source-level
//
// api_internal_test.go builds a hermetic router that skips the strict-IP
// limiter (that layer needs live Redis). The version of this guard that shipped
// in the first revision of this branch was worse than nothing: it built its own
// router, registered `record("ipLimit"), record("auth"), handler` in that order,
// and then asserted it observed them in that order. It asserted a fact about
// its own three lines, never called (*Space).Route, and would have stayed green
// if production had moved the auth middleware onto the /v1/internal group —
// precisely the regression it claimed to prevent.
//
// wkhttp does not expose the gin.Engine internals needed to walk the registered
// handler chain from Go code, so a topology assertion is out of reach without
// live infra. What we CAN pin is the source of Route() itself, which is what
// modules/internal_resolve/route_wiring_test.go does; this file is its sibling.
//
// If a future refactor drops the limiter, drops auth, reorders them, or moves
// the path, one of these assertions fails.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSpaceRouteBodyPinsInternalMiddlewareChain guards the contents of
// modules/space/api.go's Route(). If the function moves or the anchors no
// longer hold after a legitimate refactor, update this test to the new anchor
// points — do not delete it. The invariants (limiter built, group carries no
// middleware, limiter before auth before handler, path registered) still
// matter.
func TestSpaceRouteBodyPinsInternalMiddlewareChain(t *testing.T) {
	root, err := spaceRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	apiPath := filepath.Join(root, "modules/space/api.go")
	src, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatalf("read %s: %v", apiPath, err)
	}
	body := extractSpaceRouteBody(t, string(src))

	assertions := []struct {
		name        string
		mustHave    string
		whyItBreaks string
	}{
		{
			name: "builds the strict-IP rate limiter with the dedicated tag and sanitized knobs",
			mustHave: `r.StrictIPRateLimitMiddleware(
		context.Background(), rlRedis, spaceMemberRoleRateLimitTag,
		sanitizedSpaceMemberRoleRPS(), sanitizedSpaceMemberRoleBurst(),
	)`,
			whyItBreaks: "The per-endpoint IP rate limiter is missing, lost its " +
				"dedicated Redis keyspace tag, or stopped sanitizing its env knobs. " +
				"`.octospec/rules/rate-limit.md` (load_bearing:true) requires the " +
				"limiter on un-user-authed routes; sharing a tag merges quotas with " +
				"an unrelated route; wkhttp.ParseRPSFromEnv admits NaN/+Inf, which " +
				"silently DISABLES the limiter inside the Redis Lua script.",
		},
		{
			name:     "creates the /v1/internal group WITHOUT middleware on the group",
			mustHave: `internal := r.Group("/v1/internal")`,
			whyItBreaks: "The group must be constructed with NO middleware. Attaching " +
				"auth (or the limiter) to the group re-orders execution because Gin " +
				"combines group handlers ahead of route handlers — a missing token " +
				"would then abort before the strict-IP bucket is consumed, exposing " +
				"the endpoint to token probing throttled only by the wider global " +
				"bucket.",
		},
		{
			name: "mounts ipLimit → auth → handler in that exact order on the GET",
			mustHave: `internal.GET(
		"/spaces/:space_id/members/:uid/role",
		memberRoleIPLimit,
		s.marketplaceInternalTokenMiddleware(),
		s.getSpaceMemberRole,
	)`,
			whyItBreaks: "The concrete GET must attach handlers in the order " +
				"memberRoleIPLimit → auth → getSpaceMemberRole. Any other order lets " +
				"unauthenticated requests bypass the strict-IP bucket. If the path " +
				"itself moved, update this test AND docs/space-internal-role-api.md " +
				"AND the octo-marketplace client.",
		},
	}

	for _, a := range assertions {
		if !strings.Contains(body, a.mustHave) {
			t.Errorf("Route() body assertion %q failed:\n"+
				"  missing substring: %q\n"+
				"  why it matters:   %s",
				a.name, a.mustHave, a.whyItBreaks)
		}
	}

	// The deleted roster endpoint must stay deleted. It leaked verified legal
	// names cross-tenant (see api_internal.go); a copy-paste revival would be
	// silent otherwise.
	for _, gone := range []string{
		`"/spaces/:space_id/admins"`,
		"getSpaceAdmins",
		"marketplaceAdminListTokenMiddleware",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("Route() re-registers the deleted admin-roster endpoint (%q). "+
				"It returned {uid, legal name, role} for any Space to any holder of "+
				"one shared token; the replacement is the single-subject role lookup.", gone)
		}
	}

	// Ordering: the limiter must be built BEFORE the GET that consumes it,
	// otherwise memberRoleIPLimit is not defined at mount time. Raw index
	// positions survive comment and blank-line churn.
	limIdx := strings.Index(body, "memberRoleIPLimit :=")
	getIdx := strings.Index(body, "internal.GET(")
	if limIdx < 0 || getIdx < 0 {
		t.Fatalf("Route() body no longer contains both the limiter assignment and the "+
			"internal GET registration (limIdx=%d getIdx=%d)", limIdx, getIdx)
	}
	if limIdx >= getIdx {
		t.Error("Route() body order regression: the limiter is assigned at or after " +
			"the GET registration; memberRoleIPLimit would not be defined at mount time")
	}
}

// TestSpaceDoesNotImportNotifyOrUser pins the dependency DIRECTION that makes
// space.ActiveAdminUIDs (admin_targets.go) usable from modules/notify.
//
// modules/notify imports modules/space to resolve role-targeted recipients
// rather than restating the space_member predicates. That only works while the
// arrow points one way. The Go compiler does enforce this — it would refuse to
// build a cycle — but "import cycle not allowed" spread across five packages is
// a miserable way to discover a design constraint, so fail here first with the
// reason.
//
// Only PRODUCTION files are checked: test files may import anything (the
// external space_test package already imports modules/internal_resolve's
// siblings without affecting the production graph).
func TestSpaceDoesNotImportNotifyOrUser(t *testing.T) {
	root, err := spaceRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	dir := filepath.Join(root, "modules/space")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	forbidden := map[string]string{
		"github.com/Mininglamp-OSS/octo-server/modules/notify": "modules/notify imports " +
			"modules/space for space.ActiveAdminUIDs; importing it back creates a cycle " +
			"and breaks role-targeted notify delivery",
		"github.com/Mininglamp-OSS/octo-server/modules/user": "modules/user imports " +
			"modules/space, and modules/notify imports modules/user; importing user " +
			"here creates a cycle",
	}

	checked := 0
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		checked++
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if why, bad := forbidden[path]; bad {
				t.Errorf("%s imports %s — %s", name, path, why)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production .go files found in modules/space; this guard would pass vacuously")
	}
}

// extractSpaceRouteBody returns the source between the opening `{` and the
// matching closing `}` of `func (s *Space) Route(r *wkhttp.WKHttp)`. Balanced
// on braces so nested blocks do not close the match early.
func extractSpaceRouteBody(t *testing.T, src string) string {
	t.Helper()
	needle := "func (s *Space) Route(r *wkhttp.WKHttp) {"
	start := strings.Index(src, needle)
	if start < 0 {
		t.Fatalf("modules/space/api.go no longer contains `%s`; if Route() moved or "+
			"was renamed, move this test's anchor with it — the wiring invariants "+
			"still matter", needle)
	}
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
		t.Fatalf("modules/space/api.go: could not find balanced closing brace for Route()")
	}
	return src[start+len(needle) : end]
}
