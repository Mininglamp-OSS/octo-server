package oidc

// endSessionEndpointForLogout 报出可用于 RP-Initiated Logout 的上游端点,
// 拿不到时返回空串。
//
// 这个判断决定"要不要装 id_token 缓存",而缓存缺失的后果是静默的:logout 拿不到
// id_token_hint → LogoutURL 返回 ("", false) → 用户登出 DMWork 之后在 IdP 侧仍是
// 登录态。所以判断依据必须是**能力声明**,不是具体类型。
// 曾经这里写的是 `p.(*oidcProvider)` —— 对具体类型断言。任何一层包装(装饰器、
// 将来接的第三个 id_token 能力 provider)都会让断言失败,于是这个能力静默消失。
// 而 provider.go 开头就写着"业务分支一律读 Capabilities,不读 Kind"。
func endSessionEndpointForLogout(p AuthProvider) string {
	if p == nil || !p.Capabilities().IDToken {
		// 没有 id_token 就没有 id_token_hint,RP-Initiated Logout 无从构造。
		// plain-OAuth2 走的是自己的 SLO 端点,不经过这里。
		return ""
	}
	return p.EndSessionEndpoint()
}
