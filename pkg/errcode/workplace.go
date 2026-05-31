package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.workplace.* — modules/workplace business error codes (api.go /
// api_manager.go). DefaultMessage holds the en-US source (D4); the zh-CN runtime
// translation lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes
// never surface their message on the wire — callers MUST log the underlying err
// with full context via the module logger before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrWorkplaceRequestInvalid is the catch-all for missing/malformed request
	// input (empty app_id / category_no / banner_no path params, empty required
	// body fields, BindJSON failure, common.ErrData). The offending field is
	// surfaced via Details when the caller can identify it.
	ErrWorkplaceRequestInvalid = register(codes.Code{
		ID:             "err.server.workplace.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrWorkplaceAppNotFound covers an app that does not exist, has been
	// deleted, or is disabled (add/update/record paths).
	ErrWorkplaceAppNotFound = register(codes.Code{
		ID:             "err.server.workplace.app_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The app does not exist or is unavailable.",
	})
	ErrWorkplaceCategoryNotFound = register(codes.Code{
		ID:             "err.server.workplace.category_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The category does not exist.",
	})

	// ---- conflict (409) ------------------------------------------------------

	ErrWorkplaceAppNameExists = register(codes.Code{
		ID:             "err.server.workplace.app_name_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "An app with this name already exists.",
	})
	ErrWorkplaceCategoryNameExists = register(codes.Code{
		ID:             "err.server.workplace.category_name_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "A category with this name already exists.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrWorkplaceQueryFailed covers read-path failures (DB SELECT/search/count).
	// Log the underlying err before responding.
	ErrWorkplaceQueryFailed = register(codes.Code{
		ID:             "err.server.workplace.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query workplace data.",
		Internal:       true,
	})
	// ErrWorkplaceStoreFailed covers mutation-path failures (DB write, transaction
	// begin/commit/rollback). Log the underlying err before responding.
	ErrWorkplaceStoreFailed = register(codes.Code{
		ID:             "err.server.workplace.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update workplace data.",
		Internal:       true,
	})
)
