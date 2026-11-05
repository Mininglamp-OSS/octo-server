package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.search.* — modules/search business error codes (api.go).
var (
	ErrSearchNameQueryChanged = register(codes.Code{
		ID: "err.server.search.query_changed", HTTPStatus: http.StatusConflict,
		DefaultMessage: "Search results changed. Please search again.",
	})
	ErrSearchNameForbidden = register(codes.Code{
		ID: "err.server.search.name_forbidden", HTTPStatus: http.StatusForbidden,
		DefaultMessage: "This search scope is not accessible.",
	})
	ErrSearchNameQueryFailed = register(codes.Code{
		ID: "err.server.search.name_query_failed", HTTPStatus: http.StatusInternalServerError,
		DefaultMessage: "Failed to search names.", Internal: true,
	})
	ErrSearchRequestInvalid = register(codes.Code{
		ID:             "err.server.search.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid search request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrSearchMessageQueryFailed = register(codes.Code{
		ID:             "err.server.search.message_query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query message search data.",
		Internal:       true,
	})
	ErrSearchGroupQueryFailed = register(codes.Code{
		ID:             "err.server.search.group_query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query group search data.",
		Internal:       true,
	})
	ErrSearchUserQueryFailed = register(codes.Code{
		ID:             "err.server.search.user_query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query user search data.",
		Internal:       true,
	})
)
