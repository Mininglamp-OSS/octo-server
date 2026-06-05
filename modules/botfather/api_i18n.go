package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/botfather. Most migrated sites call
// httperr.ResponseErrorL / ResponseErrorLWithStatus directly; the helpers below
// exist for the shapes that carry a Detail field (so the SafeDetailKeys
// contract stays in one place) and for the User-API-Key auth middleware
// (status-preserving, anti-enumeration).
//
// Scope: after the bot endpoints moved to modules/bot_api (#277/#278),
// botfather's live HTTP surface is the doc .md endpoints, Robot Apply
// (api_apply.go), and User Bot management (api_user.go). These are dmwork-facing
// apply/management endpoints (legacy c.ResponseError → wire 400, D14) plus the
// User-API-Key auth path, which preserves its real 401 via
// ResponseErrorLWithStatus (external adapters branch on HTTP 401).
//
// Internal=true codes (ErrBotfatherQueryFailed / StoreFailed) never surface
// their message on the wire — each call site keeps its existing
// bf.Error(..., zap.Error(err)) log so ops can debug from logs.

// respondBotfatherRequestInvalid covers the common BindJSON-failure / "X 不能为空"
// / invalid-format shape — one code, one optional field detail. An empty field
// is omitted so the renderer does not surface a noisy empty key.
func respondBotfatherRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrBotfatherRequestInvalid, nil, details)
}

// respondBotfatherUsernameTaken renders the username-collision conflict,
// preserving the real 409 (the legacy site used ResponseErrorWithStatus 409),
// and surfaces the offending username via Details.
func respondBotfatherUsernameTaken(c *wkhttp.Context, username string) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherUsernameTaken, nil, i18n.Details{"username": username})
}

// ---- bot / User-API-Key auth middleware (status-preserving, anti-enum) -------

// respondBotfatherAuthFailed renders the single anti-enumeration 401 for the
// bot-token / API-Key auth middleware, preserving the real 401 wire status
// (external adapters branch on HTTP 401, not the D14 fixed-400), then aborts the
// gin chain so the protected handler never runs. The specific reason is logged
// at the call site, never returned.
func respondBotfatherAuthFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherAuthFailed, nil, nil)
	c.Abort()
}
