package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.qrcode.* — modules/qrcode business error codes (api.go).
var (
	ErrQRCodeRequestInvalid = register(codes.Code{
		ID:             "err.server.qrcode.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid QR code request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrQRCodeTokenRequired = register(codes.Code{
		ID:             "err.server.qrcode.token_required",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Token is required.",
		SafeDetailKeys: []string{"field"},
	})
	ErrQRCodeTokenInvalid = register(codes.Code{
		ID:             "err.server.qrcode.token_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Token information is invalid.",
	})
	ErrQRCodeNotFound = register(codes.Code{
		ID:             "err.server.qrcode.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The QR code is invalid or has expired.",
	})
	ErrQRCodeUserNotFound = register(codes.Code{
		ID:             "err.server.qrcode.user_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "User not found.",
	})
	ErrQRCodeGroupNotFound = register(codes.Code{
		ID:             "err.server.qrcode.group_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Group not found.",
	})
	ErrQRCodeGroupSpaceForbidden = register(codes.Code{
		ID:             "err.server.qrcode.group_space_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only members of this Space can join the group chat.",
	})
	ErrQRCodeQueryFailed = register(codes.Code{
		ID:             "err.server.qrcode.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query QR code data.",
		Internal:       true,
	})
	ErrQRCodeStoreFailed = register(codes.Code{
		ID:             "err.server.qrcode.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update QR code data.",
		Internal:       true,
	})
)
