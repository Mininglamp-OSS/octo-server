package group

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respond helpers for modules/group. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrGroupXxx, nil, nil) directly; the
// helpers below exist only for the high-frequency shapes that either carry a
// Detail field (so the SafeDetailKeys contract stays in one place) or resolve a
// shared err.shared.* code at init.
//
// Internal=true codes (ErrGroupQueryFailed / ErrGroupStoreFailed /
// ErrGroupNotifyFailed) are intentionally NOT wrapped: each call site keeps its
// existing g.Error(..., zap.Error(err)) log so ops can debug from logs, and the
// wire response carries no message.

// respondGroupRequestInvalid covers the common "X 不能为空" / "数据格式有误" /
// BindJSON-failure shape — one code, one optional field detail. An empty field
// is omitted so the renderer does not surface a noisy empty key to clients.
func respondGroupRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrGroupRequestInvalid, nil, details)
}

// respondGroupMdContentTooLarge surfaces the GROUP.md size cap so the client can
// render a localized hint without hard-coding the limit.
func respondGroupMdContentTooLarge(c *wkhttp.Context, maxSize int) {
	httperr.ResponseErrorL(c, errcode.ErrGroupMdContentTooLarge, nil, i18n.Details{
		"field":    "content",
		"max_size": maxSize,
	})
}

// errSharedAuthRequired / errSharedForbidden cache the shared auth codes so the
// per-handler login / permission guards do not pay a registry lookup on every
// miss. Looked up at package init; a missing registration panics loudly rather
// than silently rendering an empty envelope at request time.
var (
	errSharedAuthRequired = mustLookupSharedCode("err.shared.auth.required")
	errSharedForbidden    = mustLookupSharedCode("err.shared.auth.forbidden")
)

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/group: shared code not registered: " + id)
	}
	return c
}

// respondGroupNotLoggedIn renders the shared 401 envelope for the public routes
// (group scan-join / invite authorize) whose legacy "请先登录" guard runs before
// AuthMiddleware would reject the request.
func respondGroupNotLoggedIn(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedAuthRequired, nil, nil)
}

// respondGroupForbidden renders the shared 403 envelope for the generic
// "用户无权执行此操作" / "操作用户权限不够" guards that carry no role-specific
// hint. Role-specific gates use the dedicated err.server.group.creator_only /
// manager_only / creator_or_manager_only codes instead.
func respondGroupForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}

// errGroupInfoQueryFailed / errGroupInfoNotFound are getGroupInfo's sentinel
// returns. Call sites map them to the right envelope via errors.Is
// (respondGroupInfoError) instead of leaking the underlying Chinese string
// behind a fixed HTTP 400.
var (
	errGroupInfoQueryFailed = errors.New("query group failed")
	errGroupInfoNotFound    = errors.New("group not found or disbanded")
)

// respondGroupInfoError maps getGroupInfo's sentinel error to the localized
// envelope: a missing / disbanded group is 404, any other (query) failure is
// 500 (Internal). getGroupInfo already logged the underlying DB error, so the
// query branch does not log again.
func respondGroupInfoError(c *wkhttp.Context, err error) {
	if errors.Is(err, errGroupInfoNotFound) {
		httperr.ResponseErrorL(c, errcode.ErrGroupNotFound, nil, nil)
		return
	}
	httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
}
