package authz

import "testing"

func TestGeneratedPermissionsMatchManifest(t *testing.T) {
	manifest := repositoryManifest(t)
	if PermissionContractSchemaVersion != manifest.SchemaVersion {
		t.Fatalf("generated schema version = %d, want %d", PermissionContractSchemaVersion, manifest.SchemaVersion)
	}
	if len(GeneratedPermissions) != len(manifest.Permissions) {
		t.Fatalf("generated permission count = %d, want %d", len(GeneratedPermissions), len(manifest.Permissions))
	}
	for _, permission := range manifest.Permissions {
		metadata, ok := GeneratedPermissions[permission.Key]
		if !ok || !IsKnownPermission(permission.Key) {
			t.Errorf("generated registry is missing %q", permission.Key)
			continue
		}
		if metadata.Resource != permission.Resource ||
			metadata.Action != permission.Action ||
			metadata.Description != permission.Description ||
			metadata.Sensitivity != permission.Sensitivity {
			t.Errorf("generated metadata for %q differs from manifest", permission.Key)
		}
	}
	if IsKnownPermission("unknown.permission") {
		t.Error("generated registry accepted an unknown permission")
	}
}
