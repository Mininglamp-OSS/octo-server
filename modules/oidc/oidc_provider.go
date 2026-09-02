package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
)

// 本文件把既有的 *Client(go-oidc + oauth2 的封装)包装成 AuthProvider。
//
// 引入这层的代价必须说清楚:标准 OIDC 的登录代码路径被改写了,而它是现网
// 正在跑的认证路径。原先散在 callback handler 里的
// "取 id_token → 验签 → 判 needUserInfo → 拉 /userinfo → 交叉校验 sub → 合并 claims"
// 整段迁到 Identity 里。迁移必须保持四件事不变,它们是最容易被"塌缩成一次调用"
// 顺手丢掉的,而其中三条是安全检查:
//
//	1. id_token 验签(缺失或验不过一律拒登)
//	2. /userinfo 的 sub 必须等于 id_token 的 sub(否则账号串台)
//	3. nonce 必须能被 handler 比对 —— 比对本身留在 handler(它持有 state 里的
//	   期望值),但 provider 必须把解出的 nonce 交出去,否则那道校验会无声失效
//	4. /userinfo 失败不阻断登录(只是失去自动绑定能力)
//
// provider_conformance_test.go 与 TestOIDCProvider_PreservesSecurityChecks
// 就是为了钉住这四条。

// ErrIdentityUntrusted provider 无法确立可信身份。
//
// Identity 的所有失败都 wrap 它,让 handler 用 errors.Is 归成同一处理分支:
// 记 IP 失败计数 + 返回统一的通用错误。具体原因只进日志 —— 反枚举要求
// 认证失败对外只有一种表现,不能按原因分出可区分的响应。
var ErrIdentityUntrusted = errors.New("oidc: identity could not be established")

// oidcProviderConfig 构造标准 OIDC provider 所需的配置。
type oidcProviderConfig struct {
	// Client 已完成 Discovery 的客户端。
	Client *Client
	// Scopes 与 Client 同源(都来自 ProviderConfig.Scopes)。
	// 单独传一份而不从 Client 读,是为了不改动既有文件的导出面。
	Scopes []string

	// PostLogoutRedirectURI RP-Initiated Logout 的回跳地址,运维写死。
	PostLogoutRedirectURI string
	// EndSessionURLOverride 覆盖/兜底 Discovery 解出的 end_session 端点。
	EndSessionURLOverride string

	// OnWarn 可选:provider 内部遇到"不阻断但值得记录"的情况时回调
	// (目前只有 /userinfo 补全失败一种)。设计成回调而不是注入 logger,
	// 是为了让 provider 不依赖具体日志设施,同时不丢失这条信息 ——
	// 它是排查"为什么这个用户没自动绑定"的唯一线索。
	OnWarn func(msg string, err error)
}

// oidcProvider 标准 OpenID Connect provider。
type oidcProvider struct {
	cfg oidcProviderConfig
}

func newOIDCProvider(cfg oidcProviderConfig) (*oidcProvider, error) {
	// Client 在两种情况下可为 nil:
	//   - 单测只测 URL 拼装逻辑(传 EndSessionURLOverride 但不做 HTTP 调用);
	//   - 未来构造期错误降级路径。
	// 生产路径 Init() 保证 cfg.Client 非 nil(否则 provider 不挂);此处不强制
	// 报错,让单测/降级路径可以独立用 URL 构造逻辑。
	if cfg.Client == nil && cfg.EndSessionURLOverride == "" {
		return nil, fmt.Errorf("oidc: oidc provider: client is required")
	}
	// issuer 参与 uk_issuer_subject,必须落在 VARCHAR(255) 内。这里的值来自
	// Discovery 而不是本地配置,所以更需要在构造期挡一次:运行期才发现意味着
	// 每个用户在建号之后才 INSERT 失败(或被静默截断)。
	//
	// Client 为 nil 是单测/降级形态(见上),此时无 issuer 可查,跳过。
	if cfg.Client != nil {
		if n := len(cfg.Client.Issuer()); n > issuerMaxLen {
			return nil, fmt.Errorf("oidc: oidc provider: discovered issuer is %d bytes, exceeds "+
				"the %d-byte identity column width", n, issuerMaxLen)
		}
	}
	return &oidcProvider{cfg: cfg}, nil
}

func (p *oidcProvider) Kind() ProviderKind { return KindOIDC }

func (p *oidcProvider) Issuer() string { return p.cfg.Client.Issuer() }

// Capabilities 标准 OIDC 具备全部能力。
//
// 注意这里描述的是**协议**能力,不是当前配置是否齐备:UpstreamLogout=true
// 表示协议支持 RP-Initiated Logout,而端点或回跳地址没配时由 LogoutURL
// 返回 ("", false) 降级。
func (p *oidcProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		PKCE:           true,
		Nonce:          true,
		IDToken:        true,
		RefreshToken:   true,
		CrossCheckSub:  true, // id_token 与 /userinfo 两个独立来源
		UpstreamLogout: true,
	}
}

func (p *oidcProvider) AuthCodeURL(params AuthCodeParams) (string, error) {
	// Client 侧要求三者齐备(PKCE + nonce 是本 provider 的既有强制项)。
	return p.cfg.Client.AuthCodeURL(params.State, params.Nonce, params.CodeChallenge)
}

func (p *oidcProvider) Exchange(ctx context.Context, code, codeVerifier string) (*TokenSet, error) {
	tok, err := p.cfg.Client.Exchange(ctx, code, codeVerifier)
	if err != nil {
		return nil, err
	}
	// id_token 从 oauth2.Token 的 extra 里取 —— 这是唯一需要知道该细节的地方,
	// 抽象出 TokenSet 后 handler 不再直接 tok.Extra("id_token")。
	rawID, _ := tok.Extra("id_token").(string)
	return &TokenSet{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		RawIDToken:   rawID,
	}, nil
}

// Identity 验签 id_token,按需拉 /userinfo 补全并交叉校验 sub,返回合并后的 claims。
//
// 与迁移前的 handler 逻辑逐条对齐(见文件头注释的四条)。nonce 比对**不**在这里:
// 期望值存在 state 里、由 handler 持有,provider 只保证把解出的 nonce 放进
// claims.Nonce 交出去。
func (p *oidcProvider) Identity(ctx context.Context, tok *TokenSet) (*IdentityClaims, error) {
	if tok == nil {
		return nil, fmt.Errorf("%w: token set is nil", ErrIdentityUntrusted)
	}
	if strings.TrimSpace(tok.RawIDToken) == "" {
		return nil, fmt.Errorf("%w: id_token missing from token response", ErrIdentityUntrusted)
	}

	claims, err := p.cfg.Client.VerifyIDToken(ctx, tok.RawIDToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIdentityUntrusted, err)
	}
	// 契约要求:subject 非空。空 subject 配 UNIQUE(issuer,subject) 会把所有
	// 空 sub 用户塌成同一行,互相登进对方账号。标准 OIDC 下 go-oidc 已校验
	// 必需 claim,这里仍显式兜一道 —— 契约不该依赖上游库的实现细节。
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, fmt.Errorf("%w: subject is empty", ErrIdentityUntrusted)
	}
	// 形态守卫:上游断言的 subject 不能是"短纯数字"(像工号)。
	//
	// 签名证明这个 sub 是该 IdP 断言的,不证明它适合当不可变主键 —— 工号会在
	// 离职/入职之间被复用,于是 (issuer, subject) 迟早把新人指到前任的账号上。
	// 这条论证与协议无关,所以两个 provider 都要有;漂移由
	// TestAuthProviderConformance_RefusesShortNumericUpstreamSubject 钉住。
	//
	// 长度上限不在这里查 —— 那是存储性质,由共享入口统一施加(identity_bounds.go),
	// 覆盖包括业务 JWT 在内的每条路径。
	if err := checkUpstreamSubjectShape(strings.TrimSpace(claims.Subject)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIdentityUntrusted, err)
	}

	// 部分 IdP 只在 /userinfo 暴露 email/phone/name,id_token 仅含 sub。
	// 自动绑定历史账号依赖 email/phone;name 缺失会让新建用户落到随机名兜底。
	// 所以缺啥补啥。
	//
	// identity_verification scope 同理:不同部署把这 5 个字段分别放在 id_token
	// 或 /userinfo,甚至只放一部分。只要 scope 已配置且任一必需字段未就位,
	// 就触发一次合并。
	needUserInfo := claims.Email == "" || claims.PhoneNumber == "" || claims.Name == ""
	if !needUserInfo && hasIdentityVerificationScope(p.cfg.Scopes) &&
		!hasCompleteVerificationClaims(claims) {
		needUserInfo = true
	}
	if !needUserInfo {
		return claims, nil
	}

	ui, uerr := p.cfg.Client.UserInfo(ctx, &oauth2.Token{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	})
	if uerr != nil {
		// 不阻断登录:等价于 IdP 没返这些 claim,代价只是失去自动绑定能力。
		// 但必须让上层有机会记录 —— 这是排查"为何未自动绑定"的唯一线索。
		if p.cfg.OnWarn != nil {
			p.cfg.OnWarn("OIDC userinfo 拉取失败,跳过补全", uerr)
		}
		return claims, nil
	}
	// 安全检查:/userinfo 的 sub 必须等于 id_token 的 sub,否则视为账号串台。
	// 注意这条**不是** go-oidc 提供的 —— 它的 UserInfo 只发 GET 并解码,
	// 不做任何 sub 比对。
	if ui.Subject != claims.Subject {
		return nil, fmt.Errorf("%w: userinfo sub mismatch: idtoken=%s userinfo=%s",
			ErrIdentityUntrusted, subHash(claims.Subject), subHash(ui.Subject))
	}

	mergeUserInfoClaims(claims, ui)
	return claims, nil
}

// mergeUserInfoClaims 把 /userinfo 的字段合并进 id_token claims。
//
// 合并方向:id_token 是签名权威,只在其对应字段为空时才取 /userinfo 的值,
// 避免 IdP 两边不一致时静默覆盖。
// 例外是 IsVerified —— 它只有 true 才承载"已实名"语义,false 可能只是
// 该字段没放进 id_token,所以 /userinfo 的 true 可以覆盖(更新语义)。
func mergeUserInfoClaims(claims *IDTokenClaims, ui *UserInfoClaims) {
	if claims.Email == "" {
		claims.Email = ui.Email
		claims.EmailVerified = ui.EmailVerified
	}
	if claims.PhoneNumber == "" {
		claims.PhoneNumber = ui.PhoneNumber
		claims.PhoneVerified = ui.PhoneVerified
	}
	if claims.Name == "" {
		claims.Name = ui.Name
	}
	if ui.IsVerified.Bool() {
		claims.IsVerified = true
	}
	if claims.VerifiedAt == 0 {
		claims.VerifiedAt = ui.VerifiedAt
	}
	if claims.VerifiedProvider == "" {
		claims.VerifiedProvider = ui.VerifiedProvider
	}
	if claims.LegalName == "" {
		claims.LegalName = ui.LegalName
	}
	if claims.LegalEmail == "" {
		claims.LegalEmail = ui.LegalEmail
	}
}

// LogoutURL 构造 RP-Initiated Logout 地址。
//
// 与迁移前的 buildEndSessionURL 行为一致:端点取 config override 优先、否则
// Discovery 值;强制绝对 https(拦 IdP 万一下发 http 把带 id_token 的 URL 降级);
// 材料不全时返回 ("", false) 让上层降级为仅清本地。
//
// id_token 由 handler 从其一次性存储里取出后经 hint 传入 —— provider 不持有
// 该存储,避免与 Redis 设施耦合。
func (p *oidcProvider) LogoutURL(_ context.Context, hint LogoutHint) (string, bool) {
	if p.cfg.PostLogoutRedirectURI == "" || strings.TrimSpace(hint.RawIDToken) == "" {
		return "", false
	}
	endpoint := p.endSessionEndpoint()
	if endpoint == "" {
		return "", false
	}
	if err := validateLogoutURL("end_session_endpoint", endpoint); err != nil {
		return "", false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false
	}
	q := u.Query()
	q.Set("id_token_hint", hint.RawIDToken)
	q.Set("post_logout_redirect_uri", p.cfg.PostLogoutRedirectURI)
	u.RawQuery = q.Encode()
	return u.String(), true
}

// endSessionEndpoint config override 优先,否则取 Discovery 解析值。
func (p *oidcProvider) endSessionEndpoint() string {
	if p.cfg.EndSessionURLOverride != "" {
		return p.cfg.EndSessionURLOverride
	}
	return p.cfg.Client.EndSessionEndpoint()
}

// IdentityFromClientCredential 把客户端出示的 id_token 验签后映射为 claims。
//
// 标准 OIDC 下客户端手上唯一可独立验证的凭据就是 id_token —— access_token 在
// 这个协议里是给资源服务器用的不透明串,拿它当身份证明等于不验签。所以这里
// 走的是与 Identity 相同的验签路径,只是凭据从"我方换回来的 TokenSet"变成
// "客户端带过来的字符串"。
//
// 复用 Identity 而不是直接调 VerifyIDToken,是为了让 /userinfo 交叉校验、nonce
// 处理这些步骤对两种来源保持一致 —— 少走任何一步都是一处只在这条路径上存在的
// 安全缺口。nonce 留空:客户端出示凭据时没有我方发起的 authorize 请求可绑定,
// Identity 对空 nonce 的处理与 callback 路径一致。
func (p *oidcProvider) IdentityFromClientCredential(ctx context.Context, raw string) (*IdentityClaims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: credential is empty", ErrIdentityUntrusted)
	}
	return p.Identity(ctx, &TokenSet{RawIDToken: raw})
}

// EndSessionEndpoint 实现 AuthProvider:标准 OIDC 有 RP-Initiated Logout。
//
// 值来自 Discovery,允许 override(部分部署的 Discovery 文档不声明该端点)。
// 端点是否可用(https、可解析)由调用方校验 —— 这里只负责如实报出配置值。
func (p *oidcProvider) EndSessionEndpoint() string { return p.endSessionEndpoint() }
