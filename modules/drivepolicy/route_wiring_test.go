package drivepolicy

// Source-guard test: pin the production middleware shape and ordering in
// api.go's Route() at the source level, complementing the runtime-order test.
// Mirrors modules/internal_resolve/route_wiring_test.go.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRouteBodyPinsMiddlewareChain(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "modules/drivepolicy/api.go"))
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	body := extractRouteBody(t, string(src))

	assertions := []struct{ name, mustHave, why string }{
		{
			name:     "builds the strict-IP rate limiter",
			mustHave: "m.ipRateLimit(r)",
			why:      "The per-endpoint IP rate limiter builder is missing; the load-bearing rate-limit rule requires it on un-user-authed sensitive routes.",
		},
		{
			name:     "creates /v1/internal group WITHOUT group middleware",
			mustHave: `r.Group("/v1/internal")`,
			why:      "Group must carry NO middleware. Attaching auth (or the limiter) to the group re-orders execution because Gin combines group handlers ahead of route handlers, so a missing token would abort before the strict-IP bucket is consumed.",
		},
		{
			name: "mounts ipLimit → auth → handler on the concrete GET",
			mustHave: `internal.GET(
		"/drive/upload-policy",
		ipLimit,
		m.internalAuthMiddleware(),
		m.getUploadPolicy,
	)`,
			why: "The concrete GET must attach handlers in ipLimit → auth → handler order. If the sub-path moved, update this test and every doc that mentions /v1/internal/drive/upload-policy.",
		},
	}
	for _, a := range assertions {
		if !strings.Contains(body, a.mustHave) {
			t.Errorf("Route() assertion %q failed:\n  missing: %q\n  why: %s\n  body:\n%s",
				a.name, a.mustHave, a.why, body)
		}
	}

	limIdx := strings.Index(body, "m.ipRateLimit(r)")
	getIdx := strings.Index(body, "internal.GET(")
	if limIdx >= 0 && getIdx >= 0 && !(limIdx < getIdx) {
		t.Errorf("Route() order regression: limiter builder appears at or after the GET registration")
	}
}

func extractRouteBody(t *testing.T, src string) string {
	t.Helper()
	needle := "func (m *Module) Route(r *wkhttp.WKHttp) {"
	start := strings.Index(src, needle)
	if start < 0 {
		t.Fatalf("api.go no longer contains %q; move this test's anchor with the function", needle)
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
		t.Fatalf("api.go: unbalanced braces in Route()")
	}
	return src[start+len(needle) : end]
}

func findRepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errNoCaller
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNoRepoRoot
		}
		dir = parent
	}
}

var (
	errNoCaller   = routeErr("cannot determine caller file")
	errNoRepoRoot = routeErr("go.mod not found walking up from test file")
)

type routeErr string

func (e routeErr) Error() string { return string(e) }
