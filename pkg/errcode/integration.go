package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// Integration / Octo-link 错误码。
//
// integration 接口对外固定英文输出（路由层强制 en-US，外部 API 不做多语言），
// 这里登记的 zh-CN 译文仅为 i18n 体系完整性兜底，正常不会被 integration 路由使用。
var (
	// ErrIntegrationDisabled —— exchange 能力被全局开关禁用（选项 A）。
	ErrIntegrationDisabled = register(codes.Code{
		ID:              "err.server.integration.disabled",
		HTTPStatus:      http.StatusForbidden,
		DefaultMessage:  "OIDC integration exchange is disabled.",
		DefaultMessages: map[string]string{"zh-CN": "OIDC 集成换取能力已禁用。"},
	})

	// ErrIntegrationUserNotLinked —— (issuer, sub) 未绑定 Octo 用户。
	ErrIntegrationUserNotLinked = register(codes.Code{
		ID:              "err.server.integration.user_not_linked",
		HTTPStatus:      http.StatusForbidden,
		DefaultMessage:  "User is not linked to an Octo account.",
		DefaultMessages: map[string]string{"zh-CN": "用户尚未关联 Octo 账号。"},
	})

	// ErrBotOccupied —— Bot 已被其他 Agent 占用；occupied_by 透传当前占用方。
	ErrBotOccupied = register(codes.Code{
		ID:              "err.server.bot.occupied",
		HTTPStatus:      http.StatusConflict,
		DefaultMessage:  "Bot is already occupied by another agent.",
		DefaultMessages: map[string]string{"zh-CN": "该 Bot 已被其他 Agent 占用。"},
		SafeDetailKeys:  []string{"occupied_by"},
	})
)

// 复用 pkg/i18n/codes 已注册的 err.shared.* 码：integration / Octo-link 与 Bot
// 占用端点的非业务错误（参数、未找到、无权限、内部错误）走这些通用码。在此以
// 强类型 var 暴露，调用点与上面的 err.server.* 对称，避免裸字符串引用 ID。
// 这些码由 codes 包 init 注册（shared.go），Go 保证被导入包的 init 先于本包 var
// 初始化执行，故 shared() 查找一定命中。
var (
	ErrSharedAuthRequired = shared("err.shared.auth.required")
	ErrSharedTokenMissing = shared("err.shared.auth.token_missing")
	ErrSharedTokenInvalid = shared("err.shared.auth.token_invalid")
	ErrSharedRateLimited  = shared("err.shared.rate.limited")
	ErrSharedParamInvalid = shared("err.shared.param.invalid")
	ErrSharedForbidden    = shared("err.shared.auth.forbidden")
	ErrSharedNotFound     = shared("err.shared.not_found")
	ErrSharedInternal     = shared("err.shared.internal")
)

// shared 解析一个**已注册**的 err.shared.* 码。未知 ID 直接 panic——在包初始化
// 期暴露拼写错误，而非延迟到请求时静默降级。
func shared(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("errcode: unknown shared code " + id)
	}
	return c
}
