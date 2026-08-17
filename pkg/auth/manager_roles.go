package auth

import "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"

// ManagerRoleDashboardReader is a temporary fixed role used before general
// manager RBAC exists. Keep it local to octo-server: adding it to octo-lib's
// CheckLoginRole would accidentally grant every admin endpoint.
const ManagerRoleDashboardReader = "dashboardReader"

// ManagerRoleMarketAdmin is a fixed role for staff who curate the platform's
// MCP and Skill catalogs without holding any other administrative power.
//
// Same shape and same rationale as ManagerRoleDashboardReader: it is deliberately
// NOT known to octo-lib's CheckLoginRole, so an account holding it passes zero
// admin/superAdmin endpoint gates in octo-server. Its only effect here is the
// mcp.*/skill.* capabilities in managerCapabilities.
//
// Enforcement lives in octo-marketplace, and only once
// Mininglamp-OSS/octo-marketplace#55 has shipped: before that PR a single gate
// guards every /api/v1/admin/* group and admits superAdmin alone, so a holder of
// this role gets 403 on the catalog surface rather than access to it. After it,
// the catalog groups (mcps, skills, skill_categories, skill uploads) admit this
// role and the Expert Market groups keep the superAdmin-only gate — which is why
// expert.* is excluded from the capabilities below.
const ManagerRoleMarketAdmin = "marketAdmin"

// IsManagerConsoleRole reports whether a role may establish a manager-console
// session and read its own /v1/manager/me capability map.
func IsManagerConsoleRole(role string) bool {
	return role == string(wkhttp.Admin) ||
		role == string(wkhttp.SuperAdmin) ||
		role == ManagerRoleDashboardReader ||
		role == ManagerRoleMarketAdmin
}

// CanAdminMarketplace is the server-authoritative policy for the platform MCP /
// Skill catalog admin surface. It is the octo-server half of a contract whose
// enforcement lives in octo-marketplace (internal/middleware/admin.go): this
// function decides what /v1/manager/me advertises, marketplace decides what the
// /api/v1/admin/* routes actually admit. Keep the two in sync.
func CanAdminMarketplace(role string) bool {
	return role == string(wkhttp.SuperAdmin) ||
		role == ManagerRoleMarketAdmin
}

// CanReadManagerDashboard is the server-authoritative policy for the global
// operations dashboard read surface. Mutating operations keep their narrower
// SuperAdmin checks at the handler.
func CanReadManagerDashboard(role string) bool {
	return role == string(wkhttp.Admin) ||
		role == string(wkhttp.SuperAdmin) ||
		role == ManagerRoleDashboardReader
}
