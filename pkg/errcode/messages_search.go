package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.messages_search.* — modules/messages_search business error codes.
//
// These codes back the R2 12-item enum shipped to API clients (per the
// docs/messages-search/octo-server-search-dev.md §8 mapping table). The R2 wire
// codes are surfaced through the i18n localized error envelope and map to the
// HTTPStatus values declared here (renderer keeps wire status pinned to 400 for
// legacy compatibility while exposing the real status in error.http_status).
var (
	// VALIDATION_ERROR — bad request body / cursor / filter.
	ErrMessagesSearchValidationFailed = register(codes.Code{
		ID:             "err.server.messages_search.validation_failed",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid search request.",
		SafeDetailKeys: []string{"field", "reason", "max_length"},
	})

	// UPSTREAM_UNAVAILABLE — OS network / timeout / 5xx.
	ErrMessagesSearchUpstreamUnavailable = register(codes.Code{
		ID:             "err.server.messages_search.upstream_unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "Search service is temporarily unavailable.",
		Internal:       true,
	})

	// INTERNAL_ERROR — OS 4xx (DSL bug) / unexpected.
	ErrMessagesSearchInternal = register(codes.Code{
		ID:             "err.server.messages_search.internal",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Internal search error.",
		Internal:       true,
	})

	// RATE_LIMITED — per-loginUID 5 QPS / 20 burst exceeded.
	ErrMessagesSearchRateLimited = register(codes.Code{
		ID:             "err.server.messages_search.rate_limited",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "Search rate limit exceeded.",
		SafeDetailKeys: []string{"retry_after"},
	})

	// NOT_FOUND — channel not visible / Space rejection.
	ErrMessagesSearchNotFound = register(codes.Code{
		ID:             "err.server.messages_search.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Channel or resource not found for search.",
		SafeDetailKeys: []string{"resource"},
	})

	// SEARCH_DISABLED — the deployment has OCTO_SEARCH_BACKEND=disabled, so
	// every search surface (the new _search* endpoints, the legacy
	// /v1/message/search path, and modules/search global search) refuses
	// uniformly instead of 500/panic/leaking results. Core IM stays fully
	// functional; the client uses appconfig.search_enabled to hide the search
	// box rather than probe this code per request.
	ErrMessagesSearchDisabled = register(codes.Code{
		ID:             "err.server.messages_search.disabled",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "Search is not enabled on this deployment.",
	})

	// DEPTH_EXCEEDED — the caller paged past the maximum cumulative result
	// depth (aligned with OpenSearch max_result_window=10000). The cap is on
	// the cumulative number of results already returned (encoded in the
	// cursor), NOT on the per-request page_size, so shrinking/growing
	// page_size cannot be used to walk past it. Clients that hit this should
	// narrow the query (keyword / time window / sender) instead of paging
	// deeper.
	ErrMessagesSearchDepthExceeded = register(codes.Code{
		ID:             "err.server.messages_search.depth_exceeded",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Search pagination depth limit reached; narrow your query.",
		SafeDetailKeys: []string{"max_depth"},
	})
)
