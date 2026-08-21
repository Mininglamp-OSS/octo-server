package authz

import (
	"strings"
	"testing"
)

func TestScanManagerRoutesFollowsSharedHelperAndDirectGate(t *testing.T) {
	root := t.TempDir()
	writeRouteFixture(t, root, false)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("ScanManagerRoutes() got %d routes, want 3: %#v", len(routes), routes)
	}
	want := map[string]string{
		"GET /v1/manager/one":    "Manager.one",
		"POST /v1/manager/two":   "Manager.two",
		"DELETE /v1/manager/raw": "Manager.raw",
	}
	gateByRoute := make(map[string]string)
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if route.Handler != want[key] {
			t.Errorf("route %s handler = %q, want %q", key, route.Handler, want[key])
		}
		if len(route.GateSites) != 1 {
			t.Errorf("route %s gate sites = %v, want one", key, route.GateSites)
			continue
		}
		gateByRoute[key] = route.GateSites[0]
	}
	if gateByRoute["GET /v1/manager/one"] != gateByRoute["POST /v1/manager/two"] {
		t.Fatal("routes using the shared helper resolved to different gates")
	}
	if gateByRoute["GET /v1/manager/one"] == gateByRoute["DELETE /v1/manager/raw"] {
		t.Fatal("shared helper and direct handler unexpectedly resolved to the same gate")
	}
}

func TestScanManagerRoutesIgnoresFormatting(t *testing.T) {
	root := t.TempDir()
	writeRouteFixture(t, root, false)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	writeRouteFixture(t, root, true)
	gates, err = ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	if routeIdentities(before) != routeIdentities(after) {
		t.Fatalf("route identities changed after formatting: before=%q after=%q", routeIdentities(before), routeIdentities(after))
	}
}

func TestScanManagerRoutesFollowsReceiverAliases(t *testing.T) {
	root := t.TempDir()
	source := routeFixtureSource(false)
	source = strings.Replace(source, `auth := r.Group("/v1/manager")`, "self := m\n\tauth := r.Group(\"/v1/manager\")", 1)
	source = strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET("/one", self.one)`, 1)
	source = strings.Replace(source, `func (m *Manager) one(c *Context) { m.requireAdmin(c) }`, `func (m *Manager) one(c *Context) { self := m; self.requireAdmin(c) }`, 1)
	writeFixture(t, root, "modules/example/api.go", source)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("ScanManagerRoutes() got %d routes, want 3: %#v", len(routes), routes)
	}
}

func TestScanManagerRoutesRejectsUnresolvedGatedHandler(t *testing.T) {
	root := t.TempDir()
	writeRouteFixture(t, root, false)
	path := "modules/example/api.go"
	contents := routeFixtureSource(false)
	contents = strings.Replace(contents, "auth.GET(\"/one\", m.one)", "auth.GET(\"/one\", wrap(m.one))", 1)
	writeFixture(t, root, path, contents)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ScanManagerRoutes(root, gates)
	if err == nil || !strings.Contains(err.Error(), "cannot resolve route handler") {
		t.Fatalf("ScanManagerRoutes() error = %v, want unresolved handler", err)
	}
}

func TestScanManagerRoutesRejectsUngatedRBACMetaRoute(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "modules/admin_rbac/api.go", `package adminrbac
type API struct{}
type Context struct{}
type Router struct{}
type Group struct{}
func (r *Router) Group(string) *Group { return nil }
func (g *Group) GET(string, ...interface{}) {}
func (a *API) Route(r *Router) {
	auth := r.Group("/v1/manager/rbac")
	auth.GET("/roles", a.listRoles)
}
func (a *API) listRoles(c *Context) {}
`)

	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ScanManagerRoutes(root, gates)
	if err == nil || !strings.Contains(err.Error(), "RBAC meta route") || !strings.Contains(err.Error(), "has no recognized gate") {
		t.Fatalf("ScanManagerRoutes() error = %v, want ungated RBAC meta route failure", err)
	}
}

func TestScanManagerRoutesRejectsUngatedRBACMetaRouteWithDynamicPath(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "modules/admin_rbac/api.go", `package adminrbac
type API struct{}
type Context struct{}
type Router struct{}
type Group struct{}
func (r *Router) Group(string) *Group { return nil }
func (g *Group) GET(string, ...interface{}) {}
func routePath() string { return "/roles" }
func (a *API) Route(r *Router) {
	auth := r.Group("/v1/manager/rbac")
	auth.GET(routePath(), a.listRoles)
}
func (a *API) listRoles(c *Context) {}
`)

	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ScanManagerRoutes(root, gates)
	if err == nil || !strings.Contains(err.Error(), "RBAC meta route") || !strings.Contains(err.Error(), "has no recognized gate") {
		t.Fatalf("ScanManagerRoutes() error = %v, want ungated dynamic RBAC meta route failure", err)
	}
}

func TestScanManagerRoutesRejectsUnresolvedGatedPrefix(t *testing.T) {
	root := t.TempDir()
	contents := strings.Replace(routeFixtureSource(false), `auth := r.Group("/v1/manager")`, `auth := unknown.Group("/v1/manager")`, 1)
	writeFixture(t, root, "modules/example/api.go", contents)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ScanManagerRoutes(root, gates)
	if err == nil || !strings.Contains(err.Error(), "cannot resolve route group prefix") {
		t.Fatalf("ScanManagerRoutes() error = %v, want unresolved prefix", err)
	}
}

func TestScanManagerRoutesIgnoresUnsupportedBusinessShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{"dynamic path", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET(routePath(), m.one)`, 1)
		}},
		{"non-identifier router", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `r.Group("/v1/space").GET("/one", m.one)`, 1)
		}},
		{"inline gate", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET("/one", func(c *Context) { _ = c.CheckLoginRole() })`, 1)
		}},
		{"gated route middleware", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET("/one", m.requireAdmin, m.one)`, 1)
		}},
		{"gated group middleware", func(source string) string {
			return source
		}},
		{"delegated registration", func(source string) string {
			source = strings.Replace(source, `auth.GET("/one", m.one)`, `m.mount(auth)`, 1)
			return source + "\nfunc (m *Manager) mount(auth *Group) { auth.GET(\"/one\", m.one) }\n"
		}},
		{"unknown verb", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.Any("/one", m.one)`, 1)
		}},
		{"handler alias", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `h := m.one
	auth.GET("/one", h)`, 1)
		}},
	}
	for _, prefix := range []string{"/v1/space", "/v1/manager/secrets"} {
		for _, test := range tests {
			t.Run(prefix+"/"+test.name, func(t *testing.T) {
				root := t.TempDir()
				source := strings.ReplaceAll(routeFixtureSource(false), "/v1/manager", prefix)
				source = test.mutate(source)
				if test.name == "gated group middleware" {
					source = strings.Replace(source, `auth := r.Group("`+prefix+`")`, `auth := r.Group("`+prefix+`", m.requireAdmin)`, 1)
				}
				writeFixture(t, root, "modules/example/api.go", source)
				gates, err := ScanDirectGates(root)
				if err != nil {
					t.Fatal(err)
				}
				routes, err := ScanManagerRoutes(root, gates)
				if err != nil {
					t.Fatalf("ScanManagerRoutes() business-surface error = %v", err)
				}
				if len(routes) != 0 {
					t.Fatalf("ScanManagerRoutes() business routes = %#v, want none", routes)
				}
			})
		}
	}
}

func TestPlatformOperationAdmission(t *testing.T) {
	tests := []struct {
		name, method, path, handler string
		want                        bool
	}{
		{"manager", "GET", "/v1/manager/users", "Manager.list", true},
		{"manager groups", "GET", "/v1/manager/groups/:group_no", "Manager.detail", true},
		{"manager spaces", "GET", "/v1/manager/spaces/:space_id", "Manager.detail", true},
		{"platform app bot", "GET", "/v1/admin/app_bot/:id", "AppBot.getBotDetail", true},
		{"appversion", "POST", "/v1/common/appversion", "Common.addAppVersion", true},
		{"platform avatar branch", "POST", "/v1/users/:uid/avatar", "User.uploadAvatar", true},
		{"group ACL", "DELETE", "/v1/groups/:group_no/members", "Group.memberRemove", false},
		{"space ACL", "GET", "/v1/space/:space_id/app_bot/:id", "AppBot.getBotDetail", false},
		{"secrets self service", "GET", "/v1/manager/secrets", "API.list", false},
		{"secrets self service child", "POST", "/v1/manager/secrets/:id", "API.create", false},
		{"avatar business handler", "POST", "/v1/users/:uid/avatar", "User.other", false},
		{"legacy statistics", "GET", "/v1/statistics/countnum", "Statistics.countNum", false},
		{"login", "POST", "/v1/user/login", "User.login", false},
		{"manager me", "GET", "/v1/manager/me", "Manager.me", false},
		{"feature probe", "GET", "/v1/bot/card/profile", "Bot.profile", false},
		{"changelog", "GET", "/v1/common/changelog", "Common.changelog", false},
		{"public", "GET", "/health", "Common.health", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPlatformOperationRoute(test.method, test.path, test.handler); got != test.want {
				t.Fatalf("isPlatformOperationRoute(%s %s, %s) = %v, want %v", test.method, test.path, test.handler, got, test.want)
			}
		})
	}
}

func TestScanManagerRoutesAdmitsOnlyReviewedSurfaces(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "modules/example/api.go", `package example
type Manager struct{}
type Context struct{}
type Router struct{}
type Group struct{}
func (r *Router) Group(string) *Group { return nil }
func (g *Group) Group(string) *Group { return nil }
func (g *Group) GET(string, ...interface{}) {}
func (c *Context) CheckLoginRole() error { return nil }
func (m *Manager) Route(r *Router) {
	manager := r.Group("/v1/manager")
	manager.GET("/groups/:group_no", m.one)
	manager.GET("/spaces/:space_id", m.one)
	manager.GET("/me", m.one)
	secrets := manager.Group("/secrets")
	secrets.GET("/:id", m.one)
	r.Group("/v1/groups").GET("/:group_no", m.one)
	r.Group("/v1/space").GET("/:space_id", m.one)
	r.Group("/v1/statistics").GET("/countnum", m.one)
	appBot := r.Group("/v1/admin/app_bot")
	appBot.GET("/:id", m.one)
	common := r.Group("/v1/common")
	common.GET("/appversion/list", m.one)
	common.GET("/changelog", m.one)
	r.Group("/v1/user").GET("/login", m.one)
	r.Group("/v1/users").GET("/:uid/avatar", m.one)
	r.Group("").GET("/health", m.one)
}
func (m *Manager) one(c *Context) { _ = c.CheckLoginRole() }
`)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"GET /v1/manager/groups/:group_no": true,
		"GET /v1/manager/spaces/:space_id": true,
		"GET /v1/admin/app_bot/:id":        true,
		"GET /v1/common/appversion/list":   true,
	}
	if len(routes) != len(want) {
		t.Fatalf("platform routes = %#v, want exactly %v", routes, want)
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if !want[key] {
			t.Errorf("unexpected admitted route %s", key)
		}
	}
}

func TestScanManagerRoutesRejectsUnsupportedGatedShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"dynamic path", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET(routePath(), m.one)`, 1)
		}, "cannot resolve route path"},
		{"non-identifier router", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `r.Group("/v1/manager").GET("/one", m.one)`, 1)
		}, "cannot resolve route base"},
		{"inline gate", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET("/one", func(c *Context) { _ = c.CheckLoginRole() })`, 1)
		}, "cannot resolve route handler"},
		{"gated route middleware", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `auth.GET("/one", m.requireAdmin, m.one)`, 1)
		}, "gated middleware before route handler"},
		{"gated group middleware", func(source string) string {
			return strings.Replace(source, `auth := r.Group("/v1/manager")`, `auth := r.Group("/v1/manager", m.requireAdmin)`, 1)
		}, "gated group middleware"},
		{"delegated registration", func(source string) string {
			source = strings.Replace(source, `auth.GET("/one", m.one)`, `m.mount(auth)`, 1)
			return source + "\nfunc (m *Manager) mount(auth *Group) { auth.GET(\"/one\", m.one) }\n"
		}, "gated route registration outside Route"},
		{"handler alias", func(source string) string {
			return strings.Replace(source, `auth.GET("/one", m.one)`, `h := m.one
	auth.GET("/one", h)`, 1)
		}, "cannot resolve route handler"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "modules/example/api.go", test.mutate(routeFixtureSource(false)))
			gates, err := ScanDirectGates(root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ScanManagerRoutes(root, gates)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ScanManagerRoutes() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestScanManagerRoutesRejectsUnsupportedPlatformVerbBeforeHandlerAnalysis(t *testing.T) {
	root := t.TempDir()
	source := strings.Replace(routeFixtureSource(false), `auth.GET("/one", m.one)`, `h := m.one
	auth.Any("/one", h)`, 1)
	writeFixture(t, root, "modules/example/api.go", source)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ScanManagerRoutes(root, gates)
	if err == nil {
		t.Fatal("ScanManagerRoutes() error = nil, want unsupported Any verb")
	}
	for _, want := range []string{"unsupported route registration verb Any", "modules/example/api.go:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ScanManagerRoutes() error = %v, want %q", err, want)
		}
	}
}

func TestScanManagerRoutesHonorsBuildConstraints(t *testing.T) {
	root := t.TempDir()
	writeRouteFixture(t, root, false)
	writeFixture(t, root, "modules/example/excluded.go", `//go:build never

package example

func (m *Manager) Route(r *Router) {}
`)
	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ScanManagerRoutes(root, gates)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("ScanManagerRoutes() got %d routes, want 3", len(routes))
	}
}

func TestValidateRouteCoverage(t *testing.T) {
	gate := "modules/example/api.go::Manager.requireAdmin#1"
	routes := []ScannedRoute{
		{Method: "GET", Path: "/v1/manager/one", Handler: "Manager.one", GateSites: []string{gate}, Source: "modules/example/api.go", Line: 10},
		{Method: "GET", Path: "/v1/space/:space_id/example", Handler: "Manager.one", GateSites: []string{gate}, Source: "modules/example/api.go", Line: 11},
	}
	operations := []Operation{{ID: "example.one", Method: "GET", Path: "/v1/manager/one", Handler: "Manager.one", GateSites: []string{gate}}}
	exclusions := []RouteBoundaryExclusion{{Method: "GET", Path: "/v1/space/:space_id/example", Handler: "Manager.one", Reason: "Space ACL"}}
	if err := ValidateRouteCoverage(routes, operations, exclusions); err != nil {
		t.Fatalf("ValidateRouteCoverage() error = %v", err)
	}

	tests := []struct {
		name       string
		routes     []ScannedRoute
		operations []Operation
		exclusions []RouteBoundaryExclusion
		want       string
	}{
		{"unregistered route", append(routes, ScannedRoute{Method: "POST", Path: "/v1/manager/two", Handler: "Manager.two", GateSites: []string{gate}, Source: "modules/example/api.go", Line: 12}), operations, exclusions, "unregistered global route"},
		{"stale operation", routes[1:], operations, exclusions, "no matching source route"},
		{"method drift", routes, []Operation{{ID: "example.one", Method: "POST", Path: "/v1/manager/one", Handler: "Manager.one", GateSites: []string{gate}}}, exclusions, "unregistered global route"},
		{"path drift", routes, []Operation{{ID: "example.one", Method: "GET", Path: "/v1/manager/other", Handler: "Manager.one", GateSites: []string{gate}}}, exclusions, "unregistered global route"},
		{"handler drift", routes, []Operation{{ID: "example.one", Method: "GET", Path: "/v1/manager/one", Handler: "Manager.other", GateSites: []string{gate}}}, exclusions, "handler drift"},
		{"module drift", routes, []Operation{{ID: "example.one", Method: "GET", Path: "/v1/manager/one", Module: "wrong", Handler: "Manager.one", GateSites: []string{gate}}}, exclusions, `operation "example.one" module drift for GET /v1/manager/one: source="" manifest="wrong"`},
		{"gate drift", routes, []Operation{{ID: "example.one", Method: "GET", Path: "/v1/manager/one", Handler: "Manager.one", GateSites: []string{"other"}}}, exclusions, "gate-site drift"},
		{"missing boundary classification", routes, operations, nil, "unregistered global route"},
		{"stale boundary", routes[:1], operations, exclusions, "fixture has no matching source route"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRouteCoverage(test.routes, test.operations, test.exclusions)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRouteCoverage() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRouteCoverageChecksMetaSurfaceGateSites(t *testing.T) {
	gate := "modules/admin_rbac/api.go::API.requireManager#1"
	route := ScannedRoute{Method: "GET", Path: "/v1/manager/rbac/roles", Handler: "API.listRoles", GateSites: []string{gate}, Source: "modules/admin_rbac/api.go", Line: 10}
	exclusion := RouteBoundaryExclusion{Method: "GET", Path: "/v1/manager/rbac/roles", Handler: "API.listRoles", GateSites: []string{gate}, Reason: "RBAC meta-surface"}
	if err := ValidateRouteCoverage([]ScannedRoute{route}, nil, []RouteBoundaryExclusion{exclusion}); err != nil {
		t.Fatalf("ValidateRouteCoverage() error = %v", err)
	}
	exclusion.GateSites = []string{"other"}
	if err := ValidateRouteCoverage([]ScannedRoute{route}, nil, []RouteBoundaryExclusion{exclusion}); err == nil || !strings.Contains(err.Error(), "gate-site drift") {
		t.Fatalf("ValidateRouteCoverage() error = %v, want meta gate-site drift", err)
	}
}

func TestManagerRouteBoundaryExclusionsAreEmpty(t *testing.T) {
	exclusions := ManagerRouteBoundaryExclusions()
	if len(exclusions) != 0 {
		t.Fatalf("boundary exclusion count = %d, want 0 because business routes are not scanned", len(exclusions))
	}
}

func writeRouteFixture(t *testing.T, root string, formatted bool) {
	t.Helper()
	writeFixture(t, root, "modules/example/api.go", routeFixtureSource(formatted))
}

func routeFixtureSource(formatted bool) string {
	gap := ""
	if formatted {
		gap = "\n\n"
	}
	return `package example
type Manager struct{}
type Context struct{}
type Router struct{}
type Group struct{}
func (r *Router) Group(string) *Group { return nil }
func (g *Group) GET(string, ...interface{}) {}
func (g *Group) POST(string, ...interface{}) {}
func (g *Group) DELETE(string, ...interface{}) {}
func (c *Context) CheckLoginRole() error { return nil }
func (c *Context) CheckLoginRoleIsSuperAdmin() error { return nil }
func wrap(interface{}) interface{} { return nil }
` + gap + `func (m *Manager) Route(r *Router) {
	auth := r.Group("/v1/manager")
	auth.GET("/one", m.one)
	auth.POST("two", m.two)
	auth.DELETE("/raw", m.raw)
}
func (m *Manager) one(c *Context) { m.requireAdmin(c) }
func (m *Manager) two(c *Context) { m.requireAdmin(c) }
func (m *Manager) raw(c *Context) { _ = c.CheckLoginRoleIsSuperAdmin() }
func (m *Manager) requireAdmin(c *Context) { _ = c.CheckLoginRole() }
`
}

func routeIdentities(routes []ScannedRoute) string {
	var values []string
	for _, route := range routes {
		values = append(values, route.Method+" "+route.Path+" "+route.Handler+" "+strings.Join(route.GateSites, ","))
	}
	return strings.Join(values, "\n")
}
