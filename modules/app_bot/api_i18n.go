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

// respondAppBotForbidden renders the localized shared 403 envelope for the
// /v1/admin/app_bot role guards. It replaces the legacy c.ResponseError(err)
// that forwarded wkhttp's raw, unlocalized framework string
// ("该用户无权执行此操作") straight onto the wire. The guards collapse to one
// generic forbidden code (anti-enumeration): the specific role reason stays in
// logs, never on the client.
func respondAppBotForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}
