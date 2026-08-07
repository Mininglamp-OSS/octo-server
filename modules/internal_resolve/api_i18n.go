package internal_resolve

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respondUnauthorized emits the shared 401 anti-enumeration envelope. Matches
// what modules/bot_mention returns on X-Internal-Token failures so external
// callers see a uniform "unauthorized" surface across internal APIs.
func respondUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
}

// respondInvalidParam emits the shared 400 with an optional `field` detail
// (mirrors bot_mention.respondBotMentionInvalid).
func respondInvalidParam(c *wkhttp.Context, field string) {
	var details i18n.Details
	if field != "" {
		details = i18n.Details{"field": field}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, details)
}

// respondInternal wraps any unexpected downstream failure. Kept generic so we
// never leak internal state (query text, DSN, etc.) to internal consumers.
//
// Note: there is no respondUserNotFound helper. The handler's contract for an
// unknown uid is a successful 200 with robot=0 (see resolveBotOwner docstring)
// — returning 404 would leak existence to a low-privilege caller and force the
// drive polling loop to special-case "human vs unknown" it does not need.
func respondInternal(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
}
