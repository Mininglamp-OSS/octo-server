package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

var (
	ErrResourceShareRequestInvalid = register(codes.Code{
		ID:             "err.server.resource_share.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid resource share request.",
		SafeDetailKeys: []string{"reason"},
	})
	ErrResourceSharePayloadTooLarge = register(codes.Code{
		ID:             "err.server.resource_share.payload_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "The resource share request is too large.",
	})
	ErrResourceShareDisabled = register(codes.Code{
		ID:             "err.server.resource_share.disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Resource sharing is not enabled.",
	})
	ErrResourceShareForbidden = register(codes.Code{
		ID:             "err.server.resource_share.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The resource share request is not authorized.",
	})
	ErrResourceShareReplayConflict = register(codes.Code{
		ID:             "err.server.resource_share.replay_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The resource share request conflicts with an earlier request.",
	})
	ErrResourceShareResourceUnavailable = register(codes.Code{
		ID:             "err.server.resource_share.resource_unavailable",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The resource can no longer be shared.",
	})
	ErrResourceShareIntentInvalid = register(codes.Code{
		ID:             "err.server.resource_share.intent_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The resource share intent is invalid.",
	})
	ErrResourceShareUnavailable = register(codes.Code{
		ID:             "err.server.resource_share.unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "Resource sharing is temporarily unavailable.",
		Internal:       true,
	})
)
