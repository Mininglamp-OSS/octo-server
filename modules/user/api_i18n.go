package user

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respondUserError is the base wrapper for legacy c.ResponseError sites that
// migrate to a localized error envelope. For codes that carry detail fields
// (field / missing_field / lock bounds), call the more specific helpers
// below instead so the SafeDetailKeys contract stays in one place.
func respondUserError(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorL(c, code, nil, nil)
}

// respondUserRequestInvalid covers the common "X 不能为空" / "数据格式有误"
// shape — one code, one optional field detail. An empty field is omitted
// so the renderer does not surface a noisy empty key to clients.
func respondUserRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrUserRequestInvalid, nil, details)
}

// respondUserUpdateNotAllowed tags the field the caller tried to mutate
// (mirrors the legacy "不允许更新【x】" / "不允许编辑！" messages). An empty
// field is omitted from details — the legacy "不允许编辑！" branch has no
// field context to surface — matching the convention of
// respondUserRequestInvalid so clients never see structured empty keys.
func respondUserUpdateNotAllowed(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrUserUpdateNotAllowed, nil, details)
}

// respondUserAuthInfoInvalid surfaces which field of the scanned QR-code
// payload was missing or malformed. An empty missingField is omitted.
func respondUserAuthInfoInvalid(c *wkhttp.Context, missingField string) {
	details := i18n.Details{}
	if missingField != "" {
		details["missing_field"] = missingField
	}
	httperr.ResponseErrorL(c, errcode.ErrUserAuthInfoInvalid, nil, details)
}

// respondUserTokenRequired tags which token parameter was omitted. Used by
// the verifyToken / verifyBot endpoints whose legacy English messages
// (`token is required`, `bot_token is required`) third-party callers may
// have keyed off — they are migrated to error.code keying.
func respondUserTokenRequired(c *wkhttp.Context, field string) {
	httperr.ResponseErrorL(c, errcode.ErrUserTokenRequired, nil, i18n.Details{"field": field})
}

// respondUserLockMinuteOutOfRange returns the lock-screen delay bounds so
// the client can render a localized hint without hard-coding the limits.
func respondUserLockMinuteOutOfRange(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrUserLockMinuteOutOfRange, nil, i18n.Details{
		"field": "lock_after_minute",
		"min":   0,
		"max":   60,
	})
}

// errSharedAuthRequired caches the shared "auth required" code so the
// per-handler "未登录" guards do not pay a registry lookup on every miss.
// Looked up at package init; a missing registration panics loudly rather
// than silently rendering an empty envelope at request time.
var errSharedAuthRequired = mustLookupSharedCode("err.shared.auth.required")

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/user: shared code not registered: " + id)
	}
	return c
}

// respondUserNotLoggedIn responds with the shared err.shared.auth.required
// code. Handlers protected by AuthMiddleware still keep a belt-and-braces
// `loginUID == ""` check for legacy public routes; this helper renders
// the consistent 401 envelope for that fallthrough.
func respondUserNotLoggedIn(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedAuthRequired, nil, nil)
}

// respondUserServiceError responds with the generic ErrUserStoreFailed
// (Internal=true). Callers MUST log the underlying err with full handler
// context via the module's zap logger before invoking this helper —
// Internal=true means the wire response carries no error message and ops
// debug entirely from logs.
//
// For known sentinel errors (e.g. ErrUnsupportedLanguage) callers branch
// explicitly to a more specific code BEFORE falling through here. Service
// layer sentinel extraction is deferred (TODOS L219 follow-up).
func respondUserServiceError(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrUserStoreFailed, nil, nil)
}
