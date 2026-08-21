package authz

import (
	"strings"
	"testing"
)

const emptyManifest = `schema_version: 1
permissions: []
legacy_capabilities: []
gate_sites: []
operations: []
`

func TestParseManifestEmptyCollections(t *testing.T) {
	manifest, err := ParseManifest([]byte(emptyManifest))
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.Permissions == nil || manifest.LegacyCapabilities == nil || manifest.GateSites == nil || manifest.Operations == nil {
		t.Fatalf("ParseManifest() did not preserve the complete empty skeleton: %#v", manifest)
	}
}

func TestParseManifestRejectsUnknownField(t *testing.T) {
	assertParseErrorContains(t, emptyManifest+"unexpected: true\n", "field unexpected not found")
}

func TestParseManifestRejectsMissingDuplicateAndWrongType(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing top-level field",
			yaml: `schema_version: 1
permissions: []
legacy_capabilities: []
gate_sites: []
`,
			wantErr: "operations: required field is missing",
		},
		{
			name: "missing nested field",
			yaml: `schema_version: 1
permissions:
  - key: user.read
    resource: user
    action: read
    description: Read users
legacy_capabilities: []
gate_sites: []
operations: []
`,
			wantErr: "permissions[0].sensitivity",
		},
		{
			name:    "duplicate field",
			yaml:    emptyManifest + "operations: []\n",
			wantErr: "manifest.operations: duplicate field",
		},
		{
			name:    "wrong collection type",
			yaml:    strings.Replace(emptyManifest, "permissions: []", "permissions: wrong", 1),
			wantErr: "permissions: expected sequence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertParseErrorContains(t, test.yaml, test.wantErr)
		})
	}
}

func TestParseManifestRejectsInvalidEnums(t *testing.T) {
	tests := []struct {
		old     string
		new     string
		wantErr string
	}{
		{"sensitivity: critical", "sensitivity: severe", "permissions[0].sensitivity"},
		{"mode: any", "mode: some", "legacy_capabilities[0].mode"},
		{"legacy_gate: super_admin", "legacy_gate: root", "gate_sites[0].legacy_gate"},
		{"scope: global_admin", "scope: global", "operations[0].scope"},
	}
	for _, test := range tests {
		assertParseErrorContains(t, strings.Replace(validManifestWithEnums(), test.old, test.new, 1), test.wantErr)
	}
}

func assertParseErrorContains(t *testing.T, input, want string) {
	t.Helper()
	_, err := ParseManifest([]byte(input))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ParseManifest() error = %v, want substring %q", err, want)
	}
}

func validManifestWithEnums() string {
	return `schema_version: 1
permissions:
  - key: user.read
    resource: user
    action: read
    description: Read users
    sensitivity: critical
legacy_capabilities:
  - key: users.read
    permissions: [user.read]
    mode: any
    description: Existing user read capability
gate_sites:
  - source: modules/user/api_manager.go::Manager.list#1
    module: user
    symbol: Manager.list
    legacy_gate: super_admin
operations:
  - id: user.list
    method: GET
    path: /v1/manager/users
    module: user
    handler: Manager.list
    permission: user.read
    gate_sites: [modules/user/api_manager.go::Manager.list#1]
    scope: global_admin
`
}
