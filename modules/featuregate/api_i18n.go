package featuregate

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// 管理端 i18n 错误 responder。端点 superadmin-only、无 legacy client，统一用
// ResponseErrorLWithStatus 保留真实语义状态（400/404/500）。superadmin 权限失败
// 不走这里，直接 c.ResponseError(c.CheckLoginRoleIsSuperAdmin()) 复用内置错误。

// gateRequestInvalid 返回 400；reason ∈ {key,body,mode,percent,description,
// scope_type,scope_id} 经安全 Details 透出。
func gateRequestInvalid(c *wkhttp.Context, reason string) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrFeatureGateRequestInvalid, nil, i18n.Details{"reason": reason})
}

// gateNotFound 返回 404 —— 删除的白名单条目不存在。
func gateNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrFeatureGateNotFound, nil, nil)
}

// gateQueryFailed 返回 500（Internal）—— 读失败，真实错误仅日志。
func gateQueryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrFeatureGateQueryFailed, nil, nil)
}

// gateOperationFailed 返回 500（Internal）—— 写失败，真实错误仅日志。
func gateOperationFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrFeatureGateOperationFailed, nil, nil)
}
