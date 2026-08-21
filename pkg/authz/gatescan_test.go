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
	if gates[0].LegacyGate != LegacyGateAdmin || gates[1].LegacyGate != LegacyGateSuperAdmin || gates[2].LegacyGate != LegacyGateManagerConsoleRole {
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
