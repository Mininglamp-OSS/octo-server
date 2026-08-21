package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanDirectGatesFindsOnlyProductionCalls(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "modules/example/api.go", `package example
type Manager struct{}
type Context struct{}
func (c *Context) CheckLoginRole() error { return nil }
func (c *Context) CheckLoginRoleIsSuperAdmin() error { return nil }
func (c *Context) CheckLoginRoleSimilar() error { return nil }
func (m *Manager) handle(c *Context) {
	// c.CheckLoginRole()
	_ = c.CheckLoginRole()
	_ = c.CheckLoginRoleSimilar()
	_ = c.CheckLoginRoleIsSuperAdmin()
	_ = auth.CanReadManagerDashboard("dashboardReader")
}
`)
	writeFixture(t, root, "modules/example/api_test.go", `package example
func ignored(c *Context) { _ = c.CheckLoginRole() }
`)
	writeFixture(t, root, "modules/example/excluded.go", `//go:build never

package example
func excluded(c *Context) { _ = c.CheckLoginRole() }
`)

	gates, err := ScanDirectGates(root)
	if err != nil {
		t.Fatalf("ScanDirectGates() error = %v", err)
	}
	if len(gates) != 3 {
		t.Fatalf("ScanDirectGates() got %d gates, want 3: %#v", len(gates), gates)
	}
	if gates[0].Source != "modules/example/api.go::Manager.handle#1" || gates[1].Source != "modules/example/api.go::Manager.handle#2" || gates[2].Source != "modules/example/api.go::Manager.handle#3" {
		t.Fatalf("unexpected identities: %#v", gates)
	}
	if gates[0].LegacyGate != LegacyGateAdmin || gates[1].LegacyGate != LegacyGateSuperAdmin || gates[2].LegacyGate != LegacyGateDashboardReadPolicy {
		t.Fatalf("unexpected legacy gates: %#v", gates)
	}
}

func TestGateIdentityIgnoresFormattingAndBlankLines(t *testing.T) {
	root := t.TempDir()
	path := "modules/example/api.go"
	compact := `package example
type Manager struct{}
func (m *Manager) handle(c interface{ CheckLoginRole() error }) { _ = c.CheckLoginRole() }
`
	writeFixture(t, root, path, compact)
	before, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	formatted := strings.Replace(compact, "func (m", "\n\nfunc (m", 1)
	writeFixture(t, root, path, formatted)
	after, err := ScanDirectGates(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || before[0].Source != after[0].Source {
		t.Fatalf("identity changed after blank lines: before=%#v after=%#v", before, after)
	}
	if before[0].Line == after[0].Line {
		t.Fatalf("test fixture did not move the source line")
	}
}

func TestValidateRecognizedGateLocations(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "modules/example/api.go", `package example
func allowed(c interface{ CheckLoginRole() error }) { _ = c.CheckLoginRole() }
`)
	writeFixture(t, root, "pkg/example/api_test.go", `package example
func ignored(c interface{ CheckLoginRole() error }) { _ = c.CheckLoginRole() }
`)
	writeFixture(t, root, "internal/example/excluded.go", `//go:build never

package example
func excluded(c interface{ CheckLoginRole() error }) { _ = c.CheckLoginRole() }
`)
	for _, relative := range []string{".git/ignored.go", "vendor/ignored.go", "testdata/ignored.go"} {
		writeFixture(t, root, relative, `package ignored
func ignored(c interface{ CheckLoginRole() error }) { _ = c.CheckLoginRole() }
`)
	}
	if err := ValidateRecognizedGateLocations(root); err != nil {
		t.Fatalf("ValidateRecognizedGateLocations() error = %v", err)
	}

	tests := []struct {
		name, relative, call string
	}{
		{"pkg admin", "pkg/example/api.go", "c.CheckLoginRole()"},
		{"internal super admin", "internal/example/api.go", "c.CheckLoginRoleIsSuperAdmin()"},
		{"cmd dashboard", "cmd/example/main.go", `auth.CanReadManagerDashboard("dashboardReader")`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixtureRoot := t.TempDir()
			writeFixture(t, fixtureRoot, test.relative, "package example\nfunc rejected(c interface{}) { _ = "+test.call+" }\n")
			err := ValidateRecognizedGateLocations(fixtureRoot)
			if err == nil {
				t.Fatal("ValidateRecognizedGateLocations() error = nil, want unsupported location")
			}
			for _, want := range []string{test.relative + ":2", "recognized gate", "outside modules"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("ValidateRecognizedGateLocations() error = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestValidateGateInventory(t *testing.T) {
	scanned := []ScannedGate{{
		Source: "modules/example/api.go::Manager.handle#1", Module: "example", Symbol: "Manager.handle", LegacyGate: LegacyGateAdmin, Line: 10,
	}}
	declared := []GateSite{{
		Source: scanned[0].Source, Module: "example", Symbol: "Manager.handle", LegacyGate: LegacyGateAdmin,
	}}
	if err := ValidateGateInventory(scanned, declared); err != nil {
		t.Fatalf("ValidateGateInventory() error = %v", err)
	}

	tests := []struct {
		name    string
		scanned []ScannedGate
		items   []GateSite
		want    string
	}{
		{"unregistered", scanned, nil, "unregistered direct gate"},
		{"stale", nil, declared, "does not exist in source"},
		{"duplicate", scanned, append(declared, declared[0]), "duplicate source identity"},
		{"wrong gate", scanned, []GateSite{{Source: scanned[0].Source, Module: "example", Symbol: "Manager.handle", LegacyGate: LegacyGateSuperAdmin}}, "legacy_gate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateGateInventory(test.scanned, test.items)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateGateInventory() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPlatformGatesFiltersBusinessOnlySources(t *testing.T) {
	scanned := []ScannedGate{
		{Source: "modules/example/api.go::Manager.platform#1"},
		{Source: "modules/example/api.go::Manager.business#1"},
	}
	routes := []ScannedRoute{{
		Method: "GET", Path: "/v1/manager/example", GateSites: []string{scanned[0].Source},
	}}
	got, err := PlatformGates(scanned, routes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != scanned[0].Source {
		t.Fatalf("PlatformGates() = %#v, want only %s", got, scanned[0].Source)
	}
}

func writeFixture(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
