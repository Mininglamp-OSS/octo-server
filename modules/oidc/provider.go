package oidc

import (
	"context"
	"time"
)

// 本文件定义 AuthProvider —— 单个上游身份提供方的协议适配层。
//
// 引入它的原因:modules/oidc 原本是一个严格的 OpenID Connect 实现,把
// Discovery、id_token 验签、JWKS 与规范形态的 /userinfo 当作前提。要接入只讲
// 标准 OAuth2 authorization_code 的 IdP,这些前提逐条不成立,而把差异塞进
// handler 会在 callback 里长出一串 if kind == ... 分支。
//
// 边界画在"验签 + 拉 userinfo + 交叉校验 + 合并 claims"这一整段之外(见
// Identity),因为那几步全是标准 OIDC 的协议细节;留在 handler 里会迫使
// plain-OAuth2 实现去 stub 一堆空方法。
//
// 需要注意的取舍:业务分支一律读 Capabilities,**不读** Kind。Kind 只用于
// 日志、metric label 与启动期配置校验。否则接第三个 IdP 时又要满仓库找 switch。

// ProviderKind 协议种类。取值进 env(OCTO_OIDC_PROVIDER_KIND)与 metric label,
// 因此用稳定的小写标识,不带厂商名。
type ProviderKind string

const (
	// KindOIDC 标准 OpenID Connect(Discovery + id_token + JWKS)。
	KindOIDC ProviderKind = "oidc"
	// KindOAuth2 只有 OAuth2 authorization_code 的 IdP:无 Discovery、无
	// id_token、无 JWKS,access_token 为不透明串,/userinfo 为厂商私有信封。
	KindOAuth2 ProviderKind = "oauth2"
)

// ProviderCapabilities 声明上游协议**实际具备**的能力。
//
// 每个字段都对应一处上层必须跳过的步骤。诚实声明缺失能力比默默降级重要:
// 例如 CrossCheckSub=false 明确表示"少了一道校验",而不是让那道校验悄悄消失。
type ProviderCapabilities struct {
	// PKCE authorize 端点接受 code_challenge/S256。
	PKCE bool
	// Nonce authorize 接受 nonce,且回程有可验证的载荷能把它带回来。
	Nonce bool
	// IDToken token 响应含可验签的 id_token。为 false 时上层不得缓存
	// id_token、不得构造 RP-Initiated Logout。
	IDToken bool
	// RefreshToken 存在**文档化且可用**的刷新端点。仅返回 refresh_token
	// 但没有换取端点时必须为 false,否则 sync worker 会空转。
	RefreshToken bool
	// CrossCheckSub 身份有两个独立来源(id_token 与 /userinfo)可交叉校验。
	CrossCheckSub bool
	// UpstreamLogout IdP 提供可跳转的登出端点。
	UpstreamLogout bool

	// SubjectMayBeReusedPersonnelID 该上游的 subject **可能**是人事系统分配、
	// 会在离职/入职之间复用的标识(工号)。为 true 时上层拒绝"短纯数字"形态的
	// subject —— (issuer, subject) 是不可变主键,复用的工号迟早把新人指到
	// 前任的账号上。
	//
	// 诚实说明这一位的性质:它其实是**按部署**的事实(取决于运维接的是哪家 IdP
	// 的哪套人事系统),现在用 kind 近似 —— plain-OAuth2 只对接了一家,而那家的
	// 文档正是这条论证的来源;标准 OIDC 是存量部署对着任意 IdP 用的通用客户端,
	// 无从知道对方的 sub 从哪来。
	//
	// 所以默认必须是 false:声明这个危险等于对一个 sub 是数据库小主键(1001、42)
	// 的自建 IdP **全量拒登**,而那类 IdP 什么都不复用。接第二家 plain-OAuth2
	// IdP 时,这一位要变成按部署可配,不能继续跟着 kind 走。
	//
	// 长度上限不由这一位控制:那是存储性质,对所有来源统一施加
	// (identity_bounds.go)。
	SubjectMayBeReusedPersonnelID bool

	// OpaqueClientCredential 表示"客户端在这个 provider 上出示的凭据是不透明串,
	// 不可能是 JWT"。
	//
	// 用途只有一个:当我方的业务 JWT 验签器未配置(密钥缺失)时,一张 JWT 形态的
	// 凭据无法被归属判定认出来,会被回落转发给上游 —— 而上游那条路把凭据放在
	// URL query 上。为 true 时上层可以据此**就地拒绝** JWT 形态的输入:该 provider
	// 的 access_token 是不透明 UUID,JWT 在这条路上不可能是合法凭据,转发不可能
	// 成功、只可能把载荷 PII 和一份在客户端密钥下合法的签名送进第三方日志。
	//
	// 为什么是能力位而不是无条件:这是**厂商事实,不是协议事实**。标准 OIDC 下
	// 客户端出示的凭据本身就是 JWT(id_token),无条件拒绝会把 /exchange 在那个
	// kind 上整条掐断。同一个教训见 SubjectMayBeReusedPersonnelID。
	//
	// 这条判定只在"验签器未配置"时才需要 —— 配置正常时 HMAC 是决定性判据,
	// 不需要看形态。
	OpaqueClientCredential bool
}

// AuthCodeParams 构造 authorize URL 所需的请求级一次性参数。
//
// 标准 OIDC 会填满三个字段;plain-OAuth2 只用 State,其余由实现忽略 ——
// 调用方无需按 kind 分别构造。
type AuthCodeParams struct {
	// State CSRF 绑定,任何 provider 都必须提供。
	State string
	// Nonce 仅 Capabilities.Nonce=true 时有意义。
	Nonce string
	// CodeChallenge 仅 Capabilities.PKCE=true 时有意义。
	CodeChallenge string
}

// TokenSet 换码结果的协议中立表示。
//
// 存在的意义是不让 *oauth2.Token 穿透到 handler:一旦穿透,api.go 就会通过
// tok.Extra("id_token") 之类的调用与 oauth2 库形成隐式耦合。
type TokenSet struct {
	AccessToken  string
	TokenType    string
	RefreshToken string
	Expiry       time.Time
	// RawIDToken 为空即该 provider 不下发 id_token。
	RawIDToken string
}

// LogoutHint 构造上游登出 URL 所需的请求级材料。
type LogoutHint struct {
	UID string
	// RawIDToken 供 OIDC 的 id_token_hint 使用;plain-OAuth2 实现忽略它。
	RawIDToken string
}

// IdentityClaims 归一化身份。
//
// 刻意做成 IDTokenClaims 的**别名**而非新结构体:bind_store 在 Redis 里存的是
// 该结构体的 JSON 快照,decodeClaimsSnapshot 负责读回。改名或改 tag 会让存量
// 的在途绑定会话全部解不出来。别名让新代码用中性名字,同时保持 wire 兼容。
type IdentityClaims = IDTokenClaims

// AuthProvider 一个上游身份提供方的协议适配器。
//
// 实现契约(两个实现都必须满足,由 provider_conformance_test.go 共同钉住):
//
//   - Identity 返回的 claims 必须 Issuer != "" 且 Subject != ""。
//     user_oidc_identity.subject 是 NOT NULL DEFAULT ” 且带 UNIQUE(issuer,subject),
//     放进空串会让所有空 sub 用户塌成同一行、互相登进对方账号 —— 这是账号接管,
//     不只是脏数据,因此必须在 provider 层就拦住。
//   - Identity 内部完成本协议一切可做的完整性校验(验签 / sub 交叉校验 /
//     私有信封的 success+code 判定),对上层只暴露"可信"或 error。
//   - 任何返回给上层的 error 都不得包含带凭据的 URL —— 见 sanitizeTransportErr。
type AuthProvider interface {
	Kind() ProviderKind
	Capabilities() ProviderCapabilities

	// EndSessionEndpoint 上游可跳转的 RP-Initiated Logout 端点;不可用时返回空串。
	//
	// 契约:Capabilities().IDToken 为 false 的实现**必须**返回空串 —— 没有
	// id_token 就没有 id_token_hint,那个端点结束不了任何会话。
	//
	// 为什么要放进接口而不是让上层对具体类型做断言:上层用它决定"要不要装
	// id_token 缓存",而对 *oidcProvider 做断言会被任何一层包装(装饰器、将来
	// 的第三个 IdP)悄悄打掉,RP-Initiated Logout 于是静默消失。本文件开头
	// 那条规则("业务分支一律读 Capabilities,不读 Kind")就是为这类判断立的。
	EndSessionEndpoint() string

	// Issuer 写入 user_oidc_identity.issuer 的稳定身份命名空间。
	//
	// 标准 OIDC 下它是经 Discovery 校验过的 issuer;plain-OAuth2 下它是运维
	// 配置的常量,不具备密码学意义。该值一旦上线不可更改:改了会让全部存量
	// 绑定在登录第一步 miss,等于全员按新账号重建。
	Issuer() string

	AuthCodeURL(p AuthCodeParams) (string, error)

	// Exchange 用授权码换 token。codeVerifier 仅 Capabilities.PKCE 时有意义。
	Exchange(ctx context.Context, code, codeVerifier string) (*TokenSet, error)

	// Identity 返回本次登录的权威身份 claims。
	//
	// OIDC 实现:验签 id_token → 按需拉 /userinfo → 交叉校验 sub → 合并。
	// plain-OAuth2 实现:GET /userinfo → 解私有信封 → 映射到 claims。
	Identity(ctx context.Context, tok *TokenSet) (*IdentityClaims, error)

	// IdentityFromClientCredential 把**客户端直接出示的**上游凭据映射为 claims。
	//
	// 与 Identity 的区别不在实现,而在凭据的来源:Identity 处理的是我方用
	// authorization code 换回来的 TokenSet(我方知道里面有什么);这个方法处理的是
	// 客户端自己完成 SSO 后带过来的一个字符串(我方只知道它是个 Bearer)。
	//
	// 为什么必须由 provider 决定怎么解释这个字符串:
	//
	//   - 标准 OIDC 下客户端手上的可验证凭据是 id_token(有签名,能独立验);
	//   - plain-OAuth2 下是不透明的 access_token(没有签名,只能拿去问 /userinfo)。
	//
	// 上层拿不到这个区别就只能猜,而**按 token 长得像不像 JWT 来猜是不可接受的**:
	// 一个不透明 access_token 完全可能恰好是 JWT 形态,那时会被送进错误的验证路径
	// 并可能通过。所以由 provider 按自己的协议事实解释,上层保持 kind 无关。
	//
	// 失败一律返回 error;调用方对客户端只回一个笼统的 401(反枚举)。
	IdentityFromClientCredential(ctx context.Context, raw string) (*IdentityClaims, error)

	// LogoutURL 返回上游登出跳转地址;不支持或材料不全时返回 ("", false)。
	//
	// 返回值必须由**浏览器顶层导航**访问。服务端代理该请求不会携带用户在 IdP
	// 域下的 cookie,等于谁也没登出。
	LogoutURL(ctx context.Context, hint LogoutHint) (string, bool)
}
