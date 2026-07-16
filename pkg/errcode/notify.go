package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var ErrNotifyCardNotAllowed = register(codes.Code{
	ID:             "err.server.notify.card_not_allowed",
	HTTPStatus:     http.StatusBadRequest,
	DefaultMessage: "Card payloads are not allowed on the internal notification endpoint.",
})

var ErrNotifyCardInvalid = register(codes.Code{
	ID:             "err.server.notify.card_invalid",
	HTTPStatus:     http.StatusBadRequest,
	DefaultMessage: "The card notification request is invalid.",
})
