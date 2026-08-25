package featuregate

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// 本模块的 i18n 错误 responder。全部端点都是新增的、无 legacy client，因此统一走
// ResponseErrorLWithStatus 保留真实语义状态（400/403/404/500），而不是 D14 兼容期
// 的固定 400。

// gateRequestInvalid 返回 400；reason ∈ {key,body,mode,percent,bucket_by,
// description,scope_type,scope_id,client_visible_dimension} 经安全 Details 透出。
func gateRequestInvalid(c *wkhttp.Context, reason string) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrFeatureGateRequestInvalid, nil, i18n.Details{"reason": reason})
}

// gateForbidden 返回 403 —— superadmin 校验未通过。
//
// 刻意复用共享码 err.shared.auth.forbidden 而不是新增
// err.server.featuregate.forbidden：鉴权失败统一收敛到一个通用码是本仓的反枚举
// 约定，per-reason 的专用码会把「你差在哪个角色」透给调用方。具体原因只进日志。
//
// 本框架初版在四个 handler 里写的是 c.ResponseError(c.CheckLoginRoleIsSuperAdmin())
// —— 那是绕开 i18n 信封的 legacy 裸响应（也正因当时没有守卫测试才没被拦下）。
func gateForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
}

// gateNotFound 返回 404 —— 要删除的白名单条目不存在。
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
