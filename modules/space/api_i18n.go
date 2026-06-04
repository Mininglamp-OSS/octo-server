package space

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/space. Most migrated sites call
// httperr.ResponseErrorL(c, errcode.ErrSpaceXxx, nil, nil) directly; the helpers
// below exist only for the shapes that carry a Detail field, so the
// SafeDetailKeys contract stays in one place.
//
// Internal=true codes (ErrSpaceQueryFailed / ErrSpaceStoreFailed) are
// intentionally NOT wrapped here: each call site keeps its existing
// s.Error(..., zap.Error(err)) log so ops can debug from logs, and the renderer
// carries no message on the wire.

// respondSpaceRequestInvalid covers the common BindJSON-failure / "X 不能为空" /
// invalid-enum shape — one code, one optional field detail. An empty field is
// omitted so the renderer does not surface a noisy empty key to clients.
func respondSpaceRequestInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(c, errcode.ErrSpaceRequestInvalid, nil, details)
}

// respondSpaceFieldTooLong surfaces the offending field and its max length so
// the client can render a localized hint without hard-coding the limit.
func respondSpaceFieldTooLong(c *wkhttp.Context, field string, maxChars int) {
	httperr.ResponseErrorL(c, errcode.ErrSpaceFieldTooLong, nil, i18n.Details{
		"field":     field,
		"max_chars": maxChars,
	})
}

// respondSpaceBatchTooLarge surfaces the per-request member batch cap.
func respondSpaceBatchTooLarge(c *wkhttp.Context, max int) {
	httperr.ResponseErrorL(c, errcode.ErrSpaceBatchTooLarge, nil, i18n.Details{
		"max": max,
	})
}
