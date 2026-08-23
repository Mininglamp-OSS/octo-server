package auth

import "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"

// ManagerRoleDashboardReader is a temporary fixed role used before general
// manager RBAC exists. Keep it local to octo-server: adding it to octo-lib's
// CheckLoginRole would accidentally grant every admin endpoint.
const ManagerRoleDashboardReader = "dashboardReader"

// ManagerRoleMarketAdmin is a fixed role for staff who run the platform market —
// the MCP catalog, the Skill catalog and the Expert Market — without holding any
// other administrative power.
//
// Same shape and same rationale as ManagerRoleDashboardReader: it is deliberately
// NOT known to octo-lib's CheckLoginRole, so an account holding it passes zero
// admin/superAdmin endpoint gates in octo-server. Its only effect here is the
// mcp.* / skill.* / expert.* capabilities in managerCapabilities.
//
// Enforcement lives in octo-marketplace, whose /api/v1/admin/* groups are gated
// per resource; each group that admits this role opts into it explicitly, and a
// group registered without it stays superAdmin-only. Both sides must agree: a
// capability advertised here that marketplace does not admit renders the page
// and then 403s every call behind it. The marketplace half is octo-marketplace#55
// (per-resource gating) and #56 (admitting this role on the Expert Market groups).
//
// Before granting this to anyone, five things are worth knowing:
//
//   - It is a publishing authority, not a read-mostly editor. A holder can
//     create, edit and delete the public Skills, system MCPs and experts that
//     every user on the platform installs and runs locally, and restructure the
//     catalog taxonomy. It is genuinely narrower than superAdmin — no system
//     settings, backups, user/group writes or space destruction — but pick people
//     at a supply-chain bar, not at a "content editor" one.
//   - Do not DEPLOY this service before octo-marketplace has the matching gate
//     live. The capabilities are computed per request from CanAdminMarketplace,
//     so every account already holding the role picks up the advertised surface
//     at deploy — withholding new grants does not narrow the blast radius, and
//     the population that gains it at deploy is fixed at release time. (Later
//     grants do of course extend it; the point is that deploy order, not grant
//     policy, is the lever for the accounts that already hold the role.)
//   - A GRANT DOES NOT REACH MARKETPLACE UNTIL THE HOLDER RE-AUTHENTICATES.
//     Mirror image of the revoke trap below, same cause: /v1/manager/me reads the
//     live role through the parser's RoleResolver, while marketplace reads the
//     token snapshot through /v1/auth/verify. So granting this to someone with an
//     open console session makes the menu appear immediately and every call
//     behind it 403 until they log out and back in. Have them re-login.
//   - REVOKING THE ROLE DOES NOT CUT OFF MARKET ACCESS. Revoke the session too.
//     Marketplace resolves callers through /v1/auth/verify, which answers from
//     tokenValidator.Validate — the role snapshotted into the session token at
//     login — not from the live user.role column. (The RoleResolver that keeps
//     octo-server's own console fresh within RoleCacheTTL is wired into
//     CacheTokenParser only; see main.go.) So for an existing session, clearing
//     user.role never lands: catalog access survives for the remaining token
//     lifetime, up to Cache.TokenExpire — 30 days by default.
//     Revoking the session does work, bounded by marketplace's own token-keyed
//     identity cache (AUTH_CACHE_TTL, 30s default), which has no invalidation
//     entry point. So: role + session, and expect up to ~30s of residual access.
//   - See the fixed-role section in modules/user/api_manager.go for the two
//     lifecycle traps both fixed roles share (one-way downgrade, and accounts
//     that cannot be deleted until the role is revoked).
const ManagerRoleMarketAdmin = "marketAdmin"

// ManagerConsoleRoles is the authoritative set of roles that may establish a
// manager-console session. IsManagerConsoleRole is derived from it so a role
// added here is honoured by every consumer at once.
//
// It exists as data, not just a predicate, because callers outside the request
// path need the same set as a SQL/collection filter — e.g. the manager-2FA
// enable guard has to find every console-capable account that still lacks an
// email address. A predicate cannot express that query, and a second hand-kept
// list would silently miss whichever role was added last.
var ManagerConsoleRoles = []string{
	string(wkhttp.Admin),
	string(wkhttp.SuperAdmin),
	ManagerRoleDashboardReader,
	ManagerRoleMarketAdmin,
}

// IsManagerConsoleRole reports whether a role may establish a manager-console
// session and read its own /v1/manager/me capability map.
func IsManagerConsoleRole(role string) bool {
	for _, r := range ManagerConsoleRoles {
		if role == r {
			return true
		}
	}
	return false
}

// CanAdminMarketplace is the server-authoritative policy for the platform market
// admin surface — MCP catalog, Skill catalog and Expert Market. It is the
// octo-server half of a contract whose
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
