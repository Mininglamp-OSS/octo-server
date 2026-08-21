package adminrbac

import (
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
)

var roleKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func validateRoleKey(roleKey string) error {
	if !roleKeyPattern.MatchString(strings.TrimSpace(roleKey)) {
		return ErrInvalidRoleKey
	}
	return nil
}

func validatePermissionKey(permissionKey string) error {
	if !authz.IsKnownPermission(permissionKey) {
		return ErrInvalidPermission
	}
	return nil
}

func validateGlobalScope(groupNo, spaceID, robotID, resourceID string) error {
	if strings.TrimSpace(groupNo) != "" || strings.TrimSpace(spaceID) != "" ||
		strings.TrimSpace(robotID) != "" || strings.TrimSpace(resourceID) != "" {
		return ErrInvalidScope
	}
	return nil
}
