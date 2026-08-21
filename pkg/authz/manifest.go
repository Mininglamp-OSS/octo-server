package authz

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = 1

type Sensitivity string

const (
	SensitivityStandard Sensitivity = "standard"
	SensitivityElevated Sensitivity = "elevated"
	SensitivityCritical Sensitivity = "critical"
)

type LegacyGate string

const (
	LegacyGateAdmin              LegacyGate = "admin"
	LegacyGateSuperAdmin         LegacyGate = "super_admin"
	LegacyGateManagerConsoleRole LegacyGate = "manager_console_role"
)

type Scope string

const (
	ScopeGlobalAdmin                Scope = "global_admin"
	ScopeGlobalAdminWithBusinessACL Scope = "global_admin_with_business_acl"
)

type AggregateMode string

const (
	AggregateAny AggregateMode = "any"
	AggregateAll AggregateMode = "all"
)

type Manifest struct {
	SchemaVersion      int                `yaml:"schema_version" json:"schema_version"`
	Permissions        []Permission       `yaml:"permissions" json:"permissions"`
	LegacyCapabilities []LegacyCapability `yaml:"legacy_capabilities" json:"legacy_capabilities"`
	GateSites          []GateSite         `yaml:"gate_sites" json:"gate_sites"`
	Operations         []Operation        `yaml:"operations" json:"operations"`
}

type Permission struct {
	Key                 string               `yaml:"key" json:"key"`
	Resource            string               `yaml:"resource" json:"resource"`
	Action              string               `yaml:"action" json:"action"`
	Description         string               `yaml:"description" json:"description"`
	Sensitivity         Sensitivity          `yaml:"sensitivity" json:"sensitivity"`
	ExternalEnforcement *ExternalEnforcement `yaml:"external_enforcement,omitempty" json:"external_enforcement,omitempty"`
}

type ExternalEnforcement struct {
	Service     string `yaml:"service" json:"service"`
	Description string `yaml:"description" json:"description"`
}

type LegacyCapability struct {
	Key         string        `yaml:"key" json:"key"`
	Permissions []string      `yaml:"permissions" json:"permissions"`
	Mode        AggregateMode `yaml:"mode" json:"mode"`
	Description string        `yaml:"description" json:"description"`
}

type GateSite struct {
	Source     string     `yaml:"source" json:"source"`
	Module     string     `yaml:"module" json:"module"`
	Symbol     string     `yaml:"symbol" json:"symbol"`
	LegacyGate LegacyGate `yaml:"legacy_gate" json:"legacy_gate"`
}

type Operation struct {
	ID          string       `yaml:"id" json:"id"`
	Method      string       `yaml:"method" json:"method"`
	Path        string       `yaml:"path" json:"path"`
	Module      string       `yaml:"module" json:"module"`
	Handler     string       `yaml:"handler" json:"handler"`
	Permission  string       `yaml:"permission" json:"permission"`
	GateSites   []string     `yaml:"gate_sites" json:"gate_sites"`
	Scope       Scope        `yaml:"scope" json:"scope"`
	BusinessACL *BusinessACL `yaml:"business_acl,omitempty" json:"business_acl,omitempty"`
}

type BusinessACL struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description" json:"description"`
}

func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return ParseManifest(data)
}

func ParseManifest(data []byte) (*Manifest, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := validateManifestShape(&document); err != nil {
		return nil, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("manifest: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema_version: unsupported value %d", manifest.SchemaVersion)
	}
	if err := validateEnums(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateManifestShape(document *yaml.Node) error {
	if len(document.Content) != 1 {
		return fmt.Errorf("manifest: expected one YAML document")
	}
	root := document.Content[0]
	fields, err := mappingFields(root, "manifest")
	if err != nil {
		return err
	}
	for _, name := range []string{"schema_version", "permissions", "legacy_capabilities", "gate_sites", "operations"} {
		if fields[name] == nil {
			return fmt.Errorf("%s: required field is missing", name)
		}
	}
	if fields["schema_version"].Tag != "!!int" {
		return fmt.Errorf("schema_version: expected integer")
	}
	if err := validateSequence(fields["permissions"], "permissions", []string{"key", "resource", "action", "description", "sensitivity"}, nil); err != nil {
		return err
	}
	for i, item := range fields["permissions"].Content {
		itemFields, err := mappingFields(item, fmt.Sprintf("permissions[%d]", i))
		if err != nil {
			return err
		}
		if external := itemFields["external_enforcement"]; external != nil {
			if _, err := requireMappingFields(external, fmt.Sprintf("permissions[%d].external_enforcement", i), []string{"service", "description"}, nil); err != nil {
				return err
			}
		}
	}
	if err := validateSequence(fields["legacy_capabilities"], "legacy_capabilities", []string{"key", "permissions", "mode", "description"}, map[string]yaml.Kind{"permissions": yaml.SequenceNode}); err != nil {
		return err
	}
	if err := validateSequence(fields["gate_sites"], "gate_sites", []string{"source", "module", "symbol", "legacy_gate"}, nil); err != nil {
		return err
	}
	if err := validateSequence(fields["operations"], "operations", []string{"id", "method", "path", "module", "handler", "permission", "gate_sites", "scope"}, map[string]yaml.Kind{"gate_sites": yaml.SequenceNode}); err != nil {
		return err
	}
	for i, item := range fields["operations"].Content {
		itemFields, err := mappingFields(item, fmt.Sprintf("operations[%d]", i))
		if err != nil {
			return err
		}
		if acl := itemFields["business_acl"]; acl != nil {
			if _, err := requireMappingFields(acl, fmt.Sprintf("operations[%d].business_acl", i), []string{"type", "description"}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSequence(node *yaml.Node, path string, required []string, kinds map[string]yaml.Kind) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s: expected sequence", path)
	}
	for i, item := range node.Content {
		if _, err := requireMappingFields(item, fmt.Sprintf("%s[%d]", path, i), required, kinds); err != nil {
			return err
		}
	}
	return nil
}

func requireMappingFields(node *yaml.Node, path string, required []string, kinds map[string]yaml.Kind) (map[string]*yaml.Node, error) {
	fields, err := mappingFields(node, path)
	if err != nil {
		return nil, err
	}
	for _, name := range required {
		value := fields[name]
		if value == nil {
			return nil, fmt.Errorf("%s.%s: required field is missing", path, name)
		}
		expected := yaml.ScalarNode
		if kinds != nil && kinds[name] != 0 {
			expected = kinds[name]
		}
		if value.Kind != expected {
			return nil, fmt.Errorf("%s.%s: unexpected YAML type", path, name)
		}
	}
	return fields, nil
}

func mappingFields(node *yaml.Node, path string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected mapping", path)
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if fields[name] != nil {
			return nil, fmt.Errorf("%s.%s: duplicate field", path, name)
		}
		fields[name] = node.Content[i+1]
	}
	return fields, nil
}

func validateEnums(manifest *Manifest) error {
	for i, permission := range manifest.Permissions {
		if !oneOf(string(permission.Sensitivity), string(SensitivityStandard), string(SensitivityElevated), string(SensitivityCritical)) {
			return fmt.Errorf("permissions[%d].sensitivity: invalid value %q", i, permission.Sensitivity)
		}
	}
	for i, capability := range manifest.LegacyCapabilities {
		if !oneOf(string(capability.Mode), string(AggregateAny), string(AggregateAll)) {
			return fmt.Errorf("legacy_capabilities[%d].mode: invalid value %q", i, capability.Mode)
		}
	}
	for i, gate := range manifest.GateSites {
		if !oneOf(string(gate.LegacyGate), string(LegacyGateAdmin), string(LegacyGateSuperAdmin), string(LegacyGateManagerConsoleRole)) {
			return fmt.Errorf("gate_sites[%d].legacy_gate: invalid value %q", i, gate.LegacyGate)
		}
	}
	for i, operation := range manifest.Operations {
		if !oneOf(string(operation.Scope), string(ScopeGlobalAdmin), string(ScopeGlobalAdminWithBusinessACL)) {
			return fmt.Errorf("operations[%d].scope: invalid value %q", i, operation.Scope)
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
