package wkhttp

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// headerToken 是 octo-lib AuthMiddleware 唯一认的凭据头(见其内部的
// c.GetHeader("token"))。名字是公开的前端协议,不能改。
const headerToken = "token"

// bearerScheme RFC 6750 的 Bearer scheme。规范规定 scheme **大小写不敏感**,
// 所以这里只用它做 EqualFold 比较,不做前缀匹配。
const bearerScheme = "Bearer"

// BearerTokenCompat 把 `Authorization: Bearer <token>` 回填成 `token` 头,
// 让按标准 OAuth2 习惯开发的客户端能直接调用本服务。
//
// 为什么需要:AuthMiddleware 只读自定义的 `token` 头。接入外部 IdP 之后,
// 客户端拿到我方签发的 token 后若按标准发 Authorization 头,登录能过、
// 之后每个 API 调用都 401 —— 一个只在集成联调阶段才会暴露的断点。
//
// 为什么不改 AuthMiddleware:它在 octo-lib 共享库里。改它要升库版本,
// 且影响所有依赖该库的服务。本仓加一层全局前置件即可,影响面收敛在本服务内。
//
// 三条约束:
//
//   - **`token` 头优先**。两个头都在且值不同时,用 `token`、不比较、不报错。
//     报错会让既有客户端在本中间件上线的瞬间开始失败;而这里的目的是纯增量兼容。
//   - **只认 Bearer**。Basic 等其它 scheme 原样放过 —— 仍有 webhook 在用。
//   - **不接受 query 参数兜底**。凭据放 URL 会进浏览器历史与 Referer,
//     accesslog 的脱敏只能挡住日志这一层,挡不住那两处。
//
// 副作用边界:本件**只**在 `token` 头缺失时新增该头,从不修改或删除
// Authorization 头 —— `bot_api` / `botfather` / `integration` /
// `bot_provision` / `usersecret` 都直接读 Authorization,必须保持原样。
//
// bot token 会不会被误当用户 token:不会。bot 路由不挂 AuthMiddleware;
// 即便挂了,bot token 在用户 token 缓存里查不到,结果仍是 401。
//
// 挂载位置:main.go 的全局中间件段。全局件先于各 route group 的
// AuthMiddleware 执行,回填才能生效。
func BearerTokenCompat() gin.HandlerFunc {
	return func(c *gin.Context) {
		// token 头优先:非空即原样放过,完全不看 Authorization。
		if c.GetHeader(headerToken) != "" {
			c.Next()
			return
		}
		if tok := extractBearerCredential(c.GetHeader("Authorization")); tok != "" {
			// 写进 Request.Header,后续 c.GetHeader 才能读到。
			c.Request.Header.Set(headerToken, tok)
		}
		c.Next()
	}
}

// extractBearerCredential 从 Authorization 头取出 Bearer 凭据;
// 不是 Bearer、或形态不明确时返回空串。
//
// 用 strings.Fields 而不是 TrimPrefix:前者顺带处理 scheme 与凭据之间的
// 多余空白,并且能识别"只有 scheme 没有凭据"的情形 —— 那种情况必须返回空,
// 否则会回填一个空 token 头,把 AuthMiddleware 的"token 为空"分支
// (它有专门的 err.shared.auth.token_missing)错误地变成"token 无效"。
//
// 恰好两段才接受:多出一段说明形态不符合 RFC 6750 的 credentials 语法,
// 保守拒绝而不去猜哪一段是凭据。
func extractBearerCredential(auth string) string {
	parts := strings.Fields(auth)
	if len(parts) != 2 || !strings.EqualFold(parts[0], bearerScheme) {
		return ""
	}
	return parts[1]
}
