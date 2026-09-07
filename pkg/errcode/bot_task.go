package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrBotTaskForbidden           = register(codes.Code{ID: "err.server.bot_task.forbidden", HTTPStatus: http.StatusForbidden, DefaultMessage: "This source cannot deliver tasks to the requested bot."})
	ErrBotTaskInProgress          = register(codes.Code{ID: "err.server.bot_task.in_progress", HTTPStatus: http.StatusConflict, DefaultMessage: "A task with this idempotency key is still being accepted."})
	ErrBotTaskIdempotencyConflict = register(codes.Code{ID: "err.server.bot_task.idempotency_conflict", HTTPStatus: http.StatusConflict, DefaultMessage: "This idempotency key was already used with a different task."})
	ErrBotTaskStoreFailed         = register(codes.Code{ID: "err.server.bot_task.store_failed", HTTPStatus: http.StatusInternalServerError, DefaultMessage: "Failed to accept the bot task.", Internal: true})
)
