package authz

import (
	"strings"
	"testing"
)

func TestValidateManifestAcceptsManyToManyGateReferences(t *testing.T) {
	manifest := referenceManifest()
	if err := ValidateManifest(&manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestValidatePermissions(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"invalid key", func(m *Manifest) { m.Permissions[0].Key = "UserRead" }, "permissions[0].key"},
		{"duplicate key", func(m *Manifest) { m.Permissions[1].Key = m.Permissions[0].Key }, "duplicate permission key"},
		{"empty resource", func(m *Manifest) { m.Permissions[0].Resource = "" }, "permissions[0].resource"},
		{"empty action", func(m *Manifest) { m.Permissions[0].Action = "" }, "permissions[0].action"},
		{"empty description", func(m *Manifest) { m.Permissions[0].Description = "" }, "permissions[0].description"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := referenceManifest()
			test.mutate(&manifest)
			assertValidationErrorContains(t, &manifest, test.wantErr)
		})
	}
}

func TestValidateGateSitesAndOperations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"invalid source", func(m *Manifest) { m.GateSites[0].Source = "bad" }, "gate_sites[0].source"},
		{"duplicate source", func(m *Manifest) {
			m.GateSites[1].Source = m.GateSites[0].Source
			m.GateSites[1].Symbol = m.GateSites[0].Symbol
		}, "duplicate source identity"},
		{"source symbol mismatch", func(m *Manifest) { m.GateSites[0].Symbol = "Manager.other" }, "does not match source symbol"},
		{"duplicate operation ID", func(m *Manifest) { m.Operations[1].ID = m.Operations[0].ID }, "duplicate operation ID"},
		{"invalid method", func(m *Manifest) { m.Operations[0].Method = "get" }, "operations[0].method"},
		{"invalid path", func(m *Manifest) { m.Operations[0].Path = "v1//users" }, "operations[0].path"},
		{"missing handler", func(m *Manifest) { m.Operations[0].Handler = "" }, "operations[0].handler"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := referenceManifest()
			test.mutate(&manifest)
			assertValidationErrorContains(t, &manifest, test.wantErr)
		})
	}
}

func TestValidateReferences(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"unknown permission", func(m *Manifest) { m.Operations[0].Permission = "unknown.read" }, "unknown permission"},
		{"unknown gate site", func(m *Manifest) { m.Operations[0].GateSites[0] = "modules/unknown.go::Manager.nope#1" }, "unknown gate site"},
		{"operation without gate", func(m *Manifest) { m.Operations[0].GateSites = nil }, "operations[0].gate_sites"},
		{"orphan gate", func(m *Manifest) {
			m.Operations[0].GateSites = m.Operations[0].GateSites[:1]
			m.Operations[1].GateSites = m.Operations[1].GateSites[:1]
		}, "is not referenced by any operation"},
		{"unreferenced permission", func(m *Manifest) {
			m.LegacyCapabilities = nil
			m.Operations[1].Permission = m.Operations[0].Permission
			m.Permissions[1].ExternalEnforcement = &ExternalEnforcement{Service: "external", Description: "classified but unused"}
		}, "is not referenced by any operation or legacy capability"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := referenceManifest()
			test.mutate(&manifest)
			assertValidationErrorContains(t, &manifest, test.wantErr)
		})
	}
}

func TestValidateLegacyCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"duplicate key", func(m *Manifest) { m.LegacyCapabilities = append(m.LegacyCapabilities, m.LegacyCapabilities[0]) }, "duplicate legacy capability key"},
		{"empty aggregate", func(m *Manifest) { m.LegacyCapabilities[0].Permissions = nil }, "legacy_capabilities[0].permissions"},
		{"unknown permission", func(m *Manifest) { m.LegacyCapabilities[0].Permissions[0] = "unknown.read" }, "unknown permission"},
		{"duplicate permission", func(m *Manifest) { m.LegacyCapabilities[0].Permissions = []string{"user.read", "user.read"} }, "duplicate permission"},
		{"empty description", func(m *Manifest) { m.LegacyCapabilities[0].Description = "" }, "legacy_capabilities[0].description"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := referenceManifest()
			test.mutate(&manifest)
			assertValidationErrorContains(t, &manifest, test.wantErr)
		})
	}
}

func TestValidateCriticalPermissions(t *testing.T) {
	manifest := Manifest{}
	for _, key := range criticalPermissionKeys {
		manifest.Permissions = append(manifest.Permissions, Permission{Key: key, Sensitivity: SensitivityCritical})
	}
	if err := ValidateCriticalPermissions(&manifest); err != nil {
		t.Fatalf("ValidateCriticalPermissions() error = %v", err)
	}

	missing := manifest
	missing.Permissions = append([]Permission(nil), manifest.Permissions[1:]...)
	if err := ValidateCriticalPermissions(&missing); err == nil || !strings.Contains(err.Error(), criticalPermissionKeys[0]) {
		t.Fatalf("missing critical permission error = %v", err)
	}

	wrongSensitivity := manifest
	wrongSensitivity.Permissions = append([]Permission(nil), manifest.Permissions...)
	wrongSensitivity.Permissions[0].Sensitivity = SensitivityElevated
	if err := ValidateCriticalPermissions(&wrongSensitivity); err == nil || !strings.Contains(err.Error(), "sensitivity") {
		t.Fatalf("wrong critical sensitivity error = %v", err)
	}
}

func TestValidateExternalEnforcement(t *testing.T) {
	externalOnly := referenceManifest()
	externalOnly.Operations = externalOnly.Operations[:1]
	externalOnly.LegacyCapabilities[0].Permissions = append(externalOnly.LegacyCapabilities[0].Permissions, "user.write")
	externalOnly.Permissions[1].ExternalEnforcement = &ExternalEnforcement{Service: "octo-marketplace", Description: "external policy"}
	if err := ValidateManifest(&externalOnly); err != nil {
		t.Fatalf("ValidateManifest() external-only permission error = %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"missing classification", func(m *Manifest) {
			m.Operations = m.Operations[:1]
			m.LegacyCapabilities[0].Permissions = append(m.LegacyCapabilities[0].Permissions, "user.write")
		}, "external_enforcement: required"},
		{"local operation marked external", func(m *Manifest) {
			m.Permissions[0].ExternalEnforcement = &ExternalEnforcement{Service: "other", Description: "wrong classification"}
		}, "must be absent"},
		{"missing service", func(m *Manifest) {
			m.Permissions[0].ExternalEnforcement = &ExternalEnforcement{Description: "missing service"}
		}, "external_enforcement.service"},
		{"missing description", func(m *Manifest) {
			m.Permissions[0].ExternalEnforcement = &ExternalEnforcement{Service: "other"}
		}, "external_enforcement.description"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := referenceManifest()
			test.mutate(&manifest)
			assertValidationErrorContains(t, &manifest, test.wantErr)
		})
	}
}

func assertValidationErrorContains(t *testing.T, manifest *Manifest, want string) {
	t.Helper()
	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ValidateManifest() error = %v, want substring %q", err, want)
	}
}

func referenceManifest() Manifest {
	gateA := "modules/user/api_manager.go::Manager.requireAdmin#1"
	gateB := "modules/user/api_manager.go::Manager.requireSuperAdmin#1"
	return Manifest{
		SchemaVersion: SchemaVersion,
		Permissions: []Permission{
			{Key: "user.read", Resource: "user", Action: "read", Description: "Read users", Sensitivity: SensitivityStandard},
			{Key: "user.write", Resource: "user", Action: "write", Description: "Write users", Sensitivity: SensitivityElevated},
		},
		LegacyCapabilities: []LegacyCapability{
			{Key: "users.read", Permissions: []string{"user.read"}, Mode: AggregateAny, Description: "Existing user read capability"},
		},
		GateSites: []GateSite{
			{Source: gateA, Module: "user", Symbol: "Manager.requireAdmin", LegacyGate: LegacyGateAdmin},
			{Source: gateB, Module: "user", Symbol: "Manager.requireSuperAdmin", LegacyGate: LegacyGateSuperAdmin},
		},
		Operations: []Operation{
			{ID: "user.list", Method: "GET", Path: "/v1/manager/users", Module: "user", Handler: "Manager.list", Permission: "user.read", GateSites: []string{gateA, gateB}, Scope: ScopeGlobalAdmin},
			{ID: "user.update", Method: "PUT", Path: "/v1/manager/users/:uid", Module: "user", Handler: "Manager.update", Permission: "user.write", GateSites: []string{gateA}, Scope: ScopeGlobalAdmin},
		},
	}
}
