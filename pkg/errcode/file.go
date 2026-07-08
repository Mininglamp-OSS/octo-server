package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.file.* — modules/file business error codes.
var (
	ErrFileRequestInvalid = register(codes.Code{
		ID:             "err.server.file.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid file request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrFileForbidden = register(codes.Code{
		ID:             "err.server.file.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The file link is invalid or expired.",
	})
	ErrFileNotFound = register(codes.Code{
		ID:             "err.server.file.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "File not found.",
	})
	ErrFilePayloadTooLarge = register(codes.Code{
		ID:             "err.server.file.payload_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "The uploaded file exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"max_mb"},
	})
	ErrFileMethodNotAllowed = register(codes.Code{
		ID:             "err.server.file.method_not_allowed",
		HTTPStatus:     http.StatusMethodNotAllowed,
		DefaultMessage: "Method not allowed.",
	})
	ErrFileStoreFailed = register(codes.Code{
		ID:             "err.server.file.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to store file data.",
		Internal:       true,
	})
)
