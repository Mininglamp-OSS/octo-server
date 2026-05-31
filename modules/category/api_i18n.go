package category

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/category. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrCategoryXxx, nil, nil) directly; the
// helpers below exist only for the shapes that carry a Detail field, so the
// SafeDetailKeys contract stays in one place.
//
// Internal=true codes (ErrCategoryQueryFailed / ErrCategoryStoreFailed) are
// intentionally NOT wrapped: each call site keeps its existing
// c.Error(..., zap.Error(err)) log so ops can debug from logs, and the wire
// response carries no message.

// respondCategoryRequestInvalid covers the common BindJSON-failure / "X 不能为空"
// shape — one code, one optional field detail. An empty field is omitted so the
// renderer does not surface a noisy empty key to clients.
func respondCategoryRequestInvalid(ctx *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(ctx, errcode.ErrCategoryRequestInvalid, nil, details)
}

// respondCategoryNameTooLong surfaces the name length cap so the client can
// render a localized hint without hard-coding the limit.
func respondCategoryNameTooLong(ctx *wkhttp.Context, maxLen int) {
	httperr.ResponseErrorL(ctx, errcode.ErrCategoryNameTooLong, nil, i18n.Details{
		"field":      "name",
		"max_length": maxLen,
	})
}

// respondCategoryLimitExceeded surfaces the per-space category cap so the client
// can render a localized hint without hard-coding the limit.
func respondCategoryLimitExceeded(ctx *wkhttp.Context, max int) {
	httperr.ResponseErrorL(ctx, errcode.ErrCategoryLimitExceeded, nil, i18n.Details{
		"max": max,
	})
}
