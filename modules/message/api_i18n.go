package message

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respond helpers for modules/message. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrMessageXxx, nil, nil) directly; the
// helpers below cover the high-frequency shapes that carry a Detail field or
// resolve a shared err.shared.* code at init.
//
// Internal=true codes (ErrMessageQueryFailed / ErrMessageStoreFailed /
// ErrMessageNotifyFailed / ErrMessageSearchFailed) are called directly at each
// site, which keeps its existing m.Error(..., zap.Error(err)) log so ops can
// debug from logs while the wire response carries no message.

// respondMessageRequestInvalid covers the common "X 不能为空" / "数据格式有误" /
// bad-format / BindJSON-failure shape — one code, one optional field detail. An
// empty field is omitted so the renderer does not surface a noisy empty key.
func respondMessageRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrMessageRequestInvalid, nil, details)
}

// respondMessagePinnedLimitExceeded surfaces the pinned-message cap so the
// client can render a localized hint without hard-coding the limit.
func respondMessagePinnedLimitExceeded(c *wkhttp.Context, max int) {
	httperr.ResponseErrorL(c, errcode.ErrMessagePinnedLimitExceeded, nil, i18n.Details{"max": max})
}

// errSharedAuthRequired / errSharedTokenInvalid cache the shared auth codes so
// the per-handler login / token guards do not pay a registry lookup on every
// miss. Looked up at package init; a missing registration panics loudly rather
// than silently rendering an empty envelope at request time.
var (
	errSharedAuthRequired = mustLookupSharedCode("err.shared.auth.required")
	errSharedTokenInvalid = mustLookupSharedCode("err.shared.auth.token_invalid")
	errSharedForbidden    = mustLookupSharedCode("err.shared.auth.forbidden")
)

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/message: shared code not registered: " + id)
	}
	return c
}

// respondMessageNotLoggedIn renders the shared 401 envelope for the public
// proxy-send path whose legacy "请先登录" guard runs before AuthMiddleware would
// reject the request.
func respondMessageNotLoggedIn(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedAuthRequired, nil, nil)
}

// respondMessageTokenInvalid renders the shared token-invalid envelope for the
// proxy-send token-parse guards ("token错误" / "解析token错误").
func respondMessageTokenInvalid(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedTokenInvalid, nil, nil)
}

// respondMessageForbidden renders the shared 403 envelope for the management
// console role guards (CheckLoginRoleIsSuperAdmin), mirroring the user/group
// manager-forbidden mapping.
func respondMessageForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}
