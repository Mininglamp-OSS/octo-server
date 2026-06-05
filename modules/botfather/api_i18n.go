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
// contract stays in one place), for the defensive identity guard, and for the
// bot/User-API-Key auth middleware (status-preserving, anti-enumeration).
//
// Audience: botfather mixes dmwork-facing apply/management endpoints (legacy
// c.ResponseError → wire 400, D14) with external bot-adapter endpoints
// (bf_/uk_ tokens). Sites that already returned a real HTTP status
// (401/403/404/409) keep it via ResponseErrorLWithStatus; legacy c.ResponseError
// sites stay at wire 400 (D14).
//
// Internal=true codes (ErrBotfatherQueryFailed / StoreFailed / SendFailed /
// UploadFailed / IMTokenFailed) never surface their message on the wire — each
// call site keeps its existing bf.Error(..., zap.Error(err)) log so ops can
// debug from logs.

// respondBotfatherIdentityMissing renders the defensive guard for a handler
// that found no robot_id in context — authBot / authUserAPIKey is mounted on
// every protected route and must have populated it, so an empty value is an
// internal assertion failure: it is logged and rendered as a generic Internal
// 500 (never a probe-able 4xx). Legacy wire 400 (these were c.ResponseError).
func (bf *BotFather) respondBotfatherIdentityMissing(c *wkhttp.Context) {
	bf.Error("robot_id missing in context after auth middleware")
	httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
}

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

// respondBotfatherContentTooLarge surfaces the content field and its byte cap.
func respondBotfatherContentTooLarge(c *wkhttp.Context, field string, maxBytes int) {
	details := i18n.Details{"max_bytes": maxBytes}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrBotfatherContentTooLarge, nil, details)
}

// respondBotfatherFileTooLarge surfaces the upload size cap (in MB) for the
// legacy (wire-400) upload paths.
func respondBotfatherFileTooLarge(c *wkhttp.Context, maxMB int64) {
	httperr.ResponseErrorL(c, errcode.ErrBotfatherFileTooLarge, nil, i18n.Details{"max_mb": maxMB})
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

// respondBotfatherBotNotGroupMember renders the bot-not-a-group-member guard,
// preserving the real 403 (the legacy aborting 403 sites), then aborts the chain.
func respondBotfatherBotNotGroupMember(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotGroupMember, nil, nil)
	c.Abort()
}

// respondBotfatherBotNotGroupAdmin renders the bot-not-a-bot_admin guard,
// preserving the real 403, then aborts the chain.
func respondBotfatherBotNotGroupAdmin(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotGroupAdmin, nil, nil)
	c.Abort()
}
