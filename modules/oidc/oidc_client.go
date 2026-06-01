package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// IsVerifiedClaim 兜底 is_verified 的 bool / number / string 三种序列化形态。
//
// 背景:Aegis 当前实测把 is_verified 返成 JSON bool,但对接文档又写成 string
// ("true" / "false")。Keycloak / Auth0 / Okta 等 IdP 的自定义 claim 写法也普遍
// 按厂商管控面下发成 string(管理后台手动填值,UI 落 JSON 时加了引号)。
//
// aud 字段历史踩过一次类似坑(Verify 阶段 json.Unmarshal TypeError 直接挂掉所有登录,
// 见 IDTokenClaims 的 aud 注释)。为防下次 IdP 把 wire 类型改了就全站登录失败,
// is_verified 用 Custom Unmarshal 接 bool / number / string / null。
//
// 语义映射:
//   - JSON bool  true / false       → true / false
//   - JSON number 0/1 及其他         → 非零为 true
//   - JSON string "true" / "1" /
//     "yes"(大小写/空白不敏感)     → true
//   - 其他字符串 / null / 缺字段     → false(保守,等价于 "未实名")
//   - 类型完全无法识别时保留 error,落到 decode 阶段 → 登录失败但会带可读错误,
//     而不是整个 Unmarshal 静默吞掉其他字段
type IsVerifiedClaim bool

// Bool 返回内部 bool 值,用于和 Go 原生 bool 互转(比如写 user 模块时)。
func (c IsVerifiedClaim) Bool() bool { return bool(c) }

func (c *IsVerifiedClaim) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*c = false
		return nil
	}
	// try JSON bool
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*c = IsVerifiedClaim(b)
		return nil
	}
	// try JSON number (int / float)
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		*c = IsVerifiedClaim(n != 0)
		return nil
	}
	// try JSON string("true"/"1"/"yes" 视为 true,其他为 false)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			*c = true
		default:
			*c = false
		}
		return nil
	}
	return fmt.Errorf("oidc: IsVerifiedClaim: unsupported JSON type: %s", string(data))
}

// VerifiedAtClaim 兜底 verified_at 的 number / string 两种序列化形态(Unix 秒)。
//
// 背景:Aegis 当前实测返 JSON number(Unix 秒),但:
//   - 部分 IdP 管理后台把数字类型 claim 落成 string("1778331902")
//   - 某些前置网关把大数用 JSON number 下发时会变成 JS float(1.778e9),
//     Go 直接 Unmarshal 成 int64 会 TypeError(float64 不能收 int64)
//
// 处理:接 int64 / float64 / string("123")/ null / 缺字段。非法值返回 0,
// 下游 hasCompleteVerificationClaims / UpsertVerificationFromOIDC 已在 VerifiedAt<=0
// 分支拒写库,不会污染 user_verification 表。
type VerifiedAtClaim int64

// Int64 返回内部 Unix 秒(与 time.Unix(sec, 0) 对齐)。
func (c VerifiedAtClaim) Int64() int64 { return int64(c) }

func (c *VerifiedAtClaim) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*c = 0
		return nil
	}
	// try JSON int64(最常见,也最准确,不走 float 丢精度)
	var i int64
	if err := json.Unmarshal(data, &i); err == nil {
		*c = VerifiedAtClaim(i)
		return nil
	}
	// try JSON float(JS/网关 Stringify 过一遍的 Unix 秒)
	var f float64
	if err := json.Unmarshal(data, &f); err == nil {
		*c = VerifiedAtClaim(int64(f))
		return nil
	}
	// try JSON string("1778331902")
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			*c = 0
			return nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			*c = VerifiedAtClaim(n)
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			*c = VerifiedAtClaim(int64(f))
			return nil
		}
		// 字符串非数字:保守当 0(未提供实名时间),让下游 VerifiedAt<=0 分支保护。
		*c = 0
		return nil
	}
	return fmt.Errorf("oidc: VerifiedAtClaim: unsupported JSON type: %s", string(data))
}

// ClientConfig OIDC Client 构造参数。从 ProviderConfig 派生,只保留 Client 自身需要的字段。
type ClientConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string

	HTTPTimeout time.Duration
	ClockSkew   time.Duration
}

// Client 封装 go-oidc Provider + oauth2.Config。
//
// 接口设计:Client 不持任何请求级状态(state / nonce / verifier),由 service 层管理;
// Client 只做 Discovery / 构造授权 URL / 换 token / 验签 / 拉 userinfo / 刷新这几件事。
type Client struct {
	cfg      ClientConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth2   *oauth2.Config
	http     *http.Client

	// endSession 从 Discovery 文档的 end_session_endpoint 解出(RP-Initiated Logout)。
	// go-oidc 的 Provider 只暴露 auth/token/userinfo/jwks,end_session 需要额外
	// 从原始 metadata claims 取。Discovery 未声明时为空,logout 退回 config override 或降级。
	endSession string
}

// NewClient 走 Discovery 拉 issuer metadata,构造 Client。
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	hc := &http.Client{Timeout: cfg.HTTPTimeout}
	dctx := oidc.ClientContext(ctx, hc)

	provider, err := oidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover issuer %q: %w", cfg.Issuer, err)
	}

	// ClockSkew 兜底:dmwork 服务器时钟若领先 IdP,exp 检查会把刚过期的 token 拒掉。
	// go-oidc v3.9 的 oidc.Config 没有专门的 leeway 字段,但暴露了 Now func — 把
	// "当前时间"往回拨 skew,等价于 exp/iat 各加 skew 的容忍。
	// 副作用:iat-in-future 检查会更严(now 变小),实践上 IdP 不会签 iat>>now 的 token,
	// 所以这里影响可忽略;最关键的是允许接收方时钟轻微领先时仍能接受快到期的 token。
	skew := cfg.ClockSkew
	verifier := provider.Verifier(&oidc.Config{
		ClientID: cfg.ClientID,
		Now: func() time.Time {
			return time.Now().Add(-skew)
		},
	})

	// end_session_endpoint 不在 go-oidc 的 Provider 公开字段里,从原始 Discovery
	// metadata 解。解析失败/字段缺失时留空 —— 不阻断 Client 构造,logout 自行降级。
	var extra struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&extra)

	return &Client{
		cfg:      cfg,
		provider: provider,
		verifier: verifier,
		oauth2: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURI,
			Endpoint:     provider.Endpoint(),
			Scopes:       cfg.Scopes,
		},
		http:       hc,
		endSession: extra.EndSessionEndpoint,
	}, nil
}

// EndSessionEndpoint 返回 Discovery 暴露的 RP-Initiated Logout 端点;
// IdP 未声明时返回空字符串。
func (c *Client) EndSessionEndpoint() string { return c.endSession }

// Issuer 返回 issuer URL(便于断言/审计)。
func (c *Client) Issuer() string { return c.cfg.Issuer }

// Refresh 用 refresh_token 换新 token。RT 轮换语义由 IdP 决定,
// 调用方拿到新 token 后应替换 DB 中的密文 RT(rotate)。
//
// 失败语义:
//   - "invalid_grant" 等价于 RT 失效(被吊销 / 用户登出 / 自然过期),
//     调用方应把对应 identity 标记吊销,前端引导重新登录。
//   - 其他错误(网络 / 5xx)调用方可重试。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("oidc: Refresh: refreshToken required")
	}
	dctx := oidc.ClientContext(ctx, c.http)
	ts := c.oauth2.TokenSource(dctx, &oauth2.Token{RefreshToken: refreshToken})
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("oidc: refresh: %w", err)
	}
	return tok, nil
}

// UserInfoClaims /userinfo 端点返回的 claims(子集,与 IDTokenClaims 交集字段)。
//
// /userinfo 比 ID Token 通常更新(IdP 侧后台变更可立即反映),
// 是登录后做"账户信息同步"的权威源。
//
// identity_verification scope 下的 5 个字段也从 /userinfo 解出:部分 Aegis
// 部署把它们放 ID Token,另一些只在 /userinfo 暴露,拉完 /userinfo 后由
// callback 合并到 IDTokenClaims(YUJ-382 codex review 点)。
type UserInfoClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	PhoneNumber   string `json:"phone_number"`
	PhoneVerified bool   `json:"phone_number_verified"`
	Name          string `json:"name"`

	// identity_verification scope 字段。类型与 IDTokenClaims 一致(POC 实测 + bool/number/string 兜底)。
	IsVerified       IsVerifiedClaim `json:"is_verified"`
	VerifiedAt       VerifiedAtClaim `json:"verified_at"`
	VerifiedProvider string          `json:"verified_provider"`
	LegalName        string          `json:"legal_name"`
	LegalEmail       string          `json:"legal_email"`
}

// UserInfo 用 oauth2.Token 拉 /userinfo claims。
//
// go-oidc 的 UserInfo 内部会校验 sub 与 ID Token 的 sub 一致性(若 token 含 ID Token);
// 这里只暴露 claims,sub 校验由调用方决定信不信任。
func (c *Client) UserInfo(ctx context.Context, tok *oauth2.Token) (*UserInfoClaims, error) {
	if tok == nil {
		return nil, fmt.Errorf("oidc: UserInfo: token required")
	}
	dctx := oidc.ClientContext(ctx, c.http)
	ui, err := c.provider.UserInfo(dctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, fmt.Errorf("oidc: fetch userinfo: %w", err)
	}
	var claims UserInfoClaims
	if err := ui.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: decode userinfo claims: %w", err)
	}
	if claims.Subject == "" {
		claims.Subject = ui.Subject
	}
	return &claims, nil
}

// IDTokenClaims 解析后的 ID Token 标准 claims + 业务关心字段。
//
// 不暴露原始 Token 对象,避免上层耦合 go-oidc 类型。
type IDTokenClaims struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	// 不解 aud:OIDC Core §2 允许 aud 是 string 或 []string,go-oidc 的 Verify
	// 已经做了完整 aud 校验,业务层不需要这个字段。曾经写成 `string` 接,Keycloak /
	// Auth0 多 audience 场景返回 ["client-id"] 时 json.Unmarshal 会 TypeError
	// 直接挂掉所有登录 — 删字段比加自定义 unmarshal 简单。
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	PhoneNumber   string `json:"phone_number"`
	PhoneVerified bool   `json:"phone_number_verified"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
	IssuedAt      int64  `json:"iat"`
	Expiry        int64  `json:"exp"`

	// Aegis identity_verification scope claims(YUJ-382 / Aegis OIDC Phase 1)。
	//
	// 类型按 POC 实测 + wire-format 兜底:Aegis 对接文档把 is_verified / verified_at 写成 string,
	// 实际 token payload 里 is_verified 是 JSON bool、verified_at 是 Unix 秒(number)。
	// 任何一端改了 wire 类型(比如切 Keycloak/Auth0,或 Aegis 管理后台把字段重落 string)
	// 就直接全站登录失败(json.Unmarshal TypeError),aud 字段历史已经踩过一次坑。
	// 这里用 IsVerifiedClaim / VerifiedAtClaim 自定义 UnmarshalJSON 兜底 bool/number/string。
	//
	// 语义:
	//   - IsVerified=true + LegalName != "" 视为"该 IdP 返回了一次有效实名结果",
	//     dmworkim 侧据此 upsert user_verification 表。
	//   - VerifiedProvider 是具体来源域名(如 "cas.example.com"),strip 到一级
	//     (cas/wecom/feishu)再写库 + allowlist 校验,防 Aegis 返回意外值污染 source。
	//   - LegalEmail 允许空,LegalName 必填(空字符串视为未实名,不写库)。
	IsVerified       IsVerifiedClaim `json:"is_verified"`
	VerifiedAt       VerifiedAtClaim `json:"verified_at"`
	VerifiedProvider string          `json:"verified_provider"`
	LegalName        string          `json:"legal_name"`
	LegalEmail       string          `json:"legal_email"`
}

// VerifyIDToken 用 issuer JWKS 验签 ID Token,并把 claims 解码到 IDTokenClaims。
//
// 不在此处校验 nonce(由 service 层对 StateStore 中的 nonce 比较),
// 这里只做 RFC 7519 / OIDC Core §3.1.3.7 的签名 + iss + aud + exp 校验(go-oidc 默认行为)。
func (c *Client) VerifyIDToken(ctx context.Context, rawIDToken string) (*IDTokenClaims, error) {
	if rawIDToken == "" {
		return nil, fmt.Errorf("oidc: VerifyIDToken: rawIDToken required")
	}
	tok, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	var claims IDTokenClaims
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: decode id_token claims: %w", err)
	}
	return &claims, nil
}

// Exchange 用 authorization_code + code_verifier 换 token。
//
// PKCE 的 code_verifier 通过 oauth2 的 SetAuthURLParam 传给 IdP;调用方需
// 确保和 AuthCodeURL 阶段的 challenge 是同一对(从 StateStore 取出)。
func (c *Client) Exchange(ctx context.Context, code, codeVerifier string) (*oauth2.Token, error) {
	if code == "" {
		return nil, fmt.Errorf("oidc: Exchange: code required")
	}
	dctx := oidc.ClientContext(ctx, c.http)
	tok, err := c.oauth2.Exchange(dctx, code,
		oauth2.SetAuthURLParam("code_verifier", codeVerifier),
	)
	if err != nil {
		return nil, fmt.Errorf("oidc: token exchange: %w", err)
	}
	return tok, nil
}

// AuthCodeURL 构造带 PKCE(S256) + state + nonce 的授权 URL。
//
// 调用方负责生成并持久化 state / nonce / code_verifier(写 StateStore),
// 这里只把 code_challenge 透传给 IdP。verifier 永远不上 URL。
func (c *Client) AuthCodeURL(state, nonce, codeChallenge string) (string, error) {
	if state == "" || nonce == "" || codeChallenge == "" {
		return "", fmt.Errorf("oidc: AuthCodeURL: state/nonce/codeChallenge required")
	}
	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	return c.oauth2.AuthCodeURL(state, opts...), nil
}
