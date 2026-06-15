package app_bot

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// This file localizes the app_bot error responses. Every handler used to return
// raw, unlocalized strings — c.ResponseError(errors.New("...")),
// c.AbortWithStatusJSON(403, err.Error()) and c.JSON(40x, {"msg": "..."}) —
// which leaked English/Chinese framework text straight onto the wire and could
// not be language-negotiated. They now route through these helpers onto a
// registered errcode + the i18n envelope. Status-preserving codes (404/409/403/
// 500) use ResponseErrorLWithStatus so the console keeps branching on the real
// wire status; validation stays at 400.

// respondAppBotForbidden renders the localized shared 403 for every app_bot
// authorization guard: the platform /v1/admin/app_bot system-role gates, the
// space-scoped checkSpaceAdmin gates, and the apply-flow space-membership check.
// All collapse to one generic forbidden code (anti-enumeration) — the specific
// role/membership reason stays in logs, never on the client.
func respondAppBotForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
}

// respondAppBotRequestInvalid covers malformed / empty request input (BindJSON
// failure, invalid robot_uid, empty update). An empty field is omitted so the
// renderer does not surface a noisy empty key.
func respondAppBotRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrAppBotRequestInvalid, nil, details)
}

// respondAppBotIDInvalid covers a bot id failing the format rule or colliding
// with a reserved id.
func respondAppBotIDInvalid(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrAppBotIDInvalid, nil, nil)
}

// respondAppBotNotFound renders the status-preserving 404 for a missing or
// scope-mismatched bot.
func respondAppBotNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotNotFound, nil, nil)
}

// respondAppBotNotFoundPinned renders the same not-found code at the legacy
// fixed-400 wire status (D14), for the user-facing /v1/app_bot/apply endpoint
// whose SDK clients may branch on 400. The management-console paths use
// respondAppBotNotFound (real 404) instead.
func respondAppBotNotFoundPinned(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrAppBotNotFound, nil, nil)
}

// respondAppBotIDConflict renders the 409 for a create colliding with an in-use id.
func respondAppBotIDConflict(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotIDConflict, nil, nil)
}

// respondAppBotTokenRotationConflict renders the 409 for a lost token-rotation race.
func respondAppBotTokenRotationConflict(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotTokenRotationConflict, nil, nil)
}

// respondAppBotQueryFailed / StoreFailed / IMTokenFailed / Internal render the
// status-preserving 500. Internal=true hides the message — callers MUST log the
// underlying err (zap.Error) with context before calling these.
func respondAppBotQueryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotQueryFailed, nil, nil)
}

func respondAppBotStoreFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotStoreFailed, nil, nil)
}

func respondAppBotIMTokenFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotIMTokenFailed, nil, nil)
}

func respondAppBotInternal(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAppBotInternal, nil, nil)
}
