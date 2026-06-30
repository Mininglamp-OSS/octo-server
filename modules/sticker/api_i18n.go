package sticker

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/sticker. Sites with no detail call
// httperr.ResponseErrorL(c, errcode.ErrStickerXxx, nil, nil) directly; the
// helpers below exist only for the shapes that carry a Detail field, so the
// SafeDetailKeys contract stays in one place.
//
// Internal=true codes (ErrStickerQueryFailed / ErrStickerStoreFailed) are
// intentionally NOT wrapped: each call site keeps its existing
// s.Error(..., zap.Error(err)) log so ops can debug from logs, and the wire
// response carries no message.

// respondStickerRequestInvalid covers the BindJSON-failure / "X 不能为空" shape —
// one code, one optional field detail. An empty field is omitted so the renderer
// does not surface a noisy empty key to clients.
func respondStickerRequestInvalid(ctx *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorL(ctx, errcode.ErrStickerRequestInvalid, nil, details)
}

// respondStickerFormatUnsupported surfaces the offending format so the client
// can render a localized hint without hard-coding the accepted set.
func respondStickerFormatUnsupported(ctx *wkhttp.Context, format string) {
	httperr.ResponseErrorL(ctx, errcode.ErrStickerFormatUnsupported, nil, i18n.Details{
		"field":  "format",
		"format": format,
	})
}

// respondStickerQuotaExceeded surfaces the effective per-user cap so the client
// can render a localized hint without hard-coding the limit.
func respondStickerQuotaExceeded(ctx *wkhttp.Context, max int) {
	httperr.ResponseErrorL(ctx, errcode.ErrStickerQuotaExceeded, nil, i18n.Details{
		"max": max,
	})
}

func respondStickerShortcodeInvalid(ctx *wkhttp.Context) {
	httperr.ResponseErrorL(ctx, errcode.ErrStickerShortcodeInvalid, nil, i18n.Details{
		"field": "shortcode",
	})
}

func respondStickerKeywordsInvalid(ctx *wkhttp.Context) {
	httperr.ResponseErrorL(ctx, errcode.ErrStickerKeywordsInvalid, nil, i18n.Details{
		"field": "keywords",
	})
}

func respondStickerShortcodeConflict(ctx *wkhttp.Context) {
	httperr.ResponseErrorL(ctx, errcode.ErrStickerShortcodeConflict, nil, i18n.Details{
		"field": "shortcode",
	})
}
