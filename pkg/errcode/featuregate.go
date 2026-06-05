package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.featuregate.* — modules/featuregate 管理端点错误码（list / update /
// addScope / delScope）。superadmin-only、无 legacy client，统一经
// httperr.ResponseErrorLWithStatus 渲染并保留真实 HTTP status。zh-CN 翻译在
// pkg/i18n/locales/active.zh-CN.toml。
var (
	// ErrFeatureGateRequestInvalid (400) — 请求体或字段非法（mode/percent/
	// fail_mode/scope_type/scope_id）；具体字段经 Details["reason"] 透出。
	ErrFeatureGateRequestInvalid = register(codes.Code{
		ID:             "err.server.featuregate.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"reason"},
	})

	// ErrFeatureGateNotFound (404) — 要删除的白名单条目不存在。
	ErrFeatureGateNotFound = register(codes.Code{
		ID:             "err.server.featuregate.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The feature gate scope does not exist.",
	})

	// ErrFeatureGateQueryFailed (500, Internal) — 读规则/白名单失败；真实错误仅日志。
	ErrFeatureGateQueryFailed = register(codes.Code{
		ID:             "err.server.featuregate.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query feature gate.",
		Internal:       true,
	})

	// ErrFeatureGateOperationFailed (500, Internal) — 写规则/白名单失败；真实错误仅日志。
	ErrFeatureGateOperationFailed = register(codes.Code{
		ID:             "err.server.featuregate.operation_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "The operation failed. Please try again later.",
		Internal:       true,
	})
)
