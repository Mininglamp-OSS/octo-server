package opanalytics

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respForbidden 非超级管理员访问看板 → 403。
func respForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrOpanalyticsForbidden, nil, nil)
}

// respRequestInvalid 请求参数无效(reason ∈ {date_range, space_id, sort})。
func respRequestInvalid(c *wkhttp.Context, reason string) {
	details := i18n.Details{}
	if reason != "" {
		details["reason"] = reason
	}
	httperr.ResponseErrorL(c, errcode.ErrOpanalyticsRequestInvalid, nil, details)
}

// respNotFound Space 不存在 → 404。
func respNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrOpanalyticsNotFound, nil, nil)
}

// respQueryFailed 查询失败 → 500(Internal，渲染层隐藏细节，调用方须先记 zap 日志)。
func respQueryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorL(c, errcode.ErrOpanalyticsQueryFailed, nil, nil)
}
