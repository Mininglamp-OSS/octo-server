package authz

import (
	"fmt"
	"net/http"
	pathpkg "path"
	"regexp"
	"strings"
)

var (
	permissionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	legacyKeyPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	operationIDPattern   = permissionKeyPattern
	gateSourcePattern    = regexp.MustCompile(`^([A-Za-z0-9_./-]+\.go)::([A-Za-z_][A-Za-z0-9_.]*)#([1-9][0-9]*)$`)
)

var criticalPermissionKeys = []string{
	"user.password.reset",
	"user.admin.manage",
	"backup.download",
	"message.direct_history.read",
	"app_bot.token.reveal",
	"system_setting.write",
	"space.destroy",
}

func ValidateManifest(manifest *Manifest) error {
	if manifest == nil {
		return fmt.Errorf("manifest: must not be nil")
	}
	if err := validateEnums(manifest); err != nil {
		return err
	}
	permissions, err := validatePermissions(manifest.Permissions)
	if err != nil {
		return err
	}
	gates, err := validateGateSites(manifest.GateSites)
	if err != nil {
		return err
	}
	permissionRefs, gateRefs, err := validateOperations(manifest.Operations, permissions, gates)
	if err != nil {
		return err
	}
	if err := validateLegacyCapabilities(manifest.LegacyCapabilities, permissions, permissionRefs); err != nil {
		return err
	}
	for source := range gates {
		if gateRefs[source] == 0 {
			return fmt.Errorf("gate_sites[%q]: is not referenced by any operation", source)
		}
	}
	for key := range permissions {
		if permissionRefs[key] == 0 {
			return fmt.Errorf("permissions[%q]: is not referenced by any operation or legacy capability", key)
		}
	}
	return nil
}

func ValidateCriticalPermissions(manifest *Manifest) error {
	permissions := make(map[string]Permission, len(manifest.Permissions))
	for _, permission := range manifest.Permissions {
		permissions[permission.Key] = permission
	}
	for _, key := range criticalPermissionKeys {
		permission, ok := permissions[key]
		if !ok {
			return fmt.Errorf("permissions[%q]: required critical permission is missing", key)
		}
		if permission.Sensitivity != SensitivityCritical {
			return fmt.Errorf("permissions[%q].sensitivity: must be %q", key, SensitivityCritical)
		}
	}
	return nil
}

func validatePermissions(items []Permission) (map[string]Permission, error) {
	result := make(map[string]Permission, len(items))
	for i, permission := range items {
		path := fmt.Sprintf("permissions[%d]", i)
		if !permissionKeyPattern.MatchString(permission.Key) {
			return nil, fmt.Errorf("%s.key: invalid permission key %q", path, permission.Key)
		}
		if _, exists := result[permission.Key]; exists {
			return nil, fmt.Errorf("%s.key: duplicate permission key %q", path, permission.Key)
		}
		if strings.TrimSpace(permission.Resource) == "" {
			return nil, fmt.Errorf("%s.resource: must not be empty", path)
		}
		if strings.TrimSpace(permission.Action) == "" {
			return nil, fmt.Errorf("%s.action: must not be empty", path)
		}
		if strings.TrimSpace(permission.Description) == "" {
			return nil, fmt.Errorf("%s.description: must not be empty", path)
		}
		result[permission.Key] = permission
	}
	return result, nil
}

func validateGateSites(items []GateSite) (map[string]GateSite, error) {
	result := make(map[string]GateSite, len(items))
	for i, gate := range items {
		path := fmt.Sprintf("gate_sites[%d]", i)
		match := gateSourcePattern.FindStringSubmatch(gate.Source)
		if match == nil || strings.Contains(match[1], "..") {
			return nil, fmt.Errorf("%s.source: invalid source identity %q", path, gate.Source)
		}
		if _, exists := result[gate.Source]; exists {
			return nil, fmt.Errorf("%s.source: duplicate source identity %q", path, gate.Source)
		}
		if strings.TrimSpace(gate.Module) == "" {
			return nil, fmt.Errorf("%s.module: must not be empty", path)
		}
		if strings.TrimSpace(gate.Symbol) == "" {
			return nil, fmt.Errorf("%s.symbol: must not be empty", path)
		}
		if gate.Symbol != match[2] {
			return nil, fmt.Errorf("%s.symbol: %q does not match source symbol %q", path, gate.Symbol, match[2])
		}
		result[gate.Source] = gate
	}
	return result, nil
}

func validateOperations(items []Operation, permissions map[string]Permission, gates map[string]GateSite) (map[string]int, map[string]int, error) {
	ids := make(map[string]struct{}, len(items))
	permissionRefs := make(map[string]int, len(permissions))
	gateRefs := make(map[string]int, len(gates))
	for i, operation := range items {
		path := fmt.Sprintf("operations[%d]", i)
		if !operationIDPattern.MatchString(operation.ID) {
			return nil, nil, fmt.Errorf("%s.id: invalid operation ID %q", path, operation.ID)
		}
		if _, exists := ids[operation.ID]; exists {
			return nil, nil, fmt.Errorf("%s.id: duplicate operation ID %q", path, operation.ID)
		}
		ids[operation.ID] = struct{}{}
		if !validHTTPMethod(operation.Method) {
			return nil, nil, fmt.Errorf("%s.method: invalid HTTP method %q", path, operation.Method)
		}
		if !normalizedPath(operation.Path) {
			return nil, nil, fmt.Errorf("%s.path: path %q is not normalized", path, operation.Path)
		}
		if strings.TrimSpace(operation.Module) == "" {
			return nil, nil, fmt.Errorf("%s.module: must not be empty", path)
		}
		if strings.TrimSpace(operation.Handler) == "" {
			return nil, nil, fmt.Errorf("%s.handler: must not be empty", path)
		}
		if _, exists := permissions[operation.Permission]; !exists {
			return nil, nil, fmt.Errorf("%s.permission: unknown permission %q", path, operation.Permission)
		}
		permissionRefs[operation.Permission]++
		if len(operation.GateSites) == 0 {
			return nil, nil, fmt.Errorf("%s.gate_sites: must not be empty", path)
		}
		seen := make(map[string]struct{}, len(operation.GateSites))
		for j, source := range operation.GateSites {
			if _, exists := gates[source]; !exists {
				return nil, nil, fmt.Errorf("%s.gate_sites[%d]: unknown gate site %q", path, j, source)
			}
			if _, exists := seen[source]; exists {
				return nil, nil, fmt.Errorf("%s.gate_sites[%d]: duplicate gate site %q", path, j, source)
			}
			seen[source] = struct{}{}
			gateRefs[source]++
		}
		if operation.Scope == ScopeGlobalAdminWithBusinessACL {
			if operation.BusinessACL == nil || strings.TrimSpace(operation.BusinessACL.Type) == "" || strings.TrimSpace(operation.BusinessACL.Description) == "" {
				return nil, nil, fmt.Errorf("%s.business_acl: type and description are required for mixed scope", path)
			}
		} else if operation.BusinessACL != nil {
			return nil, nil, fmt.Errorf("%s.business_acl: only allowed for mixed scope", path)
		}
	}
	return permissionRefs, gateRefs, nil
}

func validateLegacyCapabilities(items []LegacyCapability, permissions map[string]Permission, permissionRefs map[string]int) error {
	seen := make(map[string]struct{}, len(items))
	for i, capability := range items {
		path := fmt.Sprintf("legacy_capabilities[%d]", i)
		if !legacyKeyPattern.MatchString(capability.Key) {
			return fmt.Errorf("%s.key: invalid legacy capability key %q", path, capability.Key)
		}
		if _, exists := seen[capability.Key]; exists {
			return fmt.Errorf("%s.key: duplicate legacy capability key %q", path, capability.Key)
		}
		seen[capability.Key] = struct{}{}
		if len(capability.Permissions) == 0 {
			return fmt.Errorf("%s.permissions: must not be empty", path)
		}
		if strings.TrimSpace(capability.Description) == "" {
			return fmt.Errorf("%s.description: must not be empty", path)
		}
		permissionSeen := make(map[string]struct{}, len(capability.Permissions))
		for j, key := range capability.Permissions {
			if _, exists := permissions[key]; !exists {
				return fmt.Errorf("%s.permissions[%d]: unknown permission %q", path, j, key)
			}
			if _, exists := permissionSeen[key]; exists {
				return fmt.Errorf("%s.permissions[%d]: duplicate permission %q", path, j, key)
			}
			permissionSeen[key] = struct{}{}
			permissionRefs[key]++
		}
	}
	return nil
}

func validHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func normalizedPath(value string) bool {
	return strings.HasPrefix(value, "/") &&
		!strings.ContainsAny(value, "?#") &&
		(value == "/" || !strings.HasSuffix(value, "/")) &&
		pathpkg.Clean(value) == value
}
