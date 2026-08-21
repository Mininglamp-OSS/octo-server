package adminrbac

import (
	"sort"
)

// Evaluate produces the global octo-admin permission set from already-loaded
// role snapshots. It is deliberately pure so it can be tested without MySQL,
// Redis or the HTTP stack.
func Evaluate(uid string, snapshots []RoleSnapshot) (EffectivePermissions, error) {
	result := EffectivePermissions{UID: uid, Permissions: []string{}, RoleVersions: []RoleVersion{}}
	permissionSet := make(map[string]struct{})
	for _, snapshot := range sortRoleSnapshots(append([]RoleSnapshot(nil), snapshots...)) {
		result.RoleVersions = append(result.RoleVersions, RoleVersion{
			RoleKey:              snapshot.RoleKey,
			AuthorizationVersion: snapshot.AuthorizationVersion,
		})
		if snapshot.Status != activeStatus {
			continue
		}
		for _, permissionKey := range snapshot.Permissions {
			if err := validatePermissionKey(permissionKey); err != nil {
				return EffectivePermissions{}, err
			}
			permissionSet[permissionKey] = struct{}{}
		}
	}
	for permissionKey := range permissionSet {
		result.Permissions = append(result.Permissions, permissionKey)
	}
	sort.Strings(result.Permissions)
	return result, nil
}

// Allows answers a global permission question. Any resource selector is
// rejected instead of being silently treated as a global allow.
func Allows(result EffectivePermissions, permissionKey string, groupNo, spaceID, robotID, resourceID string) (bool, error) {
	if err := validateGlobalScope(groupNo, spaceID, robotID, resourceID); err != nil {
		return false, err
	}
	if err := validatePermissionKey(permissionKey); err != nil {
		return false, err
	}
	index := sort.SearchStrings(result.Permissions, permissionKey)
	return index < len(result.Permissions) && result.Permissions[index] == permissionKey, nil
}

func roleVersionsEqual(left, right []RoleVersion) bool {
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
