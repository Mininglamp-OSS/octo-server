package auth

import "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"

// ManagerRoleDashboardReader is a temporary fixed role used before general
// manager RBAC exists. Keep it local to octo-server: adding it to octo-lib's
// CheckLoginRole would accidentally grant every admin endpoint.
const ManagerRoleDashboardReader = "dashboardReader"

// IsManagerConsoleRole reports whether a role may establish a manager-console
// session and read its own /v1/manager/me capability map.
func IsManagerConsoleRole(role string) bool {
	return role == string(wkhttp.Admin) ||
		role == string(wkhttp.SuperAdmin) ||
		role == ManagerRoleDashboardReader
}

// CanReadManagerDashboard is the server-authoritative policy for the global
// operations dashboard read surface. Mutating operations keep their narrower
// SuperAdmin checks at the handler.
func CanReadManagerDashboard(role string) bool {
	return IsManagerConsoleRole(role)
}
