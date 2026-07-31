package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrBotMentionInProgress = register(codes.Code{
		ID:             "err.server.bot_mention.in_progress",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "A request with this idempotency key is still in progress.",
	})
	ErrBotMentionIdempotencyConflict = register(codes.Code{
		ID:             "err.server.bot_mention.idempotency_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This idempotency key was already used with a different request.",
	})
	ErrBotMentionStoreFailed = register(codes.Code{
		ID:             "err.server.bot_mention.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process the bot mention event.",
		Internal:       true,
	})
)
