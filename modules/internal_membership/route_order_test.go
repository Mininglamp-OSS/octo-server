package internal_membership

// Middleware-order tests for Route().
//
// Two layers, because either alone is insufficient:
//
//   - the RUNTIME test proves Gin actually executes [ipLimit, auth, handler].
//     Gin runs group-attached handlers BEFORE route-attached ones regardless of
//     source order, so `Group(prefix, auth)` + `GET(path, ipLimit, handler)`
//     executes as auth → ipLimit → handler. That ordering lets an unauthenticated
//     prober abort before consuming the strict-IP bucket and fall back to the far
//     wider global bucket. modules/internal_resolve was bitten by exactly this.
//   - the SOURCE guard proves Route() still mounts both middlewares on the
//     concrete routes rather than on the group. The runtime test builds its own
//     chain (Route() needs a real config for the Redis client), so without the
//     source guard a refactor could move auth onto the group and the runtime test
//     would keep passing against its hand-built copy.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestRouteMiddlewareRuntimeOrder(t *testing.T) {
	m := &Module{
		store:         &stubStore{epochs: map[string]int64{"p1": 1}, epoch: 1, roles: map[string]int{}},
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalMembershipOrderTest"),
	}

	var seen []string
	record := func(name string, next wkhttp.HandlerFunc) wkhttp.HandlerFunc {
		return func(c *wkhttp.Context) {
			seen = append(seen, name)
			next(c)
		}
	}
	// A stand-in for the real limiter, which needs Redis. It occupies the same
	// position in the chain, which is what this test measures.
	passThroughLimit := func(c *wkhttp.Context) { c.Next() }

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs",
		record("ipLimit", passThroughLimit),
		record("auth", m.internalAuthMiddleware()),
		record("handler", m.membershipEpochs),
	)
	internal.POST("/project-memberships/_verify",
		record("ipLimit", passThroughLimit),
		record("auth", m.internalAuthMiddleware()),
		record("handler", m.verifyProjectMemberships),
	)

	want := []string{"ipLimit", "auth", "handler"}

	seen = nil
	req := httptest.NewRequest(http.MethodGet, epochsPath+"?space_id=s&project_ids=p1", nil)
	req.Header.Set(internalTokenHeader, testInternalToken)
	r.ServeHTTP(httptest.NewRecorder(), req)
	assertOrder(t, "epochs", want, seen)

	seen = nil
	body := bytes.NewReader([]byte(`{"space_id":"s","project_id":"p1","uids":["u1"]}`))
	req = httptest.NewRequest(http.MethodPost, verifyPath, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, testInternalToken)
	r.ServeHTTP(httptest.NewRecorder(), req)
	assertOrder(t, "verify", want, seen)
}

// TestRateLimitRunsBeforeAuthEvenWhenAuthRejects is the property the ordering
// exists for: a request with a bad token must still consume the strict-IP
// bucket, or token probing is free.
func TestRateLimitRunsBeforeAuthEvenWhenAuthRejects(t *testing.T) {
	m := &Module{
		store:         &stubStore{},
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalMembershipOrderTest"),
	}
	limiterRan := false

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET(epochsPath,
		func(c *wkhttp.Context) { limiterRan = true; c.Next() },
		m.internalAuthMiddleware(),
		m.membershipEpochs,
	)

	req := httptest.NewRequest(http.MethodGet, epochsPath+"?space_id=s&project_ids=p1", nil)
	req.Header.Set(internalTokenHeader, "definitely-the-wrong-token-value")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if !limiterRan {
		t.Fatal("the rate limiter must run before auth rejects, or token probing bypasses the bucket")
	}
}

func assertOrder(t *testing.T, label string, want, got []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: want %v, got %v", label, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: want %v, got %v", label, want, got)
		}
	}
}

// TestRouteMountsMiddlewareOnConcreteRoutes is the source guard described above.
func TestRouteMountsMiddlewareOnConcreteRoutes(t *testing.T) {
	body := routeFuncSource(t)

	// The group itself must carry NO middleware: any extra argument to Group
	// would execute ahead of the per-route limiter.
	groupCall := regexp.MustCompile(`Group\(\s*"/v1/internal"\s*\)`)
	if !groupCall.MatchString(body) {
		t.Errorf("Route() must call Group(\"/v1/internal\") with no middleware arguments; "+
			"a group-attached handler runs BEFORE the per-route limiter. Got:\n%s", body)
	}

	for _, route := range []string{"/membership/epochs", "/project-memberships/_verify"} {
		idx := strings.Index(body, route)
		if idx < 0 {
			t.Fatalf("Route() no longer mounts %s", route)
		}
		rest := body[idx:]
		end := strings.Index(rest, ")\n")
		if end > 0 {
			rest = rest[:end]
		}
		limitAt := strings.Index(rest, "ipLimit")
		authAt := strings.Index(rest, "internalAuthMiddleware")
		if limitAt < 0 || authAt < 0 {
			t.Errorf("%s must mount both ipLimit and internalAuthMiddleware on the concrete route, got:\n%s", route, rest)
			continue
		}
		if limitAt > authAt {
			t.Errorf("%s mounts auth before the rate limiter; token probing would bypass the strict bucket", route)
		}
	}
}

// routeFuncSource returns the body of Route() from api.go.
func routeFuncSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("api.go"))
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func (m *Module) Route(")
	if start < 0 {
		t.Fatal("Route() not found in api.go — if it moved, point this guard at the new location rather than deleting it")
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end > 0 {
		rest = rest[:end]
	}
	return rest
}
