package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.app_bot.* — modules/app_bot business error codes. These endpoints
// serve the management console (/v1/admin/app_bot, gated admin∪superAdmin) and
// space owners/admins (/v1/space/:space_id/app_bot), so — like bot_api — many
// sites returned real HTTP status codes the console branches on (404/409). Those
// are rendered via httperr.ResponseErrorLWithStatus to PRESERVE the wire status
// rather than the D14 fixed-400 path.
//
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never surface
// their message on the wire — callers MUST log the underlying err with full
// context (zap.Error) before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrAppBotRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, invalid robot_uid, empty update). The offending
	// field is surfaced via Details when identifiable.
	ErrAppBotRequestInvalid = register(codes.Code{
		ID:             "err.server.app_bot.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrAppBotIDInvalid covers a bot id that fails the format rule
	// (^[a-z0-9][a-z0-9_-]{0,29}$) or collides with a reserved id.
	ErrAppBotIDInvalid = register(codes.Code{
		ID:             "err.server.app_bot.id_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid bot id.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrAppBotNotFound covers a missing/scope-mismatched bot, and (on the
	// apply/discover flow) a bot that does not exist or is not published.
	ErrAppBotNotFound = register(codes.Code{
		ID:             "err.server.app_bot.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Bot not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	// ErrAppBotIDConflict covers a create colliding with an in-use bot id.
	ErrAppBotIDConflict = register(codes.Code{
		ID:             "err.server.app_bot.id_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The bot id is already in use.",
	})
	// ErrAppBotTokenRotationConflict covers a token rotation that lost the
	// optimistic-lock race (the token changed under us); retryable.
	ErrAppBotTokenRotationConflict = register(codes.Code{
		ID:             "err.server.app_bot.token_rotation_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The token was rotated by another request; please retry.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrAppBotQueryFailed covers read-path failures (bot list/detail SELECTs,
	// space-member and friend-relation lookups). Log the underlying err first.
	ErrAppBotQueryFailed = register(codes.Code{
		ID:             "err.server.app_bot.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query data.",
		Internal:       true,
	})
	// ErrAppBotStoreFailed covers mutation-path failures (bot create/update/
	// delete/publish, token update, user-record and friend-relation creation).
	// Log the underlying err first.
	ErrAppBotStoreFailed = register(codes.Code{
		ID:             "err.server.app_bot.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update data.",
		Internal:       true,
	})
	// ErrAppBotIMTokenFailed covers IM-token issuance failures on bot create and
	// token rotation. Log the underlying err first.
	ErrAppBotIMTokenFailed = register(codes.Code{
		ID:             "err.server.app_bot.im_token_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to obtain an IM token.",
		Internal:       true,
	})
	// ErrAppBotInternal covers token generation failures and broken invariants
	// (e.g. a space bot missing its space_id). Log the underlying err first.
	ErrAppBotInternal = register(codes.Code{
		ID:             "err.server.app_bot.internal",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Internal error.",
		Internal:       true,
	})
)
