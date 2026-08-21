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

func TestScanManagerRoutesRejectsUnresolvedGatedPrefix(t *testing.T) {
	root := t.TempDir()
	contents := strings.Replace(routeFixtureSource(false), `auth := r.Group("/v1/manager")`, `auth := unknown.Group(routePrefix())`, 1)
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

func TestManagerRouteBoundaryExclusionsContainOnlySpaceAppBotRoutes(t *testing.T) {
	exclusions := ManagerRouteBoundaryExclusions()
	if len(exclusions) != 7 {
		t.Fatalf("boundary exclusion count = %d, want 7", len(exclusions))
	}
	for _, exclusion := range exclusions {
		if !strings.HasPrefix(exclusion.Path, "/v1/space/:space_id/app_bot/") {
			t.Errorf("unexpected business ACL exclusion: %#v", exclusion)
		}
		if exclusion.Reason == "" {
			t.Errorf("boundary exclusion has no reason: %#v", exclusion)
		}
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
