package auth

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	contract "github.com/Mininglamp-OSS/octo-server/pkg/authz"
)

func TestManagerPermissionContractPreservesLegacyCapabilityMatrix(t *testing.T) {
	manifest, err := contract.LoadManifest(filepath.Join(permissionContractRepositoryRoot(t), "authz", "manager-permissions.yaml"))
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	if err := validateManagerPermissionCompatibility(manifest); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPermissionContractRejectsLegacyAliasDrift(t *testing.T) {
	manifest, err := contract.LoadManifest(filepath.Join(permissionContractRepositoryRoot(t), "authz", "manager-permissions.yaml"))
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	mutations := []struct {
		name   string
		mutate func(*contract.Manifest)
	}{
		{"added", func(value *contract.Manifest) {
			value.LegacyCapabilities = append(value.LegacyCapabilities, contract.LegacyCapability{Key: "unknown.alias"})
		}},
		{"removed", func(value *contract.Manifest) {
			value.LegacyCapabilities = value.LegacyCapabilities[1:]
		}},
		{"renamed", func(value *contract.Manifest) {
			value.LegacyCapabilities[0].Key = "renamed.alias"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := *manifest
			candidate.LegacyCapabilities = append([]contract.LegacyCapability(nil), manifest.LegacyCapabilities...)
			mutation.mutate(&candidate)
			if err := validateManagerPermissionCompatibility(&candidate); err == nil {
				t.Fatalf("legacy alias %s mutation was accepted", mutation.name)
			}
		})
	}
}

func validateManagerPermissionCompatibility(manifest *contract.Manifest) error {
	manifestKeys := make([]string, 0, len(manifest.LegacyCapabilities))
	for _, capability := range manifest.LegacyCapabilities {
		manifestKeys = append(manifestKeys, capability.Key)
	}
	sort.Strings(manifestKeys)

	current := ManagerCapabilities(string(wkhttp.SuperAdmin))
	currentKeys := make([]string, 0, len(current))
	for key := range current {
		currentKeys = append(currentKeys, key)
	}
	sort.Strings(currentKeys)
	if !equalStrings(manifestKeys, currentKeys) {
		return fmt.Errorf("legacy capability keys differ: manifest=%v ManagerCapabilities=%v", manifestKeys, currentKeys)
	}

	roles := map[string]map[string]bool{
		string(wkhttp.SuperAdmin): allEnabled(manifestKeys),
		string(wkhttp.Admin): enabledOnly(
			"appversion.read", "dashboard.read", "users.read", "groups.read", "space.read", "space.write",
		),
		ManagerRoleDashboardReader: enabledOnly("dashboard.read"),
		ManagerRoleMarketAdmin: enabledOnly(
			"skill.read", "skill.write", "mcp.read", "mcp.write", "expert.read", "expert.write",
		),
	}
	for role, expected := range roles {
		actual := ManagerCapabilities(role)
		for _, key := range manifestKeys {
			if actual[key] != expected[key] {
				return fmt.Errorf("role %q capability %q = %v, want %v", role, key, actual[key], expected[key])
			}
		}
	}
	return nil
}

func permissionContractRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func enabledOnly(keys ...string) map[string]bool {
	result := make(map[string]bool, len(keys))
	for _, key := range keys {
		result[key] = true
	}
	return result
}

func allEnabled(keys []string) map[string]bool {
	return enabledOnly(keys...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
