package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.document.* — modules/document business error codes.
var (
	ErrDocumentRequestInvalid = register(codes.Code{
		ID:             "err.server.document.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid document request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrDocumentForbidden = register(codes.Code{
		ID:             "err.server.document.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to access this document resource.",
	})
	ErrDocumentNotFound = register(codes.Code{
		ID:             "err.server.document.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Document resource not found.",
	})
	ErrDocumentQueryFailed = register(codes.Code{
		ID:             "err.server.document.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query document data.",
		Internal:       true,
	})
	ErrDocumentStoreFailed = register(codes.Code{
		ID:             "err.server.document.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update document data.",
		Internal:       true,
	})
)
