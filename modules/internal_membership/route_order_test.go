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
	for _, problem := range routeMountProblems(routeFuncSource(t)) {
		t.Error(problem)
	}
}

// routeMountProblems returns every mounting defect it can see in the text of
// Route(), as messages. Empty means clean.
//
// Split out from the test so the guard can be run against a fixture as well as
// against the shipped source — see TestRouteMountGuardCatchesTheHazards. A source
// guard asserts about text that is NOT there, and the only way to know it would
// notice is to hand it the text.
func routeMountProblems(body string) []string {
	var problems []string

	// The group itself must carry NO middleware: any extra argument to Group
	// would execute ahead of the per-route limiter.
	groupCall := regexp.MustCompile(`Group\(\s*"/v1/internal"\s*\)`)
	if !groupCall.MatchString(body) {
		problems = append(problems, "Route() must call Group(\"/v1/internal\") with no middleware "+
			"arguments; a group-attached handler runs BEFORE the per-route limiter")
	}

	// ...and it must not acquire one afterwards either. `internal.Use(auth)` is a
	// SEPARATE statement, so it satisfies the argument-free Group check above and
	// the per-route ordering check below while still executing auth first — Gin
	// runs group-attached handlers ahead of route-attached ones. That is the exact
	// hazard this file exists for, expressed in the one shape the guard did not
	// look at until now.
	if strings.Contains(body, ".Use(") {
		problems = append(problems, "Route() must not call .Use(): a group-attached middleware "+
			"executes BEFORE every route-attached one, so it would run ahead of the strict IP "+
			"limiter no matter where the call appears in the source. Mount on the concrete routes")
	}

	for _, route := range []string{"/membership/epochs", "/project-memberships/_verify"} {
		idx := strings.Index(body, route)
		if idx < 0 {
			problems = append(problems, "Route() no longer mounts "+route)
			continue
		}
		rest := body[idx:]
		end := strings.Index(rest, ")\n")
		if end > 0 {
			rest = rest[:end]
		}
		limitAt := strings.Index(rest, "ipLimit")
		authAt := strings.Index(rest, "internalAuthMiddleware")
		if limitAt < 0 || authAt < 0 {
			problems = append(problems, route+" must mount both ipLimit and internalAuthMiddleware on the concrete route")
			continue
		}
		if limitAt > authAt {
			problems = append(problems, route+" mounts auth before the rate limiter; token probing would bypass the strict bucket")
		}
	}
	return problems
}

// TestRouteMountGuardCatchesTheHazards feeds the guard the shapes it exists to
// reject, so "the shipped source is clean" means something.
func TestRouteMountGuardCatchesTheHazards(t *testing.T) {
	clean := `
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs", ipLimit, m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
`
	if got := routeMountProblems(clean); len(got) != 0 {
		t.Fatalf("the guard rejects a correct mounting: %v", got)
	}

	hazards := map[string]string{
		"middleware on the group": `
	internal := r.Group("/v1/internal", m.internalAuthMiddleware())
	internal.GET("/membership/epochs", ipLimit, m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
`,
		"a separate .Use() call": `
	internal := r.Group("/v1/internal")
	internal.Use(m.internalAuthMiddleware())
	internal.GET("/membership/epochs", ipLimit, m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
`,
		"auth ahead of the limiter on the route": `
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs", m.internalAuthMiddleware(), ipLimit, m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
`,
		"the limiter dropped entirely": `
	internal := r.Group("/v1/internal")
	internal.GET("/membership/epochs", m.internalAuthMiddleware(), m.membershipEpochs)
	internal.POST("/project-memberships/_verify", ipLimit, m.internalAuthMiddleware(), m.verifyProjectMemberships)
`,
	}
	for name, src := range hazards {
		t.Run(name, func(t *testing.T) {
			if got := routeMountProblems(src); len(got) == 0 {
				t.Error("the guard accepts a mounting that runs auth ahead of the strict IP limiter")
			}
		})
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
