package codes

import "net/http"

// 首期 err.shared.* 错误码集中注册。
//
// 命名约定：
//   - err.shared.<domain>.<reason>，全部小写、点分。
//   - 跨 module 的通用错误（鉴权、限流、参数、内部）归 shared.*；
//     业务专属错误归 pkg/errcode 的 err.server.<module>.<reason>。
//
// DefaultMessage 一律 en-US（source 语言，D4）；zh-CN/其他 lang 翻译落在
// pkg/i18n/locales/translate.<lang>.toml，由 goi18n merge 生成 stub 后人工补译。
//
// DefaultMessages 仅在主方案 D22「极端故障兜底」场景使用：
// bundle 文件加载失败 + source 缺译同时发生。日常运营不依赖此字段。
//
// SafeDetailKeys 白名单依据 octo-lib 中间件实际透传的 details key 制定：
// rate.limited 由 RateLimitMiddleware 设 retry_after；param.invalid 由业务侧
// ResponseErrorL 调用方传 field。其余 code 默认不透传 details。
func init() {
	Register(Code{
		ID:             "err.shared.auth.required",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Please log in to continue.",
		DefaultMessages: map[string]string{
			"zh-CN": "请先登录！",
		},
	})

	Register(Code{
		ID:             "err.shared.auth.token_missing",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Authentication token is required.",
		DefaultMessages: map[string]string{
			"zh-CN": "token不能为空，请先登录！",
		},
	})

	Register(Code{
		ID:             "err.shared.auth.token_invalid",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Authentication token is invalid.",
		DefaultMessages: map[string]string{
			"zh-CN": "token有误！",
		},
	})

	Register(Code{
		ID:             "err.shared.auth.token_expired",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Authentication token has expired.",
		DefaultMessages: map[string]string{
			"zh-CN": "登录已过期，请重新登录。",
		},
	})

	Register(Code{
		ID:             "err.shared.auth.forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to perform this action.",
		DefaultMessages: map[string]string{
			"zh-CN": "无权执行此操作。",
		},
	})

	Register(Code{
		ID:             "err.shared.rate.limited",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "Too many requests, please try again later.",
		DefaultMessages: map[string]string{
			"zh-CN": "请求过于频繁，请稍后再试。",
		},
		SafeDetailKeys: []string{"retry_after"},
	})

	Register(Code{
		ID:             "err.shared.param.invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request parameter.",
		DefaultMessages: map[string]string{
			"zh-CN": "请求参数无效。",
		},
		SafeDetailKeys: []string{"field"},
	})

	Register(Code{
		ID:             "err.shared.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Resource not found.",
		DefaultMessages: map[string]string{
			"zh-CN": "资源不存在。",
		},
	})

	// Internal=true：renderer 不应把 spec.DefaultMessage / Params / Details 暴露给客户端，
	// 应输出占位文案（如 "Internal server error."），原始细节仅记日志。
	Register(Code{
		ID:             "err.shared.internal",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Internal server error.",
		DefaultMessages: map[string]string{
			"zh-CN": "服务器内部错误。",
		},
		Internal: true,
	})
}
