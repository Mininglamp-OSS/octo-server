package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.doc_binding.* — modules/doc_binding business error codes.
// DefaultMessage holds the en-US source (D4); zh-CN translations live in
// pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never surface their
// message on the wire — callers MUST log the underlying err with zap.Error
// before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	ErrDocBindingRequestInvalid = register(codes.Code{
		ID:             "err.server.doc_binding.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})

	ErrDocBindingSlugConflict = register(codes.Code{
		ID:             "err.server.doc_binding.slug_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This slug is already bound.",
		SafeDetailKeys: []string{"field"},
	})

	// ---- authz (403) ---------------------------------------------------------
	// 权限拒绝直接 403；binding 存在但当前 caller 不该看时，走 hidden-404（ErrDocBindingNotFound）。
	ErrDocBindingForbidden = register(codes.Code{
		ID:             "err.server.doc_binding.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to modify this doc binding.",
	})

	// ---- not found (404) -----------------------------------------------------
	// 同时承担 hidden-404：真的不存在 + 非成员看不到的存在，都返回此码，避免枚举 slug 探测挂载点。
	ErrDocBindingNotFound = register(codes.Code{
		ID:             "err.server.doc_binding.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Doc binding not found.",
	})

	// ---- internal (500) ------------------------------------------------------
	ErrDocBindingStoreFailed = register(codes.Code{
		ID:             "err.server.doc_binding.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Doc binding storage operation failed.",
		Internal:       true,
	})
)
