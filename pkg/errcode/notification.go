package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrNotificationPauseInvalidTime = register(codes.Code{
		ID:             "err.server.notification.pause.invalid_time",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Notification pause time is invalid.",
	})
	ErrNotificationPauseUpdateFailed = register(codes.Code{
		ID:             "err.server.notification.pause.update_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update notification pause state.",
		Internal:       true,
	})
)
