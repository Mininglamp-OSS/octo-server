package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.category.* — modules/category business error codes (api.go).
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never surface
// their message on the wire — callers MUST log the underlying err with full
// context via the module logger before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrCategoryRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, empty category name, empty sort list). The
	// offending field is surfaced via Details when the caller can identify it.
	ErrCategoryRequestInvalid = register(codes.Code{
		ID:             "err.server.category.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrCategoryNameTooLong = register(codes.Code{
		ID:             "err.server.category.name_too_long",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The category name cannot exceed 100 characters.",
		SafeDetailKeys: []string{"field", "max_length"},
	})
	ErrCategorySortListMismatch = register(codes.Code{
		ID:             "err.server.category.sort_list_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The category list does not match the existing categories.",
	})
	ErrCategorySortListDuplicate = register(codes.Code{
		ID:             "err.server.category.sort_list_duplicate",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The category list contains duplicates.",
	})
	ErrCategoryGroupSpaceMissing = register(codes.Code{
		ID:             "err.server.category.group_space_missing",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The group does not belong to any space.",
	})
	ErrCategorySpaceMismatch = register(codes.Code{
		ID:             "err.server.category.space_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The group and the category are not in the same space.",
	})

	// ---- permission / authorization (403) ------------------------------------

	ErrCategorySpaceMemberRequired = register(codes.Code{
		ID:             "err.server.category.space_member_required",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this space.",
	})
	ErrCategoryGroupMemberRequired = register(codes.Code{
		ID:             "err.server.category.group_member_required",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this group.",
	})
	// ErrCategoryPermissionDenied covers the ownership guards on update / delete /
	// move (cat.UID != loginUID) — all the same "not your category" condition.
	ErrCategoryPermissionDenied = register(codes.Code{
		ID:             "err.server.category.permission_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to operate on this category.",
	})
	ErrCategoryDefaultImmutable = register(codes.Code{
		ID:             "err.server.category.default_immutable",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The default category cannot be modified.",
	})
	ErrCategoryDefaultUndeletable = register(codes.Code{
		ID:             "err.server.category.default_undeletable",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The default category cannot be deleted.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrCategoryNotFound covers both a missing category and the deliberate
	// "not found or no permission" merge in the sort handler (no enumeration).
	ErrCategoryNotFound = register(codes.Code{
		ID:             "err.server.category.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Category not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	ErrCategoryLimitExceeded = register(codes.Code{
		ID:             "err.server.category.limit_exceeded",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You can create at most 20 categories per space.",
		SafeDetailKeys: []string{"max"},
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrCategoryQueryFailed covers read-path failures (membership check, DB
	// SELECT/count). Log the underlying err before responding.
	ErrCategoryQueryFailed = register(codes.Code{
		ID:             "err.server.category.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query category data.",
		Internal:       true,
	})
	// ErrCategoryStoreFailed covers mutation-path failures (DB write, transaction
	// begin/commit/rollback, sequence generation, follow-version bump). Log the
	// underlying err before responding.
	ErrCategoryStoreFailed = register(codes.Code{
		ID:             "err.server.category.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update category data.",
		Internal:       true,
	})
)
