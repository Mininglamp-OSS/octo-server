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

// errSharedForbidden caches the shared 403 code used by the manager role
// guards (CheckLoginRole / CheckLoginRoleIsSuperAdmin).
var errSharedForbidden = mustLookupSharedCode("err.shared.auth.forbidden")

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

// respondManagerForbidden renders the shared 403 envelope for the management
// console role guards. wkhttp's CheckLoginRole / CheckLoginRoleIsSuperAdmin
// return a plain Chinese error ("登录用户角色错误" / "该用户无权执行此操作");
// both are authorization failures, so they collapse to err.shared.auth.forbidden
// rather than leaking the raw framework string on the wire. This raises the
// legacy HTTP 400 to a semantically-correct 403 (consistent with the rest of
// the Phase 2.1 status-code migration).
func respondManagerForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}

// respondUserPinnedLimitExceeded surfaces the per-space pinned-channel cap so
// the client can render a localized hint without hard-coding the limit.
func respondUserPinnedLimitExceeded(c *wkhttp.Context, max int) {
	httperr.ResponseErrorL(c, errcode.ErrUserPinnedLimitExceeded, nil, i18n.Details{"max": max})
}

// respondUserListFilterConflict reports a pair of mutually-exclusive list
// filters (e.g. bot_only + exclude_bot). The offending filter and the one it
// conflicts with are surfaced as details so a frontend dev can spot the bad
// query construction.
func respondUserListFilterConflict(c *wkhttp.Context, filter, conflictsWith string) {
	httperr.ResponseErrorL(c, errcode.ErrUserListFilterConflict, nil, i18n.Details{
		"filter":         filter,
		"conflicts_with": conflictsWith,
	})
}
