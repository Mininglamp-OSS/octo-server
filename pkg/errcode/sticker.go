package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.sticker.* — modules/sticker business error codes (api.go).
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never surface
// their message on the wire — callers MUST log the underlying err with full
// context via the module logger before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrStickerRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, empty path). The offending field is surfaced via
	// Details when the caller can identify it.
	ErrStickerRequestInvalid = register(codes.Code{
		ID:             "err.server.sticker.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrStickerFormatUnsupported covers an empty or out-of-whitelist sticker
	// format. The attempted format is surfaced so the client can render a
	// localized hint without hard-coding the accepted set.
	ErrStickerFormatUnsupported = register(codes.Code{
		ID:             "err.server.sticker.format_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported sticker format. Allowed: gif, png, jpg, jpeg, webp.",
		SafeDetailKeys: []string{"field", "format"},
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrStickerNotFound covers both a missing sticker and the deliberate
	// "not found or not yours" merge on delete (no cross-user enumeration).
	ErrStickerNotFound = register(codes.Code{
		ID:             "err.server.sticker.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Sticker not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	// ErrStickerQuotaExceeded is returned when a user is already at their
	// per-user custom-sticker cap (admin-configurable system_setting
	// sticker.user_max_count, default 100). The effective cap is surfaced via
	// the `max` detail.
	ErrStickerQuotaExceeded = register(codes.Code{
		ID:             "err.server.sticker.quota_exceeded",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You have reached the maximum number of custom stickers.",
		SafeDetailKeys: []string{"max"},
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrStickerQueryFailed covers read-path failures (DB SELECT/count). Log the
	// underlying err before responding.
	ErrStickerQueryFailed = register(codes.Code{
		ID:             "err.server.sticker.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query sticker data.",
		Internal:       true,
	})
	// ErrStickerStoreFailed covers mutation-path failures (DB insert/update). Log
	// the underlying err before responding.
	ErrStickerStoreFailed = register(codes.Code{
		ID:             "err.server.sticker.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update sticker data.",
		Internal:       true,
	})
)
