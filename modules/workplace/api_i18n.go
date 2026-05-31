package workplace

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respond helpers for modules/workplace. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrWorkplaceXxx, nil, nil) directly; the
// helpers below exist only for the high-frequency shapes that either carry a
// Detail field (so the SafeDetailKeys contract stays in one place) or resolve a
// shared err.shared.* code at init.
//
// Internal=true codes (ErrWorkplaceQueryFailed / ErrWorkplaceStoreFailed) are
// intentionally NOT wrapped: each call site keeps its existing
// w.Error(..., zap.Error(err)) log so ops can debug from logs, and the wire
// response carries no message.

// respondWorkplaceRequestInvalid covers the common "X 不能为空" / "数据格式有误"
// (common.ErrData) / BindJSON-failure shape — one code, one optional field
// detail. An empty field is omitted so the renderer does not surface a noisy
// empty key to clients.
func respondWorkplaceRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrWorkplaceRequestInvalid, nil, details)
}

// errSharedForbidden / errSharedInternal cache the shared codes so the
// per-handler role guards and panic-recovery paths do not pay a registry lookup
// on every miss. Looked up at package init; a missing registration panics
// loudly rather than silently rendering an empty envelope at request time.
var (
	errSharedForbidden = mustLookupSharedCode("err.shared.auth.forbidden")
	errSharedInternal  = mustLookupSharedCode("err.shared.internal")
)

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/workplace: shared code not registered: " + id)
	}
	return c
}

// respondWorkplaceForbidden renders the shared 403 envelope for the
// CheckLoginRole / CheckLoginRoleIsSuperAdmin role guards, which previously
// passed octo-lib's raw role error straight to c.ResponseError.
func respondWorkplaceForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedForbidden, nil, nil)
}

// respondWorkplaceInternal renders the shared 500 envelope for the deferred
// panic-recovery paths ("服务器内部错误") where no business code applies.
func respondWorkplaceInternal(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errSharedInternal, nil, nil)
}
