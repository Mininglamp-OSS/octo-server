package app_bot

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// errSharedForbidden caches the shared 403 code used by the platform-level
// (/v1/admin/app_bot) role guards. Looked up at package init so a missing
// registration panics loudly here rather than rendering an empty envelope at
// request time.
var errSharedForbidden = mustLookupSharedCode("err.shared.auth.forbidden")

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/app_bot: shared code not registered: " + id)
	}
	return c
}

// respondAppBotForbidden renders the localized shared forbidden envelope for the
// platform /v1/admin/app_bot role guards. It replaces the legacy
// c.ResponseError(err) that forwarded wkhttp's raw, unlocalized framework string
// ("该用户无权执行此操作") straight onto the wire. ResponseErrorL keeps the
// legacy fixed-400 wire status these guards already returned (D14). The guards
// collapse to one generic forbidden code (anti-enumeration): the specific role
// reason stays in logs, never on the client.
func respondAppBotForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}

// respondAppBotSpaceForbidden renders the same localized shared forbidden
// envelope for the space-scoped /v1/space/:space_id/app_bot guards, which
// replaced a raw c.AbortWithStatusJSON(403, err.Error()) that leaked
// checkSpaceAdmin's English string ("no permission: requires space admin").
// Unlike the platform guards, these already returned a real 403, so this uses
// ResponseErrorLWithStatus to preserve that status (the code's real 403) rather
// than collapse it to the legacy 400 — no client-visible status change.
func respondAppBotSpaceForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errSharedForbidden, nil, nil)
}
