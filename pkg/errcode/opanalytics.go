package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.opanalytics.* — modules/opanalytics 运营分析看板(管理端 superAdmin
// 跨 space 只读)的业务错误码。DefaultMessage 是 en-US 源；zh-CN 运行时翻译在
// pkg/i18n/locales/active.zh-CN.toml。5xx ⟺ Internal=true(渲染层隐藏 message/details)。
var (
	ErrOpanalyticsForbidden = register(codes.Code{
		ID:             "err.server.opanalytics.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only a super admin can access the analytics dashboard.",
	})
	ErrOpanalyticsRequestInvalid = register(codes.Code{
		ID:             "err.server.opanalytics.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid analytics request.",
		SafeDetailKeys: []string{"reason"},
	})
	ErrOpanalyticsNotFound = register(codes.Code{
		ID:             "err.server.opanalytics.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The space does not exist.",
	})
	ErrOpanalyticsQueryFailed = register(codes.Code{
		ID:             "err.server.opanalytics.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query analytics data.",
		Internal:       true,
	})
	ErrOpanalyticsETLAlreadyRunning = register(codes.Code{
		ID:             "err.server.opanalytics.etl_already_running",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Analytics ETL is already running.",
	})
	ErrOpanalyticsETLTriggerFailed = register(codes.Code{
		ID:             "err.server.opanalytics.etl_trigger_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to trigger analytics ETL.",
		Internal:       true,
	})
)
